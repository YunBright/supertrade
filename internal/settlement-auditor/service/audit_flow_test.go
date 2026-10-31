package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
)

// ---- 测试基建 ----

// newFlowTestService 建一个带**流程配置三张表**的 service。
func newFlowTestService(t *testing.T) (*Service, *gorm.DB, *fakeCube, *fakePublisher) {
	t.Helper()
	db, err := testdb.OpenSQLite(":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.ApprovalInstance{}, &model.ApprovalSettlement{}, &model.ApprovalAttachment{}, &model.AuditSignal{},
		&model.AuditLog{}, &model.LLMVerdict{}, &model.ApprovalStocktakeLink{},
		&model.SettlementPayState{}, &model.StocktakeNotifyGroup{},
		&model.ApprovalItem{}, &model.ApprovalPayeeAccount{},
		&model.AuditSupplierFlow{}, &model.AuditPaymentItemFlow{}, &model.AuditRegexPattern{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cube := &fakeCube{}
	pub := &fakePublisher{}
	svc := New(db, Config{
		CorpID:     "wcorp-test",
		TemplateID: "tpl-1",
	})
	svc.SetCube(cube)
	svc.SetPublisher(pub)
	svc.SetClock(func() time.Time { return fixedNow })
	return svc, db, cube, pub
}

// ---- 所属日期提取（用户口径例："9月水电费"里的"9月"）----

// TestParsePeriodText 锁住「从备注提取款项所属日期」的解析。
//
// ⚠️ 判别力自检：把 parsePeriodText 里的"月"正则去掉 ⇒ 本用例立刻红。
func TestParsePeriodText(t *testing.T) {
	cases := []struct {
		in      string
		wantY   int
		wantM   int
		wantOK  bool
		comment string
	}{
		{in: "9", wantY: 2026, wantM: 9, wantOK: true, comment: "纯数字当作月份"},
		{in: "9月", wantY: 2026, wantM: 9, wantOK: true, comment: "用户口径原例"},
		{in: "9月份", wantY: 2026, wantM: 9, wantOK: true},
		{in: "2026年9月", wantY: 2026, wantM: 9, wantOK: true},
		{in: "2026-09", wantY: 2026, wantM: 9, wantOK: true},
		{in: "12月", wantY: 2026, wantM: 12, wantOK: true},
		{in: "水电费", wantOK: false, comment: "没有月份信息 ⇒ 提不出来"},
		{in: "13月", wantOK: false, comment: "13 月不是合法月份"},
		{in: "26年9月", wantY: 2026, wantM: 9, wantOK: true,
			comment: "2 位年份不猜 ⇒ 年份取缺省年(2026),月照样认"},
	}
	for _, c := range cases {
		y, m, ok := parsePeriodText(c.in, 2026)
		if ok != c.wantOK {
			t.Errorf("parsePeriodText(%q) ok=%v, want %v(%s)", c.in, ok, c.wantOK, c.comment)
			continue
		}
		if !ok {
			continue
		}
		if y != c.wantY || m != c.wantM {
			t.Errorf("parsePeriodText(%q) = %d-%d, want %d-%d(%s)",
				c.in, y, m, c.wantY, c.wantM, c.comment)
		}
	}
}

// TestRegexTest_ExtractsMonthFromRemark 是管理后台「正则测试」的后端内核。
//
// 用户口径：例 "9月水电费" 里的 "9月"。
func TestRegexTest_ExtractsMonthFromRemark(t *testing.T) {
	svc, _, _, _ := newFlowTestService(t)
	res, err := svc.TestRegex(context.Background(), "B1", RegexTestInput{
		Pattern:    `(\d{1,2})月`,
		Sample:     "9月水电费",
		GroupIndex: intPtr(1),
	})
	if err != nil {
		t.Fatalf("TestRegex: %v", err)
	}
	if !res.Matched {
		t.Fatalf("没匹配上: %+v", res)
	}
	if res.Value != "9" {
		t.Errorf("捕获值 = %q, want \"9\"", res.Value)
	}
	if res.Month != 9 {
		t.Errorf("月份 = %d, want 9", res.Month)
	}
}

// TestRegexTest_GroupOutOfRangeIsExplicit 组号越界必须**明确报错**。
//
// ⚠️ 静默返回空串 ⇒ 用户以为"正则不匹配"，实际是组号填错了 ⇒ 改不对。
func TestRegexTest_GroupOutOfRangeIsExplicit(t *testing.T) {
	svc, _, _, _ := newFlowTestService(t)
	res, err := svc.TestRegex(context.Background(), "B1", RegexTestInput{
		Pattern:    `(\d{1,2})月`,
		Sample:     "9月水电费",
		GroupIndex: intPtr(5),
	})
	if err != nil {
		t.Fatalf("TestRegex: %v", err)
	}
	if res.Error == "" {
		t.Fatal("组号越界必须有明确报错")
	}
	if !strings.Contains(res.Error, "越界") {
		t.Errorf("报错文案应说明越界, got %q", res.Error)
	}
}

func intPtr(v int) *int { return &v }

// ---- 「每日有且仅有一单」（用户口径：查结算单结算到的采购订单）----

// TestEvaluateDailySingle 锁住「每日有且仅有一单」的判定。
//
// 用户口径（2026-10-28）：「查结算单的具体结算的采购订单，采购订单每日只有 1 张。」
// ⇒ 分组维度 = 采购单的**制单日期**。
//
// ⚠️ 判别力自检：把分组键从 CreatedAt 改成供应商 id ⇒ 本用例立刻红。
func TestEvaluateDailySingle(t *testing.T) {
	t.Run("同一天两张采购单 ⇒ 不满足", func(t *testing.T) {
		ok, reason := evaluateDailySingle([]PurchaseSheetRow{
			{ID: "P1", SupplierID: "SUP1", CreatedAt: "2026-09-01"},
			{ID: "P2", SupplierID: "SUP1", CreatedAt: "2026-09-01"},
		})
		if ok {
			t.Fatal("同一天两张采购单应不满足")
		}
		if !strings.Contains(reason, "2026-09-01") {
			t.Errorf("理由应写明是哪一天: %q", reason)
		}
	})
	t.Run("不同日期各一张 ⇒ 满足", func(t *testing.T) {
		ok, reason := evaluateDailySingle([]PurchaseSheetRow{
			{ID: "P1", SupplierID: "SUP1", CreatedAt: "2026-09-01"},
			{ID: "P2", SupplierID: "SUP1", CreatedAt: "2026-09-02"},
		})
		if !ok {
			t.Fatalf("不同日期各一张应满足: %s", reason)
		}
	})
	t.Run("同一天两张但供应商不同 ⇒ 仍然不满足(按日分组,不看供应商)", func(t *testing.T) {
		ok, _ := evaluateDailySingle([]PurchaseSheetRow{
			{ID: "P1", SupplierID: "SUP1", CreatedAt: "2026-09-01"},
			{ID: "P2", SupplierID: "SUP2", CreatedAt: "2026-09-01"},
		})
		if ok {
			t.Fatal("同一天两张采购单应不满足(分组维度是日期)")
		}
	})
	t.Run("0 张采购单 ⇒ 满足(条件无对象)", func(t *testing.T) {
		ok, reason := evaluateDailySingle(nil)
		if !ok {
			t.Fatalf("无采购单不该被卡: %s", reason)
		}
	})
	t.Run("取不到日期的采购单不参与分组(不误判同日多单)", func(t *testing.T) {
		ok, reason := evaluateDailySingle([]PurchaseSheetRow{
			{ID: "P1", CreatedAt: ""},
			{ID: "P2", CreatedAt: ""},
		})
		if !ok {
			t.Fatalf("两张都没日期不该误判为同日多单: %s", reason)
		}
	})
}

// ---- 「每月有且仅有一单」（付款项：所属年/月）----

// TestEvaluateMonthlySingle 锁住「每月有且仅有一单」。
func TestEvaluateMonthlySingle(t *testing.T) {
	ctx := context.Background()

	setup := func(t *testing.T) (*Service, *gorm.DB, *model.AuditPaymentItemFlow) {
		t.Helper()
		svc, db, _, _ := newFlowTestService(t)
		re, err := svc.CreateRegexPattern(ctx, "B1", "op", RegexPatternInput{
			Name: "月份", Pattern: `(\d{1,2})月`, GroupIndex: 1, Enabled: true,
		})
		if err != nil {
			t.Fatalf("建正则: %v", err)
		}
		flow, err := svc.CreateItemFlow(ctx, "B1", "op", ItemFlowInput{
			ItemType: "水电费", Mode: model.FlowModeLogic, LogicKind: model.LogicMonthlySingle,
			RegexID: re.ID, Enabled: true,
		})
		if err != nil {
			t.Fatalf("建付款项流程: %v", err)
		}
		return svc, db, flow
	}

	ap := &model.ApprovalInstance{
		ID: "ap-1", BranchID: "B1", CreatedAt: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC),
	}

	t.Run("本单 1 笔 9 月 ⇒ 满足", func(t *testing.T) {
		svc, db, flow := setup(t)
		if err := db.Create(ap).Error; err != nil {
			t.Fatalf("建审批: %v", err)
		}
		items := []model.ApprovalItem{{
			ApprovalID: "ap-1", BranchID: "B1", Direction: ItemDirectionPayment,
			ItemType: "水电费", Remark: "9月水电费", AmountYuan: dec("100"),
		}}
		ok, reason, err := svc.evaluateMonthlySingle(ctx, ap, flow, items)
		if err != nil {
			t.Fatalf("evaluateMonthlySingle: %v", err)
		}
		if !ok {
			t.Fatalf("应满足: %s", reason)
		}
	})

	t.Run("另有 1 笔 9 月(在生效的审批里) ⇒ 2 笔 ⇒ 不满足", func(t *testing.T) {
		svc, db, flow := setup(t)
		if err := db.Create(ap).Error; err != nil {
			t.Fatalf("建审批: %v", err)
		}
		other := &model.ApprovalInstance{
			ID: "ap-2", BranchID: "B1", Status: model.StatusApproved,
			CreatedAt: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC),
		}
		if err := db.Create(other).Error; err != nil {
			t.Fatalf("建历史审批: %v", err)
		}
		if err := db.Create(&model.ApprovalItem{
			ApprovalID: "ap-2", BranchID: "B1", Direction: ItemDirectionPayment,
			ItemType: "水电费", Remark: "9月水电费", AmountYuan: dec("100"),
		}).Error; err != nil {
			t.Fatalf("建历史明细: %v", err)
		}
		items := []model.ApprovalItem{{
			ApprovalID: "ap-1", BranchID: "B1", Direction: ItemDirectionPayment,
			ItemType: "水电费", Remark: "9月水电费", AmountYuan: dec("100"),
		}}
		ok, reason, err := svc.evaluateMonthlySingle(ctx, ap, flow, items)
		if err != nil {
			t.Fatalf("evaluateMonthlySingle: %v", err)
		}
		if ok {
			t.Fatal("同月两笔应不满足")
		}
		if !strings.Contains(reason, "9 月") && !strings.Contains(reason, "9月") {
			t.Errorf("理由应写明月份: %q", reason)
		}
	})

	t.Run("历史那笔被机器拒了 ⇒ 不计 ⇒ 仍满足(避免「拒了再提交」死循环)", func(t *testing.T) {
		svc, db, flow := setup(t)
		if err := db.Create(ap).Error; err != nil {
			t.Fatalf("建审批: %v", err)
		}
		other := &model.ApprovalInstance{
			ID: "ap-2", BranchID: "B1", Status: model.StatusRejected,
			CreatedAt: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC),
		}
		if err := db.Create(other).Error; err != nil {
			t.Fatalf("建历史审批: %v", err)
		}
		if err := db.Create(&model.ApprovalItem{
			ApprovalID: "ap-2", BranchID: "B1", Direction: ItemDirectionPayment,
			ItemType: "水电费", Remark: "9月水电费", AmountYuan: dec("100"),
		}).Error; err != nil {
			t.Fatalf("建历史明细: %v", err)
		}
		items := []model.ApprovalItem{{
			ApprovalID: "ap-1", BranchID: "B1", Direction: ItemDirectionPayment,
			ItemType: "水电费", Remark: "9月水电费", AmountYuan: dec("100"),
		}}
		ok, reason, err := svc.evaluateMonthlySingle(ctx, ap, flow, items)
		if err != nil {
			t.Fatalf("evaluateMonthlySingle: %v", err)
		}
		if !ok {
			t.Fatalf("被拒的单没付过款,不该占额度: %s", reason)
		}
	})

	t.Run("历史备注提不出月份 ⇒ 不满足(不再静默跳过)", func(t *testing.T) {
		svc, db, flow := setup(t)
		if err := db.Create(ap).Error; err != nil {
			t.Fatalf("建审批: %v", err)
		}
		other := &model.ApprovalInstance{
			ID: "ap-2", BranchID: "B1", Status: model.StatusApproved,
			CreatedAt: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC),
		}
		if err := db.Create(other).Error; err != nil {
			t.Fatalf("建历史审批: %v", err)
		}
		if err := db.Create(&model.ApprovalItem{
			ApprovalID: "ap-2", BranchID: "B1", Direction: ItemDirectionPayment,
			ItemType: "水电费", Remark: "上期水电费已结清", AmountYuan: dec("100"),
		}).Error; err != nil {
			t.Fatalf("建历史明细: %v", err)
		}
		items := []model.ApprovalItem{{
			ApprovalID: "ap-1", BranchID: "B1", Direction: ItemDirectionPayment,
			ItemType: "水电费", Remark: "9月水电费", AmountYuan: dec("100"),
		}}
		ok, reason, err := svc.evaluateMonthlySingle(ctx, ap, flow, items)
		if err != nil {
			t.Fatalf("evaluateMonthlySingle: %v", err)
		}
		if ok {
			t.Fatal("历史行提不出月份时不能静默跳过(会漏掉同月已付过的那笔)")
		}
		if !strings.Contains(reason, "历史") || !strings.Contains(reason, "ap-2") {
			t.Errorf("理由应指出是哪条历史备注: %q", reason)
		}
	})

	t.Run("备注提不出日期 ⇒ 不满足(宁可不通过)", func(t *testing.T) {
		svc, db, flow := setup(t)
		if err := db.Create(ap).Error; err != nil {
			t.Fatalf("建审批: %v", err)
		}
		items := []model.ApprovalItem{{
			ApprovalID: "ap-1", BranchID: "B1", Direction: ItemDirectionPayment,
			ItemType: "水电费", Remark: "水电费", AmountYuan: dec("100"),
		}}
		ok, reason, err := svc.evaluateMonthlySingle(ctx, ap, flow, items)
		if err != nil {
			t.Fatalf("evaluateMonthlySingle: %v", err)
		}
		if ok {
			t.Fatal("提不出日期不该放行")
		}
		if !strings.Contains(reason, "无法") {
			t.Errorf("理由应说明提不出日期: %q", reason)
		}
	})
}

