package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubeclient/cubeclientfake"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/service"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// 排除域(猪肉)相关用例。
//
// 用真门店号("01" 佳乐 / "00" 富百家)而不是 "S001":
// 排除域是**按门店**配置的,门店号必须与配置行里的 branch_id 一致才生效。

// setupScopeService 起一个带排除域配置的测试 service。
//
// 与 setupTestService 的区别:这里要往库里写**配置行**,所以把 db 一起返回。
func setupScopeService(t *testing.T) (*service.Service, *cubeclientfake.Client, *gorm.DB) {
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
		&model.StocktakeExcludedScope{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cube := cubeclientfake.New()
	svc := service.New(db, cube)
	fixed := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return fixed })
	cube.SetClock(func() time.Time { return fixed })
	// 两个生产门店的库存(fake 的 seed 只有 S001/S002)。
	cube.UpsertStock("01", "P-2001", decimal.NewFromFloat(15.5), decimal.NewFromFloat(38.0))
	cube.UpsertStock("01", "P-1001", decimal.NewFromInt(100), decimal.NewFromFloat(2.5))
	cube.UpsertStock("00", "P-2001", decimal.NewFromFloat(12.0), decimal.NewFromFloat(38.0))
	cube.UpsertStock("00", "P-1001", decimal.NewFromInt(80), decimal.NewFromFloat(2.5))
	return svc, cube, db
}

// excludeScope 写一条排除域配置。
func excludeScope(t *testing.T, db *gorm.DB, branchID, scopeValue, note string) {
	t.Helper()
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	rec := model.StocktakeExcludedScope{
		ID:         "SES-" + branchID + "-" + scopeValue,
		BranchID:   branchID,
		ScopeKind:  model.ScopeKindCategory,
		ScopeValue: scopeValue,
		Note:       note,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := db.Create(&rec).Error; err != nil {
		t.Fatalf("写排除域配置: %v", err)
	}
}

// newHeader 在指定门店建一张盘点单。
func newHeader(t *testing.T, svc *service.Service, branchID string) *model.StocktakeHeader {
	t.Helper()
	cur := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return cur })
	h, err := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: branchID, OperatorID: "u-1", Type: model.TypeGeneral,
	})
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}
	cur = cur.Add(time.Minute)
	return h
}

// ---- AddLine ----

// TestService_AddLine_ExcludedCategoryRejected —— 猪肉分类里的商品不许录。
//
// 这是本条改动的核心断言:同一批猪肉被通用盘点与鲜肉盘点各盘一次 ⇒
// 两个数、没人说得清哪个对,而失败是静默的。
func TestService_AddLine_ExcludedCategoryRejected(t *testing.T) {
	svc, _, db := setupScopeService(t)
	// P-2001(五花肉)在 fake 里的分类是 CAT-02。
	excludeScope(t, db, "01", "CAT-02", "猪肉请用鲜肉盘点(生鲜肉盘点)")

	h := newHeader(t, svc, "01")
	_, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID: "P-2001",
		ActualQty: decimal.NewFromFloat(15.0),
	})
	if !errors.Is(err, service.ErrProductScopeExcluded) {
		t.Fatalf("猪肉商品应被拒绝, got %v", err)
	}
	// 消息里必须带"该去哪个流程盘"的可操作提示。
	if got := err.Error(); !strings.Contains(got, "鲜肉盘点") {
		t.Errorf("错误消息应点名正确流程, got %q", got)
	}
	// 拒绝 ⇒ 一行都没落库。
	var n int64
	if err := db.Model(&model.StocktakeLine{}).Count(&n).Error; err != nil {
		t.Fatalf("count lines: %v", err)
	}
	if n != 0 {
		t.Errorf("被拒绝的商品不应落库, lines=%d", n)
	}
}

