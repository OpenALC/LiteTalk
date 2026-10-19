//go:build windows

package store

// Windows 不适用 POSIX 权限位（os.Chmod 只能切换只读标志，真实访问控制走 ACL，
// 由数据目录的默认继承决定）。保留同签名 no-op，保证打开路径三平台一致编译；
// 不做「假装收紧了」的操作。

func tightenSQLitePerms(dir, dbPath string) error { return nil }

func precreateSQLiteFile(path string) error { return nil }