// ---- 决策：分类 / 逐个评估 / 是否调 LLM ----

// TestDecideAuditFlow_SupplierBranch 锁住「有结算单 → 走供应商判断」。
func TestDecideAuditFlow_SupplierBranch(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newFlowTestService(t)
	ap := &model.ApprovalInstance{ID: "ap-1", BranchID: "B1", CreatedAt: fixedNow}
	settlements := []SettlementRow{{ID: "S1", SupplierID: "SUP1", TotalGoodsYuan: dec("100")}}

	// 未配置 ⇒ 走机器人审核流程（Applied=false）。
	dec, err := svc.decideAuditFlow(ctx, ap, settlements, nil, nil, nil)
	if err != nil {
		t.Fatalf("decideAuditFlow: %v", err)
	}
	if dec.Applied {
		t.Fatal("未配置供应商不该命中流程")
	}

	if _, err := svc.CreateSupplierFlow(ctx, "B1", "op", SupplierFlowInput{
		SupplierID: "SUP1", SupplierName: "某供应商", Mode: model.FlowModeLogic,
		LogicKind: model.LogicDailySingle, Enabled: true,
	}); err != nil {
		t.Fatalf("建供应商流程: %v", err)
	}
	dec, err = svc.decideAuditFlow(ctx, ap, settlements, nil, nil, nil)
	if err != nil {
		t.Fatalf("decideAuditFlow: %v", err)
	}
	if !dec.Applied {
		t.Fatal("配了供应商流程却没命中")
	}
	if dec.Path != FlowPathSupplier {
		t.Errorf("路径 = %q, want %q", dec.Path, FlowPathSupplier)
	}
	if len(dec.Subjects) != 1 || dec.Subjects[0].ID != "SUP1" {
		t.Errorf("评估明细 = %+v", dec.Subjects)
	}
}