// TestService_AddLine_NonExcludedCategoryAccepted —— 非猪肉照常录。
func TestService_AddLine_NonExcludedCategoryAccepted(t *testing.T) {
	svc, cube, db := setupScopeService(t)
	excludeScope(t, db, "01", "CAT-02", "猪肉请用鲜肉盘点(生鲜肉盘点)")
	cube.UpsertStock("01", "P-3001", decimal.NewFromInt(50), decimal.NewFromFloat(3.5))

	h := newHeader(t, svc, "01")
	// P-1001(可口可乐)在 CAT-01。
	line, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID: "P-1001",
		ActualQty: decimal.NewFromInt(95),
	})
	if err != nil {
		t.Fatalf("非排除域商品应放行, got %v", err)
	}
	if line == nil || line.ProductID != "P-1001" {
		t.Fatalf("line 不对: %+v", line)
	}
	// P-3001(大白菜)在 CAT-03「生鲜蔬果」⇒ 蔬果盘点必须照常能用。
	if _, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID: "P-3001",
		ActualQty: decimal.NewFromInt(48),
	}); err != nil {
		t.Fatalf("蔬果(其它分类)应放行, got %v", err)
	}
}

// TestService_AddLine_ExcludedScopeIsPerBranch —— 01 店的配置不牵连 00 店。
//
// 分类 id 是**门店各自编码**的(佳乐 1201 / 富百家 010303),
// 同一个 id 在另一个门店可能压根不是猪肉 ⇒ 必须按门店隔离。
func TestService_AddLine_ExcludedScopeIsPerBranch(t *testing.T) {
	svc, _, db := setupScopeService(t)
	excludeScope(t, db, "01", "CAT-02", "猪肉请用鲜肉盘点(生鲜肉盘点)")

	// 00 店没有配置 ⇒ CAT-02 的 P-2001 在 00 店照常能录。
	h := newHeader(t, svc, "00")
	if _, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID: "P-2001",
		ActualQty: decimal.NewFromFloat(12.0),
	}); err != nil {
		t.Fatalf("另一门店不该被牵连, got %v", err)
	}
}

// TestService_AddLine_UnknownCategoryFailsOpen —— 分类取不到 ⇒ 放行。
//
// fail open:cube 不可达不该让整个通用盘点不能用。
func TestService_AddLine_UnknownCategoryFailsOpen(t *testing.T) {
	svc, cube, db := setupScopeService(t)
	excludeScope(t, db, "01", "CAT-02", "猪肉请用鲜肉盘点(生鲜肉盘点)")
	// 把商品的分类抹掉 ⇒ 分类未知。
	cube.UpsertProductCategory("P-2001", "")

	h := newHeader(t, svc, "01")
	if _, err := svc.AddLine(context.Background(), h.ID, service.AddLineInput{
		ProductID: "P-2001",
		ActualQty: decimal.NewFromFloat(15.0),
	}); err != nil {
		t.Fatalf("分类未知应 fail open 放行, got %v", err)
	}
}

// ---- AddPlanItems ----

// TestService_AddPlanItems_ExcludedCategoryRejected_WholeBatch ——
// 计划清单里圈了猪肉 ⇒ **整批拒绝**,一行都不写。
//
// 整批拒绝(而不是"跳过那一条")的理由见 service.AddPlanItems 的注释:
// 与既有语义(重复商品整批回滚)一致,而且"整批失败"比"半批入库"更好查。
func TestService_AddPlanItems_ExcludedCategoryRejected_WholeBatch(t *testing.T) {
	svc, _, db := setupScopeService(t)
	excludeScope(t, db, "01", "CAT-02", "猪肉请用鲜肉盘点(生鲜肉盘点)")

	h := newHeader(t, svc, "01")
	_, err := svc.AddPlanItems(context.Background(), h.ID, service.AddPlanItemsInput{
		Items: []service.PlanItemInput{
			{ProductID: "P-1001", SortOrder: 1},
			{ProductID: "P-2001", SortOrder: 2}, // 猪肉
		},
	})
	if !errors.Is(err, service.ErrProductScopeExcluded) {
		t.Fatalf("计划清单含猪肉应被拒绝, got %v", err)
	}
	items, gerr := svc.GetPlanItems(context.Background(), h.ID)
	if gerr != nil {
		t.Fatalf("GetPlanItems: %v", gerr)
	}
	if len(items) != 0 {
		t.Errorf("整批拒绝 ⇒ 一行都不该写, got %d 行", len(items))
	}
}

