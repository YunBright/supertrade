// service/monthly_items_test.go —— 企微审批月度总表（计划 §9）的口径单测。
//
// 锁住四件事：
//  1. 备注里「所属年月」的解析（年份缺省 = 审批单年份）；
//  2. 付款状态映射（10 ⇒ paid，1/2/3/4/6 ⇒ unpaid）；
//  3. 写入即投影、重复写幂等；懒回填只回填一次；重建只重算派生列；
//  4. 历史统计读月度表且 fail-closed（解析不出月份的行让审核拒绝）。
package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// ---- 解析 ----

func TestParseYearMonth(t *testing.T) {
	cases := []struct {
		in      string
		year    int
		want    string
		wantOK  bool
		comment string
	}{
		{in: "2026年9月水电费", year: 2026, want: "2026-09", wantOK: true},
		{in: "2026-09 物业费", year: 2026, want: "2026-09", wantOK: true},
		{in: "2026/9", year: 2026, want: "2026-09", wantOK: true},
		{in: "9月水电费", year: 2026, want: "2026-09", wantOK: true, comment: "年份取审批单年份"},
		{in: "9月", year: 2025, want: "2025-09", wantOK: true, comment: "跨年补缴不落到错误年份"},
		{in: "9月", year: 0, wantOK: false, comment: "没有缺省年份就不猜"},
		{in: "水电费", wantOK: false},
		{in: "2026年13月", wantOK: false, comment: "13 月不是合法月份"},
	}
	for _, c := range cases {
		got, ok := parseYearMonth(c.in, c.year)
		if ok != c.wantOK {
			t.Errorf("parseYearMonth(%q, %d) ok = %v, want %v (%s)", c.in, c.year, ok, c.wantOK, c.comment)
			continue
		}
		if ok && got != c.want {
			t.Errorf("parseYearMonth(%q, %d) = %q, want %q", c.in, c.year, got, c.want)
		}
	}
}

func TestMonthlyPayState(t *testing.T) {
	cases := map[int]string{
		SpStatusApproved:      model.PayStatePaid,
		SpStatusPaid:          model.PayStatePaid,
		SpStatusApproving:     model.PayStateUnpaid,
		SpStatusRejected:      model.PayStateUnpaid,
		SpStatusCancelled:     model.PayStateUnpaid,
		SpStatusCancelAfterOK: model.PayStateUnpaid,
		0:                     "",
	}
	for in, want := range cases {
		if got := monthlyPayState(in); got != want {
			t.Errorf("monthlyPayState(%d) = %q, want %q", in, got, want)
		}
	}
}

// ---- 写入投影 ----

// seedInstance 造一条审批实例（核准年月缺省值用它的 CreatedAt.Year()）。
func seedInstance(t *testing.T, db *gorm.DB, id, status string, spStatus int, createdAt time.Time) {
	t.Helper()
	inst := &model.ApprovalInstance{
		ID: id, CorpID: "wcorp-test", BranchID: "B1",
		ApplicantUserID: "u1", Remark: "ut", Status: status, SpStatus: spStatus,
		ApproverUserIDs: []byte("[]"), CcUserIDs: []byte("[]"),
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	if err := db.Create(inst).Error; err != nil {
		t.Fatalf("seed instance %s: %v", id, err)
	}
}

// TestWriteMonthlyItemsTx_ProjectsAndIsIdempotent —— 写明细即投影；重写不新增行。
func TestWriteMonthlyItemsTx_ProjectsAndIsIdempotent(t *testing.T) {
	_, db, _, _ := newFlowTestService(t)
	now := fixedNow
	ap := &model.ApprovalInstance{
		ID: "AP-1", BranchID: "B1", Status: model.StatusAuditing,
		SpStatus: SpStatusApproving, CreatedAt: now,
	}
	items := []model.ApprovalItem{
		{
			ID: "I-1", ApprovalID: ap.ID, BranchID: "B1", Direction: ItemDirectionPayment,
			ItemType: PaymentTypeUtility, Remark: "9月水电费",
			AmountYuan: decimal.RequireFromString("100.00"), CreatedAt: now,
		},
		{
			ID: "I-2", ApprovalID: ap.ID, BranchID: "B1", Direction: ItemDirectionReceipt,
			ItemType: ReceiptTypeDisplay, Remark: "无日期",
			AmountYuan: decimal.RequireFromString("20.00"), CreatedAt: now,
		},
	}
	if err := writeMonthlyItemsTx(db, ap, items, now); err != nil {
		t.Fatalf("writeMonthlyItemsTx: %v", err)
	}

	var rows []model.ApprovalMonthlyItem
	if err := db.Where("approval_id = ?", ap.ID).Order("item_id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("read monthly rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("月度行数 = %d, want 2", len(rows))
	}
	if rows[0].YearMonth != "2026-09" || rows[0].ParseStatus != model.MonthlyParseOK {
		t.Errorf("I-1 = %q/%q, want 2026-09/ok", rows[0].YearMonth, rows[0].ParseStatus)
	}
	if rows[1].ParseStatus != model.MonthlyParseUnmatched {
		t.Errorf("I-2 parse_status = %q, want unmatched", rows[1].ParseStatus)
	}
	if rows[0].PayState != model.PayStateUnpaid {
		t.Errorf("审批中应 unpaid, got %q", rows[0].PayState)
	}

	// 重写同一批 ⇒ 仍 2 行（按 branch+approval+item upsert）。
	if err := writeMonthlyItemsTx(db, ap, items, now); err != nil {
		t.Fatalf("重写: %v", err)
	}
	var n int64
	if err := db.Model(&model.ApprovalMonthlyItem{}).Where("approval_id = ?", ap.ID).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("重写后行数 = %d, want 2（必须幂等）", n)
	}
}

