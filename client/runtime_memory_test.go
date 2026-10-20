package main

import "testing"

// 验证默认内存策略只补缺省值：GOGC/GOMEMLIMIT 一旦存在（哪怕空值或非法值）
// 就不覆盖，交由 runtime 自行处理。默认档是 GOGC=off + 18MiB 软上限：GC 只在
// 堆接近上限时触发，CPU 占用更低更平稳。gcKeep/memLimitKeep 表示不覆盖。
func TestRuntimeMemoryPolicy(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		wantGC    int
		wantLimit int64
	}{
		{"均未设置时默认 off + 18MiB", nil, gcOff, 18 << 20},
		{"仅 GOGC=100", map[string]string{"GOGC": "100"}, gcKeep, 18 << 20},
		{"仅 GOGC=off", map[string]string{"GOGC": "off"}, gcKeep, 18 << 20},
		{"仅 GOGC 空值", map[string]string{"GOGC": ""}, gcKeep, 18 << 20},
		{"仅 GOGC 非法值", map[string]string{"GOGC": "abc"}, gcKeep, 18 << 20},
		{"仅 GOMEMLIMIT 显式设置", map[string]string{"GOMEMLIMIT": "10MiB"}, gcOff, memLimitKeep},
		{"两者均显式设置", map[string]string{"GOGC": "100", "GOMEMLIMIT": "13MiB"}, gcKeep, memLimitKeep},
		{"空值也算显式设置", map[string]string{"GOGC": "", "GOMEMLIMIT": ""}, gcKeep, memLimitKeep},
		{"非法值只判断存在性不解析", map[string]string{"GOGC": "abc", "GOMEMLIMIT": "not-a-size"}, gcKeep, memLimitKeep},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(key string) (string, bool) {
				v, ok := tt.env[key]
				return v, ok
			}
			gotGC, gotLimit := runtimeMemoryPolicy(lookup)
			if gotGC != tt.wantGC {
				t.Errorf("gcPercent = %d, want %d", gotGC, tt.wantGC)
			}
			if gotLimit != tt.wantLimit {
				t.Errorf("memLimit = %d, want %d", gotLimit, tt.wantLimit)
			}
		})
	}
}

// 验证 app.log 的生效值描述：未设置时打印默认 off/18MiB，显式设置时原样打印
// 环境变量值（含空值与非法值），便于排障时区分用户配置与默认档。
func TestFormatRuntimeMemoryLog(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"均未设置打印默认档", nil, "runtime memory: GOGC=off GOMEMLIMIT=18874368"},
		{"仅 GOGC 显式", map[string]string{"GOGC": "100"}, "runtime memory: GOGC=100 GOMEMLIMIT=18874368"},
		{"仅 GOMEMLIMIT 显式", map[string]string{"GOMEMLIMIT": "13MiB"}, "runtime memory: GOGC=off GOMEMLIMIT=13MiB"},
		{"两者显式原样打印", map[string]string{"GOGC": "off", "GOMEMLIMIT": "20MiB"}, "runtime memory: GOGC=off GOMEMLIMIT=20MiB"},
		{"空值也原样打印", map[string]string{"GOGC": "", "GOMEMLIMIT": ""}, "runtime memory: GOGC= GOMEMLIMIT="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(key string) (string, bool) {
				v, ok := tt.env[key]
				return v, ok
			}
			if got := formatRuntimeMemoryLog(lookup); got != tt.want {
				t.Errorf("formatRuntimeMemoryLog() = %q, want %q", got, tt.want)
			}
		})
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