// TestService_AddPlanItems_NonExcludedAccepted —— 非猪肉照常进计划清单。
func TestService_AddPlanItems_NonExcludedAccepted(t *testing.T) {
	svc, _, db := setupScopeService(t)
	excludeScope(t, db, "01", "CAT-02", "猪肉请用鲜肉盘点(生鲜肉盘点)")

	h := newHeader(t, svc, "01")
	items, err := svc.AddPlanItems(context.Background(), h.ID, service.AddPlanItemsInput{
		Items: []service.PlanItemInput{{ProductID: "P-1001", SortOrder: 1}},
	})
	if err != nil {
		t.Fatalf("非排除域商品应放行, got %v", err)
	}
	if len(items) != 1 || items[0].ProductID != "P-1001" {
		t.Fatalf("items 不对: %+v", items)
	}
}

// TestService_AddPlanItems_ExcludedScopeIsPerBranch —— 按门店隔离。
func TestService_AddPlanItems_ExcludedScopeIsPerBranch(t *testing.T) {
	svc, _, db := setupScopeService(t)
	excludeScope(t, db, "01", "CAT-02", "猪肉请用鲜肉盘点(生鲜肉盘点)")

	h := newHeader(t, svc, "00")
	items, err := svc.AddPlanItems(context.Background(), h.ID, service.AddPlanItemsInput{
		Items: []service.PlanItemInput{{ProductID: "P-2001", SortOrder: 1}},
	})
	if err != nil {
		t.Fatalf("另一门店不该被牵连, got %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items 应有 1 条, got %d", len(items))
	}
}

// failingProductCube 让 GetProduct 一律失败(模拟 cube 不可达 / 商品查不到)。
type failingProductCube struct {
	*cubeclientfake.Client
	err error
}

func (c *failingProductCube) GetProduct(_ context.Context, _ string) (*cubeclient.ProductDTO, error) {
	return nil, c.err
}

// TestService_AddPlanItems_CubeLookupFailureFailsOpen —— cube 问不到商品 ⇒ 放行。
//
// 客户端自己带了 name/barcode(不依赖 cube)时,cube 挂掉不该让整批计划单建不出来。
func TestService_AddPlanItems_CubeLookupFailureFailsOpen(t *testing.T) {
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
		&model.StocktakeExcludedScope{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	base := cubeclientfake.New()
	cube := &failingProductCube{Client: base, err: errors.New("cube unreachable")}
	svc := service.New(db, cube)
	fixed := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return fixed })

	// 排除域配了 CAT-02 —— 但 cube 挂了,分类根本查不到 ⇒ 不该拦。
	now := fixed
	if err := db.Create(&model.StocktakeExcludedScope{
		ID: "SES-01-CAT-02", BranchID: "01",
		ScopeKind: model.ScopeKindCategory, ScopeValue: "CAT-02",
		Note:      "猪肉请用鲜肉盘点(生鲜肉盘点)",
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("写排除域配置: %v", err)
	}

	h, err := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "01", OperatorID: "u-1", Type: model.TypePlan,
	})
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}

	items, err := svc.AddPlanItems(context.Background(), h.ID, service.AddPlanItemsInput{
		Items: []service.PlanItemInput{{
			ProductID: "P-2001", ProductName: "五花肉", Barcode: "2001", SortOrder: 1,
		}},
	})
	if err != nil {
		t.Fatalf("cube 问不到商品应 fail open 放行, got %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items 应有 1 条, got %d", len(items))
	}
}
