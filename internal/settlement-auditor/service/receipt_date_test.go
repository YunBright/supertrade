// Package service / receipt_date_test.go —— 采购单备注「实际收货日期」提取。
//
// 用例数据全部取自生产实测（2026-10-31 排查被拒审批
// 21170868728a4a57a9d3e82d14f42611 时从 t_pm_sheet_master.other1 抄下来的备注原文）。
package service

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

func TestExtractReceiptDate_ProductionRemarks(t *testing.T) {
	cases := []struct {
		name      string
		remark    string
		createdAt string
		today     string
		want      string
		ok        bool
	}{
		// 被拒单里的两张关键单：9.18 / 10.9 收货，10.21 才录单。
		{"点号+号收货", "2026.9.18号收货", "2026-10-21", "2026-10-31", "2026-09-18", true},
		{"点号+号收货(两位数)", "2026.10.9号收货", "2026-10-21", "2026-10-31", "2026-10-09", true},
		{"全角括号收货", "2021.11.10（收货）", "2021-11-15", "2021-12-01", "2021-11-10", true},
		{"短日期+收货", "11.10收货", "2021-11-15", "2021-12-01", "2021-11-10", true},
		{"年月日+上午收货", "2021年11月12日上午收货", "2021-11-15", "2021-12-01", "2021-11-12", true},
		{"年月混合", "2021.11月11日（收货）", "2021-11-15", "2021-12-01", "2021-11-11", true},
		{"短日期+全角括号", "11.11（收货）", "2021-11-15", "2021-12-01", "2021-11-11", true},
		// 跨年：制单日 2027-01-03，备注写 12.28 ⇒ 应是上一年的 12-28。
		{"跨年短日期", "12.28收货", "2027-01-03", "2027-01-05", "2026-12-28", true},
		// 备注是自由文本：没有收货类关键词 ⇒ 不猜，回退制单日期。
		{"无收货关键词", "2026.10.27 结清上期", "2026-10-28", "2026-10-31", "", false},
		// 非法日期（2 月 30 日）⇒ 不采信。
		{"非法日期", "2026.2.30号收货", "2026-10-21", "2026-10-31", "", false},
		// 空备注。
		{"空备注", "", "2026-10-21", "2026-10-31", "", false},
		// 备注是年龄/电话之类，不应误判。
		{"无关数字", "电话13800138000收货时联系", "2026-10-21", "2026-10-31", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := extractReceiptDate(c.remark, c.createdAt, c.today)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (remark=%q)", ok, c.ok, c.remark)
			}
			if got != c.want {
				t.Fatalf("date = %q, want %q (remark=%q)", got, c.want, c.remark)
			}
		})
	}
}

func TestPurchaseSheetEffectiveDate_FallbackToCreatedAt(t *testing.T) {
	sh := PurchaseSheetRow{ID: "PI1", CreatedAt: "2026-10-21", Remark: "无日期说明"}
	if got := sh.effectiveDate("2026-10-31"); got != "2026-10-21" {
		t.Fatalf("effectiveDate = %q, want 制单日期 2026-10-21", got)
	}
	sh.Remark = "2026.9.18号收货"
	if got := sh.effectiveDate("2026-10-31"); got != "2026-09-18" {
		t.Fatalf("effectiveDate = %q, want 实际收货日期 2026-09-18", got)
	}
}

