package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// #37 B1：频道密码/人数上限/子频道树的迁移与三实现契约。

// TestChannelOptionsMigrationSQLite 0011 up/down/up：旧行默认无密码/不限/顶层。
func TestChannelOptionsMigrationSQLite(t *testing.T) {
	ctx := context.Background()
	m := openTestSQLite(t)
	full := findMigrationsSQLite(t)
	until0011 := migrationsBefore(t, full, "0012")
	if err := m.Migrate(ctx, migrationsBefore(t, full, "0011")); err != nil {
		t.Fatalf("迁移到 0010 失败: %v", err)
	}
	// 旧数据：迁移前的频道行。
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO users (id, email, nickname, tag, password_hash) VALUES (9101, 'c@ex.test', 'c9101', '0001', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO servers (id, name, owner_id) VALUES (9201, 's', 9101)`); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DB().ExecContext(ctx,
		`INSERT INTO channels (id, server_id, name, type, position) VALUES (9301, 9201, 'old', 'voice', 0)`); err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(ctx, until0011); err != nil {
		t.Fatalf("应用 0011 失败: %v", err)
	}
	var hash string
	var limit int
	var parent sql.NullInt64
	if err := m.DB().QueryRowContext(ctx,
		`SELECT password_hash, user_limit, parent_id FROM channels WHERE id = 9301`).
		Scan(&hash, &limit, &parent); err != nil {
		t.Fatal(err)
	}
	if hash != "" || limit != 0 || parent.Valid {
		t.Fatalf("旧行默认值不符: hash=%q limit=%d parent=%v", hash, limit, parent)
	}
	// 索引存在。
	var idx int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_channels_parent'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("parent 索引应存在: %d %v", idx, err)
	}

	// down：三列与索引消失。
	if err := m.RollbackLast(ctx, until0011); err != nil {
		t.Fatalf("down 失败: %v", err)
	}
	var cols int
	if err := m.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('channels') WHERE name IN ('password_hash','user_limit','parent_id')`).
		Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("down 后三列应消失: %d %v", cols, err)
	}
	// 再 up。
	if err := m.Migrate(ctx, until0011); err != nil {
		t.Fatalf("再 up 失败: %v", err)
	}
	if err := m.DB().QueryRowContext(ctx,
		`SELECT password_hash, user_limit, parent_id FROM channels WHERE id = 9301`).
		Scan(&hash, &limit, &parent); err != nil {
		t.Fatal(err)
	}
}

// channelOptsSeed 建一套服务器/分类/父子频道/消息/覆盖，返回引用。
type channelOptsSeed struct {
	srv                *Server
	cat                *Category
	parent, child      *Channel
	top                *Channel
	msgParent, msgChan int64 // 消息 id
}

func seedChannelOptions(t *testing.T, st Store) *channelOptsSeed {
	t.Helper()
	ctx := context.Background()
	srv := &Server{Name: "opts", OwnerID: 1, IconName: "music", IconColor: "#4FA4F3"}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	// 覆盖的目标成员必须真实存在（SetChannelOverwrite 校验同服成员）。
	if err := st.AddMember(ctx, &ServerMember{ServerID: srv.ID, UserID: 1, Role: RoleOwner}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	cat := &Category{ServerID: srv.ID, Name: "c"}
	if err := st.CreateCategory(ctx, cat); err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}
	parent := &Channel{ServerID: srv.ID, Name: "父", Type: ChannelVoice, CategoryID: &cat.ID, Position: 1,
		PasswordHash: "hash-parent", UserLimit: 5}
	if err := st.CreateChannel(ctx, parent); err != nil {
		t.Fatalf("CreateChannel(parent): %v", err)
	}
	child := &Channel{ServerID: srv.ID, Name: "子", Type: ChannelVoice, CategoryID: &cat.ID, Position: 2,
		ParentID: &parent.ID}
	if err := st.CreateChannel(ctx, child); err != nil {
		t.Fatalf("CreateChannel(child): %v", err)
	}
	top := &Channel{ServerID: srv.ID, Name: "顶层", Type: ChannelText, Position: 3}
	if err := st.CreateChannel(ctx, top); err != nil {
		t.Fatalf("CreateChannel(top): %v", err)
	}
	mp := &Message{ChannelID: parent.ID, UserID: 1, Content: "p"}
	if err := st.CreateMessage(ctx, mp); err != nil {
		t.Fatalf("CreateMessage(parent): %v", err)
	}
	mc := &Message{ChannelID: child.ID, UserID: 1, Content: "c"}
	if err := st.CreateMessage(ctx, mc); err != nil {
		t.Fatalf("CreateMessage(child): %v", err)
	}
	if err := st.SetChannelOverwrite(ctx, &ChannelOverwrite{
		ChannelID: parent.ID, TargetType: OverwriteTargetMember, TargetID: 1, Deny: ViewChannel,
	}); err != nil {
		t.Fatalf("SetChannelOverwrite(parent): %v", err)
	}
	if err := st.SetChannelOverwrite(ctx, &ChannelOverwrite{
		ChannelID: child.ID, TargetType: OverwriteTargetMember, TargetID: 1, Deny: ViewChannel,
	}); err != nil {
		t.Fatalf("SetChannelOverwrite(child): %v", err)
	}
	return &channelOptsSeed{srv: srv, cat: cat, parent: parent, child: child, top: top,
		msgParent: mp.ID, msgChan: mc.ID}
}

