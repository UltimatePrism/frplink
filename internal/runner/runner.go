// Package runner 把 frp 客户端封装为可管理的多服务器运行器：
// 每个服务器条目对应一个独立的 frp 客户端连接，支持启动/停止/重启、
// 状态查询、失败自动重试。
package runner

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	frpclient "github.com/fatedier/frp/client"
	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/config/source"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/fatedier/frp/pkg/config/v1/validation"
	"github.com/samber/lo"

	"frplink/internal/model"
	"frplink/internal/store"
)

// Status 服务器运行状态。
type Status string

const (
	StatusStopped    Status = "stopped"    // 已停止
	StatusConnecting Status = "connecting" // 启动中/登录中
	StatusOnline     Status = "online"     // 已连接 frps
	StatusRetrying   Status = "retrying"   // 失败等待重试
	StatusError      Status = "error"      // 已停止（出错）
)

// StatusText 中文状态描述。
func (s Status) StatusText() string {
	switch s {
	case StatusOnline:
		return "已连接"
	case StatusConnecting:
		return "连接中"
	case StatusRetrying:
		return "重试中"
	case StatusError:
		return "错误"
	default:
		return "已停止"
	}
}

// ProxyStatus 单条穿透规则的状态。
type ProxyStatus struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Phase      string `json:"phase"` // running / error / ...
	Err        string `json:"err,omitempty"`
	RemoteAddr string `json:"remoteAddr,omitempty"`
}

// ServerStatus 一个服务器的完整运行状态。
type ServerStatus struct {
	ID         string        `json:"id"`
	Status     Status        `json:"status"`
	StatusText string        `json:"statusText"`
	LastErr    string        `json:"lastErr,omitempty"`
	StartedAt  *time.Time    `json:"startedAt,omitempty"`
	Attempts   int           `json:"attempts"`  // 已尝试连接次数
	SpeedUp    int64         `json:"speedUp"`   // 当前上行速度（字节/秒）
	SpeedDown  int64         `json:"speedDown"` // 当前下行速度（字节/秒）
	Traffic    int64         `json:"traffic"`   // 累计使用流量（字节，上+下行）
	Proxies    []ProxyStatus `json:"proxies,omitempty"`
}

// run 单个服务器的运行实例。
type run struct {
	entry  *model.ServerEntry // 启动时的快照
	cancel context.CancelFunc
	done   chan struct{}

	mu        sync.Mutex
	status    Status
	lastErr   string
	startedAt time.Time
	attempts  int
	svr       *frpclient.Service

	// 流量统计：累计字节数（跨重连持续累加）与最近 1 秒速度
	txTotal, rxTotal   atomic.Int64
	trafficBase        int64 // 启动时条目上已有的累计流量
	lastTx, lastRx     int64
	speedUp, speedDown int64
}

func (r *run) setStatus(s Status, errMsg string) {
	r.mu.Lock()
	r.status = s
	if errMsg != "" {
		r.lastErr = errMsg
	}
	if errMsg == "" && s != StatusError && s != StatusRetrying {
		r.lastErr = ""
	}
	r.mu.Unlock()
}

// setStoppedUnlessError 置为已停止，但不覆盖看门狗等已写入的终态错误。
func (r *run) setStoppedUnlessError() {
	r.mu.Lock()
	if r.status != StatusError {
		r.status = StatusStopped
		r.lastErr = ""
	}
	r.mu.Unlock()
}

func (r *run) setSvr(svr *frpclient.Service) {
	r.mu.Lock()
	r.svr = svr
	r.mu.Unlock()
}

// Manager 管理所有服务器的运行。
type Manager struct {
	ctx   context.Context
	store *store.Store

	mu   sync.Mutex
	runs map[string]*run
}

// New 创建管理器。
func New(ctx context.Context, st *store.Store) *Manager {
	m := &Manager{ctx: ctx, store: st, runs: map[string]*run{}}
	go m.speedLoop(ctx)
	return m
}

// trafficTotal 当前累计流量（基线 + 本次运行的收发合计）。
func (r *run) trafficTotal() int64 {
	return r.trafficBase + r.txTotal.Load() + r.rxTotal.Load()
}

