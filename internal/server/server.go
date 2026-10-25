// Package server 提供 FRPLink 的本地 Web GUI 与 REST API。
// 出于安全考虑：仅监听本机回环地址，所有 /api 请求需携带本次启动
// 生成的随机访问密钥（URL ?k= 或 X-Frp-Link-Key 头）。
package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/fatedier/frp/pkg/util/version"

	"frplink/internal/logbuf"
	"frplink/internal/model"
	"frplink/internal/runner"
	"frplink/internal/sharecode"
	"frplink/internal/store"
)

//go:embed web
var webFS embed.FS

// Server Web GUI 服务。
type Server struct {
	st      *store.Store
	mgr     *runner.Manager
	logs    *logbuf.Buffer
	token   string
	appVer  string
	dataDir string
}

// New 创建服务。
func New(st *store.Store, mgr *runner.Manager, logs *logbuf.Buffer, token, appVer, dataDir string) *Server {
	return &Server{st: st, mgr: mgr, logs: logs, token: token, appVer: appVer, dataDir: dataDir}
}

// Listen 在 addr（如 127.0.0.1:0）上监听并返回实际地址。不阻塞。
func (s *Server) Listen(addr string) (net.Addr, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr(), nil
}

// Handler 组装全部路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/info", s.auth(s.handleInfo))
	mux.HandleFunc("GET /api/servers", s.auth(s.handleServerList))
	mux.HandleFunc("POST /api/servers", s.auth(s.handleServerCreate))
	mux.HandleFunc("PUT /api/servers/{id}", s.auth(s.handleServerUpdate))
	mux.HandleFunc("PUT /api/servers/order", s.auth(s.handleServerOrder))
	mux.HandleFunc("DELETE /api/servers/{id}", s.auth(s.handleServerDelete))
	mux.HandleFunc("POST /api/servers/{id}/start", s.auth(s.handleServerStart))
	mux.HandleFunc("POST /api/servers/{id}/stop", s.auth(s.handleServerStop))
	mux.HandleFunc("POST /api/servers/{id}/restart", s.auth(s.handleServerRestart))
	mux.HandleFunc("GET /api/servers/{id}/export", s.auth(s.handleServerExport))
	mux.HandleFunc("POST /api/preview", s.auth(s.handlePreview))
	mux.HandleFunc("POST /api/import", s.auth(s.handleImport))
	mux.HandleFunc("POST /api/gencode", s.auth(s.handleGenCode))
	mux.HandleFunc("POST /api/test", s.auth(s.handleTest))
	mux.HandleFunc("GET /api/logs", s.auth(s.handleLogs))
	mux.HandleFunc("GET /api/settings", s.auth(s.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", s.auth(s.handlePutSettings))

	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(sub))
	return mux
}

// ---- 基础设施 ----

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Frp-Link-Key")
		if key == "" {
			key = r.URL.Query().Get("k")
		}
		if key != s.token {
			writeErr(w, http.StatusUnauthorized, "无效的访问密钥，请从程序输出的地址打开本页面")
			return
		}
		next(w, r)
	}
}

type apiResp struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiResp{OK: status < 400, Data: v})
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiResp{OK: false, Error: msg})
}

func readBody(r *http.Request, v any) error {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("请求体为空")
	}
	return json.Unmarshal(data, v)
}

// ---- 处理器 ----

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"app":        "FRPLink",
		"version":    s.appVer,
		"frpVersion": version.Full(),
		"dataFile":   s.st.Path(),
		"dataDir":    s.dataDir,
		"proxyTypes": model.AllProxyTypes,
		"protocols":  model.AllProtocols,
		"serverTime": time.Now().Format("2006-01-02 15:04:05"),
	})
}

// serverWithStatus 条目 + 运行状态。
type serverWithStatus struct {
	model.ServerEntry
	Running bool                `json:"running"`
	Status  runner.ServerStatus `json:"status"`
}

func (s *Server) serverWithStatus(e *model.ServerEntry) serverWithStatus {
	out := serverWithStatus{ServerEntry: *e, Status: s.mgr.Status(e.ID)}
	out.Running = out.Status.Status != runner.StatusStopped
	if out.Status.Traffic == 0 && e.Traffic > 0 {
		out.Status.Traffic = e.Traffic // 未运行时显示历史累计
	}
	return out
}

