// Package sharecode 实现"配置分享码"：服务端把连接信息编码为一串可复制的文本，
// 客户端粘贴后即可一键添加对应服务器。
//
// 格式: FRPLINK1.<base64url(JSON)>，单行、可直接复制粘贴。
package sharecode

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"frplink/internal/model"
)

// Prefix 所有分享码的统一前缀（v1）。
const Prefix = "FRPLINK1."

// Payload 分享码携带的数据。字段名刻意精简以缩短编码长度。
type Payload struct {
	Ver   int               `json:"v"`              // 版本号，当前为 1
	Name  string            `json:"name,omitempty"` // 服务器备注名（客户端里显示的名称）
	Addr  string            `json:"addr"`           // frps 地址
	Port  int               `json:"port,omitempty"`
	User  string            `json:"user,omitempty"`
	Token string            `json:"token,omitempty"`
	Note  string            `json:"note,omitempty"` // 服务端管理员的备注（如"有效期一个月"）
	Exp   int64             `json:"exp,omitempty"`  // 过期时间戳（秒），0 表示不过期
	TLS   *bool             `json:"tls,omitempty"`
	Proto string            `json:"proto,omitempty"`
	Enc   bool              `json:"enc,omitempty"`
	Cmp   bool              `json:"cmp,omitempty"`
	Px    []model.ProxyRule `json:"px,omitempty"` // 预置的穿透规则模板
}

// Encode 将 Payload 编码为分享码字符串。
func Encode(p *Payload) (string, error) {
	if p == nil {
		return "", fmt.Errorf("payload 为空")
	}
	if p.Ver == 0 {
		p.Ver = 1
	}
	if p.Ver != 1 {
		return "", fmt.Errorf("不支持的版本号 %d", p.Ver)
	}
	if p.Addr == "" {
		return "", fmt.Errorf("服务器地址不能为空")
	}
	if p.Port == 0 {
		p.Port = 7000
	}
	data, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(data), nil
}

// Decode 解析分享码字符串（允许首尾空白、允许缺少前缀）。
func Decode(s string) (*Payload, error) {
	s = strings.TrimSpace(s)
	// 容错：去掉聊天工具常见的手动换行
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")
	if s == "" {
		return nil, fmt.Errorf("分享码为空")
	}
	body := s
	if strings.HasPrefix(strings.ToUpper(s), Prefix) || strings.HasPrefix(s, "frplink1.") {
		body = s[len(Prefix):]
	} else if i := strings.LastIndex(s, "."); i >= 0 && !strings.ContainsAny(s[:i], "=+/") {
		// 形如 xxx.BASE64 的也尝试去掉前缀
		body = s[i+1:]
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(body, "="))
	if err != nil {
		// 兼容标准 base64
		data, err = base64.StdEncoding.DecodeString(body)
		if err != nil {
			return nil, fmt.Errorf("无法解析分享码，请确认完整复制")
		}
	}
	var p Payload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("分享码内容无效，请确认完整复制")
	}
	if p.Ver > 1 {
		return nil, fmt.Errorf("分享码版本过高（v%d），请升级 FRPLink", p.Ver)
	}
	if p.Addr == "" {
		return nil, fmt.Errorf("分享码中缺少服务器地址")
	}
	if p.Port == 0 {
		p.Port = 7000
	}
	if p.Exp > 0 && time.Now().Unix() > p.Exp {
		return nil, fmt.Errorf("分享码已过期（过期时间 %s）", time.Unix(p.Exp, 0).Format("2006-01-02 15:04"))
	}
	return &p, nil
}

// FromServerEntry 把本地已有的服务器条目导出为 Payload（用于客户端之间分享）。
func FromServerEntry(e *model.ServerEntry) *Payload {
	tls := e.TLS
	return &Payload{
		Ver:   1,
		Name:  e.Name,
		Addr:  e.ServerAddr,
		Port:  e.ServerPort,
		User:  e.User,
		Token: e.Token,
		TLS:   &tls,
		Proto: e.Protocol,
		Enc:   e.UseEncryption,
		Cmp:   e.UseCompression,
		Px:    append([]model.ProxyRule(nil), e.Proxies...),
	}
}

// ToServerEntry 把分享码转换成新的服务器条目（不落盘）。
// nameOverride 非空时用于覆盖名称。
func ToServerEntry(p *Payload, nameOverride string) (*model.ServerEntry, error) {
	if p == nil {
		return nil, fmt.Errorf("payload 为空")
	}
	e := &model.ServerEntry{
		Name:           p.Name,
		ServerAddr:     p.Addr,
		ServerPort:     p.Port,
		User:           p.User,
		Token:          p.Token,
		TLS:            p.TLS == nil || *p.TLS, // 默认 true
		Protocol:       p.Proto,
		UseEncryption:  p.Enc,
		UseCompression: p.Cmp,
		AutoRetry:      true,
		Proxies:        append([]model.ProxyRule(nil), p.Px...),
	}
	// 重新生成规则 ID，避免与本地已有条目冲突
	for i := range e.Proxies {
		e.Proxies[i].ID = ""
	}
	if nameOverride != "" {
		e.Name = nameOverride
	}
	if err := e.Validate(); err != nil {
		return nil, fmt.Errorf("分享码内容有问题: %w", err)
	}
	return e.Normalized(), nil
}

// EncodeFromServerEntry 一步到位：条目 → 分享码。
func EncodeFromServerEntry(e *model.ServerEntry) (string, error) {
	return Encode(FromServerEntry(e))
}
