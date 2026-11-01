package service_test

// 2026-10-30:「加入样本」的**完整性校验**必须跟毛利看板走**同一条销售读路径**。
//
// # 门店实测的 bug(2026-10-30)
//
//	富百家(店 00)点「按一天生成基线」⇒ 报
//	「2026-10-29 这一天数据不完整,缺:销售(当天没有销售流水)」
//	⇒ 那天明明卖了一整天(看板上有 3553.40 元营收)。
//
// # 根因
//
//	完整性校验只查**本地** `line_sales_by_pigs`
//	⇒ 而那张表只由 Dapr 的 sale.paid 事件写入 ⇒ **只对无源门店(本地 POS)有数据**
//	⇒ 有源门店(店 00/01)的销售在**它自己的 cube 源**里
//	⇒ 只查本地 ⇒ 有源门店**永远**报「缺:销售」⇒ 生成按钮对有源门店直接不可用。
//
// # 修法
//
//	销售这一项交给 `hasSalesOnDay` ⇒ 它用 `salesReadPath`(cube / local)
//	⇒ 与**毛利看板完全一致**(看板能出数,这里就不能说"没卖")。
//
// # 本文件锁什么
//
//	① 有源门店:cube 里有销售 ⇒ **不缺**销售(★ 核心回归)
//	② 有源门店:cube 里没销售 ⇒ 仍报缺销售(判据不是"查过 cube 就算有")
//	③ 有源门店**不读**本地零售事件(别把别家店的数据混进来)
//	④ 统收(本地表)**所有店都算**销售
//	⑤ cube 读失败 ⇒ **上抛**(503),不回落成"这天没卖"
//	⑥ 无源门店:本地零售算销售(别把老路改坏)
//
// ⚠️ 2026-10-30 owner 重新定义模型后,「按一天生成基线」已换成「按一天**加入样本**」
// ⇒ 本文件跟着改成走 [service.Service.CollectForecastSamples]。

import (
	"context"
	"errors"
	"strings"
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

// fmGenSetupSourced 起一个"有源门店"的加入样本环境(cube 里有销售)。
//
// ⚠️ 与 fmSetupSales 的区别:那边固定用 fmDay2(10-02),这边用 fmGenDate(10-29)
//
//	⇒ 假身**必须按 oper_date 过滤**,否则别天的销售会被算进来。
func fmGenSetupSourced(t *testing.T, cubeRows []map[string]any) (*service.Service, context.Context, string) {
	t.Helper()
	svc, _ := setupTestService(t)
	ctx := context.Background()
	svc.SetClock(fmClock(splitDay))
	_, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-BELLY"})
	mustNoErr(t, err, "CreateBranchCutMapping")

	cube := &cubereadfake.Client{}
	cube.Handler = func(m string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		if m != "sales_report" {
			return nil, 0, nil
		}
		// ⚠️ **必须按 oper_date 过滤**:成本链逐日回溯时每一天都会问一次 cube,
		//   假身若对任何日期都返回同一批销售 ⇒ 销售被重复计入。
		if d := fmCubeOperDate(q); d != "" && d != fmGenDate {
			return nil, 0, nil
		}
		return cubeRows, len(cubeRows), nil
	}
	svc.SetCubeReader(cube)
	// 有源 + 启用 ⇒ 走 cube(与生产店 00 的形态一致)。
	svc.SetSourceResolver(fmFakeResolver{src: cubesource.Source{
		BranchID: "S001", Configured: true, Enabled: true, CubeSourceName: "cube-sixun-ysx-fb",
	}})
	return svc, ctx, "P-BELLY"
}

// fmGenSeedDayExceptSales 铺"录入 + 搬台 + 完整盘点"三项(**销售留给 cube**)。
//
// 返回猪 ID(用来补一条分割实录 ⇒ 那一天有样本)。
func fmGenSeedDayExceptSales(t *testing.T, svc *service.Service, ctx context.Context, sku string) string {
	t.Helper()
	pigID := estPig(t, svc, ctx, "S001", "EB-GEN-SRC", "200", splitDay)
	// 搬台:0 kg 的标记行(有流水 = 搬过这一步)。
	_, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		Lines: []service.CounterLoadLine{{QtyKg: decimal.Zero}},
	}, "u-1")
	mustNoErr(t, err, "搬台标记")
	// 盘点:完整盘点单。
	estStocktake(t, svc, ctx, "S001", sku, "5", "")
	return pigID
}

