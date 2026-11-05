package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"sync"
	"testing"

	mysqldrv "github.com/go-sql-driver/mysql"
)

func TestBlobOwnerMemDeploymentIsolation(t *testing.T) {
	ctx := context.Background()
	a, b := NewMem(), NewMem()
	first, err := a.BlobOwnerID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.BlobOwnerID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	other, err := b.BlobOwnerID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !validBlobOwnerID(first) || first != again || first == other {
		t.Fatalf("Mem 部署归属稳定/隔离失败")
	}
}

func TestBlobOwnerSQLiteConcurrentRestartAndIsolation(t *testing.T) {
	t.Setenv("LITETALK_SQLITE_PATH", "")
	path := filepath.Join(t.TempDir(), "a.db")
	a, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatal(err)
	}
	b, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	ids := concurrentBlobOwners(t, a, b)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	again, err := restarted.BlobOwnerID(context.Background())
	if err != nil || again != ids[0] {
		t.Fatalf("重启后归属漂移")
	}
	otherPath := filepath.Join(t.TempDir(), "b.db")
	other, err := OpenSQLite(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatal(err)
	}
	otherID, err := other.BlobOwnerID(context.Background())
	if err != nil || otherID == ids[0] {
		t.Fatalf("不同数据库归属未隔离")
	}
}

func TestBlobOwnerMySQLConcurrentRestart(t *testing.T) {
	a := openRolesMySQL(t)
	if err := a.Migrate(context.Background(), findMigrations(t)); err != nil {
		t.Fatal(err)
	}
	b := openBlobOwnerMySQLPeer(t, a)
	ids := concurrentBlobOwners(t, a, b)
	restarted := openBlobOwnerMySQLPeer(t, a)
	again, err := restarted.BlobOwnerID(context.Background())
	if err != nil || again != ids[0] {
		t.Fatalf("MySQL 重连后归属漂移")
	}
}

func openBlobOwnerMySQLPeer(t *testing.T, source *MySQL) *MySQL {
	t.Helper()
	var schema string
	if err := source.DB().QueryRow(`SELECT DATABASE()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^litetalk_ci_roles_[0-9a-f]{16}$`).MatchString(schema) {
		t.Fatalf("非临时测试库: %q", schema)
	}
	cfg, err := mysqldrv.ParseDSN(testMySQLDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = schema
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	db.SetMaxOpenConns(20)
	m := &MySQL{db: db}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func concurrentBlobOwners(t *testing.T, a, b Store) [2]string {
	t.Helper()
	start := make(chan struct{})
	var wg sync.WaitGroup
	var ids [2]string
	var errs [2]error
	for i, st := range []Store{a, b} {
		wg.Add(1)
		go func(i int, st Store) {
			defer wg.Done()
			<-start
			ids[i], errs[i] = st.BlobOwnerID(context.Background())
		}(i, st)
	}
	close(start)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || !validBlobOwnerID(ids[0]) || ids[0] != ids[1] {
		t.Fatalf("并发归属不一致: ids=%v errors=%v", ids, errs)
	}
	return ids
}
