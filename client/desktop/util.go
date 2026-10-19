package desktop

// DerefInt 解引用可选 int 指针，nil 回落默认值。
func DerefInt(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}