// speedLoop 每秒采样一次各运行的累计流量，得出当前上/下行速度。
func (m *Manager) speedLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	ticks := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ticks++
		m.mu.Lock()
		runs := make([]*run, 0, len(m.runs))
		for _, r := range m.runs {
			runs = append(runs, r)
		}
		m.mu.Unlock()
		persist := ticks%30 == 0
		for _, r := range runs {
			tx, rx := r.txTotal.Load(), r.rxTotal.Load()
			r.mu.Lock()
			r.speedUp, r.speedDown = tx-r.lastTx, rx-r.lastRx
			r.lastTx, r.lastRx = tx, rx
			r.mu.Unlock()
			if persist && m.store != nil {
				_ = m.store.SetTraffic(r.entry.ID, r.trafficTotal())
			}
		}
	}
}

// Start 启动指定服务器（若已在运行则忽略）。
func (m *Manager) Start(id string) error {
	entry := m.store.Get(id)
	if entry == nil {
		return fmt.Errorf("服务器条目不存在")
	}
	m.mu.Lock()
	if r, ok := m.runs[id]; ok {
		select {
		case <-r.done:
			// 上一个运行已结束（终态错误/正常退出），清理后允许重新启动
			delete(m.runs, id)
		default:
			m.mu.Unlock()
			return nil // 仍在运行
		}
	}
	ctx, cancel := context.WithCancel(m.ctx)
	r := &run{entry: entry, cancel: cancel, done: make(chan struct{}), trafficBase: entry.Traffic}
	r.status = StatusConnecting
	r.startedAt = time.Now()
	m.runs[id] = r
	m.mu.Unlock()

	go m.runLoop(ctx, r)
	go m.watchConnect(r, m.connectTimeout())
	return nil
}

// connectTimeout 读取设置中的连接超时（夹在 3-300 秒，默认 10 秒）。
func (m *Manager) connectTimeout() time.Duration {
	sec := 10
	if m.store != nil {
		sec = m.store.Settings().ConnectTimeoutSec
	}
	if sec < 3 {
		sec = 3
	}
	if sec > 300 {
		sec = 300
	}
	return time.Duration(sec) * time.Second
}

// watchConnect 连接看门狗：持续处于"连接中/重试中"超过设定时长且始终未连上，
// 判定超时，自动停止本次运行（重新点击启动可再次尝试）。
func (m *Manager) watchConnect(r *run, timeout time.Duration) {
	tk := time.NewTicker(500 * time.Millisecond)
	defer tk.Stop()
	notOnlineSince := time.Now()
	for {
		select {
		case <-r.done:
			return
		case <-tk.C:
		}
		r.mu.Lock()
		st := r.status
		r.mu.Unlock()
		switch st {
		case StatusOnline:
			notOnlineSince = time.Now() // 已连接，重置计时（掉线重连时重新计）
		case StatusConnecting, StatusRetrying:
			if time.Since(notOnlineSince) > timeout {
				r.setStatus(StatusError, fmt.Sprintf("连接超时：%.0f 秒内未能连上服务器，已自动停止（时长可在设置中调整）", timeout.Seconds()))
				r.cancel()
				return
			}
		default: // stopped / error：运行已结束
			return
		}
	}
}

// Stop 停止指定服务器，最多等待 wait 完成清理。
func (m *Manager) Stop(id string, wait time.Duration) {
	m.mu.Lock()
	r, ok := m.runs[id]
	if ok {
		delete(m.runs, id)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(wait):
	}
	if m.store != nil {
		_ = m.store.SetTraffic(r.entry.ID, r.trafficTotal())
	}
}

// StopAll 停止全部（应用退出时调用）。
func (m *Manager) StopAll(wait time.Duration) {
	m.mu.Lock()
	ids := make([]string, 0, len(m.runs))
	for id := range m.runs {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			m.Stop(id, wait)
		}(id)
	}
	wg.Wait()
}

// Restart 重启（配置更新后调用）。
func (m *Manager) Restart(id string) error {
	m.Stop(id, 8*time.Second)
	return m.Start(id)
}

// StartAutoStart 启动所有标记了 autoStart 的服务器。
func (m *Manager) StartAutoStart() {
	for _, e := range m.store.List() {
		if e.AutoStart {
			_ = m.Start(e.ID)
		}
	}
}

