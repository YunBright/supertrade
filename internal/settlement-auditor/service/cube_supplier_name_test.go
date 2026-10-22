package service

import (
	"context"
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

// 结算单行要显示 "<供应商id>-<供应商名称>" ⇒ 后端必须把供应商名补到
// SettlementRow.SupplierName 上。
//
// # 为什么名字要 Go 侧 join(cube 侧 JOIN 不了)
//
// settlement 是 **live** model(实时打思迅源库),supplier 是 **duck** model
// (同步落 DuckDB)。cube 的 JOIN 目前只支持 live↔live,跨 storage 明确报错
// (cube pkg/cubequery/join.go)。⇒ 名字只能在 Go 侧 join,
// 与 catalog 域取"商品→供应商名"同一套路。
//
// 所以本文件测的是**这个 join**,而不是"cube 有没有 supplier.name 列"。
//
// 用 newTestService(t)(带 sqlite 内存库):include_paid=false 那条路径要读
// 付款台账(SettlementPayState),没有库会 panic —— 那正是"看起来只是取个名字,
// 实际踩到台账表"的现实提醒。

// TestListCubeSettlements_FillsSupplierName 是主用例:字典里有名字 → 行上就有。
func TestListCubeSettlements_FillsSupplierName(t *testing.T) {
	svc, _, cube, _, _ := newTestService(t)
	cube.settlements = []SettlementRow{
		{ID: "DP001", SupplierID: "01"},
		{ID: "DP002", SupplierID: "02"},
	}
	cube.suppliers = map[string]string{
		"01": "岑溪市泰鼎粮油食品有限公司",
		"02": "广西梧州XX商贸",
	}

	rows, _, err := svc.ListCubeSettlements(context.Background(), "B1", "", "", "", 200, false)
	if err != nil {
		t.Fatalf("ListCubeSettlements: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("应有 2 行,got %d", len(rows))
	}
	if rows[0].SupplierName != "岑溪市泰鼎粮油食品有限公司" {
		t.Errorf("第 1 行供应商名 = %q,应为「岑溪市泰鼎粮油食品有限公司」", rows[0].SupplierName)
	}
	if rows[1].SupplierName != "广西梧州XX商贸" {
		t.Errorf("第 2 行供应商名 = %q,应为「广西梧州XX商贸」", rows[1].SupplierName)
	}
}

// TestListCubeSettlements_UnknownSupplierIDLeavesNameEmpty 字典里没有的
// 供应商 ⇒ 名字留空,**不许编**。
//
// 编造占位名("供应商01"之类)比留空更糟:页面上看着有名字,店长照着这个名字
// 去付款,付款对象却是错的。留空至少是"这里我没查到"。
func TestListCubeSettlements_UnknownSupplierIDLeavesNameEmpty(t *testing.T) {
	svc, _, cube, _, _ := newTestService(t)
	cube.settlements = []SettlementRow{{ID: "DP001", SupplierID: "99"}}
	cube.suppliers = map[string]string{"01": "某某公司"}

	rows, _, err := svc.ListCubeSettlements(context.Background(), "B1", "", "", "", 200, false)
	if err != nil {
		t.Fatalf("ListCubeSettlements: %v", err)
	}
	if rows[0].SupplierName != "" {
		t.Errorf("字典里没有的供应商应留空名字,got %q", rows[0].SupplierName)
	}
}

// TestListCubeSettlements_SupplierDictFailureStillReturnsRows 是**降级**用例。
//
// 供应商字典挂了 ⇒ 列表照样出(名字留空)。理由:结算单是主数据,供应商名是
// 显示层;让"名字查不到"升级成"整个列表 503",等于为了显示一行小字把
// 付款审批入口堵死,店长看到的还是"数据源不可用",猜不到真正挂的是字典。
func TestListCubeSettlements_SupplierDictFailureStillReturnsRows(t *testing.T) {
	svc, _, cube, _, _ := newTestService(t)
	cube.settlements = []SettlementRow{{ID: "DP001", SupplierID: "01"}}
	cube.supplierErr = errors.New("cube supplier query: boom")

	rows, _, err := svc.ListCubeSettlements(context.Background(), "B1", "", "", "", 200, false)
	if err != nil {
		t.Fatalf("字典失败不应让整个列表失败,got err = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应有 1 行,got %d", len(rows))
	}
	if rows[0].SupplierName != "" {
		t.Errorf("字典失败时名字应留空,got %q", rows[0].SupplierName)
	}
}

// TestFillSupplierNames_TrimsBothSides 钉住键的空白处理。
//
// 思迅的 sup_no / supcust_no 是 char 定长,源库带尾随空格("01 ")。
// cube 查询层已统一 RTRIM,但结算单这一侧(settlement.supplier_id)与字典
// 那一侧(supplier.id)是**两次独立的读取**——
// 只要有一侧没 trim,join 就静默失配:结果 200 + 名字全空,零报错。
func TestFillSupplierNames_TrimsBothSides(t *testing.T) {
	svc, _, cube, _, _ := newTestService(t)
	cube.settlements = []SettlementRow{{ID: "DP001", SupplierID: " 01 "}}
	cube.suppliers = map[string]string{"01": "某某公司"}

	rows, _, err := svc.ListCubeSettlements(context.Background(), "B1", "", "", "", 200, false)
	if err != nil {
		t.Fatalf("ListCubeSettlements: %v", err)
	}
	if rows[0].SupplierName != "某某公司" {
		t.Errorf("首尾空白的供应商号也应 join 上名字,got %q", rows[0].SupplierName)
	}
}

// TestListCubeSettlements_IncludePaidAlsoGetsNames 初始化结算页
// (include_paid=true)看的是**已付款**的那批单,同样要显示供应商名。
//
// 这条容易被漏:include_paid 分支在"剔除已付款"之前就 return 了,
// 补名字必须在这之前做,否则两页显示口径不一样(同一批单在审批页有名字、
// 在初始化结算页没名字)。
func TestListCubeSettlements_IncludePaidAlsoGetsNames(t *testing.T) {
	svc, _, cube, _, _ := newTestService(t)
	cube.settlements = []SettlementRow{{ID: "DP001", SupplierID: "01"}}
	cube.suppliers = map[string]string{"01": "某某公司"}

	rows, hidden, err := svc.ListCubeSettlements(context.Background(), "B1", "", "", "", 200, true)
	if err != nil {
		t.Fatalf("ListCubeSettlements: %v", err)
	}
	if hidden != 0 {
		t.Errorf("include_paid=true 时不应有被剔除的张数,got %d", hidden)
	}
	if len(rows) != 1 || rows[0].SupplierName != "某某公司" {
		t.Errorf("include_paid 路径也要补名字,got %#v", rows)
	}
}

// ---- 初始化结算的**服务端**关键字筛选 ----
//
// 初始化结算页的关键字是**服务端**筛的(ResolveSettlementSet),
// 与 wx-h5 发起页的客户端筛法不同。
// ⇒ 这里不补名字,财务在页面上按供应商**名称**搜就命中 0 张 ——
// 而服务端"命中 0 张"会被回显成"本次影响 0 张",看起来像"没有待补录的单",
// 实际是筛选条件没生效。静默且极难察觉。

// TestSettlementRowMatches_MatchesSupplierName 是纯函数判据。
func TestSettlementRowMatches_MatchesSupplierName(t *testing.T) {
	row := SettlementRow{
		ID:           "CP002610214499",
		SupplierID:   "029",
		SupplierName: "猪肉自营",
	}
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"CP0026", true}, // 单号
		{"029", true},    // 供应商编号
		{"猪肉", true},     // 供应商名称(中文片段)
		{"猪肉自营", true},   // 供应商名称全称
		{"猪", true},      // 名称片段
		{"PORK", false},  // 大小写:中文无意义,但英文名要小写化后匹配
		{"永裕", false},    // 别的供应商
		{"不存在的供应商", false},
	} {
		if got := settlementRowMatches(row, tc.query); got != tc.want {
			t.Errorf("settlementRowMatches(%q) = %v,应为 %v", tc.query, got, tc.want)
		}
	}
}

