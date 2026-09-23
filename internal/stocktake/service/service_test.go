package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/service"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
	"github.com/shopspring/decimal"
)

// setupTestService 用 SQLite in-memory + InMemoryCubeClient 起一个测试 service。
//
// 返回 service + cube(mock,可手动调 UpsertStock 模拟"录入实盘后 cube 库存变化")。
//
// DB 走 internal/stocktake/testdb(测试专用,生产代码不引用)。
// 生产代码只支持 PostgreSQL(见 stocktake.OpenPostgres)。
func setupTestService(t *testing.T) (*service.Service, *cubeclient.InMemoryClient) {
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
	cube := cubeclient.NewInMemoryClient()
	svc := service.New(db, cube)
	// 注入固定时间,便于断言
	fixed := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return fixed })
	cube.SetClock(func() time.Time { return fixed })
	return svc, cube
}

func ptrDec(d decimal.Decimal) *decimal.Decimal { return &d }

// ---- Header ----

func TestService_CreateHeader_OK(t *testing.T) {
	svc, _ := setupTestService(t)
	h, err := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID:   "S001",
		OperatorID: "u-1",
		Type:       model.TypeGeneral,
		Remark:     "营业中盘点 demo",
	})
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}
	if h.Status != model.StatusCounting {
		t.Errorf("status 应为 counting, got %q", h.Status)
	}
	if h.BranchID != "S001" {
		t.Errorf("branch_id = %q, want S001", h.BranchID)
	}
	if h.ID == "" || len(h.ID) < 5 {
		t.Errorf("id 应非空 + ST 前缀, got %q", h.ID)
	}
}

func TestService_CreateHeader_EmptyBranch(t *testing.T) {
	svc, _ := setupTestService(t)
	_, err := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		OperatorID: "u-1",
	})
	if err == nil {
		t.Errorf("branch_id 空应报错")
	}
}

func TestService_CreateHeader_EmptyOperator(t *testing.T) {
	svc, _ := setupTestService(t)
	_, err := svc.CreateHeader(context.Background(), service.CreateHeaderInput{
		BranchID: "S001",
	})
	if err == nil {
		t.Errorf("operator_id 空应报错")
	}
}

// ---- AddLine ----

func TestService_AddLine_CalculatesDiff(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	h, err := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}

	// cube S001.P-1001 = 100 瓶,cost 2.5
	// 实盘 95 → diff_qty = -5,diff_amount = -12.5
	line, err := svc.AddLine(ctx, h.ID, service.AddLineInput{
		ProductID:  "P-1001",
		ActualQty:  decimal.NewFromInt(95),
		DiffReason: model.ReasonLoss,
	})
	if err != nil {
		t.Fatalf("AddLine: %v", err)
	}
	if line == nil {
		t.Fatalf("AddLine returned nil line")
	}

	if !line.BookQty.Equal(decimal.NewFromInt(100)) {
		t.Errorf("book_qty = %s, want 100", line.BookQty)
	}
	if !line.DiffQty.Equal(decimal.NewFromInt(-5)) {
		t.Errorf("diff_qty = %s, want -5", line.DiffQty)
	}
	if !line.DiffAmountYuan.Equal(decimal.NewFromFloat(-12.5)) {
		t.Errorf("diff_amount_yuan = %s, want -12.5", line.DiffAmountYuan)
	}
	if !line.AvgCostYuan.Equal(decimal.NewFromFloat(2.5)) {
		t.Errorf("avg_cost_yuan = %s, want 2.5", line.AvgCostYuan)
	}
	if line.DiffReason != model.ReasonLoss {
		t.Errorf("diff_reason = %q, want loss", line.DiffReason)
	}
	if line.ProductName != "可口可乐 330ml" {
		t.Errorf("product_name 应冗余从 cube 拉, got %q", line.ProductName)
	}
}

func TestService_AddLine_CrossBranchBlocked(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	// 创建 S001 的盘点单
	h, err := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}

	// 但试图录 P-1003(S001 有 200),改为录 P-9999(cube 不存在)
	_, err = svc.AddLine(ctx, h.ID, service.AddLineInput{
		ProductID: "P-9999",
		ActualQty: decimal.NewFromInt(10),
	})
	if err == nil {
		t.Errorf("不存在的商品应报错")
	}
	if !errors.Is(err, service.ErrProductNotFound) {
		t.Errorf("err 应为 ErrProductNotFound, got %v", err)
	}
}

