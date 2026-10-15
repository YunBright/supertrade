package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
	"gorm.io/gorm"
)

// 本文件覆盖付款台账（settlement_pay_states）与"过滤已结算结算单"。
//
// ## 这个功能最容易假绿的三处
//
//  1. **"过滤已付款"只挡新增不挡已存在**。发起页按筛选条件拉一张列表就返回，
//     看起来过滤生效了；但初始化页把同一批单标成已付款后再拉，那张单**必须消失**。
//     只测"新拉的干净列表"永远绿。
//  2. **两个设置方式互相顶掉**。初始化补录写 init、审批通过写 approval，
//     两边都是 upsert。少写一条 DoUpdates 的判据,就会出现"审批把历史补录的单
//     记成自己付的",审计字段从此不可信 —— 而功能看起来完全正常。
//  3. **集合不完整还照写**。cube 一次最多回 5000 行,触顶时"全选"其实只覆盖
//     了一部分。页面显示"影响 800 张"、实际写 500 张,剩下 300 张静默消失。

// seedSettlementRows 往 fake cube 铺一批结算单。
func seedSettlementRows(t *testing.T, cube *fakeCube, rows ...SettlementRow) {
	t.Helper()
	cube.mu.Lock()
	defer cube.mu.Unlock()
	cube.settlements = append(cube.settlements, rows...)
}

func row(id, sup, docType, amount string) SettlementRow {
	return SettlementRow{
		ID: id, SupplierID: sup, DocumentType: docType, ApproveStatus: "1",
		PeriodStart: "2026-09-01", PeriodEnd: "2026-09-30",
		TotalGoodsYuan: dec(amount),
	}
}

// seedPayState 直接写一行台账(模拟"别人已经标过了")。
func seedPayState(t *testing.T, db *gorm.DB, branchID, sid, status, method, operator string) {
	t.Helper()
	now := fixedNow
	err := db.Create(&model.SettlementPayState{
		ID: sid + "-pay", BranchID: branchID, SettlementID: sid,
		SupplierID: "S1", DocumentType: "DP",
		PaidStatus: status, SetMethod: method,
		OperatorID: operator, GoodsAmountYuan: dec("100.00"),
		CreatedAt: now, UpdatedAt: now,
	}).Error
	if err != nil {
		t.Fatalf("seed pay state %s: %v", sid, err)
	}
}

func payStateOf(t *testing.T, db *gorm.DB, branchID, sid string) model.SettlementPayState {
	t.Helper()
	var st model.SettlementPayState
	if err := db.Where("branch_id = ? AND settlement_id = ?", branchID, sid).
		First(&st).Error; err != nil {
		t.Fatalf("读台账 %s: %v", sid, err)
	}
	return st
}

var admin = PayStateOperator{UserID: "u-admin", Name: "财务老王"}

// ---- 集合解析 ----

// TestResolveSettlementSet_Filters —— 筛选条件真的收窄了集合。
//
// 三种收窄各测一次:document_type 精确、query 包含(单号/供应商各一次)。
// cube 的 /query 只有精确算子,包含匹配在 Go 侧做,这条单测就是那道 Go 侧逻辑
// 唯一的护栏 —— 它写错时 SQL 编译与单测都不会响,只有店长会找不到单。
func TestResolveSettlementSet_Filters(t *testing.T) {
	svc, _, cube, _, _ := newTestService(t)
	seedSettlementRows(t, cube,
		row("DP0001", "5011", "DP", "100.00"),
		row("CP0002", "5012", "CP", "200.00"),
		row("DP0003", "5011", "DP", "300.00"),
	)
	ctx := context.Background()

	set, err := svc.ResolveSettlementSet(ctx, "B1", SettlementFilter{DocumentType: "DP"})
	if err != nil {
		t.Fatalf("document_type=DP: %v", err)
	}
	if len(set.Rows) != 2 {
		t.Fatalf("document_type=DP 应命中 2 张,实际 %d: %+v", len(set.Rows), set.Rows)
	}

	set, err = svc.ResolveSettlementSet(ctx, "B1", SettlementFilter{Query: "0003"})
	if err != nil {
		t.Fatalf("query=0003: %v", err)
	}
	if len(set.Rows) != 1 || set.Rows[0].ID != "DP0003" {
		t.Fatalf("query=0003 应命中 DP0003,实际 %+v", set.Rows)
	}

	// 关键字命中供应商也要能查到(店长往往只记得供应商)。
	set, err = svc.ResolveSettlementSet(ctx, "B1", SettlementFilter{Query: "5012"})
	if err != nil {
		t.Fatalf("query=5012: %v", err)
	}
	if len(set.Rows) != 1 || set.Rows[0].ID != "CP0002" {
		t.Fatalf("query=5012 应命中 CP0002,实际 %+v", set.Rows)
	}
}

