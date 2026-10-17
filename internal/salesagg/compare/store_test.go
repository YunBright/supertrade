package compare

import (
	"context"
	"errors"
	"testing"
	"time"

	// modernc 是纯 Go 的 SQLite（无需 cgo），gorm 的 sqlite.Dialector
	// 只认 database/sql 注册名 —— 必须空白导入注册驱动。
	_ "modernc.org/sqlite"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newStoreDB 起一个进程内 SQLite 库（纯 Go 驱动 modernc，不依赖 cgo）。
//
// _time_format=sqlite：时间存 INTEGER（unix 秒），与 gorm time.Time 兼容；
// MaxOpenConns(1)：":memory:" 库每个连接是独立实例，>1 时建表与查询
// 可能落在不同连接上，表现为"表不存在"。
func newStoreDB(t *testing.T) *Store {
	t.Helper()
	dialector := sqlite.Dialector{
		DriverName: "sqlite",
		DSN:        ":memory:?_pragma=foreign_keys(1)&_time_format=sqlite",
	}
	db, err := gorm.Open(dialector, &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&Template{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	return NewStore(db)
}

func seedTemplate(t *testing.T, s *Store, name string) *Template {
	t.Helper()
	tpl, err := TemplateFromInput(SaveInput{
		Name:      name,
		Dimension: DimCategory,
		Stores:    []string{"S001", "S002"},
		Rows: []RowJSON{{
			Key:   "r1",
			Label: "猪肉档",
			Items: map[string]*string{"S001": strPtr("01"), "S002": nil},
		}},
	})
	if err != nil {
		t.Fatalf("TemplateFromInput: %v", err)
	}
	if err := s.Create(context.Background(), tpl); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return tpl
}

// TestStore_CreateGetRoundtrip 创建后取回，业务列与身份列完整往返，
// revision 从 1 起，stores/rows 里的 null 编码不丢。
func TestStore_CreateGetRoundtrip(t *testing.T) {
	s := newStoreDB(t)
	tpl := seedTemplate(t, s, "对比 A")

	if tpl.ID == "" {
		t.Fatal("Create 未分配 uuid")
	}
	if tpl.Revision != 1 {
		t.Fatalf("revision = %d, want 1", tpl.Revision)
	}
	got, err := s.Get(context.Background(), tpl.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "对比 A" || got.Dimension != DimCategory {
		t.Fatalf("Get 业务列不匹配: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatal("时间戳未由 gorm 自动填充")
	}
	tj, err := got.ToJson()
	if err != nil {
		t.Fatalf("ToJson: %v", err)
	}
	if len(tj.Stores) != 2 || tj.Stores[0] != "S001" {
		t.Fatalf("stores 往返失真: %v", tj.Stores)
	}
	if len(tj.Rows) != 1 || tj.Rows[0].Items["S001"] == nil || *tj.Rows[0].Items["S001"] != "01" {
		t.Fatalf("rows 往返失真: %+v", tj.Rows)
	}
	if tj.Rows[0].Items["S002"] != nil {
		t.Fatal("null 编码往返后应仍为 null")
	}
}

// TestStore_UpdateOptimisticLock 乐观锁三分支：正确 revision 更新成功
// （revision +1、created_* 不动）；旧 revision → ErrConflict；
// 不存在的 id → ErrNotFound（而不是含糊的冲突）。
func TestStore_UpdateOptimisticLock(t *testing.T) {
	s := newStoreDB(t)
	tpl := seedTemplate(t, s, "对比 B")
	ctx := context.Background()

	// 正确 revision：改名 + 改行，updated_by 落库，created_by 保持。
	upd, err := TemplateFromInput(SaveInput{
		Name:      "对比 B2",
		Dimension: DimProduct,
		Stores:    []string{"S001", "S003"},
		Rows:      []RowJSON{{Key: "k", Label: "行", Items: nil}},
	})
	if err != nil {
		t.Fatalf("TemplateFromInput: %v", err)
	}
	upd.ID = tpl.ID
	upd.UpdatedBy = "user-9"
	if err := s.Update(ctx, upd, 1); err != nil {
		t.Fatalf("Update(正确 revision): %v", err)
	}
	if upd.Revision != 2 {
		t.Fatalf("revision = %d, want 2", upd.Revision)
	}
	got, err := s.Get(ctx, tpl.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "对比 B2" || got.Dimension != DimProduct || got.UpdatedBy != "user-9" {
		t.Fatalf("Update 未落库: %+v", got)
	}
	if got.CreatedBy != tpl.CreatedBy || !got.CreatedAt.Equal(tpl.CreatedAt) {
		t.Fatalf("created_* 被意外更新: %+v", got)
	}
	if got.Revision != 2 {
		t.Fatalf("库里 revision = %d, want 2", got.Revision)
	}

	// 旧 revision（1）→ ErrConflict。
	stale, _ := TemplateFromInput(SaveInput{
		Name: "对比 C", Dimension: DimCategory,
		Stores: []string{"A", "B"}, Rows: []RowJSON{{Key: "k", Label: "行"}},
	})
	stale.ID = tpl.ID
	if err := s.Update(ctx, stale, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("Update(旧 revision) = %v, want ErrConflict", err)
	}

	// 不存在的 id → ErrNotFound。
	ghost, _ := TemplateFromInput(SaveInput{
		Name: "幽灵", Dimension: DimCategory,
		Stores: []string{"A", "B"}, Rows: []RowJSON{{Key: "k", Label: "行"}},
	})
	ghost.ID = "no-such-id"
	if err := s.Update(ctx, ghost, 7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update(不存在) = %v, want ErrNotFound", err)
	}
}

// TestStore_ListOrderByUpdatedAtDesc List 按 updated_at 倒序。
// 用 UpdateColumn 直改时间绕开 gorm 自动时间戳（否则两条记录落在
// 同一秒内，排序不稳定）。
func TestStore_ListOrderByUpdatedAtDesc(t *testing.T) {
	s := newStoreDB(t)
	ctx := context.Background()
	first := seedTemplate(t, s, "旧")
	second := seedTemplate(t, s, "新")

	if err := s.db.WithContext(ctx).Model(&Template{}).
		Where("id = ?", first.ID).
		UpdateColumn("updated_at", time.Now().Add(-2*time.Hour)).Error; err != nil {
		t.Fatalf("backdate: %v", err)
	}
	_ = second

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List 长度 = %d, want 2", len(list))
	}
	if list[0].ID != second.ID || list[1].ID != first.ID {
		t.Fatalf("List 顺序错误: [%s, %s], want [%s, %s]",
			list[0].Name, list[1].Name, second.Name, first.Name)
	}
}

// TestStore_Delete 删除三分支：存在 → 删掉且 Get 变 NotFound；
// 不存在 → ErrNotFound（幂等失败比幂等成功诚实 —— 删一个已消失的
// 模板应让调用方知道，而不是装作成功）。
func TestStore_Delete(t *testing.T) {
	s := newStoreDB(t)
	ctx := context.Background()
	tpl := seedTemplate(t, s, "待删")

	if err := s.Delete(ctx, tpl.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, tpl.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后 Get = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, tpl.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复 Delete = %v, want ErrNotFound", err)
	}
}