// TestDecideAuditFlow_PaymentItemBranch 锁住「无结算单 → 走付款项目判断」。
//
// ⚠️ 关键：**有结算单时不会掉到付款项分支**（哪怕付款项也配了）。
func TestDecideAuditFlow_PaymentItemBranch(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newFlowTestService(t)
	ap := &model.ApprovalInstance{ID: "ap-1", BranchID: "B1", CreatedAt: fixedNow}
	items := []model.ApprovalItem{{
		ApprovalID: "ap-1", BranchID: "B1", Direction: ItemDirectionPayment,
		ItemType: "水电费", Remark: "9月水电费", AmountYuan: dec("100"),
	}}
	if _, err := svc.CreateItemFlow(ctx, "B1", "op", ItemFlowInput{
		ItemType: "水电费", Mode: model.FlowModeAutoPass, Enabled: true,
	}); err != nil {
		t.Fatalf("建付款项流程: %v", err)
	}

	// 无结算单 ⇒ 付款项分支。
	dec, err := svc.decideAuditFlow(ctx, ap, nil, nil, nil, items)
	if err != nil {
		t.Fatalf("decideAuditFlow: %v", err)
	}
	if !dec.Applied || dec.Path != FlowPathPaymentItem {
		t.Fatalf("应走付款项分支, got applied=%v path=%q", dec.Applied, dec.Path)
	}

	// 有结算单 ⇒ 供应商分支（**不**掉到付款项分支）。
	dec, err = svc.decideAuditFlow(ctx, ap,
		[]SettlementRow{{ID: "S1", SupplierID: "SUP1"}}, nil, nil, items)
	if err != nil {
		t.Fatalf("decideAuditFlow: %v", err)
	}
	if dec.Applied {
		t.Fatalf("有结算单时不该命中付款项配置: %+v", dec.Subjects)
	}
}