func TestService_AddLine_BranchNotInCubeStock(t *testing.T) {
	// 关键:商品存在,但该分店下不存在 cube stock → 跨店阻断
	svc, _ := setupTestService(t)
	ctx := context.Background()

	// 创建 S002 的盘点单
	h, err := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S002", OperatorID: "u-1",
	})
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}

	// P-1003 在 S002 不存在(S002 mock 没种)
	_, err = svc.AddLine(ctx, h.ID, service.AddLineInput{
		ProductID: "P-1003",
		ActualQty: decimal.NewFromInt(50),
	})
	if err == nil {
		t.Errorf("跨店盘点应报错")
	}
	if !errors.Is(err, service.ErrStockNotFound) {
		t.Errorf("err 应为 ErrStockNotFound, got %v", err)
	}
}

// ---- UpdateLine ----

func TestService_UpdateLine_RecomputeDiff(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	h, _ := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	// 第一次录 actual=95 → diff=-5
	line, _ := svc.AddLine(ctx, h.ID, service.AddLineInput{
		ProductID: "P-1001", ActualQty: decimal.NewFromInt(95),
	})
	// 改 actual=97 → diff 变 -3,diff_amount 变 -7.5
	upd, err := svc.UpdateLine(ctx, line.ID, service.UpdateLineInput{
		ActualQty: ptrDec(decimal.NewFromInt(97)),
	})
	if err != nil {
		t.Fatalf("UpdateLine: %v", err)
	}
	if !upd.DiffQty.Equal(decimal.NewFromInt(-3)) {
		t.Errorf("diff_qty = %s, want -3", upd.DiffQty)
	}
	if !upd.DiffAmountYuan.Equal(decimal.NewFromFloat(-7.5)) {
		t.Errorf("diff_amount_yuan = %s, want -7.5", upd.DiffAmountYuan)
	}
	// book_qty 不变
	if !upd.BookQty.Equal(decimal.NewFromInt(100)) {
		t.Errorf("book_qty 应不变 = 100, got %s", upd.BookQty)
	}
}

func TestService_UpdateLine_NotInCounting(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	h, _ := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	line, _ := svc.AddLine(ctx, h.ID, service.AddLineInput{
		ProductID: "P-1001", ActualQty: decimal.NewFromInt(95),
	})
	// submit 后状态变 adjusted
	_, err := svc.Submit(ctx, h.ID)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// 此时 UpdateLine 应报错
	_, err = svc.UpdateLine(ctx, line.ID, service.UpdateLineInput{
		ActualQty: ptrDec(decimal.NewFromInt(90)),
	})
	if !errors.Is(err, service.ErrInvalidStatus) {
		t.Errorf("adjusted 状态改 actual 应报错, got %v", err)
	}
}

func TestService_DeleteLine_CountingOnly(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	h, _ := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	line, _ := svc.AddLine(ctx, h.ID, service.AddLineInput{
		ProductID: "P-1001", ActualQty: decimal.NewFromInt(95),
	})

	// counting 可删
	if err := svc.DeleteLine(ctx, line.ID, "u-1", "u-1", model.MethodManual); err != nil {
		t.Fatalf("DeleteLine in counting: %v", err)
	}
	// 已删,Get 应失败
	_, err := svc.GetHeaderWithLines(ctx, h.ID)
	if err != nil {
		t.Fatalf("GetHeaderWithLines: %v", err)
	}
	// 重新添加 → submit
	line2, _ := svc.AddLine(ctx, h.ID, service.AddLineInput{
		ProductID: "P-1001", ActualQty: decimal.NewFromInt(95),
	})
	_, _ = svc.Submit(ctx, h.ID)
	if err := svc.DeleteLine(ctx, line2.ID, "u-1", "u-1", model.MethodManual); !errors.Is(err, service.ErrInvalidStatus) {
		t.Errorf("adjusted 状态应禁止删, got %v", err)
	}
}

// ---- Submit / Approve ----

