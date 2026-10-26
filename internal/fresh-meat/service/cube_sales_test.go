// 有源门店的销售读取(2026-10-26 门店反馈)。
//
// 门店反馈:「10-01 设了期初、10-02 录了猪,但毛利看板看不到门店 00 的任何销售数据」。
//
// 根因:moving_average.go 的 loadSkuFlows 只读本地 PG 的 line_sales_by_pigs,
// 而那张表**只由 sale.paid 事件写入** ⇒ 那是**无源门店**(店 10,本地 POS)的事件
// ⇒ 有源门店(店 00 / 店 01)永远是空的 ⇒ 毛利看板恒 revenue=0。
//
// ⇒ 修法:**有源查 cube 源,无源查本地 PG**。
// 本文件把这条分流锁死 —— 防止以后有人"简化"回只读本地。
package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead/cubereadfake"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
)

// fmFakeResolver 是 cubesource.Resolver 的测试替身。
type fmFakeResolver struct {
	src cubesource.Source
	err error
}

func (r fmFakeResolver) Resolve(_ context.Context, _ string) (cubesource.Source, error) {
	return r.src, r.err
}

// fmCubeSalesRow 造一行 cube sales_report 返回。
//
// ⚠️ 用**明文字符串**而不是 base64:base64 是 MSSQL live 源的传输形态,
// 明文是同一个解析器也认的形态(生产实测走的就是 base64,见 cubeclient.ParseDecimal)。
func fmCubeSalesRow(sku, qty, amount string) map[string]any {
	return map[string]any{
		"sales_report.item_no":         sku,
		"sales_report.total_net_qty":    qty,
		"sales_report.total_net_amount": amount,
		"sales_report.count":            "1",
	}
}

// fmSetupSales 造一个"有源门店"的毛利环境。
//
// 返回:svc / cube 替身 / 上下文。
func fmSetupSales(t *testing.T, cubeRows []map[string]any) (*service.Service, *cubereadfake.Client, context.Context) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()

	// 登记部位↔货号映射(毛利按部位看 ⇒ 没有映射就没有猪肉销售)。
	_, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-BELLY"})
	mustNoErr(t, err, "CreateBranchCutMapping")
	_, err = svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutTrotter, CubeProductID: "P-TROT"})
	mustNoErr(t, err, "CreateBranchCutMapping trotter")

	cube := &cubereadfake.Client{}
	cube.Handler = func(model string, _ cubeclient.CubeQuery) ([]map[string]any, int, error) {
		if model != "sales_report" {
			return nil, 0, nil
		}
		return cubeRows, len(cubeRows), nil
	}
	svc.SetCubeReader(cube)
	// 有源 + 启用 ⇒ 走 cube。
	svc.SetSourceResolver(fmFakeResolver{src: cubesource.Source{
		BranchID: "S001", Configured: true, Enabled: true, CubeSourceName: "cube-sixun-ysx-fb",
	}})
	return svc, cube, ctx
}

// TestGrossMargin_SourcedBranchReadsSalesFromCube ⭐ 核心用例。
//
// 有源门店的销售**必须来自 cube**,且要出现在毛利看板的收入里。
// ⇒ 这是门店反馈的那句话的回归锁:「看不到任何销售数据」。
func TestGrossMargin_SourcedBranchReadsSalesFromCube(t *testing.T) {
	svc, cube, ctx := fmSetupSales(t, []map[string]any{
		fmCubeSalesRow("P-BELLY", "8", "160"),
		fmCubeSalesRow("P-TROT", "2", "40"),
	})

	rep, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ComputeGrossMargin")

	// ① cube 被真的查了(model = sales_report)。
	qs := cube.QueriesFor("sales_report")
	if len(qs) != 1 {
		t.Fatalf("应查 cube sales_report 一次,got %d", len(qs))
	}
	// ② 日期过滤进了 query(不是"查了但没过滤")。
	dates := qs[0].FilterValues("sales_report.oper_date")
	if len(dates) != 1 || dates[0] != "2026-10-02" {
		t.Fatalf("oper_date 过滤应是 2026-10-02,got %v", dates)
	}
	// ③ 只查映射表登记过的货号(非猪肉不能混进毛利)。
	items := qs[0].FilterValues("sales_report.item_no")
	if len(items) != 2 {
		t.Fatalf("应按 2 个登记货号过滤,got %v", items)
	}

	// ④ 收入进了看板:160 + 40 = 200。
	if !rep.Total.RevenueYuan.Equal(d("200")) {
		t.Errorf("看板收入应是 200,got %s", rep.Total.RevenueYuan)
	}
	// ⑤ 销量进了看板:8 + 2 = 10 kg。
	totalSold := d("0")
	for _, r := range rep.PerSku {
		totalSold = totalSold.Add(r.SoldKg)
	}
	if !totalSold.Equal(d("10")) {
		t.Errorf("看板销量应是 10 kg,got %s", totalSold)
	}
	// ⑥ 部位行也有(销售按部位归集)。
	var bellySold, trotSold decimal.Decimal
	for _, c := range rep.PerCut {
		switch c.CutType {
		case model.CutBelly:
			bellySold = c.SoldKg
		case model.CutTrotter:
			trotSold = c.SoldKg
		}
	}
	if !bellySold.Equal(d("8")) {
		t.Errorf("belly 销量应是 8,got %s", bellySold)
	}
	if !trotSold.Equal(d("2")) {
		t.Errorf("trotter 销量应是 2,got %s", trotSold)
	}
}

