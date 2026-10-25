//go:build integration

package runner_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"frplink/internal/model"
	"frplink/internal/runner"
	"frplink/internal/store"
)

var frpsBin string

// testContext 返回随测试结束自动取消的 context。
func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "frplink-it")
	if err != nil {
		fmt.Println("tempdir:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	name := "frps"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	frpsBin = filepath.Join(dir, name)
	// 在独立临时模块中构建 frps（避免把服务端依赖混入主工程 go.mod），
	// noweb 标签跳过未随源码发布的内嵌 Web 资源。
	buildDir := filepath.Join(dir, "build")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		fmt.Println("mkdir:", err)
		os.Exit(1)
	}
	script := "go mod init frpsbuild >/dev/null 2>&1; GOFLAGS=-mod=mod go get github.com/fatedier/frp/cmd/frps@v0.71.1 && go build -tags noweb -o '" +
		frpsBin + "' github.com/fatedier/frp/cmd/frps"
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("bash", "-c", script)
	} else {
		cmd = exec.Command("sh", "-c", script)
	}
	cmd.Dir = buildDir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("build frps failed: %v\n%s\n", err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// freePort 申请一个临时可用端口（监听后立即释放，存在微小竞争窗口）。
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startEcho 在本地启动回显服务，返回监听地址。
func startEcho(t *testing.T, addr string) {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	t.Cleanup(func() { l.Close() })
}

// startFrps 启动真实 frps。
func startFrps(t *testing.T, cfg string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "frps.toml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(dir, "frps.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(frpsBin, "-c", path)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		logFile.Close()
	})
}

