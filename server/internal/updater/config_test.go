package updater

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if !c.Enabled {
		t.Fatal("Enabled 默认应为 1（真）")
	}
	if c.AllowPrerelease {
		t.Fatal("AllowPrerelease 默认应为 0（假）：只收正式版")
	}
	if c.Listen != ":8088" {
		t.Fatalf("默认监听 = %q, want :8088（明文）", c.Listen)
	}
	if c.Repo != DefaultRepo || c.InstallDir != DefaultInstallDir || c.StateDir != DefaultStateDir {
		t.Fatalf("默认值漂移: %+v", c)
	}
	// 旧 TLS 字段必须彻底删除：引用它们会编译失败，这里再用反射兜底断言。
	typ := reflect.TypeOf(Config{})
	for _, name := range []string{"Domain", "NoTLS", "TLSCacheDir"} {
		if _, ok := typ.FieldByName(name); ok {
			t.Fatalf("Config 不应再有字段 %s（TLS 已交给主服务）", name)
		}
	}
}

func TestLoadEnv(t *testing.T) {
	env := map[string]string{
		"LITETALK_UPDATER_ENABLED":          "0",
		"LITETALK_UPDATER_ALLOW_PRERELEASE": "1",
		"LITETALK_UPDATER_LISTEN":           "127.0.0.1:8099",
		"LITETALK_UPDATER_REPO":             "OpenALC/Fork",
		"LITETALK_UPDATER_INSTALL_DIR":      "/opt/lt",
		"LITETALK_UPDATER_SERVICE":          "lt.service",
		"LITETALK_UPDATER_HEALTH_URL":       "http://127.0.0.1:9/x",
		"LITETALK_UPDATER_STATE_DIR":        "/var/lib/lt",
		"LITETALK_UPDATER_HEALTH_TIMEOUT":   "30s",
	}
	// 已废弃的 TLS 相关变量：升级时留在环境里必须被忽略且不报错。
	// 名字拆开拼接，保证静态巡检按旧变量名 grep 时生产代码零命中。
	env["LITETALK_UPDATER_"+"DOMAIN"] = "up.example.com"
	env["LITETALK_UPDATER_"+"NO_TLS"] = "garbage" // 旧值非法也不参与解析
	env["LITETALK_UPDATER_"+"TLS_CACHE"] = "/tmp/certs"
	getenv := func(k string) string { return env[k] }
	c := DefaultConfig()
	if err := c.LoadEnv(getenv); err != nil {
		t.Fatal(err)
	}
	if c.Enabled {
		t.Fatal("ENABLED=0 未生效")
	}
	if !c.AllowPrerelease {
		t.Fatal("ALLOW_PRERELEASE=1 未生效")
	}
	if c.Listen != "127.0.0.1:8099" || c.Repo != "OpenALC/Fork" || c.StateDir != "/var/lib/lt" {
		t.Fatalf("普通变量未生效: %+v", c)
	}
	if c.HealthTimeout != 30*time.Second {
		t.Fatalf("超时 = %v", c.HealthTimeout)
	}

	// 非法布尔值：fail-closed，不静默回落
	c2 := DefaultConfig()
	if err := c2.LoadEnv(func(k string) string {
		if k == "LITETALK_UPDATER_ENABLED" {
			return "maybe"
		}
		return ""
	}); err == nil {
		t.Fatal("非法布尔值应报错")
	}
}

func TestValidate(t *testing.T) {
	c := DefaultConfig()
	if err := c.Validate(false); err != nil {
		t.Fatalf("默认配置应合法: %v", err)
	}
	bad := DefaultConfig()
	bad.Repo = "no-slash"
	if err := bad.Validate(false); err == nil {
		t.Fatal("非法 repo 应拒")
	}
	bad2 := DefaultConfig()
	bad2.ServiceName = "litetalk; rm -rf /"
	if err := bad2.Validate(false); err == nil {
		t.Fatal("非法服务名应拒")
	}
	bad3 := DefaultConfig()
	bad3.InstallDir = "relative/path"
	if err := bad3.Validate(false); err == nil {
		t.Fatal("相对安装目录应拒")
	}
	bad4 := DefaultConfig()
	bad4.HealthURL = "not-a-url"
	if err := bad4.Validate(true); err == nil {
		t.Fatal("非法 healthz URL 应拒")
	}
}