// TestSettlementRowMatches_EnglishNameIsCaseInsensitive 英文供应商名大小写不敏感。
func TestSettlementRowMatches_EnglishNameIsCaseInsensitive(t *testing.T) {
	row := SettlementRow{ID: "DP1", SupplierID: "S1", SupplierName: "Coca-Cola"}
	if !settlementRowMatches(row, "coca") {
		t.Errorf("英文供应商名应大小写不敏感,got 未命中")
	}
	if !settlementRowMatches(row, "COLA") {
		t.Errorf("英文供应商名应大小写不敏感,got 未命中")
	}
}

// TestResolveSettlementSet_FiltersBySupplierName 是集成判据:
// 按**名称**筛,服务端必须真的筛出那一张。
//
// 这条锁的是"名字在筛选之前被填上"这个次序 —— 补晚了的话,
// 名称这一路恒不命中,服务端返回 0 张,而且**不报错**。
func TestResolveSettlementSet_FiltersBySupplierName(t *testing.T) {
	svc, _, cube, _, _ := newTestService(t)
	cube.settlements = []SettlementRow{
		{ID: "DP001", SupplierID: "01"},
		{ID: "DP002", SupplierID: "02"},
	}
	cube.suppliers = map[string]string{
		"01": "岑溪市泰鼎粮油食品有限公司",
		"02": "广西梧州XX商贸",
	}

	set, err := svc.ResolveSettlementSet(context.Background(), "B1", SettlementFilter{Query: "泰鼎"})
	if err != nil {
		t.Fatalf("ResolveSettlementSet: %v", err)
	}
	if len(set.Rows) != 1 {
		t.Fatalf("按供应商名称应命中 1 张,got %d: %#v", len(set.Rows), set.Rows)
	}
	if set.Rows[0].ID != "DP001" {
		t.Errorf("命中的应是 DP001,got %q", set.Rows[0].ID)
	}
	// 返回给客户端的样本行也要带名字(前端要显示 "<id>-<name>")。
	if set.Rows[0].SupplierName != "岑溪市泰鼎粮油食品有限公司" {
		t.Errorf("返回行应带供应商名,got %q", set.Rows[0].SupplierName)
	}
}

