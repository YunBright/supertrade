// 本文件覆盖 2026-10-08 报的两个前端症状的后端根因:
//
//  1. 「商品当前售价 -」—— SearchProductRow.Price 恒为 nil。
//     根因:GetProduct 只查 product.count,没查价;service 里还写着
//     "cube 标准 product 无 price 字段(扩展,本期 mock)" —— 这句是错的,
//     price_yuan 一直在 cube 的 product 表里。
//
//  2. 「本盘点单已有记录:操作人 未知」—— StocktakeLine 上**根本没有**操作人字段,
//     而 wx-h5 读的是 line.operator_name / line.operator_id,永远 undefined。
//     数据一直在 stocktake_line_operations.actor_name 里。
package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/cubeclient/cubeclientfake"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/service"
	"github.com/shopspring/decimal"
)

var testCountDate = time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)

// ---- #1 售价 ----

func TestSearchProducts_CarriesPriceFromCube(t *testing.T) {
	svc := setupWithCube(t, cubeclientfake.New())

	out, err := svc.SearchProducts(
		context.Background(),
		service.SearchProductsInput{Barcode: "P-1001", BranchID: "S001", Limit: 10},
		true, false,
	)
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	if len(out.Products) == 0 {
		t.Fatalf("应有 1 条结果")
	}
	if out.Products[0].Price == nil {
		t.Fatalf("Price 恒为 nil 会让 wx-h5 显示「商品当前售价 -」,回归了")
	}
}

// cube 没给价(该商品未维护)→ Price 必须是 nil 而不是 0。
// 0 元是合法商品价,"没维护价"不是,合并成 Zero 会让店员以为商品免费。
func TestSearchProducts_MissingPriceStaysNilNotZero(t *testing.T) {
	fake := cubeclientfake.New()
	fake.UpsertProductPrice("P-1002", nil) // 模拟"该商品未维护售价"
	svc := setupWithCube(t, fake)

	out, err := svc.SearchProducts(
		context.Background(),
		service.SearchProductsInput{Barcode: "P-1002", BranchID: "S001", Limit: 10},
		true, false,
	)
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	if len(out.Products) == 0 {
		t.Fatalf("应有 1 条结果")
	}
	if got := out.Products[0].Price; got != nil {
		t.Fatalf("cube 未提供售价时 Price 应为 nil,实际 %v(会被误显示成 ¥ 0.00)", got)
	}
}

// ---- #2 操作人回填 ----

func TestGetHeaderWithLines_FillsOperatorFromLastOperation(t *testing.T) {
	svc := setupWithCube(t, cubeclientfake.New())
	ctx := context.Background()

	h, err := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", CountDate: testCountDate, Type: model.TypeGeneral, OperatorID: "op1",
	})
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}

	line, err := svc.AddLine(ctx, h.ID, service.AddLineInput{
		ProductID: "P-1001", ActualQty: decimal.NewFromInt(10),
		ActorID: "u-alice", ActorName: "张三", Method: model.MethodScan,
	})
	if err != nil {
		t.Fatalf("AddLine: %v", err)
	}
	// AddLine 的响应本身就该带操作人 —— 前端保存成功后立刻把这行塞回本地列表,
	// 不回填就会出现"刚保存的那条显示未知,刷新一次才显示名字"。
	if line.OperatorName != "张三" {
		t.Fatalf("AddLine 响应 operator_name=%q, want 张三", line.OperatorName)
	}
	if line.OperatorAt == "" {
		t.Fatalf("AddLine 响应 operator_at 应被回填")
	}

	h2, err := svc.GetHeaderWithLines(ctx, h.ID)
	if err != nil {
		t.Fatalf("GetHeaderWithLines: %v", err)
	}
	if len(h2.Lines) == 0 {
		t.Fatalf("应有 1 条明细")
	}
	if got := h2.Lines[0].OperatorName; got != "张三" {
		t.Fatalf("operator_name=%q, want 张三(wx-h5 会显示「操作人 未知」)", got)
	}
	if h2.Lines[0].OperatorAt == "" {
		t.Fatalf("operator_at 应被回填")
	}
}

// 多次修改后必须取**最后一次**操作人,不是第一次。
func TestGetHeaderWithLines_OperatorTracksLatestOp(t *testing.T) {
	svc := setupWithCube(t, cubeclientfake.New())
	ctx := context.Background()

	h, err := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", CountDate: testCountDate, Type: model.TypeGeneral, OperatorID: "op1",
	})
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}
	line, err := svc.AddLine(ctx, h.ID, service.AddLineInput{
		ProductID: "P-1002", ActualQty: decimal.NewFromInt(10),
		ActorID: "u-alice", ActorName: "张三", Method: model.MethodScan,
	})
	if err != nil {
		t.Fatalf("AddLine: %v", err)
	}

	qty := decimal.NewFromInt(25)
	updated, err := svc.UpdateLine(ctx, line.ID, service.UpdateLineInput{
		ActualQty: &qty, OpType: model.OpOverwrite,
		ActorID: "u-bob", ActorName: "李四", Method: model.MethodManual,
	})
	if err != nil {
		t.Fatalf("UpdateLine: %v", err)
	}
	if updated.OperatorName != "李四" {
		t.Fatalf("UpdateLine 响应 operator_name=%q, want 李四", updated.OperatorName)
	}

	h2, err := svc.GetHeaderWithLines(ctx, h.ID)
	if err != nil {
		t.Fatalf("GetHeaderWithLines: %v", err)
	}
	if got := h2.Lines[0].OperatorName; got != "李四" {
		t.Fatalf("回填应取最后一次操作人, got %q, want 李四", got)
	}
}

// 没有明细时不能崩。
func TestGetHeaderWithLines_EmptyHeader_DoesNotCrash(t *testing.T) {
	svc := setupWithCube(t, cubeclientfake.New())
	ctx := context.Background()

	h, err := svc.CreateHeader(ctx, service.CreateHeaderInput{
		BranchID: "S001", CountDate: testCountDate, Type: model.TypeGeneral, OperatorID: "op1",
	})
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}
	h2, err := svc.GetHeaderWithLines(ctx, h.ID)
	if err != nil {
		t.Fatalf("空 header 不该报错: %v", err)
	}
	if len(h2.Lines) != 0 {
		t.Fatalf("应无明细")
	}
}