func waitStatus(t *testing.T, m *runner.Manager, id string, want runner.Status, timeout time.Duration) runner.ServerStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last runner.ServerStatus
	for time.Now().Before(deadline) {
		last = m.Status(id)
		if last.Status == want {
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待状态 %s 超时, 当前 %s (err=%s attempts=%d)", want, last.Status, last.LastErr, last.Attempts)
	return last
}

func newTestStore(t *testing.T, entries ...*model.ServerEntry) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "servers.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, err := s.Create(e); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// TestConnectAndTCPTunnel 端到端: 真实 frps + runner + TCP 穿透回显验证。
func TestConnectAndTCPTunnel(t *testing.T) {
	frpsPort := freePort(t)
	echoPort := freePort(t)
	remotePort := freePort(t)

	startFrps(t, fmt.Sprintf("bindPort = %d\nauth.token = \"it-token\"\n", frpsPort))
	startEcho(t, fmt.Sprintf("127.0.0.1:%d", echoPort))

	s := newTestStore(t, &model.ServerEntry{
		Name:       "集成测试节点",
		ServerAddr: "127.0.0.1",
		ServerPort: frpsPort,
		Token:      "it-token",
		TLS:        true,
		AutoRetry:  true,
		Proxies: []model.ProxyRule{
			{Name: "echo", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: echoPort, RemotePort: remotePort},
		},
	})
	entry := s.List()[0]

	m := runner.New(testContext(t), s)
	if err := m.Start(entry.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.StopAll(5 * time.Second) })

	st := waitStatus(t, m, entry.ID, runner.StatusOnline, 15*time.Second)
	if len(st.Proxies) != 1 || st.Proxies[0].Phase != "running" {
		t.Fatalf("代理状态异常: %+v", st.Proxies)
	}

	// 通过 frps 上暴露的远程端口访问本地回显
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", remotePort), 5*time.Second)
	if err != nil {
		t.Fatalf("连接穿透端口失败: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	msg := "hello-frplink"
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != msg {
		t.Fatalf("回显内容不符: %q", buf)
	}

	// 停止后状态应回到 stopped
	m.Stop(entry.ID, 5*time.Second)
	if st := m.Status(entry.ID); st.Status != runner.StatusStopped {
		t.Fatalf("停止后状态应为 stopped, got %s", st.Status)
	}
}

// TestWrongToken 错误令牌: AutoRetry=false 时应进入 error 状态并带错误信息。
func TestWrongToken(t *testing.T) {
	frpsPort := freePort(t)
	startFrps(t, fmt.Sprintf("bindPort = %d\nauth.token = \"right\"\n", frpsPort))

	s := newTestStore(t, &model.ServerEntry{
		Name:       "错误令牌节点",
		ServerAddr: "127.0.0.1",
		ServerPort: frpsPort,
		Token:      "wrong",
		TLS:        true,
		AutoRetry:  false,
		Proxies: []model.ProxyRule{
			{Name: "echo", Type: "tcp", LocalPort: 80, RemotePort: 18080},
		},
	})
	entry := s.List()[0]

	m := runner.New(testContext(t), s)
	if err := m.Start(entry.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.StopAll(5 * time.Second) })

	st := waitStatus(t, m, entry.ID, runner.StatusError, 15*time.Second)
	if st.LastErr == "" {
		t.Fatalf("error 状态应包含错误信息")
	}
}

// TestTestConnection 连接测试: 正确令牌应成功并返回耗时, 错误令牌应返回登录失败。
func TestTestConnection(t *testing.T) {
	frpsPort := freePort(t)
	startFrps(t, fmt.Sprintf("bindPort = %d\nauth.token = \"right\"\n", frpsPort))

	ctx := testContext(t)
	waitPortReady(t, frpsPort, 5*time.Second)
	okEntry := &model.ServerEntry{
		Name: "测试连接-正确", ServerAddr: "127.0.0.1", ServerPort: frpsPort,
		Token: "right", TLS: true,
	}
	ms, err := runner.TestConnection(ctx, okEntry, 15*time.Second)
	if err != nil {
		t.Fatalf("正确令牌应当测试通过: %v (took %dms)", err, ms)
	}
	t.Logf("连接测试耗时 %dms", ms.Milliseconds())

	badEntry := &model.ServerEntry{
		Name: "测试连接-错误", ServerAddr: "127.0.0.1", ServerPort: frpsPort,
		Token: "wrong", TLS: true,
	}
	if _, err := runner.TestConnection(ctx, badEntry, 15*time.Second); err == nil {
		t.Fatalf("错误令牌应当测试失败")
	} else {
		t.Logf("错误令牌返回: %v", err)
	}

	unreach := &model.ServerEntry{
		Name: "测试连接-不可达", ServerAddr: "127.0.0.1", ServerPort: freePort(t),
		Token: "x", TLS: true,
	}
	if _, err := runner.TestConnection(ctx, unreach, 5*time.Second); err == nil {
		t.Fatalf("不可达地址应当测试失败")
	}
}

// waitPortReady 等待端口可连接（frps 进程启动需要一点时间）。
func waitPortReady(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("frps 端口 %d 在 %s 内未就绪", port, timeout)
}

// TestConnectTimeout 连接看门狗: 不可达地址持续重试超过设置的超时秒数后应自动停止。
func TestConnectTimeout(t *testing.T) {
	unreach := freePort(t)
	s := newTestStore(t, &model.ServerEntry{
		Name: "超时测试节点", ServerAddr: "127.0.0.1", ServerPort: unreach,
		Token: "x", TLS: true, AutoRetry: true, // 持续重试, 让看门狗触发
		Proxies: []model.ProxyRule{{Name: "p", Type: "tcp", LocalPort: 80, RemotePort: 18080}},
	})
	if err := s.SaveSettings(store.Settings{ConnectTimeoutSec: 3}); err != nil {
		t.Fatal(err)
	}
	entry := s.List()[0]
	m := runner.New(testContext(t), s)
	if err := m.Start(entry.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.StopAll(3 * time.Second) })

	st := waitStatus(t, m, entry.ID, runner.StatusError, 10*time.Second)
	if !strings.Contains(st.LastErr, "连接超时") {
		t.Fatalf("应为连接超时错误, got: %s", st.LastErr)
	}
}