// TestResolveSettlementSet_NameFilterFallsBackWhenDictUnavailable 字典挂了的降级:
// 按名称搜**匹配不到**,但不能因此让整个请求失败。
//
// 降级后的表现是"搜名字搜不到"(退回到只能按编号搜),而不是 503。
// 这与列表页的取舍一致(catalog/supplier 字典是旁路,不是主数据)。
func TestResolveSettlementSet_NameFilterFallsBackWhenDictUnavailable(t *testing.T) {
	svc, _, cube, _, _ := newTestService(t)
	cube.settlements = []SettlementRow{{ID: "DP001", SupplierID: "01"}}
	cube.supplierErr = errors.New("cube supplier query: boom")

	// 按编号仍然能筛出来(主路径不受字典影响)。
	set, err := svc.ResolveSettlementSet(context.Background(), "B1", SettlementFilter{Query: "01"})
	if err != nil {
		t.Fatalf("字典失败不应让整个解析失败,got err = %v", err)
	}
	if len(set.Rows) != 1 {
		t.Fatalf("按编号应命中 1 张,got %d", len(set.Rows))
	}
	if set.Rows[0].SupplierName != "" {
		t.Errorf("字典失败时名字应留空,got %q", set.Rows[0].SupplierName)
	}
}

// ---- 审批详情页的供应商名 ----
//
// 审批详情页要显示「<供应商id>-<供应商名称>」,与发起页/初始化结算页一致。
// 它的数据是 PG 里的**审批快照行**(approval_settlements),那一行上没有名字
// ⇒ 详情端点读出来时用字典补(见 fillApprovalSettlementNames)。

