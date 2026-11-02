package config

// 本文件承载 v1.10.0 GTK4 迁移（issue #5：G1 门禁不通过 → 迁移计划书 §3.2
// 降级 1）的休眠模式降级逻辑。平台缺省值见 hibernate_mode_linux.go /
// hibernate_mode_other.go，GTK4 构建标记见 hibernate_gtk4.go /
// hibernate_not_gtk4.go。

// HibernateModeDefault 平台休眠模式缺省值：Linux 为 hibernate（v1.10.0 起），
// 其余平台 off。只用于配置缺省（无值/非法值），不覆盖用户显式选择。
func HibernateModeDefault() string { return defaultHibernateMode }

// HibernateDestroyAvailable 当前构建是否允许 destroy 休眠模式。
// 仅 GTK4 构建为 false，设置页据此隐藏「彻底卸载」选项。
func HibernateDestroyAvailable() bool { return !gtk4Build }

// downgradeDestroyForGTK4 GTK4 构建把 destroy 强降为 hibernate，其余原样返回。
//
// 为什么强降（三点均来自 2026-11-02 W0 真机实测，详见基线数据文档）：
//  1. 泄漏机制：wails v3.0.0-beta.30 上游 linux_cgo.c 的 windowNew 对
//     GtkWindow/WebKitWebView 做 g_object_ref_sink 后没有配对 unref，GTK4
//     路径也没有可借道的 dispose 链——每建/销一轮窗口就滞留一个
//     WebKitWebProcess（WebKit 的进程模型是每窗口一个进程）及其全部状态。
//  2. 实测数据：destroy 模式连续 20 轮建/销窗口，WebKitWebProcess 数从 2 涨到
//     21、永不退出，进程树 RSS 线性涨到 7.4 GiB（约 +348MiB/轮）；gtk3 对照
//     平稳。用户选了 destroy 就等于每关一次窗泄漏一份整页内存，因此即使
//     存量配置里写的是 destroy 也必须强制降级。
//  3. 如何恢复：wails 修复窗口对象泄漏（升级版本属单独任务、需用户批准）后，
//     删掉本函数的强降分支（及 hibernate_gtk4.go / hibernate_not_gtk4.go 的
//     gtk4Build 常量）即可——本函数从不改写文件，destroy 的原始值一直留在
//     config.json 里，读路径恢复后自动回到用户原选择。
//
// 只降读路径、不回写文件：gtk3 legacy 构建里 destroy 仍正常工作，回写会把
// 用户回滚到 gtk3 后的原始选择永久破坏掉。本进程内也不再改写 m.cfg——
// Update/Save 以 m.cfg 为底稿，一旦改写，后续任何无关保存都会把强降值
// 带回磁盘。强降由 HibernateMode() 读路径承载，自 Manager 加载起即生效。
func downgradeDestroyForGTK4(mode string) string {
	if gtk4Build && mode == "destroy" {
		return "hibernate"
	}
	return mode
}