// TestResolveSettlementSet_RejectsBadFilter —— 非法取值必须显式报错。
//
// 静默降级成"不限"比报错危险得多:pay_status 拼错一个字母,
// 页面会变成"把全部门店所有单都标成已付款",而用户以为自己筛的是未付款。
func TestResolveSettlementSet_RejectsBadFilter(t *testing.T) {
	svc, _, cube, _, _ := newTestService(t)
	seedSettlementRows(t, cube, row("DP0001", "S1", "DP", "10.00"))

	for name, f := range map[string]SettlementFilter{
		"pay_status":     {PayStatus: "unpadi"},
		"document_type":  {DocumentType: "XX"},
		"from":           {From: "20261001"},
		"negative_limit": {Limit: -1},
	} {
		if _, err := svc.ResolveSettlementSet(context.Background(), "B1", f); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s 非法取值应返 ErrInvalidInput,实际 %v", name, err)
		}
	}
}

// TestResolveSettlementSet_TruncatedWhenFetchHitsCap —— 触到拉取上限必须自报。
//
// cube 一次最多回 cubeSettlementFetchCap 行,回满即意味着"可能还有"。
// 这条用例铺满上限行、再用关键字把命中收窄到 3 张 —— 证明即使**过滤后**很小,
// 只要源侧取数触了顶,就必须标 truncated(否则执行端会把 3 张当全集)。
func TestResolveSettlementSet_TruncatedWhenFetchHitsCap(t *testing.T) {
	svc, _, cube, _, _ := newTestService(t)
	rows := make([]SettlementRow, 0, cubeSettlementFetchCap+3)
	for i := 0; i < cubeSettlementFetchCap; i++ {
		rows = append(rows, row(fmt.Sprintf("DP%07d", i), "S1", "DP", "1.00"))
	}
	rows = append(rows,
		row("TARGET-A", "S1", "DP", "10.00"),
		row("TARGET-B", "S1", "DP", "20.00"),
		row("TARGET-C", "S1", "DP", "30.00"),
	)
	seedSettlementRows(t, cube, rows...)

	set, err := svc.ResolveSettlementSet(context.Background(), "B1", SettlementFilter{Query: "TARGET-"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(set.Rows) != 3 {
		t.Fatalf("关键字应命中 3 张,实际 %d", len(set.Rows))
	}
	if !set.Truncated {
		t.Fatal("源侧取数已触上限(>=5000 行),Truncated 必须为 true —— " +
			"否则执行端会把过滤后的 3 张当成全集,其余单静默漏掉")
	}
}

// TestResolveSettlementSet_IdsIntersectAndReportUnmatched —— 勾选语义的两条断言。
//
// a) 勾选是**交集**:给了 id 列表就只保留其中命中的,不是替换整个集合。
// b) 勾了但查不到的必须报数:客户端勾了 800 张、实际只处理 760 张,
//
//	剩下 40 张既没标记也没跳过,不报出来就是静默丢数据。
func TestResolveSettlementSet_IdsIntersectAndReportUnmatched(t *testing.T) {
	svc, _, cube, _, _ := newTestService(t)
	seedSettlementRows(t, cube,
		row("DP1", "S1", "DP", "100.00"),
		row("DP2", "S2", "DP", "200.00"),
		row("DP3", "S3", "DP", "300.00"),
	)

	set, err := svc.ResolveSettlementSet(context.Background(), "B1",
		SettlementFilter{Ids: []string{"DP1", "DP3"}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(set.Rows) != 2 {
		t.Fatalf("勾选 DP1/DP3 应命中 2 张,实际 %d: %+v", len(set.Rows), set.Rows)
	}
	if set.Requested != 2 || set.Unmatched != 0 {
		t.Errorf("两张都命中时 requested=2 unmatched=0,实际 %d/%d", set.Requested, set.Unmatched)
	}

	// 勾了一个不存在的单号。
	set, err = svc.ResolveSettlementSet(context.Background(), "B1",
		SettlementFilter{Ids: []string{"DP1", "DP-NOPE", "  ", "DP1"}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(set.Rows) != 1 {
		t.Fatalf("去重后应只命中 DP1,实际 %d: %+v", len(set.Rows), set.Rows)
	}
	if set.Requested != 2 {
		t.Errorf("去重后 requested 应为 2,实际 %d", set.Requested)
	}
	if set.Unmatched != 1 {
		t.Errorf("查不到的 DP-NOPE 必须计入 unmatched,实际 %d —— 不报就是静默丢单", set.Unmatched)
	}
}

// TestApplySettlementInit_AppliesOnlyTicked —— 勾选模式下只改勾过的那几张。
//
// 这是「复选框不是装饰」的判据:同一个筛选条件下,传 ids 与不传 ids
// 落库的行数必须不同。少写这条,把 ids 忽略掉也能全绿。
func TestApplySettlementInit_AppliesOnlyTicked(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	seedSettlementRows(t, cube,
		row("DP1", "S1", "DP", "100.00"),
		row("DP2", "S2", "DP", "200.00"),
		row("DP3", "S3", "DP", "300.00"),
	)

	res, err := svc.ApplySettlementInit(context.Background(), "B1",
		SettlementFilter{Ids: []string{"DP2"}}, admin, "")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Changed != 1 || res.Matched != 1 {
		t.Fatalf("只应改 1 张,实际 matched=%d changed=%d", res.Matched, res.Changed)
	}
	if res.TotalGoodsYuan != "200.00" {
		t.Errorf("合计应为勾中那张的 200.00,实际 %s", res.TotalGoodsYuan)
	}
	var n int64
	if err := db.Model(&model.SettlementPayState{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("台账只该有 1 行,实际 %d —— 没勾的单被一起改了", n)
	}
}

// ---- 预览 ----

// TestPreviewSettlementInit_CountsAndSplits —— 预览要把"会生效多少/被什么挡住"讲清。
func TestPreviewSettlementInit_CountsAndSplits(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	seedSettlementRows(t, cube,
		row("DP1", "S1", "DP", "100.00"),
		row("DP2", "S2", "DP", "200.00"),
		row("DP3", "S3", "DP", "300.00"),
	)
	seedPayState(t, db, "B1", "DP1", model.PayStatusPaid, model.PayMethodInit, "u-x")
	seedPayState(t, db, "B1", "DP2", model.PayStatusPaid, model.PayMethodApproval, "u-y")
	ctx := context.Background()

	pv, err := svc.PreviewSettlementInit(ctx, "B1", SettlementFilter{PayStatus: model.PayStatusUnpaid})
	if err != nil {
		t.Fatalf("preview unpaid: %v", err)
	}
	if pv.Count != 1 || pv.TotalGoodsYuan != "300.00" {
		t.Fatalf("未付款集合应只剩 DP3(300.00),实际 count=%d total=%s", pv.Count, pv.TotalGoodsYuan)
	}
	if pv.AlreadyPaid != 0 {
		t.Errorf("未付款集合里 AlreadyPaid 应为 0,实际 %d", pv.AlreadyPaid)
	}

	pv, err = svc.PreviewSettlementInit(ctx, "B1", SettlementFilter{PayStatus: model.PayStatusPaid})
	if err != nil {
		t.Fatalf("preview paid: %v", err)
	}
	if pv.Count != 2 || pv.TotalGoodsYuan != "300.00" {
		t.Fatalf("已付款集合应是 DP1+DP2=300.00,实际 count=%d total=%s", pv.Count, pv.TotalGoodsYuan)
	}
	if pv.ByApproval != 1 {
		t.Errorf("ByApproval 应为 1(只有 DP2 走审批),实际 %d", pv.ByApproval)
	}
}

// ---- 初始化执行 ----

// TestApplySettlementInit_WritesInitMethodAndOperator —— 本功能的核心断言。
//
// 「设置方式=初始化、设置人=当前登录管理员」这两个字段是**服务端**写死的:
// 请求体里没有 set_method,也没有 operator。少写一条,审计字段就变成空,
// 而功能看起来完全正常。
func TestApplySettlementInit_WritesInitMethodAndOperator(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	seedSettlementRows(t, cube,
		row("DP1", "S1", "DP", "100.00"),
		row("DP2", "S2", "DP", "200.50"),
	)

	res, err := svc.ApplySettlementInit(context.Background(), "B1",
		SettlementFilter{}, admin, "上线前已付")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Changed != 2 || res.Matched != 2 {
		t.Fatalf("应改 2 张,实际 matched=%d changed=%d", res.Matched, res.Changed)
	}
	if res.TotalGoodsYuan != "300.50" {
		t.Errorf("合计金额应 300.50,实际 %s", res.TotalGoodsYuan)
	}
	for _, sid := range []string{"DP1", "DP2"} {
		st := payStateOf(t, db, "B1", sid)
		if st.SetMethod != model.PayMethodInit {
			t.Errorf("%s 设置方式应为 init,实际 %q", sid, st.SetMethod)
		}
		if st.OperatorID != admin.UserID || st.OperatorName != admin.Name {
			t.Errorf("%s 设置人应为当前登录管理员 %s/%s,实际 %s/%s",
				sid, admin.UserID, admin.Name, st.OperatorID, st.OperatorName)
		}
		if !st.IsPaid() || st.ApprovalID != "" {
			t.Errorf("%s 应为 paid 且不挂审批,实际 paid=%v approval_id=%q",
				sid, st.IsPaid(), st.ApprovalID)
		}
	}
}

// TestApplySettlementInit_IsIdempotentAndKeepsMethod —— 重复执行不能改写来源。
//
// 同一批单点两次:第二次 changed=0(幂等),并且**原来的审批来源不被顶成 init** ——
// 否则"这张单是走流程付的还是有人补的"从此不可知。
func TestApplySettlementInit_IsIdempotentAndKeepsMethod(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	seedSettlementRows(t, cube, row("DP1", "S1", "DP", "100.00"))
	seedPayState(t, db, "B1", "DP1", model.PayStatusPaid, model.PayMethodApproval, "u-flow")
	ctx := context.Background()

	res, err := svc.ApplySettlementInit(ctx, "B1", SettlementFilter{}, admin, "")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Changed != 0 || res.AlreadyPaid != 1 || res.ByApproval != 1 {
		t.Fatalf("审批付过的单应被跳过,实际 changed=%d already_paid=%d by_approval=%d",
			res.Changed, res.AlreadyPaid, res.ByApproval)
	}
	st := payStateOf(t, db, "B1", "DP1")
	if st.SetMethod != model.PayMethodApproval || st.OperatorID != "u-flow" {
		t.Errorf("重复执行不得改写设置方式/设置人,实际 %s/%s", st.SetMethod, st.OperatorID)
	}
}

// TestApplySettlementInit_RejectsTruncated —— 集合不完整拒绝执行。
//
// 这是"全选"语义的底线:预览说 N 张,执行就得写 N 张。
// 触顶时写一半,剩下的一半既没被标记也没被跳过,事后对不上账。
func TestApplySettlementInit_RejectsTruncated(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	rows := make([]SettlementRow, 0, cubeSettlementFetchCap+1)
	for i := 0; i < cubeSettlementFetchCap; i++ {
		rows = append(rows, row(fmt.Sprintf("DP%07d", i), "S1", "DP", "1.00"))
	}
	rows = append(rows, row("TARGET-A", "S1", "DP", "10.00"))
	seedSettlementRows(t, cube, rows...)

	_, err := svc.ApplySettlementInit(context.Background(), "B1",
		SettlementFilter{Query: "TARGET-"}, admin, "")
	if !errors.Is(err, ErrResultTruncated) {
		t.Fatalf("源侧触顶时必须拒执行,实际 %v", err)
	}
	var n int64
	if err := db.Model(&model.SettlementPayState{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("拒执行时不得写入任何台账行,实际 %d 行", n)
	}
}

// TestApplySettlementInit_RequiresOperator —— 没有设置人就拒。
//
// 否则台账里会出现"谁付的"为空的行,而这正是这个表存在的理由。
func TestApplySettlementInit_RequiresOperator(t *testing.T) {
	svc, _, cube, _, _ := newTestService(t)
	seedSettlementRows(t, cube, row("DP1", "S1", "DP", "10.00"))

	if _, err := svc.ApplySettlementInit(context.Background(), "B1",
		SettlementFilter{}, PayStateOperator{}, ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("缺设置人应返 ErrInvalidInput,实际 %v", err)
	}
}

// TestRevertSettlementInit_OnlyRevertsInitRows —— 撤销只碰初始化补录的。
//
// 审批流程付的单必须原样保留;而初始化补录的那张,撤销后要能看到
// "谁补的"(原设置人保留)与"谁撤的"(reverted_by),不能被抹成一片空白。
func TestRevertSettlementInit_OnlyRevertsInitRows(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	seedSettlementRows(t, cube, row("DP1", "S1", "DP", "10.00"), row("DP2", "S2", "DP", "20.00"))
	seedPayState(t, db, "B1", "DP1", model.PayStatusPaid, model.PayMethodInit, admin.UserID)
	seedPayState(t, db, "B1", "DP2", model.PayStatusPaid, model.PayMethodApproval, "u-flow")

	res, err := svc.RevertSettlementInit(context.Background(), "B1", SettlementFilter{}, admin)
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if res.Changed != 1 || res.SkippedNonInit != 1 {
		t.Fatalf("应只撤销 1 张(DP1),实际 changed=%d skipped=%d", res.Changed, res.SkippedNonInit)
	}

	reverted := payStateOf(t, db, "B1", "DP1")
	if reverted.PaidStatus != model.PayStatusUnpaid {
		t.Errorf("DP1 撤销后应为 unpaid,实际 %s", reverted.PaidStatus)
	}
	if reverted.OperatorID != admin.UserID {
		t.Errorf("撤销必须保留原设置人,实际 %q", reverted.OperatorID)
	}
	if reverted.RevertedBy != admin.UserID || reverted.RevertedAt == nil {
		t.Errorf("撤销人/撤销时间必须留痕,实际 by=%q at=%v", reverted.RevertedBy, reverted.RevertedAt)
	}

	flow := payStateOf(t, db, "B1", "DP2")
	if flow.PaidStatus != model.PayStatusPaid || flow.SetMethod != model.PayMethodApproval {
		t.Errorf("审批付的 DP2 不该被撤销,实际 %s/%s", flow.PaidStatus, flow.SetMethod)
	}
}

// ---- 发起页过滤(Part A) ----

// TestListCubeSettlements_HidesPaidByDefault —— 「过滤已结算」这条需求本身。
//
// 判据分两步,缺一不可:
//
//	a) 标成已付款的**默认不出现**(includePaid=false);
//	b) includePaid=true 时又出现 —— 初始化页要看得见自己标过的东西。
//
// 只测 (a) 的话,把过滤写成"永远返回空"也能绿。
func TestListCubeSettlements_HidesPaidByDefault(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	seedSettlementRows(t, cube,
		row("DP1", "S1", "DP", "100.00"),
		row("DP2", "S2", "DP", "200.00"),
		row("DP3", "S3", "DP", "300.00"),
	)
	seedPayState(t, db, "B1", "DP2", model.PayStatusPaid, model.PayMethodInit, admin.UserID)
	ctx := context.Background()

	rows, hidden, err := svc.ListCubeSettlements(ctx, "B1", "", "", "", 200, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 || rows[0].ID == "DP2" || rows[1].ID == "DP2" {
		t.Fatalf("已付款的 DP2 不该出现,实际 %+v", rows)
	}
	if hidden != 1 {
		t.Errorf("hidden 应为 1(前端要据此说明「列表少了 N 张」),实际 %d", hidden)
	}

	rows, hidden, err = svc.ListCubeSettlements(ctx, "B1", "", "", "", 200, true)
	if err != nil {
		t.Fatalf("list includePaid: %v", err)
	}
	if len(rows) != 3 || hidden != 0 {
		t.Fatalf("include_paid=true 应返回全量 3 张且 hidden=0,实际 %d/%d", len(rows), hidden)
	}
}

// TestListCubeSettlements_RevertedBecomesSelectable —— 撤销后重新可勾选。
//
// 台账行还在、只是 paid_status 回到 unpaid。若过滤只按"行是否存在"判,
// 撤销就永远不生效 —— 这是最自然的一种写错方式,因为它在撤销测试里看不见。
func TestListCubeSettlements_RevertedBecomesSelectable(t *testing.T) {
	svc, _, cube, _, _ := newTestService(t)
	seedSettlementRows(t, cube, row("DP1", "S1", "DP", "100.00"))
	ctx := context.Background()

	if _, err := svc.ApplySettlementInit(ctx, "B1", SettlementFilter{}, admin, ""); err != nil {
		t.Fatalf("apply: %v", err)
	}
	rows, _, err := svc.ListCubeSettlements(ctx, "B1", "", "", "", 200, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("标记后应从勾选列表消失,实际仍有 %d 张", len(rows))
	}

	if _, err := svc.RevertSettlementInit(ctx, "B1", SettlementFilter{}, admin); err != nil {
		t.Fatalf("revert: %v", err)
	}
	rows, _, err = svc.ListCubeSettlements(ctx, "B1", "", "", "", 200, false)
	if err != nil {
		t.Fatalf("list after revert: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("撤销后应重新可勾选,实际 %d 张", len(rows))
	}
}

// ---- 审批侧写入 ----

// TestMarkApprovalSettlementsPaid_DoesNotOverwriteInitLedger —— 两个来源互不覆盖。
//
// 反向断言(approval 不能把 init 改掉)是这条功能的核心语义:
// 初始化记的是"上线前就付过的",审批记的是"这次流程付的"。
// 顶掉之后,那张单到底怎么付的就再也说不清了,而页面上看不出任何异常。
func TestMarkApprovalSettlementsPaid_DoesNotOverwriteInitLedger(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	now := fixedNow
	ap := &model.ApprovalInstance{
		ID: "AP1", BranchID: "B1", ApplicantUserID: "u-applicant",
		Status: model.StatusApproved, ApproverUserIDs: []byte("[]"), CcUserIDs: []byte("[]"),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(ap).Error; err != nil {
		t.Fatalf("seed approval: %v", err)
	}
	for _, r := range []model.ApprovalSettlement{
		{ID: "as1", ApprovalID: "AP1", SettlementID: "DP1", SupplierID: "S1",
			DocumentType: "DP", GoodsAmountYuan: dec("100.00"), CreatedAt: now},
		{ID: "as2", ApprovalID: "AP1", SettlementID: "DP2", SupplierID: "S2",
			DocumentType: "DP", GoodsAmountYuan: dec("200.00"), CreatedAt: now},
	} {
		if err := db.Create(&r).Error; err != nil {
			t.Fatalf("seed approval_settlement: %v", err)
		}
	}
	// DP1 之前已被初始化补录成已付款;DP2 是全新的。
	seedPayState(t, db, "B1", "DP1", model.PayStatusPaid, model.PayMethodInit, admin.UserID)

	svc.markApprovalSettlementsPaid(context.Background(), ap)

	// DP1:设置方式必须仍是 init(它不是这次流程付的)。
	if st := payStateOf(t, db, "B1", "DP1"); st.SetMethod != model.PayMethodInit || st.OperatorID != admin.UserID {
		t.Errorf("审批不得改写初始化补录的台账,实际 %s/%s", st.SetMethod, st.OperatorID)
	}
	// DP2:新写入 approval 并挂上审批号。
	st := payStateOf(t, db, "B1", "DP2")
	if st.SetMethod != model.PayMethodApproval || st.ApprovalID != "AP1" {
		t.Errorf("DP2 应记为 approval 并关联 AP1,实际 %s/%s", st.SetMethod, st.ApprovalID)
	}
	if st.OperatorID != "u-applicant" {
		t.Errorf("审批写入的设置人应为发起人,实际 %q", st.OperatorID)
	}
	if !st.IsPaid() {
		t.Error("审批付的单应为 paid")
	}
}

// TestOnStatusChanged_MarksPaidOnHumanApproved —— 端到端:回调触发写台账。
//
// 只测 markApprovalSettlementsPaid 是不够的 —— 那条钩子**接没接上**
// 只有从 OnStatusChanged 走一遍才知道。这是"函数被实现了但没被调用"那一族。
func TestOnStatusChanged_MarksPaidOnHumanApproved(t *testing.T) {
	svc, db, _, pub, _ := newTestService(t)
	now := fixedNow
	ap := &model.ApprovalInstance{
		ID: "AP-CB", BranchID: "B1", ApplicantUserID: "u-applicant",
		Status: model.StatusApproved, SpNo: "SP-1", SpStatus: SpStatusApproving,
		ApproverUserIDs: []byte("[]"), CcUserIDs: []byte("[]"),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(ap).Error; err != nil {
		t.Fatalf("seed approval: %v", err)
	}
	if err := db.Create(&model.ApprovalSettlement{
		ID: "as1", ApprovalID: "AP-CB", SettlementID: "DP9",
		SupplierID: "S1", DocumentType: "DP", GoodsAmountYuan: dec("88.00"), CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed approval_settlement: %v", err)
	}

	if err := svc.OnStatusChanged(context.Background(), &WecomStatusChanged{
		SpNo: "SP-1", SpStatus: SpStatusApproved,
	}); err != nil {
		t.Fatalf("OnStatusChanged: %v", err)
	}
	if pub.countTopic("settlement.audit.status") == 0 {
		t.Error("状态变更事件未发布")
	}
	st := payStateOf(t, db, "B1", "DP9")
	if !st.IsPaid() || st.SetMethod != model.PayMethodApproval {
		t.Fatalf("终审通过后应落 approval 台账,实际 %s/%s", st.PaidStatus, st.SetMethod)
	}
}

// TestOnStatusChanged_RejectedDoesNotMarkPaid —— 驳回不得写台账。
//
// 这条是上面那条的**反向断言**:只断"通过了会写"的话,把 if 条件写成
// `if newStatus != ""` 也照样绿。
func TestOnStatusChanged_RejectedDoesNotMarkPaid(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	now := fixedNow
	ap := &model.ApprovalInstance{
		ID: "AP-RJ", BranchID: "B1", ApplicantUserID: "u-applicant",
		Status: model.StatusApproved, SpNo: "SP-2", SpStatus: SpStatusApproving,
		ApproverUserIDs: []byte("[]"), CcUserIDs: []byte("[]"),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(ap).Error; err != nil {
		t.Fatalf("seed approval: %v", err)
	}
	if err := db.Create(&model.ApprovalSettlement{
		ID: "as1", ApprovalID: "AP-RJ", SettlementID: "DP9",
		SupplierID: "S1", DocumentType: "DP", GoodsAmountYuan: dec("88.00"), CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed approval_settlement: %v", err)
	}

	if err := svc.OnStatusChanged(context.Background(), &WecomStatusChanged{
		SpNo: "SP-2", SpStatus: SpStatusRejected,
	}); err != nil {
		t.Fatalf("OnStatusChanged: %v", err)
	}
	var n int64
	if err := db.Model(&model.SettlementPayState{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("驳回不得写付款台账,实际写了 %d 行", n)
	}
}

// ---- 纯函数 ----

func TestFilterOutPaidSettlements(t *testing.T) {
	rows := []SettlementRow{row("DP1", "S1", "DP", "1"), row("DP2", "S1", "DP", "2"), row("DP3", "S1", "DP", "3")}
	out, hidden := FilterOutPaidSettlements(rows, map[string]bool{"DP2": true})
	if len(out) != 2 || hidden != 1 {
		t.Fatalf("应剔除 1 张留 2 张,实际 %d/%d", len(out), hidden)
	}
	// 空台账时必须原样返回(不多复制、不改内容)。
	out, hidden = FilterOutPaidSettlements(rows, nil)
	if len(out) != 3 || hidden != 0 {
		t.Fatalf("台账为空时应原样返回,实际 %d/%d", len(out), hidden)
	}
}

func TestListPayStateRecords_FiltersByMethod(t *testing.T) {
	svc, db, _, _, _ := newTestService(t)
	seedPayState(t, db, "B1", "DP1", model.PayStatusPaid, model.PayMethodInit, "u1")
	seedPayState(t, db, "B1", "DP2", model.PayStatusPaid, model.PayMethodApproval, "u2")
	seedPayState(t, db, "B2", "DP3", model.PayStatusPaid, model.PayMethodInit, "u1")

	rows, total, err := svc.ListPayStateRecords(context.Background(), "B1", "init", "", "", 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].SettlementID != "DP1" {
		t.Fatalf("B1 的 init 记录应只剩 DP1,实际 total=%d rows=%+v", total, rows)
	}
}

var _ = time.Second // 保留 time 依赖(fixedNow 用)