// ---- ① ★ 核心回归:有源门店的销售在 cube 里 ⇒ 不该报「缺:销售」 ----

// TestCollectSamples_SourcedBranch_CubeSalesCountsAsComplete 锁①。
//
// # 判别力自检
//
//	把 hasSalesOnDay 退回"只查本地 line_sales_by_pigs"⇒ 本用例立刻红
//	(本地表是空的 ⇒ 会报「缺:销售」)⇒ 这正是门店 2026-10-30 遇到的那个形状。
func TestCollectSamples_SourcedBranch_CubeSalesCountsAsComplete(t *testing.T) {
	svc, ctx, sku := fmGenSetupSourced(t, []map[string]any{
		fmCubeSalesRow("P-BELLY", "8", "160"),
	})
	pigID := fmGenSeedDayExceptSales(t, svc, ctx, sku)
	estPigCut(t, svc, ctx, "S001", pigID, sku, "B-GEN-SRC", "30")

	out, err := svc.CollectForecastSamples(ctx, "S001", service.ForecastSampleCollectInput{Date: fmGenDate})
	mustNoErr(t, err, "★ 有源门店的销售在 cube 里 ⇒ 不该报「缺:销售」")
	if out == nil {
		t.Fatalf("★ 该返回结果而不是 nil")
	}
	if out.Date != fmGenDate {
		t.Errorf("★ 返回的 date 应是 %s,got %s", fmGenDate, out.Date)
	}
}

// ---- ② cube 里没销售 ⇒ 仍要报缺销售 ----

// TestCollectSamples_SourcedBranch_NoSales_StillReportsGap 锁②。
//
// ⚠️ 判据不能退化成"查过 cube 就算有销售"
//
//	⇒ 否则**任何一天**都能加入样本 ⇒ 样本就成了纯手输,失去"数据完整"的意义。
func TestCollectSamples_SourcedBranch_NoSales_StillReportsGap(t *testing.T) {
	svc, ctx, sku := fmGenSetupSourced(t, nil) // cube 当天 0 行
	fmGenSeedDayExceptSales(t, svc, ctx, sku)

	_, err := svc.CollectForecastSamples(ctx, "S001", service.ForecastSampleCollectInput{Date: fmGenDate})
	if err == nil {
		t.Fatalf("★ cube 当天没销售 ⇒ 该报缺销售")
	}
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("应是输入校验错(400),got %v", err)
	}
	if !strings.Contains(err.Error(), "销售") {
		t.Errorf("★ 报错要点名缺[销售],got %v", err)
	}
}

// ---- ③ 有源门店不读本地零售事件 ----

// TestCollectSamples_SourcedBranch_IgnoresLocalSaleEvents 锁③。
//
// ⚠️ 本地 `line_sales_by_pigs` 只装**无源门店**(本地 POS)的事件。
//
//	有源门店若把它也算进去 ⇒ 别的店卖出去的东西会被算成本店"有销售"
//	⇒ 门槛失效(看板那边已经锁过同一条,见 TestGrossMargin_SourcedBranchIgnoresLocalSales)。
func TestCollectSamples_SourcedBranch_IgnoresLocalSaleEvents(t *testing.T) {
	svc, ctx, sku := fmGenSetupSourced(t, nil) // cube 当天 0 行
	fmGenSeedDayExceptSales(t, svc, ctx, sku)

	// 本地塞一条零售事件(模拟"别家店的事件流进了本地")。
	mustNoErr(t, testDBSingleton.Create(&model.LineSalesByPig{
		BranchID: "S001", SaleDate: datatypes.Date(splitDay), PigID: "PIG-1",
		CutType: model.CutBelly, QtyKg: d("999"), AmountYuan: d("9999"),
		UnitPriceYuan: d("10"), Source: "pos", PosLineID: "L-LOCAL",
		PosBranchID: "S001", CubeProductID: "P-BELLY", OrderStatus: "S",
		CreatedAt: time.Now(),
	}).Error, "插本地零售行")

	_, err := svc.CollectForecastSamples(ctx, "S001", service.ForecastSampleCollectInput{Date: fmGenDate})
	if err == nil {
		t.Fatalf("★ 有源门店不该把本地零售事件当成自己的销售")
	}
	if !strings.Contains(err.Error(), "销售") {
		t.Errorf("★ 报错要点名缺[销售],got %v", err)
	}
}

// ---- ④ 统收(本地表)所有店都算销售 ----