func TestService_Submit_FreezeTotals(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	h, _ := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	// 录 2 行:loss 5 瓶 × 2.5 + overage 2 瓶 × 2.5
	_, _ = svc.AddLine(ctx, h.ID, service.AddLineInput{
		ProductID: "P-1001", ActualQty: decimal.NewFromInt(95),
		DiffReason: model.ReasonLoss,
	})
	_, _ = svc.AddLine(ctx, h.ID, service.AddLineInput{
		ProductID: "P-1002", ActualQty: decimal.NewFromInt(52),
		DiffReason: model.ReasonOverage,
	})

	// submit 前 total_diff_* 应为 0
	h, _ = svc.GetHeader(ctx, h.ID)
	if !h.TotalDiffQty.Equal(decimal.Zero) {
		t.Errorf("submit 前 total_diff_qty 应 0, got %s", h.TotalDiffQty)
	}

	upd, err := svc.Submit(ctx, h.ID)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if upd.Status != model.StatusAdjusted {
		t.Errorf("status 应 adjusted, got %q", upd.Status)
	}
	// -5 + 2 = -3
	if !upd.TotalDiffQty.Equal(decimal.NewFromInt(-3)) {
		t.Errorf("total_diff_qty = %s, want -3", upd.TotalDiffQty)
	}
	// -12.5 + 5 = -7.5
	if !upd.TotalDiffAmountYuan.Equal(decimal.NewFromFloat(-7.5)) {
		t.Errorf("total_diff_amount_yuan = %s, want -7.5", upd.TotalDiffAmountYuan)
	}
}

func TestService_Submit_OnlyFromCounting(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	h, _ := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	_, _ = svc.Submit(ctx, h.ID) // counting → adjusted
	_, err := svc.Submit(ctx, h.ID)
	if !errors.Is(err, service.ErrInvalidTransition) {
		t.Errorf("adjusted 再 submit 应报错, got %v", err)
	}
}

func TestService_Approve_OnlyFromAdjusted(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	h, _ := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	_, err := svc.Approve(ctx, h.ID, "u-admin")
	if !errors.Is(err, service.ErrInvalidTransition) {
		t.Errorf("counting 不能直接 approve, got %v", err)
	}
	_, _ = svc.Submit(ctx, h.ID)
	upd, err := svc.Approve(ctx, h.ID, "u-admin")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if upd.Status != model.StatusApproved {
		t.Errorf("status 应 approved, got %q", upd.Status)
	}
	if upd.AuditorID != "u-admin" {
		t.Errorf("auditor_id = %q, want u-admin", upd.AuditorID)
	}
}

// ---- DiffReport ----

func TestService_ComputeDiffReport_AggregateAndFilter(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	h, _ := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	// 行 1: loss -5 瓶
	_, _ = svc.AddLine(ctx, h.ID, service.AddLineInput{
		ProductID: "P-1001", ActualQty: decimal.NewFromInt(95),
		DiffReason: model.ReasonLoss,
	})
	// 行 2: overage +2 瓶
	_, _ = svc.AddLine(ctx, h.ID, service.AddLineInput{
		ProductID: "P-1002", ActualQty: decimal.NewFromInt(52),
		DiffReason: model.ReasonOverage,
	})
	// 行 3: actual=100(=book) → diff=0,不出现在 lines
	_, _ = svc.AddLine(ctx, h.ID, service.AddLineInput{
		ProductID: "P-1003", ActualQty: decimal.NewFromInt(200),
	})

	rep, err := svc.ComputeDiffReport(ctx, h.ID)
	if err != nil {
		t.Fatalf("ComputeDiffReport: %v", err)
	}
	if rep.Status != model.StatusCounting {
		t.Errorf("status 应 counting, got %q", rep.Status)
	}
	if rep.Summary.TotalLines != 3 {
		t.Errorf("total_lines = %d, want 3", rep.Summary.TotalLines)
	}
	if rep.Summary.LossLines != 1 {
		t.Errorf("loss_lines = %d, want 1", rep.Summary.LossLines)
	}
	if rep.Summary.OverageLines != 1 {
		t.Errorf("overage_lines = %d, want 1", rep.Summary.OverageLines)
	}
	if rep.Summary.NoDiffLines != 1 {
		t.Errorf("no_diff_lines = %d, want 1", rep.Summary.NoDiffLines)
	}
	if !rep.Summary.TotalDiffQty.Equal(decimal.NewFromInt(-3)) {
		t.Errorf("total_diff_qty = %s, want -3", rep.Summary.TotalDiffQty)
	}
	// lines 只列有差异的 2 行
	if len(rep.Lines) != 2 {
		t.Errorf("lines 应 2 条, got %d", len(rep.Lines))
	}
	// by_reason 应 2 个 reason
	if len(rep.ByReason) != 2 {
		t.Errorf("by_reason 应 2 个, got %d", len(rep.ByReason))
	}
}

