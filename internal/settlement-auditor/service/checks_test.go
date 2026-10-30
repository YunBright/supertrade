package service

import (
	"fmt"
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
	sigs, _ := checkAmounts(settlements, lines, dec("150"), dec("0.01"), dec("1"), decimal.Zero, decimal.Zero)
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
	sigs, summary := checkAmounts(settlements, lines, dec("95"), dec("0.01"), dec("1"), decimal.Zero, decimal.Zero)
	if len(sigs) != 1 {
		t.Fatalf("信号数 = %d, want 1(手工金额勾稽)", len(sigs))
	}
	if sigs[0].SignalType != SigManualAmountMismatch || sigs[0].Level != model.SignalBlock {
		t.Errorf("type=%s level=%s, want manual_amount_mismatch/block", sigs[0].SignalType, sigs[0].Level)
	}
	if summary.Consistent {
		t.Error("差 5 元应判定为不一致")
	}
}

func TestCheckAmounts_WithinTolerance(t *testing.T) {
	settlements := []SettlementRow{{ID: "S1", TotalGoodsYuan: dec("100")}}
	lines := map[string][]SettlementLineRow{
		"S1": {{SettlementID: "S1", TotalGoodsYuan: dec("100.005")}}, // 差 0.005 < 0.01
	}
	if sigs, _ := checkAmounts(settlements, lines, dec("100"), dec("0.01"), dec("1"), decimal.Zero, decimal.Zero); len(sigs) != 0 {
		t.Errorf("容差内应无信号, got %d", len(sigs))
	}
}

// TestCheckAmounts_PaymentRoundingIsNotAnError 是 2026-10-22 生产事故的回归锁。
//
// 实测(审批 54bb82421a08480bbcfdacaf6c814b5e):手工付款额 8206.20,
// 结算单货款合计 8206.1946(7765.1946 + 441.0000),真实差额 **0.0054 元** ——
// 实际付款按元取整,这根本不是账实不符,引擎原本也判为容差内(0 条信号)。
// 但 LLM 拿显示值自己加了一遍,造出"差 0.01"并拒了单。
//
// ⇒ 付款口径必须按**元**取整:差额 < 1 元 = 同一笔付款,不阻断。
func TestCheckAmounts_PaymentRoundingIsNotAnError(t *testing.T) {
	settlements := []SettlementRow{
		{ID: "CP002609014321", TotalGoodsYuan: dec("7765.1946")},
		{ID: "CP002609014322", TotalGoodsYuan: dec("441.0000")},
	}
	lines := map[string][]SettlementLineRow{
		"CP002609014321": {{SettlementID: "CP002609014321", TotalGoodsYuan: dec("7765.1946")}},
		"CP002609014322": {{SettlementID: "CP002609014322", TotalGoodsYuan: dec("441.0000")}},
	}
	sigs, summary := checkAmounts(settlements, lines, dec("8206.20"), dec("0.01"), dec("1"), decimal.Zero, decimal.Zero)
	if len(sigs) != 0 {
		t.Fatalf("0.0054 元的取整差异不应产生信号, got %d: %+v", len(sigs), sigs)
	}
	if !summary.Consistent {
		t.Error("金额勾稽结论应为「一致」")
	}
	if summary.DiffYuan.String() != "0.0054" {
		t.Errorf("差额 = %s, want 0.0054", summary.DiffYuan.String())
	}
	if summary.SettledTotalYuan.String() != "8206.1946" {
		t.Errorf("合计 = %s, want 8206.1946", summary.SettledTotalYuan.String())
	}
}

// TestCheckAmounts_FenAndJiaoDiffNeverBlocks 锁"分位/角位差异不是问题"。
//
// 用户口径(2026-10-22):实际付款按元付,分位一般不实际支付,角位也可能被舍去。
// ⇒ 任何小于 1 元的差额都是取整内的同一笔付款。
func TestCheckAmounts_FenAndJiaoDiffNeverBlocks(t *testing.T) {
	for _, manual := range []string{"8206.21", "8206.29", "8206.49", "8207.18"} {
		t.Run(manual, func(t *testing.T) {
			settlements := []SettlementRow{{ID: "S1", TotalGoodsYuan: dec("8206.1946")}}
			lines := map[string][]SettlementLineRow{
				"S1": {{SettlementID: "S1", TotalGoodsYuan: dec("8206.1946")}},
			}
			sigs, summary := checkAmounts(settlements, lines, dec(manual), dec("0.01"), dec("1"), decimal.Zero, decimal.Zero)
			if len(sigs) != 0 {
				t.Fatalf("差额 %s 元应不阻断, got %d 条信号", manual, len(sigs))
			}
			if !summary.Consistent {
				t.Errorf("差额 %s 元应判定为一致", manual)
			}
		})
	}
}