// TestCollectSamples_BulkSettlementCountsAsSales 锁④。
//
// ⚠️ 统收**永远在本地表**(bulk_settlements)——
//
//	即使门店有 cube 源,它的统收结算也写在本域。
//	⇒ 有源门店"零售走 cube + 统收走本地"⇒ 完整性校验也必须两边都认。
func TestCollectSamples_BulkSettlementCountsAsSales(t *testing.T) {
	svc, ctx, sku := fmGenSetupSourced(t, nil) // cube 当天 0 行零售
	pigID := fmGenSeedDayExceptSales(t, svc, ctx, sku)

	// ⚠️ 补录一条分割实录 ⇒ 那一天**有样本**。
	//
	//	否则两个样本池都空 ⇒ 会报「缺:分割样本」——
	//	那是**另一个**缺口(那天确实没卖出任何部位),与本用例要锁的
	//	「统收算销售 ⇒ 不报缺销售」无关。
	estPigCut(t, svc, ctx, "S001", pigID, sku, "B-BULK-1", "30")

	// 本地插一张当天统收单。
	mustNoErr(t, testDBSingleton.Create(&model.BulkSettlement{
		ID: "BKS-GEN-1", BranchID: "S001",
		SettleDate:      datatypes.Date(time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)),
		TotalAmountYuan: d("480"), Counterparty: "某酒店", Mode: "delivery",
		SettledAt: splitDay, OperatorID: "u-1", CreatedAt: splitDay,
	}).Error, "插统收单")

	out, err := svc.CollectForecastSamples(ctx, "S001", service.ForecastSampleCollectInput{Date: fmGenDate})
	mustNoErr(t, err, "★ 当天有统收 ⇒ 不该报缺销售")
	if out == nil {
		t.Fatalf("★ 该返回结果而不是 nil")
	}
}

// ---- ⑤ cube 读失败 ⇒ 上抛,不回落成"这天没卖" ----

// TestCollectSamples_SourcedBranch_CubeErrorFailsClosed 锁⑤。
//
// ⚠️ **不能**把一次链路故障显示成「这家店没卖东西」
//
//	⇒ 那正是 2026-10-26 门店看到"毛利看板没有销售"的形状 ⇒ 不能再来一次。
//
// ⇒ cube 查询失败 ⇒ 503(ErrCubeUnavailable),**不是** 400「缺:销售」。
func TestCollectSamples_SourcedBranch_CubeErrorFailsClosed(t *testing.T) {
	svc, ctx, sku := fmGenSetupSourced(t, nil)
	fmGenSeedDayExceptSales(t, svc, ctx, sku)

	// 让 cube 查询直接失败。
	svc.SetCubeReader(&cubereadfake.Client{
		Handler: func(_ string, _ cubeclient.CubeQuery) ([]map[string]any, int, error) {
			return nil, 0, errors.New("cube 超时")
		},
	})

	_, err := svc.CollectForecastSamples(ctx, "S001", service.ForecastSampleCollectInput{Date: fmGenDate})
	if err == nil {
		t.Fatalf("★ cube 查询失败不该静默出结果")
	}
	if errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("★ cube 查询失败**不是** 400「数据不完整」,got %v", err)
	}
	if strings.Contains(err.Error(), "缺:销售") {
		t.Errorf("★ 不能把链路故障说成「缺:销售」,got %v", err)
	}
	if !errors.Is(err, service.ErrCubeUnavailable) {
		t.Errorf("★ 应是 service.ErrCubeUnavailable(503),got %v", err)
	}
}

// ---- ⑥ 无源门店:本地零售算销售(别把老路改坏) ----

// TestCollectSamples_LocalBranch_LocalSaleCountsAsComplete 锁⑥。
func TestCollectSamples_LocalBranch_LocalSaleCountsAsComplete(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	pigID := fmGenSeedDayExceptSales(t, svc, ctx, sku)
	estPigCut(t, svc, ctx, "S001", pigID, sku, "B-GEN-LOCAL", "30")
	// 本地零售。
	splitSell(t, svc, sku, "L-GEN-LOCAL", "10", "200")

	out, err := svc.CollectForecastSamples(ctx, "S001", service.ForecastSampleCollectInput{Date: fmGenDate})
	mustNoErr(t, err, "★ 无源门店的本地零售算销售")
	if out == nil {
		t.Fatalf("★ 该返回结果而不是 nil")
	}
}
