package service

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

func TestCheckSettlements_MissingAndNotApproved(t *testing.T) {
	found := []SettlementRow{
		{ID: "S1", ApproveStatus: "1"},
		{ID: "S2", ApproveStatus: "0"},
	}
	sigs := checkSettlements([]string{"S1", "S2", "S3"}, found)
	if len(sigs) != 2 {
		t.Fatalf("信号数 = %d, want 2(S2 未审核 + S3 缺失)", len(sigs))
	}
	byType := map[string]signalDraft{}
	for _, s := range sigs {
		byType[s.SignalType] = s
		if s.Level != model.SignalBlock {
			t.Errorf("%s 级别 = %s, want block", s.SignalType, s.Level)
		}
	}
	if _, ok := byType[SigSettlementNotApproved]; !ok {
		t.Errorf("缺 settlement_not_approved 信号")
	}
	if _, ok := byType[SigSettlementMissing]; !ok {
		t.Errorf("缺 settlement_missing 信号")
	}
}

func TestCheckSettlements_AllOK(t *testing.T) {
	found := []SettlementRow{{ID: "S1", ApproveStatus: "1"}}
	if sigs := checkSettlements([]string{"S1"}, found); len(sigs) != 0 {
		t.Errorf("全通过应无信号, got %d", len(sigs))
	}
}

func TestCheckSheets(t *testing.T) {
	lines := []SettlementLineRow{
		{SettlementID: "S1", VoucherID: "P1"},
		{SettlementID: "S1", VoucherID: "P2"},
		{SettlementID: "S2", VoucherID: "P3"},
	}
	sheets := []PurchaseSheetRow{
		{ID: "P1", SupplierID: "SUP1", ApproveStatus: "1"},
		{ID: "P2", SupplierID: "SUP2", ApproveStatus: "0"}, // 未审核 + 供应商不一致
	}
	sup := map[string]string{"S1": "SUP1", "S2": "SUPX"}
	sigs := checkSheets(lines, sheets, sup)
	if len(sigs) != 3 {
		t.Fatalf("信号数 = %d, want 3(P2 未审核 + P2 供应商不一致 + P3 缺失)", len(sigs))
	}
	var notApproved, mismatch, missing int
	for _, s := range sigs {
		if s.Level != model.SignalBlock {
			t.Errorf("%s 级别 = %s, want block", s.SignalType, s.Level)
		}
		switch s.SignalType {
		case SigSheetNotApproved:
			notApproved++
		case SigSupplierMismatch:
			mismatch++
		case SigSheetMissing:
			missing++
		}
	}
	if notApproved != 1 || mismatch != 1 || missing != 1 {
		t.Errorf("分布 notApproved=%d mismatch=%d missing=%d, want 各 1", notApproved, mismatch, missing)
	}
}

func TestCheckSheets_AllOK(t *testing.T) {
	lines := []SettlementLineRow{{SettlementID: "S1", VoucherID: "P1"}}
	sheets := []PurchaseSheetRow{{ID: "P1", SupplierID: "SUP1", ApproveStatus: "1"}}
	sup := map[string]string{"S1": "SUP1"}
	if sigs := checkSheets(lines, sheets, sup); len(sigs) != 0 {
		t.Errorf("全通过应无信号, got %d", len(sigs))
	}
}

func TestCheckAmounts_LineMismatch(t *testing.T) {
	settlements := []SettlementRow{
		{ID: "S1", TotalGoodsYuan: dec("100")},
		{ID: "S2", TotalGoodsYuan: dec("50")},
	}
	lines := map[string][]SettlementLineRow{
		"S1": {{SettlementID: "S1", TotalGoodsYuan: dec("100")}},
		"S2": {{SettlementID: "S2", TotalGoodsYuan: dec("40")}}, // 差 10
	}
	sigs := checkAmounts(settlements, lines, dec("150"), dec("0.01"))
	if len(sigs) != 1 {
		t.Fatalf("信号数 = %d, want 1(仅 S2 行勾稽)", len(sigs))
	}
	if sigs[0].SignalType != SigLineAmountMismatch || sigs[0].Level != model.SignalBlock {
		t.Errorf("type=%s level=%s, want line_amount_mismatch/block", sigs[0].SignalType, sigs[0].Level)
	}
	if sigs[0].SettlementID != "S2" {
		t.Errorf("settlement_id = %s, want S2", sigs[0].SettlementID)
	}
}

func TestCheckAmounts_ManualMismatch(t *testing.T) {
	settlements := []SettlementRow{{ID: "S1", TotalGoodsYuan: dec("100")}}
	lines := map[string][]SettlementLineRow{
		"S1": {{SettlementID: "S1", TotalGoodsYuan: dec("100")}},
	}
	sigs := checkAmounts(settlements, lines, dec("95"), dec("0.01"))
	if len(sigs) != 1 {
		t.Fatalf("信号数 = %d, want 1(手工金额勾稽)", len(sigs))
	}
	if sigs[0].SignalType != SigManualAmountMismatch || sigs[0].Level != model.SignalBlock {
		t.Errorf("type=%s level=%s, want manual_amount_mismatch/block", sigs[0].SignalType, sigs[0].Level)
	}
}