// TestDecideAuditFlow_AnySubjectFailsRejectsWhole 锁住用户口径③：
// 「逐个评估，任一不满足就整单不通过」。
func TestDecideAuditFlow_AnySubjectFailsRejectsWhole(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newFlowTestService(t)
	ap := &model.ApprovalInstance{ID: "ap-1", BranchID: "B1", CreatedAt: fixedNow}
	settlements := []SettlementRow{
		{ID: "S1", SupplierID: "SUP1"},
		{ID: "S2", SupplierID: "SUP2"},
	}
	// SUP1 直接通过；SUP2 逻辑判断但条件不满足（同日两张采购单）。
	if _, err := svc.CreateSupplierFlow(ctx, "B1", "op", SupplierFlowInput{
		SupplierID: "SUP1", Mode: model.FlowModeAutoPass, Enabled: true,
	}); err != nil {
		t.Fatalf("建 SUP1: %v", err)
	}
	if _, err := svc.CreateSupplierFlow(ctx, "B1", "op", SupplierFlowInput{
		SupplierID: "SUP2", Mode: model.FlowModeLogic, LogicKind: model.LogicDailySingle, Enabled: true,
	}); err != nil {
		t.Fatalf("建 SUP2: %v", err)
	}
	// ⚠️ 采购单必须**挂到结算单上**（结算明细行 → 采购单号 → 采购单头），
	//	因为「每日一单」只统计**这个供应商自己的结算单结算到的采购单**。
	lines := map[string][]SettlementLineRow{
		"S1": {{SettlementID: "S1", VoucherID: "P1", SourceDocType: "PI"}},
		"S2": {{SettlementID: "S2", VoucherID: "P2", SourceDocType: "PI"},
			{SettlementID: "S2", VoucherID: "P3", SourceDocType: "PI"}},
	}
	sheets := []PurchaseSheetRow{
		{ID: "P1", SupplierID: "SUP1", CreatedAt: "2026-09-01"},
		{ID: "P2", SupplierID: "SUP2", CreatedAt: "2026-09-01"},
		{ID: "P3", SupplierID: "SUP2", CreatedAt: "2026-09-01"},
	}
	dec, err := svc.decideAuditFlow(ctx, ap, settlements, lines, sheets, nil)
	if err != nil {
		t.Fatalf("decideAuditFlow: %v", err)
	}
	if !dec.Applied {
		t.Fatal("应命中配置")
	}
	if len(dec.Subjects) != 2 {
		t.Fatalf("应逐个评估 2 个供应商, got %d", len(dec.Subjects))
	}
	if !dec.Subjects[0].Satisfied {
		t.Errorf("SUP1(直接通过)应满足: %s", dec.Subjects[0].Reason)
	}
	if dec.Subjects[1].Satisfied {
		t.Error("SUP2 条件不满足,应判不满足")
	}
	if !strings.Contains(dec.Reason, "不通过") {
		t.Errorf("整体结论应是「不通过」: %q", dec.Reason)
	}
}