// ---- book_qty 独立快照(每行) ----

func TestService_AddLine_IndependentSnapshot(t *testing.T) {
	// 关键:同一商品在不同时间录入,book_qty 是录入当时的 cube 库存快照
	svc, cube := setupTestService(t)
	ctx := context.Background()

	// 启动时 cube 已注入固定时钟;但 service 录入时 clock 推进
	t0 := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	currentTime := t0
	svc.SetClock(func() time.Time { return currentTime })
	cube.SetClock(func() time.Time { return currentTime })

	h, _ := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})

	// t0: 实盘 95,cube.P-1001=100 → diff=-5
	currentTime = t0.Add(1 * time.Minute)
	line1, _ := svc.AddLine(ctx, h.ID, service.AddLineInput{
		ProductID: "P-1001", ActualQty: decimal.NewFromInt(95),
	})

	// 模拟"营业中销售":cube.P-1001 卖了几瓶,变成 97
	cube.UpsertStock("S001", "P-1001", decimal.NewFromInt(97), decimal.NewFromFloat(2.5))
	currentTime = t0.Add(2 * time.Minute)
	// 再录一行,book 应该是 97(新快照),而不是 100
	line2, _ := svc.AddLine(ctx, h.ID, service.AddLineInput{
		ProductID: "P-1001", ActualQty: decimal.NewFromInt(95),
	})

	// line1.book_qty = 100
	if !line1.BookQty.Equal(decimal.NewFromInt(100)) {
		t.Errorf("line1.book_qty = %s, want 100", line1.BookQty)
	}
	// line2.book_qty = 97(新快照)
	if !line2.BookQty.Equal(decimal.NewFromInt(97)) {
		t.Errorf("line2.book_qty = %s, want 97(独立快照)", line2.BookQty)
	}
	if !line1.BookQtyAt.Before(line2.BookQtyAt) {
		t.Errorf("line1.book_qty_at 应早于 line2")
	}
	// line2 diff = 95-97 = -2
	if !line2.DiffQty.Equal(decimal.NewFromInt(-2)) {
		t.Errorf("line2.diff_qty = %s, want -2", line2.DiffQty)
	}
}

// ---- SearchProducts ----

func TestService_SearchProducts_Exact13(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	out, err := svc.SearchProducts(ctx, service.SearchProductsInput{
		Barcode:  "6901234567890", // = P-1001
		BranchID: "S001",
		Limit:    10,
	}, true, true)
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	if out.Count != 1 || len(out.Products) != 1 {
		t.Fatalf("count = %d, want 1", out.Count)
	}
	row := out.Products[0]
	if row.ProductID != "P-1001" {
		t.Errorf("product_id = %q", row.ProductID)
	}
	if row.StockQty == nil {
		t.Errorf("stock_qty 应有值, got nil")
	} else if !row.StockQty.Equal(decimal.NewFromInt(100)) {
		t.Errorf("stock_qty = %s, want 100", row.StockQty)
	}
	if row.SupplierID != "SUP-001" || row.SupplierName != "可口可乐华南" {
		t.Errorf("supplier = %s/%s", row.SupplierID, row.SupplierName)
	}
	if !out.Meta.InvViewable || !out.Meta.SupplierViewable {
		t.Errorf("meta 权限标志应 true")
	}
}