func (s *Server) handleServerList(w http.ResponseWriter, r *http.Request) {
	list := s.st.List()
	out := make([]serverWithStatus, 0, len(list))
	for _, e := range list {
		out = append(out, s.serverWithStatus(e))
	}
	writeJSON(w, 200, map[string]any{"servers": out})
}

// entryPayload 创建/更新请求体。
type entryPayload struct {
	model.ServerEntry
	Start bool `json:"start"`
}

func (s *Server) handleServerCreate(w http.ResponseWriter, r *http.Request) {
	var req entryPayload
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误: "+err.Error())
		return
	}
	created, err := s.st.Create(&req.ServerEntry)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var note string
	if req.Start {
		if err := s.mgr.Start(created.ID); err != nil {
			note = "已保存，但启动失败: " + err.Error()
		}
	}
	writeJSON(w, 200, map[string]any{"server": s.serverWithStatus(created), "note": note})
}

// handleServerOrder 拖拽排序：按传入的 id 顺序写入排序权重。
func (s *Server) handleServerOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误: "+err.Error())
		return
	}
	if len(req.IDs) == 0 {
		writeErr(w, 400, "ids 不能为空")
		return
	}
	if err := s.st.SetOrder(req.IDs); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ordered": len(req.IDs)})
}

func (s *Server) handleServerUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	old := s.st.Get(id)
	if old == nil {
		writeErr(w, 404, "服务器条目不存在")
		return
	}
	var req entryPayload
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误: "+err.Error())
		return
	}
	req.ServerEntry.ID = id
	if err := s.st.Update(&req.ServerEntry); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	updated := s.st.Get(id)
	var note string
	switch {
	case s.mgr.IsRunning(id) && req.Start:
		_ = s.mgr.Restart(id) // 配置变更后重启生效
		note = "已保存并重启"
	case s.mgr.IsRunning(id) && !req.Start:
		s.mgr.Stop(id, 8*time.Second)
		note = "已保存，已停止运行"
	case !s.mgr.IsRunning(id) && req.Start:
		if err := s.mgr.Start(id); err != nil {
			note = "已保存，但启动失败: " + err.Error()
		} else {
			note = "已保存并启动"
		}
	default:
		note = "已保存"
	}
	writeJSON(w, 200, map[string]any{"server": s.serverWithStatus(updated), "note": note})
}

func (s *Server) handleServerDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.st.Get(id) == nil {
		writeErr(w, 404, "服务器条目不存在")
		return
	}
	s.mgr.Stop(id, 8*time.Second)
	if err := s.st.Delete(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

func (s *Server) handleServerStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.mgr.Start(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"started": id, "status": s.mgr.Status(id)})
}

func (s *Server) handleServerStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mgr.Stop(id, 8*time.Second)
	writeJSON(w, 200, map[string]any{"stopped": id})
}

func (s *Server) handleServerRestart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.mgr.Restart(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"restarted": id, "status": s.mgr.Status(id)})
}

func (s *Server) handleServerExport(w http.ResponseWriter, r *http.Request) {
	e := s.st.Get(r.PathValue("id"))
	if e == nil {
		writeErr(w, 404, "服务器条目不存在")
		return
	}
	code, err := sharecode.EncodeFromServerEntry(e)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"code": code})
}

// codeRequest 通用 code 请求体。
type codeRequest struct {
	Code  string `json:"code"`
	Name  string `json:"name,omitempty"`
	Start bool   `json:"start,omitempty"`
}

// previewData 脱敏后的分享码预览。
type previewData struct {
	Name        string            `json:"name"`
	Addr        string            `json:"addr"`
	Port        int               `json:"port"`
	User        string            `json:"user,omitempty"`
	TokenMasked string            `json:"tokenMasked"`
	HasToken    bool              `json:"hasToken"`
	TLS         bool              `json:"tls"`
	Protocol    string            `json:"protocol,omitempty"`
	Note        string            `json:"note,omitempty"`
	ExpireAt    string            `json:"expireAt,omitempty"`
	Proxies     []model.ProxyRule `json:"proxies"`
}

func maskToken(t string) string {
	if t == "" {
		return ""
	}
	if len(t) <= 4 {
		return "****"
	}
	return t[:2] + "****" + t[len(t)-2:] + fmt.Sprintf("（%d 字符）", len(t))
}