// TestDecideAuditFlow_MultiSupplierSameDayNotFalselyRejected 锁「按主体隔离」。
//
// # 场景
//
//	一单里两个供应商，各 1 张采购单，**制单日期同一天**。
//	门店日常就是"一天进好几家货" ⇒ 这是**正常单**，不能被误拒。
//
// # 为什么这条重要
//
//	若把整单的采购单合并后按日分组 ⇒ 同日 2 张 ⇒ 两个主体都被判不满足
//	⇒ **正常付款被卡死**。用户口径③是「逐个评估」⇒ 每个主体只看自己那份。
//
// ⚠️ 判别力自检：把 sheetsOfSupplier 改成"返回全部采购单" ⇒ 本用例立刻红。
func TestDecideAuditFlow_MultiSupplierSameDayNotFalselyRejected(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newFlowTestService(t)
	ap := &model.ApprovalInstance{ID: "ap-1", BranchID: "B1", CreatedAt: fixedNow}
	settlements := []SettlementRow{
		{ID: "S1", SupplierID: "SUP1"},
		{ID: "S2", SupplierID: "SUP2"},
	}
	for _, sid := range []string{"SUP1", "SUP2"} {
		if _, err := svc.CreateSupplierFlow(ctx, "B1", "op", SupplierFlowInput{
			SupplierID: sid, Mode: model.FlowModeLogic, LogicKind: model.LogicDailySingle, Enabled: true,
		}); err != nil {
			t.Fatalf("建 %s 流程: %v", sid, err)
		}
	}
	lines := map[string][]SettlementLineRow{
		"S1": {{SettlementID: "S1", VoucherID: "P1", SourceDocType: "PI"}},
		"S2": {{SettlementID: "S2", VoucherID: "P2", SourceDocType: "PI"}},
	}
	sheets := []PurchaseSheetRow{
		{ID: "P1", SupplierID: "SUP1", CreatedAt: "2026-09-01"},
		{ID: "P2", SupplierID: "SUP2", CreatedAt: "2026-09-01"},
	}
	dec, err := svc.decideAuditFlow(ctx, ap, settlements, lines, sheets, nil)
	if err != nil {
		t.Fatalf("decideAuditFlow: %v", err)
	}
	if !dec.Applied {
		t.Fatal("应命中配置")
	}
	if len(dec.Subjects) != 2 {
		t.Fatalf("应逐个评估 2 个供应商, got %d", len(dec.Subjects))
	}
	for _, sub := range dec.Subjects {
		if !sub.Satisfied {
			t.Errorf("[%s] 各 1 张采购单不该判不满足: %s", sub.ID, sub.Reason)
		}
	}
	if strings.Contains(dec.Reason, "不通过") {
		t.Errorf("整体应通过, got %q", dec.Reason)
	}
}

