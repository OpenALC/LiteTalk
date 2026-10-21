package updater

import (
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
	if c.Domain != DefaultDomain || c.Repo != DefaultRepo || c.InstallDir != DefaultInstallDir {
		t.Fatalf("默认值漂移: %+v", c)
	}
}

func TestLoadEnv(t *testing.T) {
	env := map[string]string{
		"LITETALK_UPDATER_ENABLED":          "0",
		"LITETALK_UPDATER_ALLOW_PRERELEASE": "1",
		"LITETALK_UPDATER_LISTEN":           "127.0.0.1:8099",
		"LITETALK_UPDATER_DOMAIN":           "up.example.com",
		"LITETALK_UPDATER_NO_TLS":           "1",
		"LITETALK_UPDATER_REPO":             "OpenALC/Fork",
		"LITETALK_UPDATER_INSTALL_DIR":      "/opt/lt",
		"LITETALK_UPDATER_SERVICE":          "lt.service",
		"LITETALK_UPDATER_HEALTH_URL":       "http://127.0.0.1:9/x",
		"LITETALK_UPDATER_STATE_DIR":        "/var/lib/lt",
		"LITETALK_UPDATER_HEALTH_TIMEOUT":   "30s",
	}
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
	if c.Listen != "127.0.0.1:8099" || c.Domain != "up.example.com" || !c.NoTLS {
		t.Fatalf("监听/域名/TLS 未生效: %+v", c)
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