func buildPreview(p *sharecode.Payload) previewData {
	pv := previewData{
		Name: p.Name, Addr: p.Addr, Port: p.Port, User: p.User,
		TokenMasked: maskToken(p.Token), HasToken: p.Token != "",
		TLS: p.TLS == nil || *p.TLS, Protocol: p.Proto, Note: p.Note,
		Proxies: p.Px,
	}
	if pv.Protocol == "" {
		pv.Protocol = "tcp"
	}
	if p.Exp > 0 {
		pv.ExpireAt = time.Unix(p.Exp, 0).Format("2006-01-02 15:04")
	}
	if pv.Proxies == nil {
		pv.Proxies = []model.ProxyRule{}
	}
	return pv
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	var req codeRequest
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误: "+err.Error())
		return
	}
	p, err := sharecode.Decode(req.Code)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, buildPreview(p))
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	var req codeRequest
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误: "+err.Error())
		return
	}
	p, err := sharecode.Decode(req.Code)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	entry, err := sharecode.ToServerEntry(p, req.Name)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	created, err := s.st.Create(entry)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var note string
	if req.Start {
		if err := s.mgr.Start(created.ID); err != nil {
			note = "已导入，但启动失败: " + err.Error()
		}
	}
	writeJSON(w, 200, map[string]any{"server": s.serverWithStatus(created), "preview": buildPreview(p), "note": note})
}

// genCodeRequest 生成分享码请求体（生成器页）。
type genCodeRequest struct {
	Name     string            `json:"name,omitempty"`
	Addr     string            `json:"addr"`
	Port     int               `json:"port,omitempty"`
	User     string            `json:"user,omitempty"`
	Token    string            `json:"token,omitempty"`
	TLS      *bool             `json:"tls,omitempty"`
	Protocol string            `json:"protocol,omitempty"`
	Enc      bool              `json:"enc,omitempty"`
	Cmp      bool              `json:"cmp,omitempty"`
	Note     string            `json:"note,omitempty"`
	ExpireAt string            `json:"expireAt,omitempty"` // "2006-01-02T15:04" 本地时间
	Proxies  []model.ProxyRule `json:"proxies,omitempty"`
}

// handleTest 用真实 frp 登录流程测试一个条目（不必先保存）能否连上 frps。
func (s *Server) handleTest(w http.ResponseWriter, r *http.Request) {
	var entry model.ServerEntry
	if err := readBody(r, &entry); err != nil {
		writeErr(w, 400, "请求格式错误: "+err.Error())
		return
	}
	if err := entry.Validate(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ms, err := runner.TestConnection(r.Context(), &entry, 12*time.Second)
	result := map[string]any{"ok": err == nil, "ms": ms.Milliseconds()}
	if err != nil {
		result["error"] = err.Error()
	}
	writeJSON(w, 200, result)
}

func (s *Server) handleGenCode(w http.ResponseWriter, r *http.Request) {
	var req genCodeRequest
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误: "+err.Error())
		return
	}
	if req.Addr == "" {
		writeErr(w, 400, "服务器地址不能为空")
		return
	}
	if req.Port == 0 {
		req.Port = 7000
	}
	// 校验预置规则合法性
	tmp := &model.ServerEntry{ServerAddr: req.Addr, ServerPort: req.Port, Proxies: req.Proxies}
	if err := tmp.Validate(); err != nil {
		writeErr(w, 400, "预置穿透规则有问题: "+err.Error())
		return
	}
	var exp int64
	if req.ExpireAt != "" {
		t, err := time.ParseInLocation("2006-01-02T15:04", req.ExpireAt, time.Local)
		if err != nil {
			writeErr(w, 400, "过期时间格式无效")
			return
		}
		exp = t.Unix()
	}
	proto := req.Protocol
	if proto == "" {
		proto = "tcp"
	}
	code, err := sharecode.Encode(&sharecode.Payload{
		Ver: 1, Name: req.Name, Addr: req.Addr, Port: req.Port,
		User: req.User, Token: req.Token, TLS: req.TLS, Proto: proto,
		Enc: req.Enc, Cmp: req.Cmp, Note: req.Note, Exp: exp,
		Px: req.Proxies,
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"code": code})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 {
		n = 300
	}
	writeJSON(w, 200, map[string]any{"lines": s.logs.Tail(n)})
}

// handleGetSettings 读取全局设置。
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.st.Settings())
}

// handlePutSettings 保存全局设置。
func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var st store.Settings
	if err := readBody(r, &st); err != nil {
		writeErr(w, 400, "请求格式错误: "+err.Error())
		return
	}
	if err := s.st.SaveSettings(st); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, s.st.Settings())
}