func TestValidateListen(t *testing.T) {
	cases := []struct {
		listen string
		ok     bool
	}{
		{":8088", true},
		{"127.0.0.1:8099", true},
		{":443", false}, // 443 归主服务 ACME
		{"0.0.0.0:80", false},
		{":0", false},
		{":65536", false},
		{"bad", false},
		{"", false},
	}
	for _, tc := range cases {
		c := DefaultConfig()
		c.Listen = tc.listen
		err := c.Validate(false)
		if (err == nil) != tc.ok {
			t.Fatalf("Validate(listen=%q) = %v, want ok=%v", tc.listen, err, tc.ok)
		}
	}
	// <1024 的错误必须指引到 8088，避免升级时继续用旧 TLS 端口
	c := DefaultConfig()
	c.Listen = ":443"
	if err := c.Validate(false); err == nil || !strings.Contains(err.Error(), "8088") {
		t.Fatalf("特权端口错误应提示改用 8088: %v", err)
	}
	// once 模式不监听端口，跳过监听校验
	c2 := DefaultConfig()
	c2.Listen = ":443"
	if err := c2.Validate(true); err != nil {
		t.Fatalf("--once 不应校验监听端口: %v", err)
	}
}

func TestParseTagAndOrder(t *testing.T) {
	if _, ok := parseTag("v1.2.3"); !ok {
		t.Fatal("正式 tag 应可解析")
	}
	if _, ok := parseTag("v1.2.3-alpha"); !ok {
		t.Fatal("alpha 应可解析")
	}
	if _, ok := parseTag("v1.2.3-alpha.1"); ok {
		t.Fatal("测试标志后加序号必须拒（版本规范）")
	}
	if _, ok := parseTag("v1.2.3;evil"); ok {
		t.Fatal("注入形态必须拒")
	}
	if isPrereleaseTag("v1.2.3") {
		t.Fatal("正式 tag 不是预发布")
	}
	if !isPrereleaseTag("v1.2.3-rc") {
		t.Fatal("rc 是预发布")
	}

	cases := []struct {
		tag, cur string
		want     bool
	}{
		{"v1.1.0-alpha", "v1.0.0-alpha", true},
		{"v1.0.0-alpha", "v1.1.0-alpha", false},
		{"v1.0.0", "v1.0.0-rc", true},
		{"v1.0.0-alpha", "v1.0.0", false},
		{"v1.0.0", "v1.0.0", false},
		{"v1.0.0-beta", "v1.0.0-alpha", true},
		{"v1.0.0-alpha", "v2.0.0-alpha", false},
		{"v1.0.0-alpha", "", true},
		{"v1.0.0-alpha", "dev", true},
	}
	for _, tc := range cases {
		if got := tagNewerThan(tc.tag, tc.cur); got != tc.want {
			t.Fatalf("tagNewerThan(%q,%q) = %v, want %v", tc.tag, tc.cur, got, tc.want)
		}
	}
}

func TestMatchSHA256(t *testing.T) {
	const asset = "litetalk-server-linux-amd64.tar.gz"
	good := strings.Repeat("ab", 32)
	sums := []byte(strings.Repeat("cd", 32) + "  other.tar.gz\n" + good + "  " + asset + "\n")
	got, err := MatchSHA256(sums, asset)
	if err != nil || got != good {
		t.Fatalf("got %q err %v", got, err)
	}
	// 只认同名行：别的文件的校验和不能混用
	if _, err := MatchSHA256([]byte(strings.Repeat("cd", 32)+"  other.tar.gz\n"), asset); err == nil {
		t.Fatal("缺同名行应报错")
	}
	// 同名多行且不一致：拒绝
	dupe := good + "  " + asset + "\n" + strings.Repeat("ef", 32) + "  " + asset + "\n"
	if _, err := MatchSHA256([]byte(dupe), asset); err == nil {
		t.Fatal("同名多校验和应报错")
	}
	// 二进制模式星号前缀可接受
	star := []byte(good + " *" + asset + "\n")
	if got, err := MatchSHA256(star, asset); err != nil || got != good {
		t.Fatalf("星号行: %q %v", got, err)
	}
}