// ---- 懒回填 ----

// TestEnsureMonthlyBackfill_RunsOnce —— 首次回填全部历史，第二次不再重复。
func TestEnsureMonthlyBackfill_RunsOnce(t *testing.T) {
	svc, db, _, _ := newFlowTestService(t)
	ctx := context.Background()
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seedInstance(t, db, "AP-H1", model.StatusHumanApproved, SpStatusApproved, created)
	seedInstance(t, db, "AP-H2", model.StatusRejected, SpStatusRejected, created)
	for _, it := range []*model.ApprovalItem{
		{ID: "H1-1", ApprovalID: "AP-H1", BranchID: "B1", Direction: ItemDirectionPayment,
			ItemType: PaymentTypeUtility, Remark: "9月水电费", AmountYuan: decimal.RequireFromString("10"), CreatedAt: created},
		{ID: "H2-1", ApprovalID: "AP-H2", BranchID: "B1", Direction: ItemDirectionPayment,
			ItemType: PaymentTypeUtility, Remark: "9月水电费", AmountYuan: decimal.RequireFromString("11"), CreatedAt: created},
	} {
		if err := db.Create(it).Error; err != nil {
			t.Fatalf("seed item: %v", err)
		}
	}

	if err := svc.EnsureMonthlyBackfill(ctx, "B1"); err != nil {
		t.Fatalf("EnsureMonthlyBackfill: %v", err)
	}
	var n int64
	if err := db.Model(&model.ApprovalMonthlyItem{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("回填行数 = %d, want 2", n)
	}
	var marker model.ApprovalMonthlyBackfill
	if err := db.First(&marker, "branch_id = ?", "B1").Error; err != nil {
		t.Fatalf("读回填标记: %v", err)
	}
	if marker.Version != model.MonthlyBackfillVersion {
		t.Errorf("标记版本 = %q, want %q", marker.Version, model.MonthlyBackfillVersion)
	}
	if marker.DoneAt.IsZero() {
		t.Errorf("DoneAt 应已写入")
	}

	// 第二次调用：标记已是最新版本 ⇒ 不再重跑（行数不变）。
	if err := svc.EnsureMonthlyBackfill(ctx, "B1"); err != nil {
		t.Fatalf("第二次 EnsureMonthlyBackfill: %v", err)
	}
	if err := db.Model(&model.ApprovalMonthlyItem{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("第二次回填后行数 = %d, want 2", n)
	}
}

// TestRebuildMonthlyItems_RecomputesDerivedColumns —— 只重算 year_month/parse_status。
func TestRebuildMonthlyItems_RecomputesDerivedColumns(t *testing.T) {
	svc, db, _, _ := newFlowTestService(t)
	ctx := context.Background()
	created := time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)
	seedInstance(t, db, "AP-R1", model.StatusHumanApproved, SpStatusPaid, created)
	seedInstance(t, db, "AP-R2", model.StatusHumanApproved, SpStatusPaid, created)
	// 月度行先以"未解析"落库（模拟旧口径/改正则前的状态）。
	for _, it := range []*model.ApprovalItem{
		{ID: "R1-1", ApprovalID: "AP-R1", BranchID: "B1", Direction: ItemDirectionPayment,
			ItemType: PaymentTypeUtility, Remark: "12月水电费", AmountYuan: decimal.RequireFromString("10"), CreatedAt: created},
		{ID: "R2-1", ApprovalID: "AP-R2", BranchID: "B1", Direction: ItemDirectionPayment,
			ItemType: PaymentTypeRent, Remark: "没有日期", AmountYuan: decimal.RequireFromString("10"), CreatedAt: created},
	} {
		if err := db.Create(it).Error; err != nil {
			t.Fatalf("seed item: %v", err)
		}
	}
	if err := svc.EnsureMonthlyBackfill(ctx, "B1"); err != nil {
		t.Fatalf("EnsureMonthlyBackfill: %v", err)
	}
	// 人为打歪派生列。
	if err := db.Model(&model.ApprovalMonthlyItem{}).
		Where("item_id IN ?", []string{"R1-1", "R2-1"}).
		Updates(map[string]any{"year_month": "", "parse_status": model.MonthlyParseUnmatched}).Error; err != nil {
		t.Fatalf("打歪: %v", err)
	}

	n, err := svc.RebuildMonthlyItems(ctx, "B1")
	if err != nil {
		t.Fatalf("RebuildMonthlyItems: %v", err)
	}
	if n != 2 {
		t.Fatalf("重建行数 = %d, want 2", n)
	}
	var r1 model.ApprovalMonthlyItem
	if err := db.First(&r1, "item_id = ?", "R1-1").Error; err != nil {
		t.Fatalf("read R1-1: %v", err)
	}
	if r1.YearMonth != "2025-12" || r1.ParseStatus != model.MonthlyParseOK {
		t.Errorf("R1-1 = %q/%q, want 2025-12/ok（缺省年份 = 审批单创建年）", r1.YearMonth, r1.ParseStatus)
	}
	if r1.PayState != model.PayStatePaid {
		t.Errorf("R1-1 pay_state = %q, want paid（重建不动审批状态）", r1.PayState)
	}
	var r2 model.ApprovalMonthlyItem
	if err := db.First(&r2, "item_id = ?", "R2-1").Error; err != nil {
		t.Fatalf("read R2-1: %v", err)
	}
	if r2.ParseStatus != model.MonthlyParseUnmatched {
		t.Errorf("R2-1 parse_status = %q, want unmatched", r2.ParseStatus)
	}
}

// ---- 历史统计 ----

// TestCountHistoricalMonthlyItems —— 读月度表，排除本单/已拒/别的类型，unmatched 让审核拒绝。
func TestCountHistoricalMonthlyItems(t *testing.T) {
	svc, db, _, _ := newFlowTestService(t)
	ctx := context.Background()
	now := fixedNow
	ap := &model.ApprovalInstance{ID: "AP-CUR", BranchID: "B1", Status: model.StatusAuditing, CreatedAt: now}

	rows := []model.ApprovalMonthlyItem{
		{ID: "M-1", BranchID: "B1", ApprovalID: "AP-OLD-OK", ItemID: "I-1", Direction: ItemDirectionPayment,
			ItemType: PaymentTypeUtility, Remark: "2026-09", YearMonth: "2026-09", ParseStatus: model.MonthlyParseOK,
			InstanceStatus: model.StatusHumanApproved, CreatedAt: now, UpdatedAt: now},
		{ID: "M-2", BranchID: "B1", ApprovalID: "AP-OLD-REJ", ItemID: "I-2", Direction: ItemDirectionPayment,
			ItemType: PaymentTypeUtility, Remark: "2026-09", YearMonth: "2026-09", ParseStatus: model.MonthlyParseOK,
			InstanceStatus: model.StatusRejected, CreatedAt: now, UpdatedAt: now},
		{ID: "M-3", BranchID: "B1", ApprovalID: "AP-CUR", ItemID: "I-3", Direction: ItemDirectionPayment,
			ItemType: PaymentTypeUtility, Remark: "2026-09", YearMonth: "2026-09", ParseStatus: model.MonthlyParseOK,
			InstanceStatus: model.StatusAuditing, CreatedAt: now, UpdatedAt: now},
		{ID: "M-4", BranchID: "B1", ApprovalID: "AP-OLD-RENT", ItemID: "I-4", Direction: ItemDirectionPayment,
			ItemType: PaymentTypeRent, Remark: "2026-09", YearMonth: "2026-09", ParseStatus: model.MonthlyParseOK,
			InstanceStatus: model.StatusHumanApproved, CreatedAt: now, UpdatedAt: now},
		{ID: "M-5", BranchID: "B1", ApprovalID: "AP-OLD-BAD", ItemID: "I-5", Direction: ItemDirectionPayment,
			ItemType: PaymentTypeUtility, Remark: "没写月份", ParseStatus: model.MonthlyParseUnmatched,
			InstanceStatus: model.StatusHumanApproved, CreatedAt: now, UpdatedAt: now},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed monthly row: %v", err)
		}
	}

	cfg := &model.AuditPaymentItemFlow{ItemType: PaymentTypeUtility}
	counts, unmatched, err := svc.countHistoricalMonthlyItems(ctx, ap, cfg)
	if err != nil {
		t.Fatalf("countHistoricalMonthlyItems: %v", err)
	}
	// 只算 AP-OLD-OK 那一笔：本单/已拒/别的类型都不算。
	if counts[yearMonth{Year: 2026, Month: 9}] != 1 {
		t.Errorf("2026-09 笔数 = %d, want 1", counts[yearMonth{Year: 2026, Month: 9}])
	}
	// 提不出月份的历史行 ⇒ 交给调用方拒绝（fail-closed）。
	if len(unmatched) != 1 || !containsStr(unmatched[0], "AP-OLD-BAD") {
		t.Errorf("unmatched = %v, want 含 AP-OLD-BAD", unmatched)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ---- 性能 ----

// TestCountHistoricalMonthlyItems_Perf100k —— 10 万行月度总表（含干扰类型）下
// 历史统计必须走索引、单次 < 200ms。
//
// # 这条用例锁什么
//
// 历史统计在**每次机器审核**都会跑。原来实时 JOIN approval_items +
// approval_instances 是 10 万行级 O(N) 全表扫，是审核流水线的主要延迟来源；
// 月度总表 + (branch_id, item_type, year_month) 索引把它降到 O(命中行)。
// 如果有人把查询改回 JOIN、或去掉了索引，这条用例会先红。
//
// 只计时**查询本身**：首次 EnsureMonthlyBackfill 是一次性成本，不计入。
func TestCountHistoricalMonthlyItems_Perf100k(t *testing.T) {
	svc, db, _, _ := newFlowTestService(t)
	ctx := context.Background()
	now := fixedNow

	const total = 100000
	const matched = 5000 // 本次要统计的付款类型行数
	rows := make([]model.ApprovalMonthlyItem, 0, total)
	for i := 0; i < total; i++ {
		itemType, yearMonth, remark := PaymentTypeRent, "2025-01", "2025年1月租金"
		if i < matched {
			// 命中行分散到 24 个月，覆盖索引的 year_month 前缀。
			itemType = PaymentTypeUtility
			month := i%12 + 1
			yearMonth = fmt.Sprintf("2026-%02d", month)
			remark = fmt.Sprintf("2026年%d月水电费", month)
		}
		rows = append(rows, model.ApprovalMonthlyItem{
			ID: fmt.Sprintf("M-%06d", i), BranchID: "B1",
			ApprovalID: fmt.Sprintf("AP-%06d", i), ItemID: fmt.Sprintf("I-%06d", i),
			Direction: ItemDirectionPayment, ItemType: itemType, Remark: remark,
			YearMonth: yearMonth, ParseStatus: model.MonthlyParseOK,
			InstanceStatus: model.StatusHumanApproved, CreatedAt: now, UpdatedAt: now,
		})
	}
	if err := db.CreateInBatches(&rows, 1000).Error; err != nil {
		t.Fatalf("seed monthly rows: %v", err)
	}

	ap := &model.ApprovalInstance{ID: "AP-CUR", BranchID: "B1", Status: model.StatusAuditing, CreatedAt: now}
	cfg := &model.AuditPaymentItemFlow{ItemType: PaymentTypeUtility}

	// 首次调用含懒回填（一次性），不计入阈值。
	if _, _, err := svc.countHistoricalMonthlyItems(ctx, ap, cfg); err != nil {
		t.Fatalf("warmup: %v", err)
	}

	start := time.Now()
	counts, _, err := svc.countHistoricalMonthlyItems(ctx, ap, cfg)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("countHistoricalMonthlyItems: %v", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("10 万行历史统计耗时 %v，超过 200ms 预算", elapsed)
	}
	got := 0
	for _, c := range counts {
		got += c
	}
	if got != matched {
		t.Fatalf("命中行数 = %d, want %d", got, matched)
	}
	t.Logf("10 万行月度总表：历史统计 %v", elapsed)
}
