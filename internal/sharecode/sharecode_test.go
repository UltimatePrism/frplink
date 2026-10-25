package sharecode

import (
	"strings"
	"testing"
	"time"

	"frplink/internal/model"
)

func samplePayload() *Payload {
	tls := false
	return &Payload{
		Ver:   1,
		Name:  "测试服务器",
		Addr:  "example.com",
		Port:  7000,
		User:  "alice",
		Token: "secret-token",
		TLS:   &tls,
		Proto: "tcp",
		Enc:   true,
		Cmp:   false,
		Note:  "内网测试",
		Px: []model.ProxyRule{
			{Type: "tcp", LocalPort: 22, RemotePort: 6022, Name: "ssh"},
			{Type: "http", LocalPort: 8080, CustomDomains: []string{"a.example.com"}, Name: "web"},
		},
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	code, err := Encode(samplePayload())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.HasPrefix(code, Prefix) {
		t.Fatalf("缺少前缀: %s", code)
	}
	if strings.ContainsAny(code, "+/=") {
		t.Fatalf("分享码应使用 URL 安全 base64: %s", code)
	}
	p, err := Decode(code)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if p.Addr != "example.com" || p.Port != 7000 || p.Token != "secret-token" || p.User != "alice" {
		t.Fatalf("round trip 字段错误: %+v", p)
	}
	if len(p.Px) != 2 || p.Px[0].Name != "ssh" || p.Px[1].Type != "http" {
		t.Fatalf("穿透规则丢失: %+v", p.Px)
	}
	if p.TLS == nil || *p.TLS {
		t.Fatalf("TLS 应为 false")
	}
}

func TestDecodeTolerant(t *testing.T) {
	code, _ := Encode(samplePayload())
	cases := []string{
		code,
		"  " + code + "  \n",             // 首尾空白
		code[len(Prefix):],               // 无前缀
		"frplink1." + code[len(Prefix):], // 小写前缀（正文不动，base64 大小写敏感）
	}
	for i, c := range cases {
		if _, err := Decode(c); err != nil {
			t.Fatalf("case %d 应当解析成功: %v", i, err)
		}
	}
}

func TestDecodeGarbage(t *testing.T) {
	for _, s := range []string{"", "hello", "FRPLINK1.!!!!", "FRPLINK1.e30"} {
		if _, err := Decode(s); err == nil {
			t.Fatalf("应当解析失败: %q", s)
		}
	}
}

func TestExpired(t *testing.T) {
	p := samplePayload()
	p.Exp = time.Now().Unix() - 10
	code, _ := Encode(p)
	if _, err := Decode(code); err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("应当报过期错误, got %v", err)
	}
}

func TestToServerEntry(t *testing.T) {
	p := samplePayload()
	e, err := ToServerEntry(p, "")
	if err != nil {
		t.Fatalf("ToServerEntry: %v", err)
	}
	if e.ServerAddr != "example.com" || e.Token != "secret-token" {
		t.Fatalf("条目字段错误: %+v", e)
	}
	if !e.AutoRetry {
		t.Fatalf("导入后应默认自动重试")
	}
	if len(e.Proxies) != 2 || e.Proxies[0].LocalIP != "127.0.0.1" {
		t.Fatalf("规则默认值未填充: %+v", e.Proxies)
	}
}

func TestEntryRoundTrip(t *testing.T) {
	e := &model.ServerEntry{
		Name:       "我的节点",
		ServerAddr: "1.2.3.4",
		ServerPort: 7000,
		Token:      "tk",
		TLS:        true,
		Proxies:    []model.ProxyRule{{Type: "tcp", LocalPort: 3389, RemotePort: 13389, Name: "rdp"}},
	}
	code, err := EncodeFromServerEntry(e)
	if err != nil {
		t.Fatalf("EncodeFromServerEntry: %v", err)
	}
	p, err := Decode(code)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	e2, err := ToServerEntry(p, "")
	if err != nil {
		t.Fatalf("ToServerEntry: %v", err)
	}
	if e2.ServerAddr != e.ServerAddr || e2.Token != e.Token || len(e2.Proxies) != 1 {
		t.Fatalf("条目 round trip 失败: %+v", e2)
	}
}