// IsRunning 判断是否运行中。
func (m *Manager) IsRunning(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.runs[id]
	return ok
}

// Status 返回指定服务器状态；未运行返回 stopped。
func (m *Manager) Status(id string) ServerStatus {
	m.mu.Lock()
	r, ok := m.runs[id]
	m.mu.Unlock()
	if !ok {
		return ServerStatus{ID: id, Status: StatusStopped, StatusText: StatusStopped.StatusText()}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := ServerStatus{
		ID:         id,
		Status:     r.status,
		StatusText: r.status.StatusText(),
		LastErr:    r.lastErr,
		Attempts:   r.attempts,
		SpeedUp:    r.speedUp,
		SpeedDown:  r.speedDown,
		Traffic:    r.trafficBase + r.txTotal.Load() + r.rxTotal.Load(),
		Proxies:    []ProxyStatus{},
	}
	if !r.startedAt.IsZero() {
		t := r.startedAt
		st.StartedAt = &t
	}
	if r.svr != nil {
		exp := r.svr.StatusExporter()
		if len(st.Proxies) == 0 {
			// 无规则条目：探针进入 running 即代表已登录
			if ws, ok := exp.GetProxyStatus(probeName(r.entry.ID)); ok && ws != nil && ws.Phase == proxyPhaseRunning && r.status == StatusConnecting {
				r.status = StatusOnline
				st.Status = StatusOnline
				st.StatusText = StatusOnline.StatusText()
			}
		}
		for i := range r.entry.Proxies {
			rule := &r.entry.Proxies[i]
			if !model.RuleEnabled(rule) {
				continue
			}
			ps := ProxyStatus{Name: rule.Name, Type: rule.Type, Phase: "wait"}
			if ws, ok := exp.GetProxyStatus(rule.Name); ok && ws != nil {
				ps.Phase = ws.Phase
				ps.Err = ws.Err
				ps.RemoteAddr = ws.RemoteAddr
				if ws.Phase == proxyPhaseRunning && r.status == StatusConnecting {
					// 首个代理进入 running 说明已登录成功
					r.status = StatusOnline
					st.Status = StatusOnline
					st.StatusText = StatusOnline.StatusText()
				}
			}
			st.Proxies = append(st.Proxies, ps)
		}
	}
	if st.Status == StatusConnecting && len(st.Proxies) > 0 {
		allRunning := true
		for _, p := range st.Proxies {
			if p.Phase != proxyPhaseRunning {
				allRunning = false
				break
			}
		}
		if allRunning && r.svr != nil {
			// 有代理在跑但状态未被上面的分支翻转为 online（例如全部已在 running）
			r.status = StatusOnline
			st.Status = StatusOnline
			st.StatusText = StatusOnline.StatusText()
		}
	}
	return st
}

// Statuses 返回全部运行中服务器的状态。
func (m *Manager) Statuses() map[string]ServerStatus {
	m.mu.Lock()
	ids := make([]string, 0, len(m.runs))
	for id := range m.runs {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	out := make(map[string]ServerStatus, len(ids))
	for _, id := range ids {
		out[id] = m.Status(id)
	}
	return out
}

const proxyPhaseRunning = "running"

// probeName 返回无规则条目的状态探针代理名。
func probeName(entryID string) string { return "frplink-probe-" + entryID }

// runLoop 服务器的生命周期主循环：构建服务 → 运行 → 失败按需重试。
func (m *Manager) runLoop(ctx context.Context, r *run) {
	defer close(r.done)
	backoff := 5 * time.Second
	for {
		r.mu.Lock()
		r.attempts++
		r.mu.Unlock()

		svr, err := buildService(r.entry, &r.txTotal, &r.rxTotal)
		if err != nil {
			// 配置构建/校验错误：重试无意义，直接终态
			r.setStatus(StatusError, fmt.Sprintf("配置错误: %v", err))
			return
		}
		r.setStatus(StatusConnecting, "")
		r.setSvr(svr)

		runErr := svr.Run(ctx)
		r.setSvr(nil)

		if ctx.Err() != nil {
			// 主动停止（用户或看门狗超时；看门狗写入的错误状态予以保留）
			r.setStoppedUnlessError()
			return
		}
		if runErr != nil {
			msg := strings.TrimSpace(runErr.Error())
			if !r.entry.AutoRetry {
				r.setStatus(StatusError, msg)
				return
			}
			r.setStatus(StatusRetrying, msg)
		} else {
			// Run 无错误返回且非用户取消：也按重试处理（如服务端主动断开后的内部退出）
			if !r.entry.AutoRetry {
				r.setStatus(StatusStopped, "")
				return
			}
			r.setStatus(StatusRetrying, "与服务器的连接已断开")
		}

		select {
		case <-ctx.Done():
			r.setStoppedUnlessError()
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// buildService 根据条目构建 frp 客户端服务。tx/rx 用于累计与 frps 之间的流量。
func buildService(e *model.ServerEntry, tx, rx *atomic.Int64) (*frpclient.Service, error) {
	common := BuildCommonConfig(e)
	if err := common.Complete(); err != nil {
		return nil, err
	}
	proxies, err := BuildProxyConfigs(e)
	if err != nil {
		return nil, err
	}
	if len(proxies) == 0 {
		// 无穿透规则的条目附加一条隐形 stcp 探针：不占端口、不产生流量，
		// 仅用于让"已连接"状态可检测（GetProxyStatus 需要至少一个代理）
		proxies = append(proxies, &v1.STCPProxyConfig{
			ProxyBaseConfig: v1.ProxyBaseConfig{
				Name:         probeName(e.ID),
				Type:         model.ProxyTypeSTCP,
				ProxyBackend: v1.ProxyBackend{LocalIP: "127.0.0.1", LocalPort: 1},
			},
			Secretkey: "frplink-probe",
		})
	}
	proxies = config.CompleteProxyConfigurers(proxies)
	if _, err := validation.ValidateAllClientConfig(common, proxies, nil, nil); err != nil {
		return nil, err
	}
	cs := source.NewConfigSource()
	if err := cs.ReplaceAll(proxies, nil); err != nil {
		return nil, err
	}
	agg := source.NewAggregator(cs)
	svr, err := frpclient.NewService(frpclient.ServiceOptions{
		Common:                 common,
		ConfigSourceAggregator: agg,
		ConnectorCreator: func(ctx context.Context, cfg *v1.ClientCommonConfig) frpclient.Connector {
			return &countingConnector{inner: frpclient.NewConnector(ctx, cfg), tx: tx, rx: rx}
		},
	})
	if err != nil {
		return nil, err
	}
	return svr, nil
}

// ---- 流量统计 ----

// countingConn 包装到 frps 的连接，按字节累计上行（Write）与下行（Read）。
type countingConn struct {
	net.Conn
	tx, rx *atomic.Int64
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.tx.Add(int64(n))
	return n, err
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.rx.Add(int64(n))
	return n, err
}

// countingConnector 包装默认 Connector，对所有 Connect() 得到的连接计量。
type countingConnector struct {
	inner frpclient.Connector
	tx    *atomic.Int64
	rx    *atomic.Int64
}

func (c *countingConnector) Open() error { return c.inner.Open() }

func (c *countingConnector) Connect() (net.Conn, error) {
	conn, err := c.inner.Connect()
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, tx: c.tx, rx: c.rx}, nil
}

func (c *countingConnector) Close() error { return c.inner.Close() }

// BuildCommonConfig 把条目转换为 frp 客户端公共配置。
func BuildCommonConfig(e *model.ServerEntry) *v1.ClientCommonConfig {
	tls := e.TLS
	proto := e.Protocol
	if proto == "" {
		proto = "tcp"
	}
	return &v1.ClientCommonConfig{
		User:       e.User,
		ClientID:   "frplink-" + e.ID,
		ServerAddr: e.ServerAddr,
		ServerPort: e.ServerPort,
		Auth: v1.AuthClientConfig{
			Method: v1.AuthMethodToken,
			Token:  e.Token,
		},
		LoginFailExit: lo.ToPtr(true), // 重试逻辑由 Manager 自己控制
		Transport: v1.ClientTransportConfig{
			Protocol: proto,
			TLS:      v1.TLSClientConfig{Enable: &tls},
		},
	}
}

// BuildProxyConfigs 把穿透规则转换为 frp 代理配置。
func BuildProxyConfigs(e *model.ServerEntry) ([]v1.ProxyConfigurer, error) {
	out := make([]v1.ProxyConfigurer, 0, len(e.Proxies))
	for i := range e.Proxies {
		r := &e.Proxies[i]
		if !model.RuleEnabled(r) {
			continue
		}
		pc, err := buildProxyConfig(e, r)
		if err != nil {
			return nil, fmt.Errorf("穿透 %q: %w", r.Name, err)
		}
		out = append(out, pc)
	}
	return out, nil
}

func buildProxyConfig(e *model.ServerEntry, r *model.ProxyRule) (v1.ProxyConfigurer, error) {
	base := v1.ProxyBaseConfig{
		Name: r.Name,
		Type: r.Type,
		Transport: v1.ProxyTransport{
			UseEncryption:  e.EffectiveEncryption(r),
			UseCompression: e.EffectiveCompression(r),
		},
		ProxyBackend: v1.ProxyBackend{
			LocalIP:   r.LocalIP,
			LocalPort: r.LocalPort,
		},
	}
	domain := v1.DomainConfig{
		CustomDomains: r.CustomDomains,
		SubDomain:     r.SubDomain,
	}
	switch r.Type {
	case model.ProxyTypeTCP:
		return &v1.TCPProxyConfig{ProxyBaseConfig: base, RemotePort: r.RemotePort}, nil
	case model.ProxyTypeUDP:
		return &v1.UDPProxyConfig{ProxyBaseConfig: base, RemotePort: r.RemotePort}, nil
	case model.ProxyTypeHTTP:
		return &v1.HTTPProxyConfig{
			ProxyBaseConfig: base,
			DomainConfig:    domain,
			HTTPUser:        r.HTTPUser,
			HTTPPassword:    r.HTTPPassword,
		}, nil
	case model.ProxyTypeHTTPS:
		return &v1.HTTPSProxyConfig{ProxyBaseConfig: base, DomainConfig: domain}, nil
	case model.ProxyTypeSTCP:
		return &v1.STCPProxyConfig{ProxyBaseConfig: base, Secretkey: r.SecretKey}, nil
	case model.ProxyTypeXTCP:
		return &v1.XTCPProxyConfig{ProxyBaseConfig: base, Secretkey: r.SecretKey}, nil
	case model.ProxyTypeSUDP:
		return &v1.SUDPProxyConfig{ProxyBaseConfig: base, Secretkey: r.SecretKey}, nil
	default:
		return nil, fmt.Errorf("不支持的类型 %q", r.Type)
	}
}

// TestConnection 用真实 frp 登录流程测试条目能否连上 frps（含令牌/TLS 校验）。
// 内部注册一条隐形 stcp 探针规则，检测到其状态即代表登录成功；测完立即断开。
// 返回耗时；无法连接时返回具体原因。
func TestConnection(ctx context.Context, e *model.ServerEntry, timeout time.Duration) (time.Duration, error) {
	entry := e.Normalized()
	probe := "frplink-probe-" + store.NewID()
	entry.Proxies = []model.ProxyRule{{
		Name: probe, Type: model.ProxyTypeSTCP,
		LocalIP: "127.0.0.1", LocalPort: 1, SecretKey: "frplink-test",
	}}
	entry.AutoRetry = false

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var tx, rx atomic.Int64
	svr, err := buildService(entry, &tx, &rx)
	if err != nil {
		return 0, fmt.Errorf("配置错误: %w", err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- svr.Run(ctx) }()

	start := time.Now()
	deadline := time.After(timeout)
	for {
		select {
		case err := <-runDone:
			if err != nil {
				return time.Since(start), fmt.Errorf("%w", err)
			}
			return time.Since(start), fmt.Errorf("连接建立后被断开")
		case <-deadline:
			return time.Since(start), fmt.Errorf("连接超时（%.0f 秒内未能完成登录），请检查地址/端口/防火墙", timeout.Seconds())
		case <-time.After(150 * time.Millisecond):
			if _, ok := svr.StatusExporter().GetProxyStatus(probe); ok {
				return time.Since(start), nil
			}
		}
	}
}
