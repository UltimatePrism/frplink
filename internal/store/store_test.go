package store

import (
	"os"
	"path/filepath"
	"testing"

	"frplink/internal/model"
)

func testEntry(name, addr string) *model.ServerEntry {
	return &model.ServerEntry{
		Name:       name,
		ServerAddr: addr,
		ServerPort: 7000,
		Token:      "tk",
		TLS:        true,
		AutoRetry:  true,
		Proxies: []model.ProxyRule{
			{Type: "tcp", LocalPort: 22, RemotePort: 6022, Name: "ssh"},
		},
	}
}

func TestCreateListUpdateDelete(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "servers.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	e1, err := s.Create(testEntry("节点一", "1.1.1.1"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if e1.ID == "" || e1.CreatedAt.IsZero() {
		t.Fatalf("Create 应生成 ID 和时间戳: %+v", e1)
	}
	if _, err := s.Create(&model.ServerEntry{Name: "bad", ServerAddr: "", ServerPort: 7000}); err == nil {
		t.Fatalf("缺地址应报错")
	}

	e2, err := s.Create(testEntry("节点二", "2.2.2.2"))
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	list := s.List()
	if len(list) != 2 {
		t.Fatalf("List 应有 2 条, got %d", len(list))
	}

	// 修改后持久化，重开验证
	e2.Token = "new-token"
	if err := s.Update(e2); err != nil {
		t.Fatalf("Update: %v", err)
	}
	s2, err := Open(s.Path())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := s2.Get(e2.ID); got == nil || got.Token != "new-token" {
		t.Fatalf("更新未持久化: %+v", got)
	}
	if got := s2.Get(e1.ID); got == nil || got.Name != "节点一" {
		t.Fatalf("第一条丢失: %+v", got)
	}

	if err := s2.Delete(e1.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s2.Get(e1.ID) != nil {
		t.Fatalf("删除后不应存在")
	}
	if err := s2.Delete(e1.ID); err == nil {
		t.Fatalf("重复删除应报错")
	}
}

func TestOpenCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.json")
	if err := writeBytes(path, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatalf("损坏文件应报错")
	}
}

func TestOpenMissing(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "none.json"))
	if err != nil {
		t.Fatalf("不存在的文件应返回空存储: %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatalf("应为空")
	}
}

func writeBytes(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}

func TestSetOrder(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "servers.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var ids []string
	for _, n := range []string{"一", "二", "三"} {
		e, err := s.Create(testEntry(n, n+".example.com"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.ID)
	}
	// 默认按创建顺序
	if got := s.List()[0].Name; got != "一" {
		t.Fatalf("默认顺序错误: %s", got)
	}
	// 倒序
	rev := []string{ids[2], ids[1], ids[0]}
	if err := s.SetOrder(rev); err != nil {
		t.Fatalf("SetOrder: %v", err)
	}
	list := s.List()
	for i, want := range []string{"三", "二", "一"} {
		if list[i].Name != want {
			t.Fatalf("排序后第 %d 位应为 %s, got %s", i+1, want, list[i].Name)
		}
	}
	// 持久化
	s2, _ := Open(s.Path())
	if s2.List()[0].Name != "三" {
		t.Fatalf("重启后顺序未保留: %s", s2.List()[0].Name)
	}
	// 新条目仍排末尾
	e4, _ := s.Create(testEntry("四", "4.example.com"))
	if s.List()[3].ID != e4.ID {
		t.Fatalf("新条目应在末尾")
	}
}

func TestSettings(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "servers.json"))
	if got := s.Settings().ConnectTimeoutSec; got != 10 {
		t.Fatalf("默认超时应为 10, got %d", got)
	}
	if err := s.SaveSettings(Settings{ConnectTimeoutSec: 25}); err != nil {
		t.Fatal(err)
	}
	s2, _ := Open(s.Path())
	if got := s2.Settings().ConnectTimeoutSec; got != 25 {
		t.Fatalf("设置未持久化: %d", got)
	}
	// 越界值被夹回范围
	_ = s.SaveSettings(Settings{ConnectTimeoutSec: 1})
	if got := s.Settings().ConnectTimeoutSec; got != 3 {
		t.Fatalf("下限应为 3, got %d", got)
	}
}