// TestCheckAmounts_YuanLevelDiffBlocks 反向锁:达到"元"级就必须拦。
func TestCheckAmounts_YuanLevelDiffBlocks(t *testing.T) {
	settlements := []SettlementRow{{ID: "S1", TotalGoodsYuan: dec("8206.1946")}}
	lines := map[string][]SettlementLineRow{
		"S1": {{SettlementID: "S1", TotalGoodsYuan: dec("8206.1946")}},
	}
	sigs, summary := checkAmounts(settlements, lines, dec("8207.20"), dec("0.01"), dec("1"), decimal.Zero, decimal.Zero)
	if len(sigs) != 1 {
		t.Fatalf("差 1.0054 元应产生 1 条信号, got %d", len(sigs))
	}
	if sigs[0].SignalType != SigManualAmountMismatch {
		t.Errorf("type = %s, want manual_amount_mismatch", sigs[0].SignalType)
	}
	if summary.Consistent {
		t.Error("达元级的差异应判定为不一致")
	}
}

// TestCheckAmounts_LineCheckKeepsStrictNumericTolerance 锁"付款取整不放宽源数据勾稽"。
//
// 单头 vs 明细是**同一套源数据**的自洽性检查(不是付款口径),
// 所以仍然用数值容差 0.01 元 —— 差 0.5 元也要报。
func TestCheckAmounts_LineCheckKeepsStrictNumericTolerance(t *testing.T) {
	settlements := []SettlementRow{{ID: "S1", TotalGoodsYuan: dec("100")}}
	lines := map[string][]SettlementLineRow{
		"S1": {{SettlementID: "S1", TotalGoodsYuan: dec("100.5")}}, // 差 0.5 > 数值容差 0.01
	}
	sigs, _ := checkAmounts(settlements, lines, dec("100"), dec("0.01"), dec("1"), decimal.Zero, decimal.Zero)
	if len(sigs) != 1 {
		t.Fatalf("单头 vs 明细差 0.5 元应产生 1 条信号, got %d", len(sigs))
	}
	if sigs[0].SignalType != SigLineAmountMismatch {
		t.Errorf("type = %s, want line_amount_mismatch", sigs[0].SignalType)
	}
}

// TestConfig_AmountTolerances 锁住两层容差的**默认值**。
//
// 付款口径默认 1 元(实际付款按元取整:分位不实际支付、角位可能被舍去);
// 数值容差仍是 0.01 元(同一套源数据内部的自洽性,不放宽)。
//
// 2026-10-22 事故的直接教训:付款口径若沿用 0.01 元,
// 任何"角位被舍去"的正常付款都会被当成账实不符。
func TestConfig_AmountTolerances(t *testing.T) {
	cfg := New(nil, Config{}).Config()
	if cfg.PaymentToleranceYuan != 1 {
		t.Errorf("PaymentToleranceYuan = %v, want 1(付款按元取整)", cfg.PaymentToleranceYuan)
	}
	if cfg.AmountToleranceYuan != 0.01 {
		t.Errorf("AmountToleranceYuan = %v, want 0.01(数值容差,单头 vs 明细)", cfg.AmountToleranceYuan)
	}
}