// TestDecideVerdict_ConfiguredFlowDoesNotCallLLM 是本模块**最重要**的一条。
//
// 命中配置 ⇒ **不调 LLM**（这是"流程定制"的意义）。
// 判别力:把 decideVerdict 里的 `if !dec.Applied` 判断去掉 ⇒ 本用例立刻红
// （verdictFn 会被调用 ⇒ called=true）。
func TestDecideVerdict_ConfiguredFlowDoesNotCallLLM(t *testing.T) {
	ctx := context.Background()
	svc, db, _, _ := newFlowTestService(t)
	ap := &model.ApprovalInstance{ID: "ap-1", BranchID: "B1", CreatedAt: fixedNow}
	if err := db.Create(ap).Error; err != nil {
		t.Fatalf("建审批: %v", err)
	}
	if _, err := svc.CreateSupplierFlow(ctx, "B1", "op", SupplierFlowInput{
		SupplierID: "SUP1", Mode: model.FlowModeAutoPass, Enabled: true,
	}); err != nil {
		t.Fatalf("建供应商流程: %v", err)
	}
	called := false
	svc.SetVerdictFn(func(_ context.Context, _ *AuditInput) (*VerdictResult, error) {
		called = true
		return nil, fmt.Errorf("命中配置时不该调 LLM")
	})
	settlements := []SettlementRow{{ID: "S1", SupplierID: "SUP1", TotalGoodsYuan: dec("100")}}
	v, err := svc.decideVerdict(ctx, ap, settlements, nil, nil, nil, nil, nil, Allowance{})
	if err != nil {
		t.Fatalf("decideVerdict: %v", err)
	}
	if called {
		t.Fatal("命中配置却仍调了 LLM")
	}
	if v.Source != model.VerdictSourceFlowConfig {
		t.Errorf("结论来源 = %q, want %q", v.Source, model.VerdictSourceFlowConfig)
	}
	if v.Conclusion != ConclusionPass {
		t.Errorf("结论 = %q, want pass", v.Conclusion)
	}
}

