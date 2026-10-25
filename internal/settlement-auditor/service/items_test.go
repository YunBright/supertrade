// service/items_test.go —— 收付款明细的口径单测（2026-10-25 用户需求）。
//
// 锁住三件事：
//  1. 明细行的校验（类型/备注/金额/行数）；
//  2. 应付金额的口径（货款 + 付款项 − 收款项，且不为负）；
//  3. 放宽额度的归类（陈列费/堆头费/补差价）。
package service

import (
	"context"
	"encoding/csv"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// ---- 校验 ----

func TestValidateItems(t *testing.T) {
	cases := []struct {
		name    string
		items   []ItemInput
		wantErr string
	}{
		{name: "正常", items: []ItemInput{{Type: "陈列费", Remark: "2026-09", AmountYuan: dec("200")}}},
		{name: "类型为空", items: []ItemInput{{Type: "  ", AmountYuan: dec("200")}}, wantErr: "缺类型"},
		{name: "金额为零", items: []ItemInput{{Type: "陈列费", AmountYuan: dec("0")}}, wantErr: "必须大于 0"},
		{name: "金额为负", items: []ItemInput{{Type: "陈列费", AmountYuan: dec("-1")}}, wantErr: "必须大于 0"},
		{name: "备注超长", items: []ItemInput{{Type: "陈列费", Remark: strings.Repeat("x", maxItemRemarkLen+1), AmountYuan: dec("1")}}, wantErr: "备注超过"},
		{name: "类型超长", items: []ItemInput{{Type: strings.Repeat("型", maxItemTypeLen+1), AmountYuan: dec("1")}}, wantErr: "类型超过"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateItems(ItemDirectionReceipt, tc.items)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("不该报错: %v", err)
				}
				if len(got) != len(tc.items) {
					t.Fatalf("行数 = %d, want %d", len(got), len(tc.items))
				}
				return
			}
			if err == nil {
				t.Fatalf("该报错,却通过了")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息 = %q, want 含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// 行数上限:超了要当场拒(这些行会进提示词/企微单,不能无限长)。
func TestValidateItems_MaxRows(t *testing.T) {
	items := make([]ItemInput, 0, maxItemsPerDirection+1)
	for i := 0; i <= maxItemsPerDirection; i++ {
		items = append(items, ItemInput{Type: "陈列费", AmountYuan: dec("10")})
	}
	if _, err := validateItems(ItemDirectionPayment, items); err == nil {
		t.Fatal("超上限的行数应被拒")
	}
}

// 「其它」可以被改成自定义文字 ⇒ 类型文本原样保留。
func TestValidateItems_KeepsCustomTypeText(t *testing.T) {
	got, err := validateItems(ItemDirectionReceipt, []ItemInput{
		{Type: "临期处理费", Remark: "2026-09", AmountYuan: dec("66.6")},
	})
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if got[0].Type != "临期处理费" {
		t.Fatalf("类型文本 = %q, want 临期处理费(原样保留)", got[0].Type)
	}
}

// ---- 金额口径 ----

// 应付金额 = 货款 + 付款项 − 收款项。
func TestComputeTotals(t *testing.T) {
	tot := computeTotals(dec("1000"), dec("300"), dec("200"))
	if tot.GoodsTotalYuan.String() != "1000" || tot.PaymentTotalYuan.String() != "300" || tot.ReceiptTotalYuan.String() != "200" {
		t.Fatalf("三笔账不对: %+v", tot)
	}
	if tot.NetYuan.String() != "1100" {
		t.Fatalf("净额 = %s, want 1100", tot.NetYuan.String())
	}
	if tot.PayableYuan.String() != "1100" {
		t.Fatalf("应付 = %s, want 1100", tot.PayableYuan.String())
	}
}

// 纯收款项（用户口径:「付款审批可以只有'纸皮回收'」）⇒ 应付金额为 0，不为负。
func TestComputeTotals_PureReceiptClampsToZero(t *testing.T) {
	tot := computeTotals(dec("0"), dec("0"), dec("500"))
	if tot.NetYuan.String() != "-500" {
		t.Fatalf("净额 = %s, want -500", tot.NetYuan.String())
	}
	if tot.PayableYuan.String() != "0" {
		t.Fatalf("应付 = %s, want 0(不为负)", tot.PayableYuan.String())
	}
}

// ---- 放宽额度 ----

func TestBuildAllowance(t *testing.T) {
	items := []model.ApprovalItem{
		{Direction: ItemDirectionReceipt, ItemType: ReceiptTypeDisplay, AmountYuan: dec("300")},
		{Direction: ItemDirectionReceipt, ItemType: ReceiptTypePromotion, AmountYuan: dec("150")},
		{Direction: ItemDirectionReceipt, ItemType: ReceiptTypePriceDiff, AmountYuan: dec("80")},
		{Direction: ItemDirectionReceipt, ItemType: "临期处理费", AmountYuan: dec("20")},
		{Direction: ItemDirectionPayment, ItemType: PaymentTypeUtility, AmountYuan: dec("500")},
	}
	al := buildAllowance(items)
	if al.DisplayYuan.String() != "300" {
		t.Fatalf("陈列费额度 = %s, want 300", al.DisplayYuan.String())
	}
	if al.PromotionYuan.String() != "150" {
		t.Fatalf("堆头费额度 = %s, want 150", al.PromotionYuan.String())
	}
	if al.PriceDiffYuan.String() != "80" {
		t.Fatalf("补差价额度 = %s, want 80", al.PriceDiffYuan.String())
	}
	if !al.HasDisplayOrPromotion() {
		t.Fatal("有陈列费/堆头费 ⇒ 应有「进货可以多些」的额度")
	}
}

// 自定义文字（「其它」被改过）不进额度:宁可少放宽,不误放宽。
func TestBuildAllowance_CustomTextNotMatched(t *testing.T) {
	al := buildAllowance([]model.ApprovalItem{
		{Direction: ItemDirectionReceipt, ItemType: "陈列费(春节档)", AmountYuan: dec("300")},
	})
	if al.HasDisplayOrPromotion() {
		t.Fatal("非精确匹配的类型不应产生放宽额度")
	}
}

// ---- 落库/读库 ----

func TestStoreAndLoadItems(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	ap := &model.ApprovalInstance{ID: "ap-items", BranchID: "B1", ApplicantUserID: "u-1"}
	if err := db.Create(ap).Error; err != nil {
		t.Fatalf("create ap: %v", err)
	}
	if err := svc.storeItems(context.Background(), ap,
		[]ItemInput{{Type: "陈列费", Remark: "2026-09", AmountYuan: dec("200")}},
		[]ItemInput{{Type: "水电费", Remark: "2026-08", AmountYuan: dec("300")}},
	); err != nil {
		t.Fatalf("storeItems: %v", err)
	}
	items := svc.loadApprovalItems(context.Background(), "ap-items")
	if len(items) != 2 {
		t.Fatalf("明细行数 = %d, want 2", len(items))
	}
	// 类型/备注按**文本**存(不转成枚举/码值)。
	var receipt model.ApprovalItem
	for _, it := range items {
		if it.Direction == ItemDirectionReceipt {
			receipt = it
		}
	}
	if receipt.ItemType != "陈列费" || receipt.Remark != "2026-09" {
		t.Fatalf("文本没原样存: %+v", receipt)
	}
	if receipt.AmountYuan.String() != "200" {
		t.Fatalf("金额 = %s, want 200", receipt.AmountYuan.String())
	}
}

// 汇总：货款来自 cube 结算单，其余来自明细行。
func TestTotalsOf(t *testing.T) {
	tot := totalsOf(
		[]SettlementRow{{ID: "CP1", TotalGoodsYuan: dec("1000")}, {ID: "CP2", TotalGoodsYuan: dec("500")}},
		[]model.ApprovalItem{
			{Direction: ItemDirectionPayment, ItemType: PaymentTypeRent, AmountYuan: dec("300")},
			{Direction: ItemDirectionReceipt, ItemType: ReceiptTypeDisplay, AmountYuan: dec("200")},
		},
	)
	if tot.GoodsTotalYuan.String() != "1500" {
		t.Fatalf("货款合计 = %s, want 1500", tot.GoodsTotalYuan.String())
	}
	if tot.PayableYuan.String() != "1600" {
		t.Fatalf("应付 = %s, want 1600", tot.PayableYuan.String())
	}
}

// 摘要文案:收款是**减项**(写成加号会让人以为要付更多)。
func TestItemsSummaryText(t *testing.T) {
	txt := itemsSummaryText(computeTotals(dec("1000"), dec("300"), dec("200")))
	if !strings.Contains(txt, "货款 ¥1000") || !strings.Contains(txt, "+ 付款项 ¥300") ||
		!strings.Contains(txt, "- 收款项 ¥200") || !strings.Contains(txt, "应付 ¥1100") {
		t.Fatalf("摘要 = %q", txt)
	}
}

// ---- 机器审核:所有收付款项都要列给模型 ----

func TestBuildAuditUserPrompt_ListsAllItemsAndAllowance(t *testing.T) {
	in := &AuditInput{
		Approval: &model.ApprovalInstance{ID: "ap1", ManualAmountYuan: dec("1100"), Remark: "9月货款"},
		Amount: &AmountCheck{
			ManualAmountYuan: dec("1100"), SettledTotalYuan: dec("1000"),
			PaymentTotalYuan: dec("300"), ReceiptTotalYuan: dec("200"),
			PayableYuan: dec("1100"), DiffYuan: dec("0"), ToleranceYuan: dec("1"), Consistent: true,
		},
		Items: []ItemRow{
			{Direction: ItemDirectionPayment, Type: ItemTypeGoods, AmountYuan: dec("1000")},
			{Direction: ItemDirectionReceipt, Type: ReceiptTypeDisplay, Remark: "2026-09", AmountYuan: dec("200")},
			{Direction: ItemDirectionPayment, Type: PaymentTypeUtility, Remark: "2026-08", AmountYuan: dec("300")},
		},
		Totals:    computeTotals(dec("1000"), dec("300"), dec("200")),
		Allowance: Allowance{DisplayYuan: dec("200"), PriceDiffYuan: dec("50")},
	}
	p := buildAuditUserPrompt(in)
	for _, kw := range []string{
		"货款 1000", "付款项 300", "收款项 200", "应付 1100",
		ItemTypeGoods, ReceiptTypeDisplay, PaymentTypeUtility,
		"2026-09", // 备注也要列(补充信息是判断依据)
		"放宽额度", "陈列费 200", "补差价 50",
	} {
		if !strings.Contains(p, kw) {
			t.Fatalf("提示词里缺 %q:\n%s", kw, p)
		}
	}
}

// 系统提示词必须写死两条放宽口径,否则模型会照旧按"进货多 = 异常"拒单。
func TestBuildAuditSystemPrompt_ReceiptAllowanceRules(t *testing.T) {
	p := buildAuditSystemPrompt()
	for _, kw := range []string{"陈列费", "堆头费", "补差价", "进货可以多些", "毛利降低", "冲减"} {
		if !strings.Contains(p, kw) {
			t.Fatalf("系统提示词里缺 %q", kw)
		}
	}
}

// ---- 附件清单:收付款明细要进 CSV ----

func TestBuildSettlementDetailCSV_WithItems(t *testing.T) {
	rows := []SettlementRow{{ID: "CP1", SupplierID: "142", SupplierName: "北方鲜鸡蛋店",
		DocumentType: "CP", PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30", TotalGoodsYuan: dec("1000")}}
	items := []model.ApprovalItem{
		{Direction: ItemDirectionReceipt, ItemType: "陈列费", Remark: "2026-09", AmountYuan: dec("200")},
		{Direction: ItemDirectionPayment, ItemType: "水电费", Remark: "2026-08", AmountYuan: dec("300")},
	}
	out := buildSettlementDetailCSV(rows, items)
	recs, err := csv.NewReader(strings.NewReader(string(out[3:]))).ReadAll()
	if err != nil {
		t.Fatalf("CSV 解析失败: %v", err)
	}
	joined := strings.Join(strings.Split(strings.TrimSpace(string(out[3:])), "\n"), "|")
	for _, kw := range []string{"收付款明细", "陈列费", "水电费", "2026-09", "应付"} {
		if !strings.Contains(joined, kw) {
			t.Fatalf("清单里缺 %q:\n%s", kw, joined)
		}
	}
	// 合计行:货款 1000 + 付款项 300 - 收款项 200 = 应付 1100
	found := false
	for _, r := range recs {
		if len(r) >= 4 && r[0] == "合计" && r[3] == "1100.00" {
			found = true
		}
	}
	if !found {
		t.Fatalf("没找到应付 1100.00 的合计行: %v", recs)
	}
}
