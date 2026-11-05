package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenALC/LiteTalk/client/core/config"
)

// 验证五档参数与环境逐项覆盖：变量一旦存在就不覆盖，交由 runtime 处理。
// 第三档保留 GOGC=off + 18MiB；gcKeep/memLimitKeep 表示不调用 setter。
func TestRuntimeMemoryPolicy(t *testing.T) {
	tests := []struct {
		name      string
		level     int
		env       map[string]string
		wantGC    int
		wantLimit int64
	}{
		{"缺省中档 off + 18MiB", 0, nil, gcOff, 18 << 20},
		{"非法档回中档", 9, nil, gcOff, 18 << 20},
		{"弱一档 off + 64MiB", 1, nil, gcOff, 64 << 20},
		{"弱二档 off + 32MiB", 2, nil, gcOff, 32 << 20},
		{"强四档 100 + 18MiB", 4, nil, 100, 18 << 20},
		{"强五档 50 + 18MiB", 5, nil, 50, 18 << 20},
		{"仅 GOGC=100", 3, map[string]string{"GOGC": "100"}, gcKeep, 18 << 20},
		{"仅 GOGC 空值", 4, map[string]string{"GOGC": ""}, gcKeep, 18 << 20},
		{"仅 GOGC 非法值", 5, map[string]string{"GOGC": "abc"}, gcKeep, 18 << 20},
		{"仅 GOMEMLIMIT 显式设置", 1, map[string]string{"GOMEMLIMIT": "10MiB"}, gcOff, memLimitKeep},
		{"两者均显式设置", 4, map[string]string{"GOGC": "100", "GOMEMLIMIT": "13MiB"}, gcKeep, memLimitKeep},
		{"空值也算显式设置", 3, map[string]string{"GOGC": "", "GOMEMLIMIT": ""}, gcKeep, memLimitKeep},
		{"非法值只判断存在性不解析", 3, map[string]string{"GOGC": "abc", "GOMEMLIMIT": "not-a-size"}, gcKeep, memLimitKeep},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(key string) (string, bool) {
				v, ok := tt.env[key]
				return v, ok
			}
			gotGC, gotLimit := runtimeMemoryPolicy(tt.level, lookup)
			if gotGC != tt.wantGC {
				t.Errorf("gcPercent = %d, want %d", gotGC, tt.wantGC)
			}
			if gotLimit != tt.wantLimit {
				t.Errorf("memLimit = %d, want %d", gotLimit, tt.wantLimit)
			}
		})
	}
}

// 日志使用 runtime 实际读数，不把环境变量原文误报为生效值。
func TestFormatRuntimeMemoryLog(t *testing.T) {
	status := RuntimeGCSettings{
		Level: 3, EffectiveGOGC: "off", EffectiveMemoryLimit: "18MiB",
		GOGCOverridden: true,
	}
	want := "runtime memory: GOGC=off GOMEMLIMIT=18MiB level=3 override(GOGC=true,GOMEMLIMIT=false)"
	if got := formatRuntimeMemoryLog(status); got != want {
		t.Errorf("formatRuntimeMemoryLog() = %q, want %q", got, want)
	}
}

// 子进程让 runtime 从指定环境变量启动，避免 SetGCPercent/SetMemoryLimit 污染其它测试。
func TestRuntimeGCSubprocess(t *testing.T) {
	tests := []struct {
		name, configJSON string
		env              []string
		level            int
		gogc, limit      string
	}{
		{"旧配置缺省", `{}`, nil, 3, "off", "18MiB"},
		{"非法档回中档", `{"gc_level":9}`, nil, 3, "off", "18MiB"},
		{"最弱档", `{"gc_level":1}`, nil, 1, "off", "64MiB"},
		{"次弱档", `{"gc_level":2}`, nil, 2, "off", "32MiB"},
		{"次强档", `{"gc_level":4}`, nil, 4, "100", "18MiB"},
		{"最强档", `{"gc_level":5}`, nil, 5, "50", "18MiB"},
		{"仅GOGC覆盖", `{"gc_level":5}`, []string{"GOGC=200"}, 5, "200", "18MiB"},
		{"非法GOGC交给runtime", `{"gc_level":5}`, []string{"GOGC=invalid"}, 5, "100", "18MiB"},
		{"仅GOMEMLIMIT覆盖", `{"gc_level":1}`, []string{"GOMEMLIMIT=24MiB"}, 1, "off", "24MiB"},
		{"空GOMEMLIMIT交给runtime", `{"gc_level":1}`, []string{"GOMEMLIMIT="}, 1, "off", "无上限"},
		{"两项分别覆盖", `{"gc_level":4}`, []string{"GOGC=75", "GOMEMLIMIT=22MiB"}, 4, "75", "22MiB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tt.configJSON), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRuntimeGCSubprocessHelper$")
			for _, value := range os.Environ() {
				key, _, _ := strings.Cut(value, "=")
				if !strings.EqualFold(key, "GOGC") && !strings.EqualFold(key, "GOMEMLIMIT") &&
					!strings.HasPrefix(strings.ToUpper(key), "LITETALK_GC_TEST_") {
					cmd.Env = append(cmd.Env, value)
				}
			}
			cmd.Env = append(cmd.Env, tt.env...)
			cmd.Env = append(cmd.Env,
				"LITETALK_GC_TEST_CHILD=1", "LITETALK_GC_TEST_CONFIG="+path,
				fmt.Sprintf("LITETALK_GC_TEST_LEVEL=%d", tt.level),
				"LITETALK_GC_TEST_GOGC="+tt.gogc,
				"LITETALK_GC_TEST_LIMIT="+tt.limit,
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("子进程检查失败: %v\n%s", err, out)
			}
		})
	}
}

