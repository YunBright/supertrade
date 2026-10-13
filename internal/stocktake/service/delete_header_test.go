package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/cubeclient/cubeclientfake"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/service"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// 本文件覆盖 DELETE /stocktake-headers/:id 的 service 实现（2026-10-13 新增）。
//
// ## 为什么必须真跑一次 DB
//
// 这个端点的全部价值在**级联**。四张从表里只有 stocktake_lines 声明了
// FK CASCADE，另外三张（line_operations / plan_items / branch_defaults）
// 是裸 string 列、无外键约束 —— 只删主表不会报错，只会留下：
//
//	- 操作历史挂在一个不存在的单号下
//	- 计划商品永远查不出来
//	- **门店默认盘点单指向已删除的单** → 前端"快速进入盘点"直接坏掉
//
// 而"留下孤儿行"是**静默**的：所有查询都不报错，只是数据在慢慢腐烂。
// 所以断言必须逐张表查"删干净了"，而不是只断言主表没了。

// setupForDelete 返回 service 与底层 db —— 级联断言需要直接查表。
func setupForDelete(t *testing.T) (*service.Service, *gorm.DB) {
	t.Helper()
	db, err := testdb.OpenSQLite(":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.StocktakeHeader{},
		&model.StocktakeLine{},
		&model.StocktakeLineOperation{},
		&model.StocktakePlanItem{},
		&model.StocktakeBranchDefault{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return service.New(db, cubeclientfake.New()), db
}

// seedHeader 造一张单据，并给四张从表各塞一行，模拟"用过一轮"的盘点单。
func seedHeader(t *testing.T, db *gorm.DB, id string, status model.StocktakeStatus) {
	t.Helper()
	now := time.Now()
	h := &model.StocktakeHeader{
		ID: id, BranchID: "00", CountDate: now, Status: status,
		Type: model.TypeGeneral, OperatorID: "u1",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(h).Error; err != nil {
		t.Fatalf("seed header: %v", err)
	}
	line := &model.StocktakeLine{
		ID: id + "-L1", HeaderID: id, ProductID: "P1", ProductName: "商品",
		BookQty: decimal.NewFromInt(10), ActualQty: decimal.NewFromInt(9),
		BookQtyAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(line).Error; err != nil {
		t.Fatalf("seed line: %v", err)
	}
	rows := []any{
		&model.StocktakeLineOperation{
			ID: id + "-O1", HeaderID: id, LineID: line.ID, OpType: model.OpCreate,
			ActorID: "u1", ActorName: "n", PrevQty: decimal.Zero,
			NewQty: decimal.NewFromInt(9), QtyDelta: decimal.NewFromInt(9),
			OpAt: now, Method: model.MethodManual, CreatedAt: now,
		},
		&model.StocktakePlanItem{
			ID: id + "-P1", HeaderID: id, ProductID: "P1", ProductName: "商品",
			CreatedAt: now,
		},
		&model.StocktakeBranchDefault{
			BranchID: "00", HeaderID: id, UpdatedBy: "u1", UpdatedAt: now,
		},
	}
	for _, r := range rows {
		if err := db.Create(r).Error; err != nil {
			t.Fatalf("seed child row: %v", err)
		}
	}
}

// countRows 统计某表里属于 headerID 的行数。
func countRows(t *testing.T, db *gorm.DB, model any, headerID string) int64 {
	t.Helper()
	var n int64
	if err := db.Model(model).Where("header_id = ?", headerID).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestDeleteHeader_RemovesAllChildren —— 本端点最核心的断言。
//
// 少删任何一张表，这条用例就红。历史上最容易被省掉的是
// stocktake_branch_defaults：它指向被删的单，前端"快速进入盘点"会直接坏。
func TestDeleteHeader_RemovesAllChildren(t *testing.T) {
	svc, db := setupForDelete(t)
	const id = "ST-DEL-1"
	seedHeader(t, db, id, model.StatusCounting)

	if err := svc.DeleteHeader(context.Background(), id); err != nil {
		t.Fatalf("DeleteHeader: %v", err)
	}

	var h model.StocktakeHeader
	if err := db.First(&h, "id = ?", id).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Errorf("主表行应被删除，实际仍可查到: %v", err)
	}
	for _, tc := range []struct {
		name  string
		model any
	}{
		{"stocktake_lines", &model.StocktakeLine{}},
		{"stocktake_line_operations", &model.StocktakeLineOperation{}},
		{"stocktake_plan_items", &model.StocktakePlanItem{}},
		{"stocktake_branch_defaults", &model.StocktakeBranchDefault{}},
	} {
		if n := countRows(t, db, tc.model, id); n != 0 {
			t.Errorf("%s 残留 %d 行 —— 这是**无外键**表,不会级联,必须显式删", tc.name, n)
		}
	}
}

// TestDeleteHeader_RejectsApproved —— 已审核的单不可删。
//
// approved 的盘点单已经过差异调整、影响过库存口径，删掉会让库存与账面
// 产生无法解释的差额。契约测试造的单都停在 counting，不受该闸门影响。
func TestDeleteHeader_RejectsApproved(t *testing.T) {
	svc, db := setupForDelete(t)
	const id = "ST-APPROVED"
	seedHeader(t, db, id, model.StatusApproved)

	err := svc.DeleteHeader(context.Background(), id)
	if !errors.Is(err, service.ErrHeaderApproved) {
		t.Fatalf("approved 单应返 ErrHeaderApproved，实际 %v", err)
	}
	if n := countRows(t, db, &model.StocktakeLine{}, id); n == 0 {
		t.Error("被拒绝的删除不该动任何从表")
	}
	var h model.StocktakeHeader
	if err := db.First(&h, "id = ?", id).Error; err != nil {
		t.Errorf("被拒绝的删除不该动主表: %v", err)
	}
}

// TestDeleteHeader_AllowsAdjusted —— adjusted(已提交未审核)仍可删。
//
// 反证闸门没有写成"只允许 counting"：那会把已提交但没审核的单也锁死，
// 而它们同样不影响库存口径。
func TestDeleteHeader_AllowsAdjusted(t *testing.T) {
	svc, db := setupForDelete(t)
	const id = "ST-ADJ"
	seedHeader(t, db, id, model.StatusAdjusted)

	if err := svc.DeleteHeader(context.Background(), id); err != nil {
		t.Fatalf("adjusted 单应可删，实际 %v", err)
	}
	if n := countRows(t, db, &model.StocktakeLineOperation{}, id); n != 0 {
		t.Errorf("从表残留 %d 行", n)
	}
}

func TestDeleteHeader_NotFound(t *testing.T) {
	svc, _ := setupForDelete(t)
	if err := svc.DeleteHeader(context.Background(), "ST-NOPE"); !errors.Is(err, service.ErrHeaderNotFound) {
		t.Fatalf("不存在的单应返 ErrHeaderNotFound，实际 %v", err)
	}
}

func TestDeleteHeader_EmptyID(t *testing.T) {
	svc, _ := setupForDelete(t)
	err := svc.DeleteHeader(context.Background(), "")
	if err == nil {
		t.Fatal("空 id 必须报错 —— 少了这条,空串会让 WHERE header_id='' 命中全表同空行的行")
	}
}