// TestGrossMargin_LocalBranchReadsLocalSales 锁"无源门店仍读本地"。
//
// ⇒ 防止修复把无源门店(店 10,本地 POS)那条路改坏。
func TestGrossMargin_LocalBranchReadsLocalSales(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	_, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-BELLY"})
	mustNoErr(t, err, "CreateBranchCutMapping")

	// 无源门店(没配数据源)⇒ 读本地 line_sales_by_pigs。
	svc.SetSourceResolver(fmFakeResolver{src: cubesource.Source{BranchID: "S001"}})

	// 往本地表塞一条销售。
	mustNoErr(t, testDBSingleton.Create(&model.LineSalesByPig{
		BranchID: "S001", SaleDate: datatypes.Date(fmDay2), PigID: "PIG-1",
		CutType: model.CutBelly, QtyKg: d("5"), AmountYuan: d("100"),
		UnitPriceYuan: d("20"), Source: "pos", PosLineID: "L1",
		PosBranchID: "S001", CubeProductID: "P-BELLY", OrderStatus: "S",
		CreatedAt: time.Now(),
	}).Error, "插本地销售")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ComputeGrossMargin")
	if !rep.Total.RevenueYuan.Equal(d("100")) {
		t.Errorf("无源门店收入应是 100,got %s", rep.Total.RevenueYuan)
	}
}

// TestGrossMargin_SourcedBranchIgnoresLocalSales 锁"有源门店**不读**本地事件表"。
//
// ⚠️ 这条最容易被"顺手改回去":有源门店若同时读了本地 line_sales_by_pigs,
// 就会把**别家店**(无源店 10)的销售混进来 ⇒ 跨店串数。
// ⇒ 店与店之间没有映射关系。
func TestGrossMargin_SourcedBranchIgnoresLocalSales(t *testing.T) {
	svc, _, ctx := fmSetupSales(t, []map[string]any{
		fmCubeSalesRow("P-BELLY", "8", "160"),
	})
	// 本地也塞一条(模拟"别家店的事件流进了本地")。
	mustNoErr(t, testDBSingleton.Create(&model.LineSalesByPig{
		BranchID: "S001", SaleDate: datatypes.Date(fmDay2), PigID: "PIG-1",
		CutType: model.CutBelly, QtyKg: d("999"), AmountYuan: d("9999"),
		UnitPriceYuan: d("10"), Source: "pos", PosLineID: "L2",
		PosBranchID: "S001", CubeProductID: "P-BELLY", OrderStatus: "S",
		CreatedAt: time.Now(),
	}).Error, "插本地销售")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ComputeGrossMargin")
	if !rep.Total.RevenueYuan.Equal(d("160")) {
		t.Errorf("有源门店收入应只取 cube 的 160,got %s(本地那条不该混进来)", rep.Total.RevenueYuan)
	}
}

// TestGrossMargin_SourceUnavailableFailsClosed 锁"问不到数据源 ⇒ 报错,不回落本地"。
//
// ⚠️ 回落本地 = 把一次链路故障显示成「这家店没卖东西」
// ⇒ 门店会以为"今天真没卖",而真相是数据源挂了。
func TestGrossMargin_SourceUnavailableFailsClosed(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	svc.SetSourceResolver(fmFakeResolver{err: errors.New("cube-router 不通")})

	_, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	if err == nil {
		t.Fatalf("问不到数据源时不该静默出结果")
	}
	if !errors.Is(err, service.ErrCubeUnavailable) {
		t.Errorf("应是 service.ErrCubeUnavailable,got %v", err)
	}
}

// TestGrossMargin_SourceDisabledFailsClosed 锁"数据源被停用 ⇒ 503,不回落本地"。
func TestGrossMargin_SourceDisabledFailsClosed(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	svc.SetSourceResolver(fmFakeResolver{src: cubesource.Source{
		BranchID: "S001", Configured: true, Enabled: false,
	}})

	_, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	if err == nil {
		t.Fatalf("数据源被停用时不该静默出结果")
	}
	if !errors.Is(err, service.ErrCubeUnavailable) {
		t.Errorf("应是 service.ErrCubeUnavailable,got %v", err)
	}
}