// TestYuanText_KeepsSubCentPrecision 锁"差额不能被显示精度掩盖"。
//
// 0.014 元被 StringFixed(2) 打成 "0.01" ⇒ 看的人以为只差 1 分,
// 实际已经过了 0.01 的数值容差 —— 显示值比真实值小 ⇒ 低估问题量级。
func TestYuanText_KeepsSubCentPrecision(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0.014", "0.014"},
		{"0.0054", "0.0054"},
		{"8206.1946", "8206.1946"},
		{"8206.2", "8206.2"},
	}
	for _, c := range cases {
		if got := yuanText(dec(c.in)); got != c.want {
			t.Errorf("yuanText(%s) = %s, want %s", c.in, got, c.want)
		}
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
			sigs := checkQuantity(receivedByProduct, sales, nil, 3, QuantityContext{})
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
	if sigs := checkQuantity(received, nil, nil, 3, QuantityContext{}); len(sigs) != 0 {
		t.Errorf("进货 0 应跳过, got %d", len(sigs))
	}
}

// TestCheckQuantity_LossDowngradesBlock 锁 2026-10-31 用户口径:
// 盘损单把"原本会 block"的数量偏离解释到可接受范围 ⇒ **block 降 warn**(不阻断放行)。
//
// 生产实况:进货 10、窗口销量 0.91(ratio ≈ 11 倍 ⇒ block),
// 但同一窗口报损 8.43 ⇒ 消耗 9.34,偏离回到 ×1~×2 的 warn 区间。
func TestCheckQuantity_LossDowngradesBlock(t *testing.T) {
	received := map[string]decimal.Decimal{"PR1": dec("10")}
	sales := map[string]SaleRow{"PR1": {ProductID: "PR1", NetSaleQuantity: dec("0.91")}}
	loss := map[string]decimal.Decimal{"PR1": dec("8.43")}

	sigs := checkQuantity(received, sales, loss, 3, QuantityContext{
		WindowFrom: map[string]string{"PR1": "2026-08-25"},
	})
	if len(sigs) != 1 {
		t.Fatalf("信号数 = %d, want 1", len(sigs))
	}
	if sigs[0].Level != model.SignalWarn || sigs[0].SignalType != SigQtyDeviation {
		t.Fatalf("level=%s type=%s, want warn/qty_deviation(盘损降级)", sigs[0].Level, sigs[0].SignalType)
	}
	// 消息里要写出盘损与合计,否则发起人看不懂"为什么 11 倍还在可接受范围"。
	for _, want := range []string{"盘损 8.43", "合计 9.34"} {
		if !strings.Contains(sigs[0].Message, want) {
			t.Errorf("降级消息应含 %q: %s", want, sigs[0].Message)
		}
	}
}

// TestCheckQuantity_LossInsufficientStillBlock 锁"证据不够就还是 block"。
//
// 报损 0.5 解释不了 10 倍的偏离 ⇒ 仍按 ×factor×2 判 block。
func TestCheckQuantity_LossInsufficientStillBlock(t *testing.T) {
	received := map[string]decimal.Decimal{"PR1": dec("10")}
	sales := map[string]SaleRow{"PR1": {ProductID: "PR1", NetSaleQuantity: dec("0.91")}}
	loss := map[string]decimal.Decimal{"PR1": dec("0.5")}

	sigs := checkQuantity(received, sales, loss, 3, QuantityContext{})
	if len(sigs) != 1 {
		t.Fatalf("信号数 = %d, want 1", len(sigs))
	}
	if sigs[0].Level != model.SignalBlock {
		t.Errorf("level = %s, want block(盘损不足)", sigs[0].Level)
	}
}

// TestCheckQuantity_LossOnly_NoSales 锁"零销量但有盘损"的边界:
//
// 分母 = 销量 0 + 盘损 > 0 ⇒ 不再是 purchase_without_sales 的 block;
// 但"不扣盘损会 block"这件事仍然成立 ⇒ 留一条 warn(降级但不静默)。
func TestCheckQuantity_LossOnly_NoSales(t *testing.T) {
	received := map[string]decimal.Decimal{"PR1": dec("8")}
	loss := map[string]decimal.Decimal{"PR1": dec("8")}

	sigs := checkQuantity(received, map[string]SaleRow{}, loss, 3, QuantityContext{})
	if len(sigs) != 1 {
		t.Fatalf("信号数 = %d, want 1", len(sigs))
	}
	if sigs[0].Level != model.SignalWarn || sigs[0].SignalType != SigQtyDeviation {
		t.Errorf("level=%s type=%s, want warn/qty_deviation(盘损解释掉无销量)", sigs[0].Level, sigs[0].SignalType)
	}
}