func TestService_SearchProducts_Suffix5to12(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	out, err := svc.SearchProducts(ctx, service.SearchProductsInput{
		Barcode:  "67890", // 后缀匹配 P-1001
		BranchID: "S001",
		Limit:    10,
	}, true, true)
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	if out.Count == 0 {
		t.Fatalf("后缀匹配应至少 1 条")
	}
	// 确认有 P-1001
	found := false
	for _, r := range out.Products {
		if r.ProductID == "P-1001" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("应命中 P-1001, got %d 条", out.Count)
	}
}

func TestService_SearchProducts_NoPermissionFiltersStock(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	// 无 inventory:view → stock_qty 不返
	out, err := svc.SearchProducts(ctx, service.SearchProductsInput{
		Barcode:  "6901234567890",
		BranchID: "S001",
	}, false, true)
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	row := out.Products[0]
	if row.StockQty != nil {
		t.Errorf("无 inv perm 时 stock_qty 应 nil, got %v", row.StockQty)
	}
	if row.SupplierID == "" {
		t.Errorf("有 supplier perm 时应返 supplier_id")
	}
	if out.Meta.InvViewable {
		t.Errorf("meta.inv_viewable 应 false")
	}
}

func TestService_SearchProducts_NoPermissionFiltersSupplier(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	// 无 supplier:view → supplier_* 不返
	out, err := svc.SearchProducts(ctx, service.SearchProductsInput{
		Barcode:  "6901234567890",
		BranchID: "S001",
	}, true, false)
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	row := out.Products[0]
	if row.SupplierID != "" || row.SupplierName != "" {
		t.Errorf("无 supplier perm 时 supplier_* 应空, got %s/%s", row.SupplierID, row.SupplierName)
	}
	if row.StockQty == nil {
		t.Errorf("有 inv perm 时 stock_qty 应有值, got nil")
	}
}

func TestService_SearchProducts_TooShortReturnsEmpty(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	out, err := svc.SearchProducts(ctx, service.SearchProductsInput{
		Barcode:  "123", // <5
		BranchID: "S001",
	}, true, true)
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	if out.Count != 0 {
		t.Errorf("<5 位应返 0 条, got %d", out.Count)
	}
}

func TestService_SearchProducts_EmptyBranch(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	_, err := svc.SearchProducts(ctx, service.SearchProductsInput{
		Barcode: "6901234567890",
		// BranchID 缺
	}, true, true)
	if err == nil {
		t.Errorf("缺 branch_id 应报错")
	}
}

// ---- Recheck auto-remark (复盘自动备注) ----

func TestService_RecheckAutoRemark(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	// generateHeaderID 用 UnixNano 后 6 位,固定时钟下多次创建会冲突;
	// 这里用单调推进的 clock 让每次 CreateHeader 拿到不同的 seq。
	t0 := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	cur := t0
	svc.SetClock(func() time.Time { return cur })

	// 先建一个原始盘点单
	parent, err := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
		Type: model.TypeGeneral,
	})
	if err != nil {
		t.Fatalf("CreateHeader parent: %v", err)
	}
	cur = cur.Add(time.Microsecond)

	// 再建一个 recheck 复盘点单,指向 parent
	recheck, err := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
		Type: model.TypeRecheck, ParentHeaderID: parent.ID,
	})
	if err != nil {
		t.Fatalf("CreateHeader recheck: %v", err)
	}
	if recheck.ParentHeaderID == nil || *recheck.ParentHeaderID != parent.ID {
		t.Fatalf("parent_header_id 应指向 parent.ID=%s, got %v", parent.ID, recheck.ParentHeaderID)
	}

	// 录一行,即使客户端传 Remark="用户填的",服务端也要强制成 "复盘"
	line, err := svc.AddLine(ctx, recheck.ID, service.AddLineInput{
		ProductID: "P-1001",
		ActualQty: decimal.NewFromInt(95),
		Remark:    "用户填的备注",
		ActorID:   "u-1", ActorName: "张三",
		Method:    model.MethodManual,
	})
	if err != nil {
		t.Fatalf("AddLine recheck: %v", err)
	}
	if line.Remark == nil || *line.Remark != "复盘" {
		t.Errorf("remark 应被服务端覆盖为 '复盘', got %v", line.Remark)
	}

	// UpdateLine 同理:即便 Remark 传 nil,服务端也要设成 "复盘"
	in := decimal.NewFromInt(97)
	upd, err := svc.UpdateLine(ctx, line.ID, service.UpdateLineInput{
		ActualQty: &in,
		OpType:    model.OpOverwrite,
		ActorID:   "u-2", ActorName: "李四",
	})
	if err != nil {
		t.Fatalf("UpdateLine recheck: %v", err)
	}
	if upd.Remark == nil || *upd.Remark != "复盘" {
		t.Errorf("update 后 remark 应保持 '复盘', got %v", upd.Remark)
	}
}