// TestGrossMargin_SourcedBranchCubeErrorFails 锁"cube 查询失败 ⇒ 上抛"。
//
// ⚠️ 不能把失败当"这天没卖" ⇒ 那会让毛利看板显示 0 销售且零报错。
func TestGrossMargin_SourcedBranchCubeErrorFails(t *testing.T) {
	svc, cube, ctx := fmSetupSales(t, nil)
	cube.Handler = func(_ string, _ cubeclient.CubeQuery) ([]map[string]any, int, error) {
		return nil, 0, errors.New("cube 超时")
	}
	_, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	if err == nil {
		t.Fatalf("cube 查询失败不该静默出结果")
	}
}

// TestCubeSales_IgnoresUnmappedSkus 锁"只统计映射表登记过的货号"。
//
// ⇒ 蔬菜/日杂不能混进鲜肉毛利。
func TestCubeSales_IgnoresUnmappedSkus(t *testing.T) {
	svc, cube, ctx := fmSetupSales(t, []map[string]any{
		fmCubeSalesRow("P-BELLY", "8", "160"),
		fmCubeSalesRow("VEG-001", "99", "9999"), // 不该被计入
	})
	rep, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ComputeGrossMargin")
	if !rep.Total.RevenueYuan.Equal(d("160")) {
		t.Errorf("收入应只含登记货号的 160,got %s", rep.Total.RevenueYuan)
	}
	// 未登记货号根本不该出现在 query 的过滤里。
	items := cube.QueriesFor("sales_report")[0].FilterValues("sales_report.item_no")
	for _, v := range items {
		if s, ok := v.(string); ok && s == "VEG-001" {
			t.Errorf("未登记货号不该进 cube 查询:%v", items)
		}
	}
}

// TestGrossMargin_SourcedBranchKeepsLocalBulkSales 锁"**有源门店的统收仍读本地**"。
//
// # 为什么这条极其重要(2026-10-26 契约实测抓到)
//
// 之前"有源 ⇒ 零售与统收**全**走 cube" ⇒ 统收结算写进本地的
// `line_sales_by_pigs(source='bulk')` 被**整段丢掉**
// ⇒ 统收成本 = 0 ⇒ **卖板油毛利率 100%**(2026-10-25 修过的老 bug 又回来了)。
//
// ⚠️ 而且"有源"是**门店级**属性,不是**数据类别**属性:
// 契约测试的一次性门店会**借用**一个已启用的 cube 实例(为了校验 supplier_id),
// 但它的统收/报损仍是**本地数据** ⇒ 统一走 cube 必然读不到。
//
// ⇒ 判据:零售走 cube;**统收永远走本地**。
func TestGrossMargin_SourcedBranchKeepsLocalBulkSales(t *testing.T) {
	svc, _, ctx := fmSetupSales(t, []map[string]any{
		fmCubeSalesRow("P-BELLY", "8", "160"),
	})
	// 本地再写一条**统收**行(source='bulk')。
	mustNoErr(t, testDBSingleton.Create(&model.LineSalesByPig{
		BranchID: "S001", SaleDate: datatypes.Date(fmDay2), PigID: "PIG-1",
		CutType: model.CutBelly, QtyKg: d("8"), AmountYuan: d("48"),
		UnitPriceYuan: d("6"), Source: "bulk", PosLineID: "BULK1",
		PosBranchID: "S001", CubeProductID: "P-BELLY", OrderStatus: "S",
		CreatedAt: time.Now(),
	}).Error, "插本地统收行")

	rep, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ComputeGrossMargin")

	// ① 零售收入来自 cube(160)。
	if !rep.Total.RevenueYuan.Equal(d("208")) {
		t.Errorf("收入应是 cube 零售 160 + 本地统收 48 = 208,got %s", rep.Total.RevenueYuan)
	}
	// ② 统收量进了 bulk 列(不是零售列)。
	var bulkKg, soldKg decimal.Decimal
	for _, c := range rep.PerSku {
		bulkKg = bulkKg.Add(c.BulkSoldKg)
		soldKg = soldKg.Add(c.SoldKg)
	}
	if !bulkKg.Equal(d("8")) {
		t.Errorf("统收量应是 8,got %s(有源门店把本地统收丢了)", bulkKg)
	}
	if !soldKg.Equal(d("8")) {
		t.Errorf("零售量应是 8(只来自 cube),got %s", soldKg)
	}
}
