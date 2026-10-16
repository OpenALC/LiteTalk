package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/OpenALC/LiteTalk/client/internal/config"
)

// 验证休眠模式三态（off/hibernate/destroy）的读写、持久化与非法值校验。
func TestHibernateMode(t *testing.T) {
	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)

	if app.GetHibernateMode() != "off" {
		t.Fatal("默认值应为 off")
	}
	if err := app.SetHibernateMode("destroy"); err != nil {
		t.Fatalf("SetHibernateMode: %v", err)
	}
	if app.GetHibernateMode() != "destroy" {
		t.Fatal("设置 destroy 后读取应为 destroy")
	}
	// 持久化：新实例读盘恢复。
	app2 := newAppWithConfig(config.NewWithPath(m.Path()))
	if app2.GetHibernateMode() != "destroy" {
		t.Fatal("休眠模式应已持久化到 config.json")
	}
	if err := app.SetHibernateMode("nope"); err == nil {
		t.Fatal("非法模式应报错")
	}
	if app.GetHibernateMode() != "destroy" {
		t.Fatal("非法值不得改动已存模式")
	}
}

// 验证旧版双布尔开关 → 三态休眠模式的一次性迁移。
func TestHibernateModeMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	legacy := `{"hibernate_on_minimize":true,"ui_destroy_on_hibernate":true}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	m := config.NewWithPath(path)
	if got := m.HibernateMode(); got != "destroy" {
		t.Fatalf("旧版双开应迁移为 destroy，得到 %s", got)
	}
	// 幂等：再次加载不变。
	m2 := config.NewWithPath(path)
	if got := m2.HibernateMode(); got != "destroy" {
		t.Fatalf("迁移应幂等，得到 %s", got)
	}
}

// 验证版本号默认值
func TestGetVersion(t *testing.T) {
	app := NewApp()
	if app.GetVersion() == "" {
		t.Fatal("版本号不应为空")
	}
}

// 验证服务器地址配置：保存 / WS 推导 / 来源
func TestServerConfig(t *testing.T) {
	m := config.NewWithPath(filepath.Join(t.TempDir(), "config.json"))
	app := newAppWithConfig(m)

	// 默认值。
	v := app.GetServerConfig()
	if v.BaseURL != config.DefaultBaseURL || v.Source != "default" {
		t.Fatalf("默认解析错误: %+v", v)
	}

	// 保存后生效（file 来源），WS 自动推导 wss。
	if err := app.SetServerConfig("https://talk.example.com"); err != nil {
		t.Fatalf("SetServerConfig: %v", err)
	}
	v = app.GetServerConfig()
	if v.BaseURL != "https://talk.example.com" || v.WSURL != "wss://talk.example.com/ws" {
		t.Fatalf("保存后解析错误: %+v", v)
	}
	if v.Source != "file" {
		t.Fatalf("来源应为 file，得到 %s", v.Source)
	}

	// 非法地址拒绝保存。
	if err := app.SetServerConfig("ftp://x"); err == nil {
		t.Fatal("非法 scheme 应报错")
	}
	if err := app.SetServerConfig("nope"); err == nil {
		t.Fatal("非法地址应报错")
	}
}

// 验证连通性测试对非法地址的快速失败。
func TestServerConnectionBadURL(t *testing.T) {
	app := newAppWithConfig(config.NewWithPath(filepath.Join(t.TempDir(), "config.json")))
	r := app.TestServerConnection("not-a-url")
	if r.OK || r.ErrorMsg == "" {
		t.Fatalf("非法地址应返回失败结果: %+v", r)
	}
}