// TestDecideVerdict_UnconfiguredFallsBackToLLM 锁住「没配置 ⇒ 走机器人审核流程」。
//
// ⚠️ 这是**绝大多数单子**的路径 ⇒ 行为必须与改造前一致。
func TestDecideVerdict_UnconfiguredFallsBackToLLM(t *testing.T) {
	ctx := context.Background()
	svc, db, _, _ := newFlowTestService(t)
	ap := &model.ApprovalInstance{ID: "ap-1", BranchID: "B1", CreatedAt: fixedNow}
	if err := db.Create(ap).Error; err != nil {
		t.Fatalf("建审批: %v", err)
	}
	called := false
	svc.SetVerdictFn(func(_ context.Context, in *AuditInput) (*VerdictResult, error) {
		called = true
		return &VerdictResult{
			Conclusion: ConclusionPass, Reason: "机器审核通过:测试",
			Confidence: dec("1"), Source: model.VerdictSourceLLM,
		}, nil
	})
	settlements := []SettlementRow{{ID: "S1", SupplierID: "SUP1", TotalGoodsYuan: dec("100")}}
	v, err := svc.decideVerdict(ctx, ap, settlements, nil, nil, nil, nil, nil, Allowance{})
	if err != nil {
		t.Fatalf("decideVerdict: %v", err)
	}
	if !called {
		t.Fatal("未配置时应走 LLM 研判")
	}
	if v.Source != model.VerdictSourceLLM {
		t.Errorf("结论来源 = %q, want %q", v.Source, model.VerdictSourceLLM)
	}
}

// TestBuildFlowVerdict_DataBlockStillStops 锁住「数据正确性硬拦」。
//
// 流程配置说"直接通过"，但这一单金额勾稽不符 ⇒ 仍然拒绝。
func TestBuildFlowVerdict_DataBlockStillStops(t *testing.T) {
	svc, _, _, _ := newFlowTestService(t)
	dec := &FlowDecision{Applied: true, Path: FlowPathSupplier, Reason: "流程定制判定通过:xxx"}
	signals := []signalDraft{
		newSignal(model.SignalBlock, SigManualAmountMismatch, "", "",
			"手工累加付款金额 100 元 与应付金额 120 元不符", nil),
	}
	v := svc.buildFlowVerdict(dec, signals)
	if v.Conclusion != ConclusionReject {
		t.Fatalf("结论 = %q, want reject(数据正确性硬拦)", v.Conclusion)
	}
	if !strings.Contains(v.Reason, "数据校验阻断") {
		t.Errorf("理由应说明被数据校验阻断: %q", v.Reason)
	}
}

// TestBuildFlowVerdict_QtySignalDoesNotStop 数量类信号**不**硬拦。
//
// 数量偏离是**业务判断**（进了货没卖可能正常）⇒ 归机器人/流程裁决。
func TestBuildFlowVerdict_QtySignalDoesNotStop(t *testing.T) {
	svc, _, _, _ := newFlowTestService(t)
	dec := &FlowDecision{Applied: true, Path: FlowPathSupplier, Reason: "流程定制判定通过:xxx"}
	signals := []signalDraft{
		newSignal(model.SignalBlock, SigPurchaseWithoutSales, "", "P1",
			"商品 P1 自 2026-09-01 采购入库后无销售记录", nil),
	}
	v := svc.buildFlowVerdict(dec, signals)
	if v.Conclusion != ConclusionPass {
		t.Fatalf("结论 = %q, want pass(数量类信号不硬拦)", v.Conclusion)
	}
}

// TestBuildFlowVerdict_NoStocktakeSuggestion 锁住「流程定制不触发盘点联动」。
//
// 2026-10-28 用户口径：「按流程逻辑通过的不需要触发盘点联动」。
//
// ⚠️ 判别力自检：把 buildFlowVerdict 里的 SuggestStocktake 改回
//    stocktakeSuggestionFromSignals(signals) ⇒ 本用例立刻红。
//
// 为什么：命中流程定制 ⇒ 这一单**根本没问机器人** ⇒ 「盘点联动」是机器人
// 研判的副产物，无中生有地建一个盘点计划 = 让人去盘一台机器没怀疑过的货。
func TestBuildFlowVerdict_NoStocktakeSuggestion(t *testing.T) {
	svc, _, _, _ := newFlowTestService(t)
	dec := &FlowDecision{Applied: true, Path: FlowPathSupplier, Reason: "流程定制判定通过:xxx"}
	// 故意给一条**会**产出盘点建议的数量类信号。
	signals := []signalDraft{
		newSignal(model.SignalBlock, SigPurchaseWithoutSales, "", "P1",
			"商品 P1 自 2026-09-01 采购入库后无销售记录", nil),
	}
	v := svc.buildFlowVerdict(dec, signals)
	if v.SuggestStocktake != nil {
		t.Fatalf("流程定制结论不该带盘点建议(会触发盘点联动): %+v", v.SuggestStocktake)
	}
	// raw JSON 里也不该再宣称 suggest_stocktake。
	if strings.Contains(string(v.RawJSON), "suggest_stocktake") {
		t.Errorf("raw 里不该再出现 suggest_stocktake: %s", string(v.RawJSON))
	}
}

