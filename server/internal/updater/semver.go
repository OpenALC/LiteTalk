package updater

import (
	"strconv"
	"strings"
)

// tagVer 解析后的版本元组。后缀权重：alpha < beta < rc < 正式版。
type tagVer struct {
	major, minor, patch int
	pre                 int // 0=alpha 1=beta 2=rc 3=stable
}

const (
	preAlpha = iota
	preBeta
	preRC
	preStable
)

// parseTag 解析 tag 白名单内形态。不匹配 TagRe 的一律 false（fail-closed）。
func parseTag(s string) (tagVer, bool) {
	if !TagRe.MatchString(s) {
		return tagVer{}, false
	}
	body := strings.TrimPrefix(s, "v")
	pre := preStable
	if i := strings.IndexByte(body, '-'); i >= 0 {
		switch body[i+1:] {
		case "alpha":
			pre = preAlpha
		case "beta":
			pre = preBeta
		case "rc":
			pre = preRC
		default:
			return tagVer{}, false
		}
		body = body[:i]
	}
	parts := strings.Split(body, ".")
	if len(parts) != 3 {
		return tagVer{}, false
	}
	var nums [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return tagVer{}, false
		}
		nums[i] = n
	}
	return tagVer{nums[0], nums[1], nums[2], pre}, true
}

// newerThan a 是否严格新于 b。
func (a tagVer) newerThan(b tagVer) bool {
	if a.major != b.major {
		return a.major > b.major
	}
	if a.minor != b.minor {
		return a.minor > b.minor
	}
	if a.patch != b.patch {
		return a.patch > b.patch
	}
	return a.pre > b.pre
}

// tagNewerThan 供 webhook 防降级：重投旧 release 不允许把线上版本打回去。
// current 为空/不可解析（如 dev）时视为可接管。
func tagNewerThan(tag, current string) bool {
	a, ok := parseTag(tag)
	if !ok {
		return false
	}
	if current == "" {
		return true
	}
	b, ok := parseTag(current)
	if !ok {
		return true
	}
	return a.newerThan(b)
}

// isPrereleaseTag tag 是否带预发布后缀（通道策略用）。
func isPrereleaseTag(tag string) bool {
	return strings.Contains(tag, "-")
}