// TestCheckQuantity_LossWidensConsumption 锁"盘损把分母撑大后**不该再误拦**"。
//
// 进货 10、销量 10、盘损 30 ⇒ ratio = 0.25,本来就不该有信号
// (分母大是"店里损耗本来就多",不是本次进货的疑点)。
func TestCheckQuantity_LossWidensConsumption(t *testing.T) {
	received := map[string]decimal.Decimal{"PR1": dec("10")}
	sales := map[string]SaleRow{"PR1": {ProductID: "PR1", NetSaleQuantity: dec("10")}}
	loss := map[string]decimal.Decimal{"PR1": dec("30")}
	if sigs := checkQuantity(received, sales, loss, 3, QuantityContext{}); len(sigs) != 0 {
		t.Errorf("盘损撑大分母后应无数量信号, got %d: %v", len(sigs), sigs)
	}
}

// TestBuildSaleWindowGroups_UsesLastPurchaseDate 锁 2026-10-22 用户口径:
// 销量窗口 = **从最后一次采购的采购入库单日期到当前日期**。
//
// 生产实况(审批 54bb…):进货发生在 2026-08-25,旧规则却拿
// 2026-09-22~10-22 的销量去比 —— 被审的那次进货根本不在窗口里。
func TestBuildSaleWindowGroups_UsesLastPurchaseDate(t *testing.T) {
	sheetDates := map[string]string{
		"PI-A": "2026-08-23",
		"PI-B": "2026-08-26",
	}
	productSheets := map[string][]string{
		"PR1": {"PI-A"},         // 只在 08-23 进过货
		"PR2": {"PI-A", "PI-B"}, // 两次都进 ⇒ 起点取**最晚**的 08-26
		"PR3": {"PI-MISSING"},   // 取不到日期 ⇒ 回退固定窗口
	}
	groups := buildSaleWindowGroups(sheetDates, productSheets, "2026-10-22", "2026-09-22", 0)
	got := map[string]string{}
	for _, g := range groups {
		for _, pid := range g.Products {
			got[pid] = g.From
		}
	}
	if got["PR1"] != "2026-08-23" {
		t.Errorf("PR1 窗口起点 = %s, want 2026-08-23", got["PR1"])
	}
	if got["PR2"] != "2026-08-26" {
		t.Errorf("PR2 窗口起点 = %s, want 2026-08-26(取最晚那次进货)", got["PR2"])
	}
	if got["PR3"] != "2026-09-22" {
		t.Errorf("PR3 取不到日期应回退 %s, got %s", "2026-09-22", got["PR3"])
	}
}

// TestBuildSaleWindowGroups_MergesWhenTooMany 锁"分组数有上限"。
//
// 一次审批可能勾几十张结算单 ⇒ 采购日期数不可控 ⇒ 查询次数不可控。
// 超上限时把起点最近的两组合并(取更早的起点 = 放宽方向:只少拦不多拦)。
func TestBuildSaleWindowGroups_MergesWhenTooMany(t *testing.T) {
	sheetDates := map[string]string{}
	productSheets := map[string][]string{}
	for i := 0; i < 20; i++ {
		sid := "PI-" + string(rune('A'+i))
		sheetDates[sid] = "2026-08-" + fmt.Sprintf("%02d", i+1)
		productSheets["PR"+string(rune('A'+i))] = []string{sid}
	}
	groups := buildSaleWindowGroups(sheetDates, productSheets, "2026-10-22", "2026-09-22", 3)
	if len(groups) != 3 {
		t.Fatalf("分组数 = %d, want 3(上限)", len(groups))
	}
	// 合并取更早的起点 ⇒ 每个商品都还在某个组里(不丢商品)。
	total := 0
	for _, g := range groups {
		total += len(g.Products)
	}
	if total != 20 {
		t.Errorf("合并后商品数 = %d, want 20(不能丢)", total)
	}
}