// TestRejectedApproval_ReceiptDateMovesWindow 复现被拒审批的关键场景。
//
// 生产事实（21170868728a4a57a9d3e82d14f42611）：
//   - 结算单 CP002610234505，供应商 116；
//   - 商品 牛腩-00126 挂在 PI002610214904 上，该单制单日 2026-10-21，
//     备注「2026.9.18号收货」；
//   - 商品 牛肉-00127 跨多张 PI 单（204..223），制单日同为 2026-10-21，
//     备注各不相同（9.18 ~ 10.9）。
//
// 旧口径按制单日 2026-10-21 起算 ⇒ 00126 窗口内无销售 ⇒ block。
// 新口径按实际收货日 2026-09-18 起算 ⇒ 窗口更长，销量算得进来。
func TestRejectedApproval_ReceiptDateMovesWindow(t *testing.T) {
	sheets := []PurchaseSheetRow{
		{ID: "PI002610214904", DocumentType: "PI", CreatedAt: "2026-10-21", Remark: "2026.9.18号收货"},
		{ID: "PI002610214905", DocumentType: "PI", CreatedAt: "2026-10-21", Remark: "2026.9.19号收货"},
		{ID: "PI002610214906", DocumentType: "PI", CreatedAt: "2026-10-21", Remark: "2026.9.20号收货"},
		{ID: "PI002610214923", DocumentType: "PI", CreatedAt: "2026-10-21", Remark: "2026.10.9号收货"},
	}
	productSheets := map[string][]string{
		"00126": {"PI002610214904"},
		"00127": {"PI002610214904", "PI002610214905", "PI002610214906", "PI002610214923"},
	}
	sheetDates := map[string]string{}
	for _, sh := range sheets {
		sheetDates[sh.ID] = sh.effectiveDate("2026-10-31")
	}

	groups := buildSaleWindowGroups(sheetDates, productSheets, "2026-10-31", "2026-09-30", 0)
	from := windowFromByProduct(groups)
	if got := from["00126"]; got != "2026-09-18" {
		t.Fatalf("00126 窗口起点 = %q, want 2026-09-18(实际收货日)", got)
	}
	// 00127 取该商品**最晚**收货日（窗口毛利口径）。
	if got := from["00127"]; got != "2026-10-09" {
		t.Fatalf("00127 窗口起点 = %q, want 2026-10-09", got)
	}
	// 实现毛利口径取最早收货日。
	earliest := windowFromByProduct(buildSaleWindowGroupsByDate(
		sheetDates, productSheets, "2026-10-31", "2026-09-30", 0, windowStartEarliest))
	if got := earliest["00127"]; got != "2026-09-18" {
		t.Fatalf("00127 实现毛利窗口起点 = %q, want 2026-09-18(最早收货日)", got)
	}

	// 新窗口下 00126 有销量 ⇒ 不再报"无销售记录"。
	sigs := checkQuantity(
		map[string]decimal.Decimal{"00126": decimal.NewFromInt(10)},
		map[string]SaleRow{"00126": {ProductID: "00126", NetSaleQuantity: decimal.NewFromFloat(6.2)}},
		nil, 1.0,
		QuantityContext{WindowFrom: from},
	)
	for _, s := range sigs {
		if s.ProductID == "00126" && s.SignalType == SigPurchaseWithoutSales {
			t.Fatalf("00126 不应再判「进货后无销售」: %s", s.Message)
		}
	}
	if len(sigs) == 0 {
		t.Fatalf("00126 进货 10 / 销量 6.2 应至少产生一条数量信号")
	}
	if sigs[0].Level != model.SignalWarn {
		t.Fatalf("级别 = %s, want warn(10/6.2≈1.6 倍)", sigs[0].Level)
	}
}

// TestEvaluateDailySingle_UsesActualReceiptDate 延后录入的单不能按制单日聚成"同日多单"。
func TestEvaluateDailySingle_UsesActualReceiptDate(t *testing.T) {
	sheets := []PurchaseSheetRow{
		{ID: "P1", CreatedAt: "2026-10-21", Remark: "2026.9.18号收货"},
		{ID: "P2", CreatedAt: "2026-10-21", Remark: "2026.9.19号收货"},
	}
	ok, reason := evaluateDailySingle(sheets)
	if !ok {
		t.Fatalf("按实际收货日分组应满足「每日一单」, got false: %s", reason)
	}
	// 真正同一天收货的两张单 ⇒ 仍要判违反。
	same := []PurchaseSheetRow{
		{ID: "P1", CreatedAt: "2026-10-21", Remark: "2026.9.18号收货"},
		{ID: "P2", CreatedAt: "2026-10-22", Remark: "2026.9.18号收货"},
	}
	if ok, _ := evaluateDailySingle(same); ok {
		t.Fatalf("同一天收货的两张单应判违反「每日一单」")
	}
}