func TestRuntimeGCSubprocessHelper(t *testing.T) {
	if os.Getenv("LITETALK_GC_TEST_CHILD") != "1" {
		return
	}
	m := config.NewWithPath(os.Getenv("LITETALK_GC_TEST_CONFIG"))
	a := newAppWithConfig(m)
	level := m.GCLevel()
	applyRuntimeMemoryPolicy(level, os.LookupEnv)
	status := a.GetGCSettings()
	wantLevel := config.GCLevelDefault
	if _, err := fmt.Sscan(os.Getenv("LITETALK_GC_TEST_LEVEL"), &wantLevel); err != nil {
		t.Fatal(err)
	}
	if status.Level != wantLevel || status.EffectiveGOGC != os.Getenv("LITETALK_GC_TEST_GOGC") ||
		status.EffectiveMemoryLimit != os.Getenv("LITETALK_GC_TEST_LIMIT") {
		t.Fatalf("runtime 状态 = %+v", status)
	}
	_, wantGOGCOverride := os.LookupEnv("GOGC")
	_, wantLimitOverride := os.LookupEnv("GOMEMLIMIT")
	if !status.Supported || status.GOGCOverridden != wantGOGCOverride ||
		status.MemoryLimitOverridden != wantLimitOverride {
		t.Fatalf("环境覆盖状态 = %+v", status)
	}
	// 启动后的频繁设置须保持配置与 runtime 同步，失败不能冒充热应用成功。
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(level int) {
			defer wg.Done()
			if _, err := a.SetGCLevel(level); err != nil {
				t.Errorf("SetGCLevel(%d): %v", level, err)
			}
		}(i%5 + 1)
	}
	wg.Wait()
	final := a.GetGCSettings()
	if final.Level != m.GCLevel() || final.Level != config.NewWithPath(m.Path()).GCLevel() {
		t.Fatalf("并发保存档位不一致: %+v", final)
	}
	wantGC, wantLimit := runtimeMemoryPolicy(final.Level, os.LookupEnv)
	if wantGC != gcKeep {
		if wantGC == gcOff && final.EffectiveGOGC != "off" ||
			wantGC >= 0 && final.EffectiveGOGC != fmt.Sprint(wantGC) {
			t.Fatalf("并发热应用 GOGC 不一致: %+v", final)
		}
	}
	if wantLimit != memLimitKeep && final.EffectiveMemoryLimit != fmt.Sprintf("%dMiB", wantLimit>>20) {
		t.Fatalf("并发热应用软上限不一致: %+v", final)
	}
}

func TestSetGCLevelSaveFailureDoesNotApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	a := newAppWithConfig(config.NewWithPath(path))
	before := a.GetGCSettings()
	if _, err := a.SetGCLevel(5); err == nil {
		t.Fatal("保存失败不应返回成功")
	}
	after := a.GetGCSettings()
	if after.Level != before.Level || after.EffectiveGOGC != before.EffectiveGOGC ||
		after.EffectiveMemoryLimit != before.EffectiveMemoryLimit {
		t.Fatalf("保存失败改变了运行时状态: before=%+v after=%+v", before, after)
	}
}

// 验证 pprof 探针只接受回环地址与合法端口，防止误配暴露到公网。
func TestParsePprofAddr(t *testing.T) {
	valid := []string{
		"127.0.0.1:6060",
		"127.0.0.2:1234", // 127/8 整段都是回环
		"localhost:6060",
		"[::1]:6060",
	}
	for _, raw := range valid {
		if got, ok := parsePprofAddr(raw); !ok || got != raw {
			t.Errorf("parsePprofAddr(%q) = %q, %v；应接受并原样返回", raw, got, ok)
		}
	}

	invalid := []string{
		"",
		"127.0.0.1",         // 缺端口
		":6060",             // 空 host 会监听所有网卡
		"0.0.0.0:6060",      // 非回环
		"192.168.1.10:6060", // 非回环
		"example.com:6060",  // 非 IP 且非 localhost
		"127.0.0.1:0",       // 端口 0 无诊断意义
		"127.0.0.1:65536",   // 端口越界
		"127.0.0.1:http",    // 端口必须是数字
		"[::1]:",            // 空端口
		"::1:6060",          // 缺方括号，SplitHostPort 失败
	}
	for _, raw := range invalid {
		if got, ok := parsePprofAddr(raw); ok {
			t.Errorf("parsePprofAddr(%q) = %q, true；应拒绝", raw, got)
		}
	}
}
