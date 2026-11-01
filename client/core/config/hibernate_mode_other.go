//go:build !linux

package config

// defaultHibernateMode 非 Linux 平台的休眠模式缺省值：off。维持历史行为，
// 不随 GTK4 迁移变化（Linux 侧为何是 hibernate 见 hibernate_mode_linux.go）。
const defaultHibernateMode = "off"