func TestCheckAmounts_WithinTolerance(t *testing.T) {
	settlements := []SettlementRow{{ID: "S1", TotalGoodsYuan: dec("100")}}
	lines := map[string][]SettlementLineRow{
		"S1": {{SettlementID: "S1", TotalGoodsYuan: dec("100.005")}}, // 差 0.005 < 0.01
	}
	if sigs := checkAmounts(settlements, lines, dec("100"), dec("0.01")); len(sigs) != 0 {
		t.Errorf("容差内应无信号, got %d", len(sigs))
	}
}

func TestCheckQuantity(t *testing.T) {
	tests := []struct {
		name      string
		recv      string
		sold      string // 空 = 无销售记录
		wantLevel string
		wantType  string
	}{
		{"无销售进货", "10", "", model.SignalBlock, SigPurchaseWithoutSales},
		{"超2倍阈值", "70", "10", model.SignalBlock, SigQtyDeviation},
		{"超1倍阈值", "35", "10", model.SignalWarn, SigQtyDeviation},
		{"正常", "25", "10", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			receivedByProduct := map[string]decimal.Decimal{"PR1": dec(tt.recv)}
			sales := map[string]SaleRow{}
			if tt.sold != "" {
				sales["PR1"] = SaleRow{ProductID: "PR1", NetSaleQuantity: dec(tt.sold)}
			}
			sigs := checkQuantity(receivedByProduct, sales, 3)
			if tt.wantType == "" {
				if len(sigs) != 0 {
					t.Fatalf("应无信号, got %v", sigs)
				}
				return
			}
			if len(sigs) != 1 {
				t.Fatalf("信号数 = %d, want 1", len(sigs))
			}
			if sigs[0].Level != tt.wantLevel || sigs[0].SignalType != tt.wantType {
				t.Errorf("level=%s type=%s, want %s/%s", sigs[0].Level, sigs[0].SignalType, tt.wantLevel, tt.wantType)
			}
			if sigs[0].ProductID != "PR1" {
				t.Errorf("product_id = %s, want PR1", sigs[0].ProductID)
			}
		})
	}
}

func TestCheckQuantity_ZeroReceivedSkip(t *testing.T) {
	received := map[string]decimal.Decimal{"PR1": dec("0")}
	if sigs := checkQuantity(received, nil, 3); len(sigs) != 0 {
		t.Errorf("进货 0 应跳过, got %d", len(sigs))
	}
}

func TestHasBlock(t *testing.T) {
	if hasBlock(nil) {
		t.Error("空信号集不应有 block")
	}
	if hasBlock([]signalDraft{{Level: model.SignalWarn}}) {
		t.Error("仅 warn 不应有 block")
	}
	if !hasBlock([]signalDraft{{Level: model.SignalWarn}, {Level: model.SignalBlock}}) {
		t.Error("含 block 应返回 true")
	}
}

func TestMarshalEvidence_Nil(t *testing.T) {
	if b := marshalEvidence(nil); b != nil {
		t.Errorf("nil evidence 应返回 nil, got %s", b)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("短串不应截断, got %q", got)
	}
	if got := truncate(strings.Repeat("x", 100), 10); len(got) != 10 {
		t.Errorf("应截断到 10, got %d", len(got))
	}
}

// TestTruncate_UTF8Boundary 锁死 2026-10-10 生产事故:
// 中文 reason 在字节上限处被切成半个三字节字符,Postgres 22021 拒收,
// llm_verdict/finish 审计日志整行丢失。截断必须退回完整 rune 边界。
func TestTruncate_UTF8Boundary(t *testing.T) {
	// "结" 3 字节;构造一个在 max 处正好切断第三字节的串。
	s := strings.Repeat("结", 100) // 300 字节
	for _, max := range []int{1, 2, 4, 5, 200, 299} {
		got := truncate(s, max)
		if !utf8.ValidString(got) {
			t.Fatalf("max=%d 截断结果不是合法 UTF-8: % x", max, got)
		}
		if len(got) > max {
			t.Fatalf("max=%d 截断后长度 %d 超上限", max, len(got))
		}
		if want := max / 3 * 3; len(got) != want {
			t.Fatalf("max=%d 应退到 rune 边界 %d, got %d", max, want, len(got))
		}
	}
	// 混合中英文也不许断。
	mixed := truncate("已拒绝并通知申请人:"+strings.Repeat("因", 100), 200)
	if !utf8.ValidString(mixed) {
		t.Fatalf("混合串截断结果不是合法 UTF-8: % x", mixed)
	}
}

func TestDedupeStrings(t *testing.T) {
	got := dedupeStrings([]string{" S1 ", "S1", "", "S2", " S2"})
	if len(got) != 2 || got[0] != "S1" || got[1] != "S2" {
		t.Errorf("dedupe = %v, want [S1 S2]", got)
	}
}