// runChannelOptionsContract 三实现共用：选项往返、NULL 与 0 不混淆、排序、级联。
func runChannelOptionsContract(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	sd := seedChannelOptions(t, st)

	// 往返：hash/limit/parent 读回一致（hash 不出 JSON 是 wire 层的事，Store 原样存）。
	got, err := st.ChannelByID(ctx, sd.parent.ID)
	if err != nil {
		t.Fatalf("ChannelByID(parent): %v", err)
	}
	if got.PasswordHash != "hash-parent" || got.UserLimit != 5 {
		t.Fatalf("父选项读回不符: %+v", got)
	}
	if got.ParentID != nil {
		t.Fatalf("顶层父应 NULL，实际 %v", *got.ParentID)
	}
	ch, err := st.ChannelByID(ctx, sd.child.ID)
	if err != nil {
		t.Fatalf("ChannelByID(child): %v", err)
	}
	if ch.ParentID == nil || *ch.ParentID != sd.parent.ID {
		t.Fatalf("子频道父级读回不符: %+v", ch)
	}
	if ch.PasswordHash != "" || ch.UserLimit != 0 {
		t.Fatalf("子频道默认选项不符: %+v", ch)
	}

	// NULL 与 0 不混淆：全行写把 parent 置回 NULL、密码清空、上限改 0。
	upd := *ch
	upd.ParentID = nil
	upd.PasswordHash = ""
	upd.UserLimit = 0
	upd.Name = "子2"
	if err := st.UpdateChannel(ctx, &upd); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	got, err = st.ChannelByID(ctx, sd.child.ID)
	if err != nil || got.ParentID != nil || got.PasswordHash != "" || got.UserLimit != 0 || got.Name != "子2" {
		t.Fatalf("提升顶层/清密码读回不符: %+v %v", got, err)
	}
	// 挂回父级（NULL→值 的另一半往返），后续级联用例要它仍是子频道。
	upd.ParentID = &sd.parent.ID
	if err := st.UpdateChannel(ctx, &upd); err != nil {
		t.Fatalf("UpdateChannel(挂回): %v", err)
	}
	got, err = st.ChannelByID(ctx, sd.child.ID)
	if err != nil || got.ParentID == nil || *got.ParentID != sd.parent.ID {
		t.Fatalf("挂回父级读回不符: %+v %v", got, err)
	}

	// 排序：position、id 升序（三实现一致）。
	list, err := st.ChannelsOfServer(ctx, sd.srv.ID)
	if err != nil {
		t.Fatalf("ChannelsOfServer: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("应 3 个频道，实际 %d", len(list))
	}
	if list[0].ID != sd.parent.ID || list[1].ID != sd.child.ID || list[2].ID != sd.top.ID {
		t.Fatalf("排序应 position,id: %+v", list)
	}

	// 级联：删父频道，子频道行/双方消息与覆盖一并消失。
	if err := st.DeleteChannel(ctx, sd.parent.ID); err != nil {
		t.Fatalf("DeleteChannel(parent): %v", err)
	}
	if _, err := st.ChannelByID(ctx, sd.child.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("子频道应随父删除，实际 %v", err)
	}
	if msgs, err := st.MessagesBefore(ctx, sd.child.ID, 0, 10); err != nil || len(msgs) != 0 {
		t.Fatalf("子频道消息应级联删除: %v %v", msgs, err)
	}
	if ows, err := st.ChannelOverwrites(ctx, sd.child.ID); err != nil || len(ows) != 0 {
		t.Fatalf("子频道覆盖应级联删除: %v %v", ows, err)
	}
	// 顶层频道不受影响。
	if _, err := st.ChannelByID(ctx, sd.top.ID); err != nil {
		t.Fatalf("顶层频道应保留: %v", err)
	}
}

func TestChannelOptionsContractMem(t *testing.T) {
	runChannelOptionsContract(t, NewMem())
}

func TestChannelOptionsContractSQLite(t *testing.T) {
	sq := openTestSQLite(t)
	if err := sq.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runChannelOptionsContract(t, sq)
}

func TestChannelOptionsContractMySQL(t *testing.T) {
	m := openRolesMySQL(t)
	if err := m.Migrate(context.Background(), findMigrations(t)); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	runChannelOptionsContract(t, m)
}

// TestChannelOptionsDeleteCategoryCascades 删分类连子频道一起走（跨分类漂移也兜住）。
func TestChannelOptionsDeleteCategoryCascades(t *testing.T) {
	backends := map[string]func(t *testing.T) Store{
		"mem": func(*testing.T) Store { return NewMem() },
		"sqlite": func(t *testing.T) Store {
			sq := openTestSQLite(t)
			if err := sq.Migrate(context.Background(), findMigrationsSQLite(t)); err != nil {
				t.Fatalf("迁移失败: %v", err)
			}
			return sq
		},
		"mysql": func(t *testing.T) Store {
			m := openRolesMySQL(t)
			if err := m.Migrate(context.Background(), findMigrations(t)); err != nil {
				t.Fatalf("迁移失败: %v", err)
			}
			return m
		},
	}
	for name, open := range backends {
		t.Run(name, func(t *testing.T) {
			st := open(t)
			ctx := context.Background()
			sd := seedChannelOptions(t, st)
			if err := st.DeleteCategoryWithChannels(ctx, sd.cat.ID); err != nil {
				t.Fatalf("DeleteCategoryWithChannels: %v", err)
			}
			for _, id := range []int64{sd.parent.ID, sd.child.ID} {
				if _, err := st.ChannelByID(ctx, id); !errors.Is(err, ErrNotFound) {
					t.Fatalf("频道 %d 应随分类删除，实际 %v", id, err)
				}
				if msgs, err := st.MessagesBefore(ctx, id, 0, 10); err != nil || len(msgs) != 0 {
					t.Fatalf("频道 %d 消息应级联删除: %v %v", id, msgs, err)
				}
			}
			// 不在该分类的顶层频道保留。
			if _, err := st.ChannelByID(ctx, sd.top.ID); err != nil {
				t.Fatalf("顶层频道应保留: %v", err)
			}
		})
	}
}
