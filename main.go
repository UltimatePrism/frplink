// FRPLink —— 基于 frp 客户端内核的多服务器图形界面管理器。
//
// 默认启动本地 Web GUI；另提供以下子命令：
//
//	frplink                  启动图形界面（自动打开浏览器）
//	frplink run              无界面运行（启动所有"随程序启动"的服务器）
//	frplink import <分享码>   粘贴分享码，直接添加服务器
//	frplink gencode -c frps.toml   [服务端] 从 frps 配置生成分享码
//	frplink list             列出已配置的服务器
//	frplink export [-id xx]  导出服务器的分享码
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	frpconfig "github.com/fatedier/frp/pkg/config"
	frplog "github.com/fatedier/frp/pkg/util/log"
	frpversion "github.com/fatedier/frp/pkg/util/version"
	golog "github.com/fatedier/golib/log"

	"frplink/internal/logbuf"
	"frplink/internal/model"
	"frplink/internal/runner"
	"frplink/internal/server"
	"frplink/internal/sharecode"
	"frplink/internal/store"
)

// Version 程序版本。
var Version = "0.1.0"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "run":
			cmdRun(os.Args[2:])
			return
		case "import":
			cmdImport(os.Args[2:])
			return
		case "gencode":
			cmdGenCode(os.Args[2:])
			return
		case "list":
			cmdList(os.Args[2:])
			return
		case "export":
			cmdExport(os.Args[2:])
			return
		case "version", "-v", "--version":
			fmt.Printf("FRPLink %s (frp kernel %s, %s/%s)\n", Version, frpversion.Full(), runtime.GOOS, runtime.GOARCH)
			return
		case "help", "-h", "--help":
			usage()
			return
		default:
			// 不认识的子命令按 GUI 启动参数处理（flag 包会报错）
		}
	}
	cmdGUI(os.Args[1:])
}

func usage() {
	fmt.Print(`FRPLink —— 基于 frp 的多服务器穿透客户端 (GUI)

用法:
  frplink [选项]                 启动图形界面
  frplink run [选项]             无界面运行（启动所有标记"随程序启动"的服务器）
  frplink import <分享码> [选项]  粘贴分享码添加服务器
  frplink gencode [选项]         【服务端】生成分享码（可直接读取 frps 配置）
  frplink list                   列出已配置的服务器
  frplink export [-id xx|-all]   导出分享码
  frplink version                显示版本

GUI 选项:
  -addr string   监听地址 (默认 127.0.0.1:0 随机端口)
  -data string   数据目录 (默认 <用户配置目录>/frplink)
  -token string  固定访问密钥 (默认每次启动随机生成)
  -no-browser    不自动打开浏览器
`)
}

// ---- 公共 ----

func openStore(dataDir string) *store.Store {
	path := ""
	if dataDir != "" {
		path = dataDir + string(os.PathSeparator) + "servers.json"
	}
	st, err := store.Open(path)
	if err != nil {
		fatal("打开配置存储失败: %v", err)
	}
	return st
}

func fatal(format string, v ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", v...)
	os.Exit(1)
}

func randToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// parseProxySpec 解析 gencode 的 -proxy 参数: TYPE:LOCALIP:LOCALPORT:REMOTE
// tcp/udp 的 REMOTE 为远程端口; http/https 为逗号分隔域名; stcp/xtcp/sudp 为密钥。
func parseProxySpec(spec string) (model.ProxyRule, error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 4 {
		return model.ProxyRule{}, fmt.Errorf("格式应为 TYPE:本地IP:本地端口:远程端口或域名")
	}
	t := parts[0]
	lp, err := strconv.Atoi(parts[2])
	if err != nil {
		return model.ProxyRule{}, fmt.Errorf("本地端口必须是数字")
	}
	r := model.ProxyRule{Type: t, LocalIP: parts[1], LocalPort: lp}
	switch t {
	case "tcp", "udp":
		rp, err := strconv.Atoi(parts[3])
		if err != nil {
			return model.ProxyRule{}, fmt.Errorf("%s 的第 4 段应为远程端口数字", t)
		}
		r.RemotePort = rp
	case "http", "https":
		for _, d := range strings.Split(parts[3], ",") {
			if d = strings.TrimSpace(d); d != "" {
				r.CustomDomains = append(r.CustomDomains, d)
			}
		}
	case "stcp", "xtcp", "sudp":
		r.SecretKey = parts[3]
	default:
		return model.ProxyRule{}, fmt.Errorf("不支持的类型 %q（可选 tcp/udp/http/https/stcp/xtcp/sudp）", t)
	}
	return r, nil
}

// ---- GUI ----

