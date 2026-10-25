// Package model 定义 FRPLink 的数据模型：服务器条目与穿透规则。
// 这些结构会被序列化到本地存储，也用于 Web API 交互。
package model

import (
	"fmt"
	"regexp"
	"time"
)

// 支持的穿透类型
const (
	ProxyTypeTCP   = "tcp"
	ProxyTypeUDP   = "udp"
	ProxyTypeHTTP  = "http"
	ProxyTypeHTTPS = "https"
	ProxyTypeSTCP  = "stcp"
	ProxyTypeXTCP  = "xtcp"
	ProxyTypeSUDP  = "sudp"
)

var AllProxyTypes = []string{
	ProxyTypeTCP, ProxyTypeUDP, ProxyTypeHTTP, ProxyTypeHTTPS,
	ProxyTypeSTCP, ProxyTypeXTCP, ProxyTypeSUDP,
}

// 支持的连接协议
var AllProtocols = []string{"tcp", "websocket", "wss", "quic", "kcp"}

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

// ProxyRule 描述一条穿透规则。
type ProxyRule struct {
	ID        string `json:"id"`
	Name      string `json:"name"` // 为空时自动生成
	Type      string `json:"type"` // tcp/udp/http/https/stcp/xtcp/sudp
	LocalIP   string `json:"localIp,omitempty"`
	LocalPort int    `json:"localPort"`
	// tcp/udp: 服务器暴露端口; http/https 可留空
	RemotePort int `json:"remotePort,omitempty"`
	// http/https 专属
	CustomDomains []string `json:"customDomains,omitempty"`
	SubDomain     string   `json:"subDomain,omitempty"`
	HTTPUser      string   `json:"httpUser,omitempty"`
	HTTPPassword  string   `json:"httpPassword,omitempty"`
	// stcp/xtcp/sudp 专属
	SecretKey string `json:"secretKey,omitempty"`
	// 三态：nil 表示继承服务器条目上的默认值
	UseEncryption  *bool `json:"useEncryption,omitempty"`
	UseCompression *bool `json:"useCompression,omitempty"`
	Enabled        *bool `json:"enabled,omitempty"`
}

// ServerEntry 描述一个 frps 服务器及其下的全部穿透规则。
type ServerEntry struct {
	ID             string      `json:"id"`
	Name           string      `json:"name"`
	ServerAddr     string      `json:"serverAddr"`
	ServerPort     int         `json:"serverPort"`
	User           string      `json:"user,omitempty"`
	Token          string      `json:"token,omitempty"`
	TLS            bool        `json:"tls"`                // 默认 true（frp v0.50+ 默认开启）
	Protocol       string      `json:"protocol,omitempty"` // tcp/websocket/wss/quic/kcp
	UseEncryption  bool        `json:"useEncryption,omitempty"`
	UseCompression bool        `json:"useCompression,omitempty"`
	AutoStart      bool        `json:"autoStart,omitempty"`
	AutoRetry      bool        `json:"autoRetry"`         // 启动/登录失败后自动重试
	Sort           int         `json:"sort,omitempty"`    // 列表显示顺序（越小越靠前）
	Traffic        int64       `json:"traffic,omitempty"` // 累计使用流量（字节，上+下行合计，跨重启保留）
	Proxies        []ProxyRule `json:"proxies,omitempty"`
	CreatedAt      time.Time   `json:"createdAt"`
	UpdatedAt      time.Time   `json:"updatedAt"`
}

// DisplayName 返回用于界面展示的名称。
func (e *ServerEntry) DisplayName() string {
	if e.Name != "" {
		return e.Name
	}
	return fmt.Sprintf("%s:%d", e.ServerAddr, e.ServerPort)
}

// Validate 检查条目合法性（不修改自身）。
func (e *ServerEntry) Validate() error {
	if e.ServerAddr == "" {
		return fmt.Errorf("服务器地址不能为空")
	}
	if e.ServerPort <= 0 || e.ServerPort > 65535 {
		return fmt.Errorf("服务器端口必须在 1-65535 之间")
	}
	if e.Protocol != "" && !contains(AllProtocols, e.Protocol) {
		return fmt.Errorf("不支持的协议 %q（可选: tcp/websocket/wss/quic/kcp）", e.Protocol)
	}
	seenName := map[string]bool{}
	for i := range e.Proxies {
		r := &e.Proxies[i]
		if !contains(AllProxyTypes, r.Type) {
			return fmt.Errorf("第 %d 条穿透: 不支持的类型 %q", i+1, r.Type)
		}
		if r.Name != "" && !nameRe.MatchString(r.Name) {
			return fmt.Errorf("第 %d 条穿透: 名称 %q 只能包含字母、数字、_ . -", i+1, r.Name)
		}
		if r.Name != "" {
			if seenName[r.Name] {
				return fmt.Errorf("穿透名称 %q 重复", r.Name)
			}
			seenName[r.Name] = true
		}
		if r.LocalPort <= 0 || r.LocalPort > 65535 {
			return fmt.Errorf("第 %d 条穿透: 本地端口必须在 1-65535 之间", i+1)
		}
		if r.RemotePort < 0 || r.RemotePort > 65535 {
			return fmt.Errorf("第 %d 条穿透: 远程端口必须在 0-65535 之间", i+1)
		}
		needsRemote := r.Type == ProxyTypeTCP || r.Type == ProxyTypeUDP
		needsDomain := r.Type == ProxyTypeHTTP || r.Type == ProxyTypeHTTPS
		if needsRemote && r.RemotePort == 0 {
			return fmt.Errorf("第 %d 条穿透: %s 类型必须填写远程端口", i+1, r.Type)
		}
		if needsDomain && len(r.CustomDomains) == 0 && r.SubDomain == "" {
			return fmt.Errorf("第 %d 条穿透: %s 类型需要填写自定义域名或子域名", i+1, r.Type)
		}
	}
	return nil
}

// Normalized 填充默认值并返回副本（不修改原条目）。
func (e *ServerEntry) Normalized() *ServerEntry {
	out := *e
	if out.ServerPort == 0 {
		out.ServerPort = 7000
	}
	if out.Protocol == "" {
		out.Protocol = "tcp"
	}
	if out.Name == "" {
		out.Name = fmt.Sprintf("%s:%d", out.ServerAddr, out.ServerPort)
	}
	used := map[string]bool{}
	for i := range out.Proxies {
		r := &out.Proxies[i]
		if r.LocalIP == "" {
			r.LocalIP = "127.0.0.1"
		}
		if r.Name == "" {
			base := fmt.Sprintf("%s-%d", r.Type, r.LocalPort)
			name := base
			for n := 2; used[name]; n++ {
				name = fmt.Sprintf("%s-%d", base, n)
			}
			r.Name = name
		}
		used[r.Name] = true
	}
	if len(out.Proxies) == 0 {
		out.Proxies = []ProxyRule{}
	}
	return &out
}

// EffectiveEncryption 计算某条规则最终是否启用加密。
func (e *ServerEntry) EffectiveEncryption(r *ProxyRule) bool {
	if r.UseEncryption != nil {
		return *r.UseEncryption
	}
	return e.UseEncryption
}

// EffectiveCompression 计算某条规则最终是否启用压缩。
func (e *ServerEntry) EffectiveCompression(r *ProxyRule) bool {
	if r.UseCompression != nil {
		return *r.UseCompression
	}
	return e.UseCompression
}

// RuleEnabled 计算某条规则是否启用。
func RuleEnabled(r *ProxyRule) bool {
	return r.Enabled == nil || *r.Enabled
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