// ---- Plan Items: unique constraint + reject duplicates ----

func TestService_PlanItemDuplicatedRejected(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	// generateHeaderID 在固定 clock 下多次 CreateHeader 会主键冲突;
	// 这里用单调推进的 clock。
	t0 := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	cur := t0
	svc.SetClock(func() time.Time { return cur })

	h, err := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
		Type: model.TypePlan,
	})
	if err != nil {
		t.Fatalf("CreateHeader plan: %v", err)
	}

	// 加 P-1001 一次
	_, err = svc.AddPlanItems(ctx, h.ID, service.AddPlanItemsInput{
		Items: []service.PlanItemInput{{ProductID: "P-1001", SortOrder: 1}},
	})
	if err != nil {
		t.Fatalf("first add: %v", err)
	}

	// 同一 header_id 再加 P-1001 应整体回滚
	_, err = svc.AddPlanItems(ctx, h.ID, service.AddPlanItemsInput{
		Items: []service.PlanItemInput{
			{ProductID: "P-1002", SortOrder: 2},
			{ProductID: "P-1001", SortOrder: 3}, // duplicate
		},
	})
	if !errors.Is(err, service.ErrPlanItemDuplicated) {
		t.Fatalf("重复 product_id 应报 ErrPlanItemDuplicated, got %v", err)
	}

	// 验证 P-1002 也未被插入(整体回滚)
	items, _ := svc.GetPlanItems(ctx, h.ID)
	for _, it := range items {
		if it.ProductID == "P-1002" {
			t.Errorf("整体回滚失败,P-1002 也被写入了")
		}
	}
}

// ---- ListHeaders: branch + status + type 过滤 ----

func TestService_ListHeaders_Filter(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	// 单调推进 clock 让 generateHeaderID 不冲突
	t0 := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	cur := t0
	svc.SetClock(func() time.Time { return cur })

	// S001 × 2 (general + recheck),S002 × 1
	h1, _ := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1", Type: model.TypeGeneral,
	})
	cur = cur.Add(time.Microsecond)
	h2, _ := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1", Type: model.TypeRecheck, ParentHeaderID: h1.ID,
	})
	cur = cur.Add(time.Microsecond)
	_, _ = svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S002", OperatorID: "u-1", Type: model.TypeGeneral,
	})

	out, err := svc.ListHeaders(ctx, service.ListHeadersFilter{BranchID: "S001"}, 1, 50)
	if err != nil {
		t.Fatalf("ListHeaders: %v", err)
	}
	if out.Total != 2 || len(out.Headers) != 2 {
		t.Errorf("S001 应有 2 条, got total=%d headers=%d", out.Total, len(out.Headers))
	}

	out2, err := svc.ListHeaders(ctx, service.ListHeadersFilter{Type: model.TypeRecheck}, 1, 50)
	if err != nil {
		t.Fatalf("ListHeaders recheck: %v", err)
	}
	if out2.Total != 1 || out2.Headers[0].ID != h2.ID {
		t.Errorf("type=recheck 应只有 1 条 (h2), got total=%d firstID=%s", out2.Total, out2.Headers[0].ID)
	}

	// 不带任何过滤 → 3 条
	out3, _ := svc.ListHeaders(ctx, service.ListHeadersFilter{}, 1, 50)
	if out3.Total != 3 {
		t.Errorf("无过滤应 3 条, got %d", out3.Total)
	}

	// 分页
	out4, _ := svc.ListHeaders(ctx, service.ListHeadersFilter{}, 1, 2)
	if out4.Total != 3 || len(out4.Headers) != 2 || out4.PageSize != 2 {
		t.Errorf("page_size=2 应返 2 条 + total=3, got %+v", out4)
	}
}