func cmdGUI(args []string) {
	fs := flag.NewFlagSet("frplink", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:0", "监听地址")
	dataDir := fs.String("data", "", "数据目录")
	token := fs.String("token", "", "固定访问密钥")
	noBrowser := fs.Bool("no-browser", false, "不自动打开浏览器")
	_ = fs.Parse(args)

	if *token == "" {
		*token = randToken()
	}
	st := openStore(*dataDir)
	logs := logbuf.New(1500)
	// 把 frp 内核日志同时送往控制台与 GUI 日志面板
	frplog.Logger = frplog.Logger.WithOptions(golog.WithOutput(io.MultiWriter(os.Stdout, logs)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mgr := runner.New(ctx, st)
	srv := server.New(st, mgr, logs, *token, Version, st.Path())

	netAddr, err := srv.Listen(*addr)
	if err != nil {
		fatal("监听 %s 失败: %v", *addr, err)
	}
	url := fmt.Sprintf("http://%s/?k=%s", netAddr.String(), *token)

	fmt.Printf("──────────────────────────────────────────────\n")
	fmt.Printf("  FRPLink %s  (frp 内核 v%s)\n", Version, frpversion.Full())
	fmt.Printf("  配置文件: %s\n", st.Path())
	fmt.Printf("  管理界面: %s\n", url)
	fmt.Printf("  浏览器没有自动打开的话，请手动复制上面的地址访问\n")
	fmt.Printf("──────────────────────────────────────────────\n")

	mgr.StartAutoStart()
	if !*noBrowser {
		openBrowser(url)
	}

	<-ctx.Done()
	fmt.Println("\n正在停止全部连接…")
	mgr.StopAll(8 * time.Second)
	fmt.Println("已退出。")
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// ---- run（无界面） ----

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dataDir := fs.String("data", "", "数据目录")
	_ = fs.Parse(args)
	st := openStore(*dataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mgr := runner.New(ctx, st)
	mgr.StartAutoStart()
	fmt.Printf("FRPLink %s 无界面运行中（Ctrl+C 退出），共 %d 个自动启动项\n", Version, countAutoStart(st))
	<-ctx.Done()
	fmt.Println("\n正在停止…")
	mgr.StopAll(8 * time.Second)
}

func countAutoStart(st *store.Store) int {
	n := 0
	for _, e := range st.List() {
		if e.AutoStart {
			n++
		}
	}
	return n
}

// ---- import ----

func cmdImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	name := fs.String("name", "", "覆盖名称")
	dataDir := fs.String("data", "", "数据目录")
	_ = fs.Parse(args)

	// Go flag 在首个位置参数后停止解析：先定位分享码，再把其余参数重新解析一遍
	_ = fs.Parse(args)
	code := ""
	if fs.NArg() >= 1 {
		code = fs.Arg(0)
	} else {
		for _, a := range args {
			if strings.HasPrefix(strings.ToUpper(a), "FRPLINK1.") {
				code = a
				break
			}
		}
	}
	if code == "" {
		fatal("用法: frplink import <分享码> [-name 名称]（也可: frplink import -name 名称 <分享码>）")
	}
	filtered := make([]string, 0, len(args))
	for _, a := range args {
		if a != code {
			filtered = append(filtered, a)
		}
	}
	if len(filtered) != len(args) {
		_ = fs.Parse(filtered)
	}
	p, err := sharecode.Decode(code)
	if err != nil {
		fatal("%v", err)
	}
	entry, err := sharecode.ToServerEntry(p, *name)
	if err != nil {
		fatal("%v", err)
	}
	st := openStore(*dataDir)
	created, err := st.Create(entry)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Printf("✅ 已导入服务器「%s」(%s:%d)，%d 条穿透规则\n",
		created.Name, created.ServerAddr, created.ServerPort, len(created.Proxies))
	fmt.Println("   启动: 打开 GUI 界面点启动，或运行 frplink run（需勾选随程序启动）")
}

// ---- gencode（服务端生成分享码） ----

func cmdGenCode(args []string) {
	fs := flag.NewFlagSet("gencode", flag.ExitOnError)
	cfgPath := fs.String("c", "", "frps 配置文件路径 (frps.toml/frps.ini/frps.yaml)")
	name := fs.String("name", "", "备注名（对方看到的名称）")
	user := fs.String("user", "", "用户名")
	addr := fs.String("addr", "", "服务器地址（默认取 frps 配置；为 0.0.0.0 时必须指定）")
	port := fs.Int("port", 0, "服务器端口（默认取 frps 配置）")
	token := fs.String("token", "", "认证令牌（默认取 frps 配置）")
	tls := fs.Bool("tls", true, "是否启用 TLS")
	note := fs.String("note", "", "给对方的备注")
	expire := fs.String("expire", "", "过期时间：时长(30d/12h) 或日期(2027-01-31)")
	_ = fs.String("proxy", "", "预置穿透规则 TYPE:本地IP:本地端口:远程端口或域名（可多次指定）")
	_ = fs.Parse(args)

	// flag 包不支持重复标量参数，手动扫描 args 收集全部 -proxy（支持 -proxy=x 与 -proxy x 两种写法）
	var proxyRules []model.ProxyRule
	for i := 0; i < len(args); i++ {
		a := args[i]
		var val string
		switch {
		case strings.HasPrefix(a, "-proxy="):
			val = strings.TrimPrefix(a, "-proxy=")
		case a == "-proxy" || a == "--proxy":
			if i+1 >= len(args) {
				fatal("-proxy 缺少参数值")
			}
			i++
			val = args[i]
		default:
			continue
		}
		r, err := parseProxySpec(val)
		if err != nil {
			fatal("解析 -proxy 失败: %v", err)
		}
		proxyRules = append(proxyRules, r)
	}

	p := &sharecode.Payload{
		Ver: 1, Name: *name, User: *user, TLS: tls, Note: *note, Px: proxyRules,
	}

	if *cfgPath != "" {
		scfg, isLegacy, err := frpconfig.LoadServerConfig(*cfgPath, false)
		if err != nil {
			fatal("读取 frps 配置失败: %v", err)
		}
		if isLegacy {
			fmt.Println("提示: 检测到旧版 ini 格式，建议迁移到 toml")
		}
		if *addr == "" {
			bind := scfg.BindAddr
			if bind == "" || bind == "0.0.0.0" || bind == "::" {
				fatal("frps 监听地址为 %q，无法作为对外地址；请用 -addr 指定公网 IP 或域名", bind)
			}
			p.Addr = bind
		}
		if *port == 0 {
			p.Port = scfg.BindPort
		}
		if *token == "" {
			p.Token = scfg.Auth.Token
		}
	}
	if *addr != "" {
		p.Addr = *addr
	}
	if *port != 0 {
		p.Port = *port
	}
	if *token != "" {
		p.Token = *token
	}
	if p.Addr == "" {
		fatal("缺少服务器地址：请用 -c 提供 frps 配置，或用 -addr 指定")
	}
	if p.Port == 0 {
		p.Port = 7000
	}
	if *expire != "" {
		var t time.Time
		if d, err := time.ParseDuration(*expire); err == nil {
			t = time.Now().Add(d)
		} else if v, err := time.ParseInLocation("2006-01-02", *expire, time.Local); err == nil {
			t = v
		} else if v, err := time.ParseInLocation("2006-01-02 15:04", *expire, time.Local); err == nil {
			t = v
		} else {
			fatal("-expire 格式应为 30d/12h 或 2027-01-31")
		}
		p.Exp = t.Unix()
	}
	if p.Name == "" {
		p.Name = p.Addr
	}

	code, err := sharecode.Encode(p)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Printf("────────── FRPLINK 分享码（复制发给使用者）──────────\n\n")
	fmt.Println(code)
	fmt.Printf("\n──────────────────────────────────────────────\n")
	fmt.Printf("服务器: %s:%d  TLS=%v  令牌=%s  预置规则=%d 条",
		p.Addr, p.Port, *tls, maskStr(p.Token), len(proxyRules))
	if p.Exp > 0 {
		fmt.Printf("  过期: %s", time.Unix(p.Exp, 0).Format("2006-01-02 15:04"))
	}
	fmt.Println()
}

func maskStr(s string) string {
	if s == "" {
		return "(无)"
	}
	if len(s) <= 4 {
		return "****"
	}
	return s[:2] + "****" + s[len(s)-2:]
}

// ---- list / export ----

func cmdList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	dataDir := fs.String("data", "", "数据目录")
	_ = fs.Parse(args)
	st := openStore(*dataDir)
	list := st.List()
	if len(list) == 0 {
		fmt.Println("（还没有已配置的服务器）")
		return
	}
	for _, e := range list {
		fmt.Printf("[%s] %s  →  %s:%d  user=%s tls=%v  穿透 %d 条  随程序启动=%v\n",
			e.ID[:8], e.Name, e.ServerAddr, e.ServerPort, orDash(e.User), e.TLS, len(e.Proxies), e.AutoStart)
		for _, r := range e.Proxies {
			target := strconv.Itoa(r.RemotePort)
			if r.Type == "http" || r.Type == "https" {
				target = strings.Join(r.CustomDomains, ",")
				if r.SubDomain != "" {
					target += " (sub:" + r.SubDomain + ")"
				}
			}
			fmt.Printf("    - %-5s %-20s %s:%d → %s\n", r.Type, r.Name, orDash(r.LocalIP), r.LocalPort, target)
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdExport(args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	id := fs.String("id", "", "服务器 ID（list 中方括号内的部分即可）")
	all := fs.Bool("all", false, "导出全部")
	dataDir := fs.String("data", "", "数据目录")
	_ = fs.Parse(args)
	st := openStore(*dataDir)

	pick := func(e *model.ServerEntry) {
		code, err := sharecode.EncodeFromServerEntry(e)
		if err != nil {
			fatal("导出 %s 失败: %v", e.Name, err)
		}
		fmt.Printf("── %s (%s:%d) ──\n%s\n\n", e.Name, e.ServerAddr, e.ServerPort, code)
	}
	if *all {
		for _, e := range st.List() {
			pick(e)
		}
		return
	}
	if *id == "" {
		fatal("请用 -id 指定服务器（frplink list 查看），或 -all 导出全部")
	}
	for _, e := range st.List() {
		if strings.HasPrefix(e.ID, *id) {
			pick(e)
			return
		}
	}
	fatal("未找到 ID 前缀为 %q 的服务器", *id)
}