// ---- 配置 CRUD ----

func TestSupplierFlowCRUD(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newFlowTestService(t)

	row, err := svc.CreateSupplierFlow(ctx, "B1", "op", SupplierFlowInput{
		SupplierID: "SUP1", SupplierName: "某供应商", Mode: model.FlowModeLogic,
		LogicKind: model.LogicDailySingle, Enabled: true, Note: "每日一单",
	})
	if err != nil {
		t.Fatalf("新建: %v", err)
	}
	if row.ID == "" {
		t.Fatal("新建后应有 id")
	}
	// 同一 (branch, supplier) 重复建 ⇒ 已存在。
	if _, err := svc.CreateSupplierFlow(ctx, "B1", "op", SupplierFlowInput{
		SupplierID: "SUP1", Mode: model.FlowModeAutoPass, Enabled: true,
	}); err == nil {
		t.Fatal("重复建应报错")
	}
	// 另一个门店可以配同一个供应商。
	if _, err := svc.CreateSupplierFlow(ctx, "B2", "op", SupplierFlowInput{
		SupplierID: "SUP1", Mode: model.FlowModeAutoPass, Enabled: true,
	}); err != nil {
		t.Fatalf("换门店应能建: %v", err)
	}
	rows, err := svc.ListSupplierFlows(ctx, "B1")
	if err != nil {
		t.Fatalf("列表: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("B1 应只有 1 条, got %d", len(rows))
	}
	up, err := svc.UpdateSupplierFlow(ctx, "B1", row.ID, "op2", SupplierFlowInput{
		SupplierID: "SUP1", Mode: model.FlowModeAutoPass, Enabled: false, Note: "停了",
	})
	if err != nil {
		t.Fatalf("修改: %v", err)
	}
	if up.Mode != model.FlowModeAutoPass || up.Enabled || up.UpdatedBy != "op2" {
		t.Errorf("修改后 = %+v", up)
	}
	if err := svc.DeleteSupplierFlow(ctx, "B1", row.ID); err != nil {
		t.Fatalf("删除: %v", err)
	}
	if err := svc.DeleteSupplierFlow(ctx, "B1", row.ID); err == nil {
		t.Fatal("删不存在应报错")
	}
}

// TestNormalizeFlowMode_LogicWhitelist 锁住「逻辑条件按分支白名单」。
//
// ⚠️ 把 monthly_single 配到供应商上 ⇒ 必须当场报错（否则会按错条件放款）。
func TestNormalizeFlowMode_LogicWhitelist(t *testing.T) {
	if _, _, err := normalizeFlowMode(model.FlowModeLogic, model.LogicMonthlySingle,
		[]string{model.LogicDailySingle}); err == nil {
		t.Fatal("供应商分支不该接受 monthly_single")
	}
	if _, _, err := normalizeFlowMode(model.FlowModeLogic, model.LogicDailySingle,
		[]string{model.LogicDailySingle}); err != nil {
		t.Fatalf("daily_single 应被接受: %v", err)
	}
	if _, _, err := normalizeFlowMode(model.FlowModeLogic, "",
		[]string{model.LogicDailySingle}); err == nil {
		t.Fatal("mode=logic 必须给 logic_kind")
	}
	m, lg, err := normalizeFlowMode(model.FlowModeAutoPass, model.LogicDailySingle,
		[]string{model.LogicDailySingle})
	if err != nil {
		t.Fatalf("auto_pass: %v", err)
	}
	if m != model.FlowModeAutoPass || lg != "" {
		t.Errorf("auto_pass 应清掉 logic_kind, got (%q,%q)", m, lg)
	}
	if _, _, err := normalizeFlowMode("xxx", "", []string{model.LogicDailySingle}); err == nil {
		t.Fatal("未知 mode 应报错")
	}
}

// TestCreateRegexPattern_RejectsInvalidPattern 非法正则**存之前**就报错。
func TestCreateRegexPattern_RejectsInvalidPattern(t *testing.T) {
	svc, _, _, _ := newFlowTestService(t)
	if _, err := svc.CreateRegexPattern(context.Background(), "B1", "op", RegexPatternInput{
		Name: "坏的", Pattern: "([", GroupIndex: 1, Enabled: true,
	}); err == nil {
		t.Fatal("非法正则应被拒")
	}
}