// ---- ListLineOperations: Add/Update/Delete 各写一条历史 ----

func TestService_ListLineOperations_AfterWrites(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()

	// 单调推进 clock,保证每次写操作的 OpAt 不同(否则 ORDER BY 不稳定)
	t0 := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	cur := t0
	svc.SetClock(func() time.Time { return cur })

	h, _ := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", OperatorID: "u-1",
	})
	cur = cur.Add(time.Second)

	// create
	line, err := svc.AddLine(ctx, h.ID, service.AddLineInput{
		ProductID: "P-1001", ActualQty: decimal.NewFromInt(95),
		ActorID: "u-1", ActorName: "张三", Method: model.MethodScan,
	})
	if err != nil {
		t.Fatalf("AddLine: %v", err)
	}
	cur = cur.Add(time.Second)

	// overwrite
	q := decimal.NewFromInt(97)
	_, err = svc.UpdateLine(ctx, line.ID, service.UpdateLineInput{
		ActualQty: &q, OpType: model.OpOverwrite,
		ActorID: "u-2", ActorName: "李四", Method: model.MethodManual,
	})
	if err != nil {
		t.Fatalf("UpdateLine: %v", err)
	}
	cur = cur.Add(time.Second)

	// accumulate
	d := decimal.NewFromInt(3)
	_, err = svc.UpdateLine(ctx, line.ID, service.UpdateLineInput{
		ActualQty: &d, OpType: model.OpAccumulate,
		ActorID: "u-3", ActorName: "王五", Method: model.MethodScan,
	})
	if err != nil {
		t.Fatalf("UpdateLine accumulate: %v", err)
	}
	cur = cur.Add(time.Second)

	// delete
	if err := svc.DeleteLine(ctx, line.ID, "u-1", "张三", model.MethodManual); err != nil {
		t.Fatalf("DeleteLine: %v", err)
	}

	ops, err := svc.ListLineOperations(ctx, h.ID, 50)
	if err != nil {
		t.Fatalf("ListLineOperations: %v", err)
	}
	if len(ops) != 4 {
		t.Fatalf("应 4 条历史 (create/overwrite/accumulate/delete), got %d", len(ops))
	}
	// 倒序:delete 在前
	if ops[0].OpType != model.OpDelete {
		t.Errorf("最新应为 delete, got %q", ops[0].OpType)
	}
	if ops[0].ActorName != "张三" {
		t.Errorf("delete 操作人 = %q, want 张三", ops[0].ActorName)
	}
	if !ops[0].QtyDelta.Equal(decimal.NewFromInt(-100)) {
		t.Errorf("delete delta 应为 -100, got %s", ops[0].QtyDelta)
	}

	// accumulate 应在 overwrite 之后(op_at DESC: [delete, accumulate, overwrite, create])
	if ops[1].OpType != model.OpAccumulate {
		t.Errorf("倒序第 2 应为 accumulate, got %q", ops[1].OpType)
	}
	if !ops[1].QtyDelta.Equal(decimal.NewFromInt(3)) {
		t.Errorf("accumulate delta 应为 +3, got %s", ops[1].QtyDelta)
	}
	if !ops[1].NewQty.Equal(decimal.NewFromInt(100)) {
		t.Errorf("accumulate new_qty 应为 100 (97+3), got %s", ops[1].NewQty)
	}

	// overwrite
	if ops[2].OpType != model.OpOverwrite {
		t.Errorf("倒序第 3 应为 overwrite, got %q", ops[2].OpType)
	}
	if !ops[2].QtyDelta.Equal(decimal.NewFromInt(2)) {
		t.Errorf("overwrite delta 应为 +2 (95→97), got %s", ops[2].QtyDelta)
	}

	// create 在最后
	if ops[3].OpType != model.OpCreate {
		t.Errorf("最早应为 create, got %q", ops[3].OpType)
	}
	if !ops[3].PrevQty.Equal(decimal.Zero) {
		t.Errorf("create prev_qty 应为 0, got %s", ops[3].PrevQty)
	}
	if !ops[3].QtyDelta.Equal(decimal.NewFromInt(95)) {
		t.Errorf("create delta 应为 95, got %s", ops[3].QtyDelta)
	}
}