// TestProductLabel 锁「<商品名称>-<商品货号>」的展示格式(用户口径 2026-10-22)。
func TestProductLabel(t *testing.T) {
	names := map[string]string{"02000116": "16.8升琅悦泉水桶装水"}
	if got := ProductLabel(names, "02000116"); got != "16.8升琅悦泉水桶装水-02000116" {
		t.Errorf("label = %q", got)
	}
	if got := ProductLabel(names, "02000999"); got != "02000999" {
		t.Errorf("查不到名字应回退纯货号, got %q", got)
	}
	if got := ProductLabel(nil, "02000116"); got != "02000116" {
		t.Errorf("无名字字典应回退纯货号, got %q", got)
	}
}

// TestCheckQuantity_NeutralWording 锁"信号只陈述事实、不做动机推断"。
//
// 这些文字是给**发起人**看的(用户口径 2026-10-22):禁止"虚进""套取付款"
// 这类推断性结论 —— 判断本来就不该由机器下。
func TestCheckQuantity_NeutralWording(t *testing.T) {
	received := map[string]decimal.Decimal{"02000116": dec("18")}
	sigs := checkQuantity(received, map[string]SaleRow{}, nil, 3, QuantityContext{
		Names:      map[string]string{"02000116": "16.8升琅悦泉水桶装水"},
		WindowFrom: map[string]string{"02000116": "2026-08-25"},
	})
	if len(sigs) != 1 {
		t.Fatalf("信号数 = %d, want 1", len(sigs))
	}
	msg := sigs[0].Message
	for _, bad := range []string{"虚进", "套取", "刷单", "舞弊", "风险", "异常"} {
		if strings.Contains(msg, bad) {
			t.Errorf("信号措辞不应出现 %q: %s", bad, msg)
		}
	}
	for _, want := range []string{"16.8升琅悦泉水桶装水-02000116", "2026-08-25", "18"} {
		if !strings.Contains(msg, want) {
			t.Errorf("信号应含 %q: %s", want, msg)
		}
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

// TestBuildSaleWindowGroupsByDate_Earliest 锁「实现毛利」的窗口起点口径。
//
// 2026-10-29 用户口径：「结算单的采购订单里该sku**最早**采购日期起
// 至发起'付款审批'止日期段」。
//
// ⚠️ 与窗口毛利的**最晚**口径不同 —— 两个窗口不同 ⇒ 销量不同 ⇒
//    两个毛利结论可能相反（这正是要同时出两个信号的原因）。
func TestBuildSaleWindowGroupsByDate_Earliest(t *testing.T) {
	sheetDates := map[string]string{
		"PI-A": "2026-08-23",
		"PI-B": "2026-08-26",
	}
	productSheets := map[string][]string{
		"PR1": {"PI-A"},
		"PR2": {"PI-A", "PI-B"}, // 两次都进 ⇒ 起点取**最早**的 08-23
		"PR3": {"PI-MISSING"},   // 取不到日期 ⇒ 回退固定窗口
	}
	groups := buildSaleWindowGroupsByDate(
		sheetDates, productSheets, "2026-10-22", "2026-09-22", 0, windowStartEarliest)
	got := map[string]string{}
	for _, g := range groups {
		for _, pid := range g.Products {
			got[pid] = g.From
		}
	}
	if got["PR1"] != "2026-08-23" {
		t.Errorf("PR1 窗口起点 = %s, want 2026-08-23", got["PR1"])
	}
	if got["PR2"] != "2026-08-23" {
		t.Errorf("PR2 窗口起点 = %s, want 2026-08-23(取最早那次进货)", got["PR2"])
	}
	if got["PR3"] != "2026-09-22" {
		t.Errorf("PR3 取不到日期应回退 %s, got %s", "2026-09-22", got["PR3"])
	}
	// 最晚口径保持原样（窗口毛利不受影响）。
	latest := buildSaleWindowGroupsByDate(
		sheetDates, productSheets, "2026-10-22", "2026-09-22", 0, windowStartLatest)
	gotLatest := map[string]string{}
	for _, g := range latest {
		for _, pid := range g.Products {
			gotLatest[pid] = g.From
		}
	}
	if gotLatest["PR2"] != "2026-08-26" {
		t.Errorf("PR2 最晚口径起点 = %s, want 2026-08-26", gotLatest["PR2"])
	}
}