// TestGetApprovalDetail_FillsSupplierNames 是主用例:详情里的结算单行带名字。
func TestGetApprovalDetail_FillsSupplierNames(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	cube.settlements = []SettlementRow{
		{ID: "S1", SupplierID: "01", ApproveStatus: "1", TotalGoodsYuan: decimal.NewFromInt(100)},
	}
	cube.suppliers = map[string]string{"01": "岑溪市泰鼎粮油食品有限公司"}

	ap := submitOK(t, svc, "100")
	waitTerminal(t, db, ap.ID)

	detail, err := svc.GetApprovalDetail(context.Background(), "B1", ap.ID)
	if err != nil {
		t.Fatalf("GetApprovalDetail: %v", err)
	}
	if len(detail.Settlements) != 1 {
		t.Fatalf("详情里应有 1 张结算单,got %d", len(detail.Settlements))
	}
	if detail.Settlements[0].SupplierID != "01" {
		t.Errorf("供应商编号 = %q,应为 01", detail.Settlements[0].SupplierID)
	}
	if detail.Settlements[0].SupplierName != "岑溪市泰鼎粮油食品有限公司" {
		t.Errorf("供应商名 = %q,应为「岑溪市泰鼎粮油食品有限公司」", detail.Settlements[0].SupplierName)
	}
}

// TestGetApprovalDetail_SupplierDictUnavailableStillReturnsDetail 降级用例。
//
// 字典挂了 ⇒ 名字留空,但**详情照常返回**。详情页的主体是审批快照,
// 为了一个显示字段让整页 503 不划算(与列表页同一取舍)。
func TestGetApprovalDetail_SupplierDictUnavailableStillReturnsDetail(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	cube.settlements = []SettlementRow{
		{ID: "S1", SupplierID: "01", ApproveStatus: "1", TotalGoodsYuan: decimal.NewFromInt(100)},
	}
	cube.supplierErr = errors.New("cube supplier query: boom")

	ap := submitOK(t, svc, "100")
	waitTerminal(t, db, ap.ID)

	detail, err := svc.GetApprovalDetail(context.Background(), "B1", ap.ID)
	if err != nil {
		t.Fatalf("字典失败不应让详情失败,got err = %v", err)
	}
	if len(detail.Settlements) != 1 {
		t.Fatalf("详情里应有 1 张结算单,got %d", len(detail.Settlements))
	}
	if detail.Settlements[0].SupplierName != "" {
		t.Errorf("字典失败时名字应留空,got %q", detail.Settlements[0].SupplierName)
	}
}

// TestGetApprovalDetail_NoCubeDoesNotPanic 服务没配 cube 时不能 panic。
//
// settlement-auditor 的 cube 是**可 nil** 的(流水线对 cube 不可用 → 实例 failed,
// 见 Service.cube 的注释)。详情页是纯 PG 读,不该因为 cube 没配就崩。
func TestGetApprovalDetail_NoCubeDoesNotPanic(t *testing.T) {
	svc, db, cube, _, _ := newTestService(t)
	cube.settlements = []SettlementRow{
		{ID: "S1", SupplierID: "01", ApproveStatus: "1", TotalGoodsYuan: decimal.NewFromInt(100)},
	}
	ap := submitOK(t, svc, "100")
	waitTerminal(t, db, ap.ID)

	// 把 cube 摘掉,模拟"没配 cube"的部署。
	svc.SetCube(nil)

	detail, err := svc.GetApprovalDetail(context.Background(), "B1", ap.ID)
	if err != nil {
		t.Fatalf("没配 cube 时详情不应失败,got err = %v", err)
	}
	if len(detail.Settlements) != 1 {
		t.Fatalf("详情里应有 1 张结算单,got %d", len(detail.Settlements))
	}
	if detail.Settlements[0].SupplierName != "" {
		t.Errorf("没配 cube 时名字应留空,got %q", detail.Settlements[0].SupplierName)
	}
}
