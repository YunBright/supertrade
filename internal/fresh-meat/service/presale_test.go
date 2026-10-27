package service_test

// 2026-10-24(门店口径第四轮):**预期毛利(预售)结转** + **重放 + 校准当前余额**。
//
// 门店口径(本轮拍板):
//
//	① 审计 = **结转**:截止日及之前的每日日终盘点齐全 ⇒ 落快照 + 冻结。
//	② 「本期产生结余,结余部分库存未进行毛利归属计算,审计后将会导致本期毛利下降」
//	   ⇒ 审计前(条件检查通过后、确认前)与审计结果里都要出现。
//	③ 预期毛利(预售):按 **SKU 当前售价**(有促销价按促销价)把结余结转掉;
//	   预售毛利与正常 FIFO 毛利**一起算到猪上**;
//	   **下一期期初的该部位单价 = 预期销售单价**
//	   ⇒ 下一期按成本价卖(没毛利)⇒ 两期总体毛利没算错。
//	④ 「价格差调整」**不**回补上一期(门店自己承担)。
//	⑤ 重放:审计后把**截止日之后的搬台等动作**重放一遍,
//	   并把当前余额校准到「截止日结余 + 重放后的结果」。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// fmFakeCube 是 fresh-meat 服务的 cube 替身(只实现 service.CubeClient)。
//
// ⚠️ 预售结转要拿 **SKU 当前售价**(门店口径 2.2)⇒ 单测必须能控价。
type fmFakeCube struct {
	// prices 是"货号 → 零售价 / 促销价"。
	prices map[string]fmFakePrice
	// err 非空 ⇒ 取价失败(测"取不到价就不预售"的降级路径)。
	err error
}

type fmFakePrice struct {
	retail string
	promo  string
}

func (f *fmFakeCube) SearchSuppliers(_ context.Context, _ string, _ int) ([]cubeclient.SupplierDTO, error) {
	return nil, nil
}

func (f *fmFakeCube) BatchCurrentPrices(_ context.Context, productIDs []string, _ string) (map[string]cubeclient.ProductPriceQuote, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]cubeclient.ProductPriceQuote{}
	for _, id := range productIDs {
		p, ok := f.prices[id]
		if !ok {
			continue // 没维护售价 ⇒ 不在返回里
		}
		q := cubeclient.ProductPriceQuote{ProductID: id}
		if p.retail != "" {
			v := decimal.RequireFromString(p.retail)
			q.PriceYuan = &v
		}
		if p.promo != "" {
			v := decimal.RequireFromString(p.promo)
			q.PromoPriceYuan = &v
		}
		out[id] = q
	}
	return out, nil
}

// fmPresaleSetup 铺两天(10-01 / 10-02),两头猪各切 10kg,两天都有完整的日终盘点。
//
// 成本口径(故意取整,断言才好写):
//
//	猪:50kg × 20 元/kg = 1000 元/头;各切 10kg ⇒ 100 元/kg
//	10-02 日终:期初 10kg/1000 + 入库 10kg/1000 ⇒ 20kg / 2000 ⇒ 移动加权 100 元/kg
//	日终结余:冷库 10(10-01 结转) + 10(10-02 结转) = 20kg
//
// ⇒ 审计(10-02)时:结余 20kg、成本 2000 元。
//   售价 150 元/kg ⇒ 预期收入 3000、预售毛利 1000。
//   售价 120 元/kg(促销)⇒ 预期收入 2400、预售毛利 400。
func fmPresaleSetup(t *testing.T) (*service.Service, context.Context, string, string) {
	t.Helper()
	svc, ctx, sku := fmBizSetup(t)
	// 10-01
	pig1 := fmSeedPigAt(t, svc, "EB-1", "50", fmDay1)
	fmSeedPigCut(t, svc, pig1, sku, "10")
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("10")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("10-01 盘点: %v", err)
	}
	// 10-02
	svc.SetClock(fmClock(fmDay2))
	pig2 := fmSeedPigAt(t, svc, "EB-2", "50", fmDay2)
	fmSeedPigCut(t, svc, pig2, sku, "10")
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("10")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("10-02 盘点: %v", err)
	}
	return svc, ctx, sku, pig1
}

// TestPresale_PreviewBeforeAudit 锁 ②③:审计**前**的预检。
//
// ⇒ 预检要给:条件是否满足、有没有结余、提示文案、预期毛利测算。
func TestPresale_PreviewBeforeAudit(t *testing.T) {
	svc, ctx, sku, _ := fmPresaleSetup(t)
	svc.SetCubeClient(&fmFakeCube{prices: map[string]fmFakePrice{
		sku: {retail: "150"},
	}})

	pre, err := svc.PrecheckBizAudit(ctx, "S001", "2026-10-02")
	mustNoErr(t, err, "PrecheckBizAudit")
	if !pre.Ready {
		t.Fatalf("条件应满足(两天都有完整盘点): %+v", pre)
	}
	if !pre.HasBalance {
		t.Fatalf("本次应产生结余: %+v", pre)
	}
	if pre.Presale == nil {
		t.Fatalf("预检应给出预期毛利测算")
	}
	if !strings.Contains(pre.Presale.Note, "实际售价与预期售价的差额落在下一期") {
		t.Errorf("预检应说明'实际售价与预期售价的差额落在下一期',got %q", pre.Presale.Note)
	}
	if !pre.Presale.TotalMarginYuan.Equal(d("1000")) {
		t.Errorf("预售毛利应是 500(预期收入 2000 − 结余成本 1500),got %s", pre.Presale.TotalMarginYuan)
	}
	if !pre.Presale.TotalExpectedRevenueYuan.Equal(d("3000")) {
		t.Errorf("预期收入应是 2000,got %s", pre.Presale.TotalExpectedRevenueYuan)
	}
	if !pre.Presale.TotalCostAmountYuan.Equal(d("2000")) {
		t.Errorf("结余成本应是 1500,got %s", pre.Presale.TotalCostAmountYuan)
	}
}

// TestPresale_AuditSettlesMarginAndRebasesNextOpening 锁 ③ 的三个动作。
//
// ① 审计当日毛利**含进**预售毛利(500);
// ② 审计日的成本日快照期末金额改成**预期售价口径**(20kg × 100 = 2000);
// ③ 下一期(D+1)的**期初单价 = 预期销售单价**(100 元/kg)。
func TestPresale_AuditSettlesMarginAndRebasesNextOpening(t *testing.T) {
	svc, ctx, sku, _ := fmPresaleSetup(t)
	svc.SetCubeClient(&fmFakeCube{prices: map[string]fmFakePrice{
		sku: {retail: "150"},
	}})

	// 审计前:10-02 的毛利 = 0(还没结转)。
	rep, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ComputeGrossMargin(审计前)")
	if !rep.Total.GrossMarginYuan.IsZero() {
		t.Fatalf("审计前 10-02 毛利应是 0,got %s", rep.Total.GrossMarginYuan)
	}

	st, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-02"}, "u-1")
	mustNoErr(t, err, "AuditBusinessDays")
	if st.Presale == nil {
		t.Fatalf("审计结果应带预期毛利结转")
	}
	if !st.Presale.TotalMarginYuan.Equal(d("1000")) {
		t.Errorf("预售毛利应是 1000,got %s", st.Presale.TotalMarginYuan)
	}
	if !strings.Contains(st.Presale.Note, "实际售价与预期售价的差额落在下一期") {
		t.Errorf("审计结果应说明'实际售价与预期售价的差额落在下一期',got %q", st.Presale.Note)
	}

	// ① 审计当日的毛利 = 零售毛利(0) + 预售毛利(1000)。
	rep2, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ComputeGrossMargin(审计后)")
	if !rep2.Total.GrossMarginYuan.Equal(d("1000")) {
		t.Errorf("审计后 10-02 毛利应是 1000(含预售毛利),got %s", rep2.Total.GrossMarginYuan)
	}
	if !rep2.Total.PresaleMarginYuan.Equal(d("1000")) {
		t.Errorf("预售毛利应单列 1000,got %s", rep2.Total.PresaleMarginYuan)
	}
	// 悬浮提示:页面上的毛利是**含**预期毛利的,提示里说清含了多少。
	if rep2.PresaleHint == nil {
		t.Fatalf("有预期毛利时应给悬浮提示")
	}
	if !rep2.PresaleHint.GrossMarginYuan.Equal(d("1000")) {
		t.Errorf("提示里的毛利应是 1000,got %s", rep2.PresaleHint.GrossMarginYuan)
	}
	if !rep2.PresaleHint.PresaleMarginYuan.Equal(d("1000")) {
		t.Errorf("提示里的预期毛利应是 1000,got %s", rep2.PresaleHint.PresaleMarginYuan)
	}
	if !strings.Contains(rep2.PresaleHint.Text, "1000") {
		t.Errorf("提示文案应带金额,got %q", rep2.PresaleHint.Text)
	}
	if rep2.Reconciliation.DiffYuan.Abs().GreaterThan(d("0.01")) {
		t.Errorf("对账行差额应仍为 0(重估单列),got %s", rep2.Reconciliation.DiffYuan)
	}
	if !rep2.Reconciliation.RevaluationYuan.Equal(d("1000")) {
		t.Errorf("重估额应是 500,got %s", rep2.Reconciliation.RevaluationYuan)
	}

	// ② 审计日的成本日快照(**部位级**:cut_cost_daily):
	//    - **成本口径**仍是 2000(20kg × 100 元/kg)—— 预售价是另一层;
	//    - **结转金额**(下一期的期初来源)= 预期售价口径 3000(20kg × 150)。
	//
	// ⚠️ 2026-10-27:成本链改部位级 ⇒ 快照落在 `cut_cost_daily`(按部位),
	//   旧的 `sku_cost_daily`(按 SKU)已停写。
	snap, ok := fmCutCostDaily(t, "S001", "2026-10-02", model.CutBelly)
	if !ok {
		t.Fatalf("查 10-02 成本日快照(部位级): 没有行")
	}
	if !snap.ClosingKg.Equal(d("20")) {
		t.Errorf("10-02 期末量应是 20kg,got %s", snap.ClosingKg)
	}
	if !snap.MovingAvgUnitCost.Equal(d("100")) {
		t.Errorf("移动加权单价仍是**成本口径**(100),got %s", snap.MovingAvgUnitCost)
	}
	if !snap.ClosingKg.Mul(snap.MovingAvgUnitCost).Equal(d("2000")) {
		t.Errorf("10-02 成本口径期末金额应是 2000(20kg × 100),got %s",
			snap.ClosingKg.Mul(snap.MovingAvgUnitCost))
	}
	if !snap.ClosingAmount.Equal(d("3000")) {
		t.Errorf("10-02 结转金额应是**预期售价口径** 3000(20kg × 150),got %s", snap.ClosingAmount)
	}

	// ③ 下一期(10-03):期初单价 = 预期销售单价(150)。
	svc.SetClock(fmClock(fmDay2.Add(24 * time.Hour)))
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-03",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("20")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("10-03 盘点: %v", err)
	}
	rep3, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2.Add(24*time.Hour))
	mustNoErr(t, err, "ComputeGrossMargin(10-03)")
	row3 := fmCutRowFor(t, rep3, model.CutBelly)
	if !row3.OpeningKg.Equal(d("20")) {
		t.Errorf("10-03 期初 kg 应是 20(上期结转),got %s", row3.OpeningKg)
	}
	// 期初金额 = 3000(预期售价口径)⇒ 加权单价 = 3000 / 20 = 150。
	if !rep3.Reconciliation.OpeningInventoryCostYuan.Equal(d("3000")) {
		t.Errorf("10-03 期初金额应是 3000(上期期末的预期售价口径),got %s",
			rep3.Reconciliation.OpeningInventoryCostYuan)
	}
	if !row3.MovingAvgUnitCost.Equal(d("150")) {
		t.Errorf("10-03 移动加权单价应是 150(上一期的预期销售单价),got %s", row3.MovingAvgUnitCost)
	}
}

// TestPresale_MarginAllocatedToPigs 锁 ③ 的"归到猪上"。
//
// ⇒ 预售毛利按 **FIFO**(袋子归属)摊到**产出它的那头猪**:
//   10-01 的那批(EB-1)10kg → EB-1;10-02 的那批(EB-2)10kg → EB-2。
// ⇒ 两头猪各拿 10kg × (100 − 75) = 250。
func TestPresale_MarginAllocatedToPigs(t *testing.T) {
	svc, ctx, sku, pig1 := fmPresaleSetup(t)
	svc.SetCubeClient(&fmFakeCube{prices: map[string]fmFakePrice{
		sku: {retail: "150"},
	}})

	st, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-02"}, "u-1")
	mustNoErr(t, err, "AuditBusinessDays")
	if st.Presale == nil || len(st.Presale.PigMargins) == 0 {
		t.Fatalf("预售毛利应分摊到猪: %+v", st.Presale)
	}
	byPig := map[string]decimal.Decimal{}
	for _, m := range st.Presale.PigMargins {
		byPig[m.PigID] = byPig[m.PigID].Add(m.MarginYuan)
	}
	if len(byPig) != 2 {
		t.Fatalf("应摊到两头猪,got %d: %+v", len(byPig), byPig)
	}
	total := decimal.Zero
	for _, v := range byPig {
		total = total.Add(v)
	}
	if !total.Equal(d("1000")) {
		t.Errorf("按猪合计应等于预售毛利 500,got %s", total)
	}
	if !byPig[pig1].Equal(d("500")) {
		t.Errorf("EB-1 应拿 250(10kg × 25),got %s", byPig[pig1])
	}
	// 库里也要有行(按 SKU 分行)。
	var rows []model.PigPresaleMargin
	if err := testDBSingleton.Where("branch_id = ?", "S001").Find(&rows).Error; err != nil {
		t.Fatalf("查 pig_presale_margins: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("pig_presale_margins 应有行")
	}
}

// TestPresale_PriceServiceDownFailsAudit 锁"取价服务不可用 ⇒ 审计失败(不冻结)"。
//
// ⇒ 审计是**不可逆**的一刀 ⇒ 预售没算出来就不能冻结 ⇒ 门店等价格服务恢复后重试。
func TestPresale_PriceServiceDownFailsAudit(t *testing.T) {
	svc, ctx, _, _ := fmPresaleSetup(t)
	svc.SetCubeClient(&fmFakeCube{err: context.DeadlineExceeded})

	if _, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-02"}, "u-1"); err == nil {
		t.Fatalf("取价服务不可用时审计不该成功(数据会被冻结,再也补不了)")
	}
	// 没冻结 ⇒ 还能补录 10-01 的盘点。
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-01",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: "P-BELLY", ActualRemainKg: d("10")},
		},
	}, "u-1"); err != nil {
		t.Errorf("审计失败 ⇒ 不该冻结(还能补录): %v", err)
	}
}

// TestPresale_SkuWithoutPriceUsesZero 锁"取不到价 ⇒ 也按 0 生成(不跳过)"。
//
// ⇒ 门店口径:「取不到也按0生成,会经过一轮人工修改,门店审计人负责」
//   ⇒ 负毛利照做、本期归属结清;修正窗口 = 审计前(预检里能看到哪些 SKU 按 0)。
func TestPresale_SkuWithoutPriceUsesZero(t *testing.T) {
	svc, ctx, sku, _ := fmPresaleSetup(t)
	// 客户端在,但这个 SKU 没有维护价(cube 返回里没有它)。
	svc.SetCubeClient(&fmFakeCube{prices: map[string]fmFakePrice{}})

	st, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-02"}, "u-1")
	mustNoErr(t, err, "AuditBusinessDays")
	if st.Presale == nil || len(st.Presale.Rows) == 0 {
		t.Fatalf("取不到价也要做预售(归属结清): %+v", st.Presale)
	}
	row := st.Presale.Rows[0].Skus[0]
	if row.PriceSource != service.PresalePriceSourceMissing {
		t.Errorf("取不到价 ⇒ 单价来源应是 missing,got %q", row.PriceSource)
	}
	if !row.ExpectedUnitYuan.IsZero() {
		t.Errorf("取不到价 ⇒ 预期价应是 0,got %s", row.ExpectedUnitYuan)
	}
	// 预期收入 0、结余成本 2000 ⇒ 预售毛利 = −2000(负毛利照做)。
	if !st.Presale.TotalMarginYuan.Equal(d("-2000")) {
		t.Errorf("预售毛利应是 −2000,got %s", st.Presale.TotalMarginYuan)
	}
	// 取不到价的 SKU 要被显式列出来(人工修改要照着这张单子改)。
	if len(st.Presale.MissingSkus) != 1 || st.Presale.MissingSkus[0] != sku {
		t.Errorf("取不到价的 SKU 应显式列出,got %+v", st.Presale.MissingSkus)
	}
	// 冻结快照的**成本口径**仍是 2000(20kg × 100 元/kg)—— 预售价是另一层,可改。
	//
	// ⚠️ 2026-10-27:成本链改部位级 ⇒ 快照落在 `cut_cost_daily`(按部位)。
	snap, ok := fmCutCostDaily(t, "S001", "2026-10-02", model.CutBelly)
	if !ok {
		t.Fatalf("查 10-02 成本日快照(部位级): 没有行")
	}
	if !snap.ClosingKg.Mul(snap.MovingAvgUnitCost).Equal(d("2000")) {
		t.Errorf("冻结快照的成本口径应是 2000(20kg × 100),got %s",
			snap.ClosingKg.Mul(snap.MovingAvgUnitCost))
	}
	if !snap.MovingAvgUnitCost.Equal(d("100")) {
		t.Errorf("移动加权单价应是**成本口径**(100),got %s", snap.MovingAvgUnitCost)
	}
	// 结转金额(下一期的期初来源)= 预期售价口径:0 元价 ⇒ 0(写减记)。
	if !snap.ClosingAmount.Equal(d("0")) {
		t.Errorf("0 元价 ⇒ 结转金额应是 0(写减记),got %s", snap.ClosingAmount)
	}
	// 下一期期初 = 成本口径 2000 + 重估(0 − 100)×20 = 0(写减记)。
	svc.SetClock(fmClock(fmDay2.Add(24 * time.Hour)))
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-03",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("20")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("10-03 盘点: %v", err)
	}
	rep3, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2.Add(24*time.Hour))
	mustNoErr(t, err, "ComputeGrossMargin(10-03)")
	if !rep3.Reconciliation.OpeningInventoryCostYuan.IsZero() {
		t.Errorf("取不到价 ⇒ 10-03 期初金额应是 0(写减记),got %s",
			rep3.Reconciliation.OpeningInventoryCostYuan)
	}
}

// TestPresale_ZeroPriceBooksNegativeMargin 锁"维护为 0 元的货号 ⇒ 照做(负毛利也结清)"。
//
// ⇒ 门店口径:「负毛利也要将本期归属算干净」⇒ 不跳过、按 0 价结转。
func TestPresale_ZeroPriceBooksNegativeMargin(t *testing.T) {
	svc, ctx, sku, _ := fmPresaleSetup(t)
	svc.SetCubeClient(&fmFakeCube{prices: map[string]fmFakePrice{
		sku: {retail: "0"},
	}})

	st, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-02"}, "u-1")
	mustNoErr(t, err, "AuditBusinessDays")
	if st.Presale == nil || len(st.Presale.Rows) == 0 {
		t.Fatalf("0 元也要做预售: %+v", st.Presale)
	}
	row := st.Presale.Rows[0].Skus[0]
	if row.PriceSource != service.PresalePriceSourceZero {
		t.Errorf("0 元价 ⇒ 单价来源应是 zero,got %q", row.PriceSource)
	}
	// 预期收入 0、结余成本 2000 ⇒ 预售毛利 = −2000(负毛利照做)。
	if !st.Presale.TotalExpectedRevenueYuan.IsZero() {
		t.Errorf("预期收入应是 0,got %s", st.Presale.TotalExpectedRevenueYuan)
	}
	if !st.Presale.TotalMarginYuan.Equal(d("-2000")) {
		t.Errorf("预售毛利应是 −2000(负毛利也结清),got %s", st.Presale.TotalMarginYuan)
	}
	// 本期毛利 = 零售毛利(0) + 预售毛利(−2000)。
	rep, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2)
	mustNoErr(t, err, "ComputeGrossMargin")
	if !rep.Total.GrossMarginYuan.Equal(d("-2000")) {
		t.Errorf("本期毛利应是 −2000,got %s", rep.Total.GrossMarginYuan)
	}
	// 下一期期初 = 成本口径 2000 + 重估(0 − 100)×20 = 0(写减记)
	// ⇒ 下一期卖出去时毛利落在下一期。
	svc.SetClock(fmClock(fmDay2.Add(24 * time.Hour)))
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-03",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("20")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("10-03 盘点: %v", err)
	}
	rep3, err := svc.ComputeGrossMargin(ctx, "S001", fmDay2.Add(24*time.Hour))
	mustNoErr(t, err, "ComputeGrossMargin(10-03)")
	if !rep3.Reconciliation.OpeningInventoryCostYuan.IsZero() {
		t.Errorf("0 元价 ⇒ 10-03 期初金额应是 0(写减记),got %s",
			rep3.Reconciliation.OpeningInventoryCostYuan)
	}
}

// TestPresale_NoPriceClientSkipsSettlement 锁"没有价格客户端 ⇒ 跳过(不是失败)"。
func TestPresale_NoPriceClientSkipsSettlement(t *testing.T) {
	svc, ctx, _, _ := fmPresaleSetup(t)
	// 不装 cube 客户端 ⇒ 没有价格源。

	st, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-02"}, "u-1")
	mustNoErr(t, err, "AuditBusinessDays")
	if st.Presale != nil && len(st.Presale.Rows) > 0 {
		t.Errorf("没有价格客户端就不该做预售: %+v", st.Presale)
	}
}

// TestPresale_PromoPriceWins 锁 2.2 的"有促销价按促销价"。
func TestPresale_PromoPriceWins(t *testing.T) {
	svc, ctx, sku, _ := fmPresaleSetup(t)
	svc.SetCubeClient(&fmFakeCube{prices: map[string]fmFakePrice{
		sku: {retail: "150", promo: "120"},
	}})

	st, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-02"}, "u-1")
	mustNoErr(t, err, "AuditBusinessDays")
	if st.Presale == nil {
		t.Fatalf("应做预售")
	}
	// 预期收入 = 20kg × 80(促销价) = 1600;结余成本 1500 ⇒ 毛利 100。
	if !st.Presale.TotalExpectedRevenueYuan.Equal(d("2400")) {
		t.Errorf("预期收入应按促销价 80 ⇒ 1600,got %s", st.Presale.TotalExpectedRevenueYuan)
	}
	if !st.Presale.TotalMarginYuan.Equal(d("400")) {
		t.Errorf("预售毛利应是 100,got %s", st.Presale.TotalMarginYuan)
	}
}

// TestAudit_ReplaysPostCutoffMovesAndCalibratesBalance 锁 ⑤。
//
// 场景:补录 10-01 的盘点(柜台 10 → 5)⇒ 余额链变了,但**位置表**没变
// (补录不写位置表)⇒ 两者分叉。
// ⇒ 审计 10-02 ⇒ ① 截止日之后的动作重放;② 当前余额被校准到审计口径。
func TestAudit_ReplaysPostCutoffMovesAndCalibratesBalance(t *testing.T) {
	svc, ctx, sku, _ := fmPresaleSetup(t)

	// 补录 10-01 的盘点:柜台实盘 10 → 5。
	svc.SetClock(fmClock(fmDay2))
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-01",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("5")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("补录 10-01: %v", err)
	}
	// 位置表的冷库仍是 20(补录不动位置表),而链上 10-02 的结余已是 15。
	cold, _ := fmPositions(t, "S001", sku)
	if !cold.Equal(d("20")) {
		t.Fatalf("补录前位置表冷库应是 20,got %s", cold)
	}

	// 审计 10-02(没有 cube ⇒ 不做预售,只验重放与校准)。
	st, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-02"}, "u-1")
	mustNoErr(t, err, "AuditBusinessDays")
	if st.Replay == nil {
		t.Fatalf("审计结果应带重放信息")
	}
	if st.Replay.CutoffDate != "2026-10-02" {
		t.Errorf("重放截止日不对: %+v", st.Replay)
	}
	// 当前余额被校准:位置表冷库 20 → 15(= 截止日结余)。
	cold2, _ := fmPositions(t, "S001", sku)
	if !cold2.Equal(d("15")) {
		t.Errorf("当前余额应被校准到 15(= 截止日结余),got %s", cold2)
	}
	// 校准明细要给门店看得见。
	if len(st.Replay.Calibrations) == 0 {
		t.Errorf("校准明细不应为空")
	}
}

// TestAudit_ZeroesNegativeStockAndShowsDiffTable 锁"负库存在审计时清零 + 结存差异表"。
//
// 门店口径(2026-10-24):「负库存部分要在审计的时候清零,不带给下一期,
//   给一个审计结存差异表(只需填入这种特殊的被强制改数量的内容,
//   如没有则不出现审计结存差异表)」
//
// ⇒ 场景:冷库 20kg,门店搬了 25kg 上柜台(负库存只告警不拦截)
//   ⇒ 10-03 日终冷库 = −5kg ⇒ 审计时清零 ⇒ 差异表出现一行。
func TestAudit_ZeroesNegativeStockAndShowsDiffTable(t *testing.T) {
	svc, ctx, sku, _ := fmPresaleSetup(t)

	// 10-03:搬台 25kg(超过冷库余额 20kg ⇒ 负库存,只告警不拦截)+ 盘点(柜台 0)。
	day3 := fmDay2.Add(24 * time.Hour)
	svc.SetClock(fmClock(day3))
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		TakenDate: "2026-10-03",
		Lines:     []service.CounterLoadLine{{CubeProductID: sku, QtyKg: d("25")}},
	}, "u-1"); err != nil {
		t.Fatalf("10-03 搬台(超库存): %v", err)
	}
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-03",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("0")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("10-03 盘点: %v", err)
	}
	// 10-03 日终冷库 = 20 + 0(结转) − 25(搬台) = −5 ⇒ 负库存。
	_, cold := fmMarginClosing(t, "S001", day3, model.CutBelly)
	if !cold.Equal(d("-5")) {
		t.Fatalf("10-03 冷库应是 −5(负库存),got %s", cold)
	}

	// 审计 10-03 ⇒ 负库存清零 + 差异表。
	st, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-03"}, "u-1")
	mustNoErr(t, err, "AuditBusinessDays")
	if len(st.BalanceDiffs) != 1 {
		t.Fatalf("应有 1 行审计结存差异,got %+v", st.BalanceDiffs)
	}
	row := st.BalanceDiffs[0]
	if row.CutType != model.CutBelly {
		t.Errorf("差异行的部位不对: %+v", row)
	}
	if !row.BeforeKg.Equal(d("-5")) || !row.AfterKg.IsZero() {
		t.Errorf("差异应是 −5 → 0,got %+v", row)
	}
	if !row.DiffKg.Equal(d("5")) {
		t.Errorf("差异量应是 +5,got %s", row.DiffKg)
	}
	if row.Reason != service.BalanceDiffReasonNegativeZero {
		t.Errorf("原因应是负库存清零,got %q", row.Reason)
	}
	// 清零 ⇒ 快照为 0(下一期从 0 起算,不带给下一期)。
	var snap model.CutStockDaily
	if err := testDBSingleton.Where("branch_id = ? AND cut_type = ? AND biz_date LIKE ?",
		"S001", string(model.CutBelly), "2026-10-03%").First(&snap).Error; err != nil {
		t.Fatalf("查 10-03 结余快照: %v", err)
	}
	if !snap.ColdStoreKg.IsZero() {
		t.Errorf("10-03 的结余快照应被清零,got %s", snap.ColdStoreKg)
	}
	// 当前余额(位置表)也不再是负数。
	cold2, _ := fmPositions(t, "S001", sku)
	if cold2.IsNegative() {
		t.Errorf("当前余额不该再是负数,got %s", cold2)
	}
}

// TestAudit_NoBalanceDiffTableWhenNothingForced 锁"没有强制改动 ⇒ 不出现差异表"。
func TestAudit_NoBalanceDiffTableWhenNothingForced(t *testing.T) {
	svc, ctx, _, _ := fmPresaleSetup(t)
	st, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-02"}, "u-1")
	mustNoErr(t, err, "AuditBusinessDays")
	if len(st.BalanceDiffs) != 0 {
		t.Errorf("没有强制改动时不该有结存差异表,got %+v", st.BalanceDiffs)
	}
}
//
// 场景:10-03 搬台 5kg + 盘点(都在**审计之前**发生)⇒ 审计 10-02。
// ⇒ 10-03 在**截止日之后** ⇒ 它的搬台必须照样进流水、照样影响 10-03 的结余
//   (审计只冻结截止日及之前 ⇒ 不能把 10-03 的动作抹掉)。
func TestAudit_ReplayKeepsPostCutoffCounterLoad(t *testing.T) {
	svc, ctx, sku, _ := fmPresaleSetup(t)

	// 10-03:搬台 5kg + 盘点(在审计 10-02 **之前**发生)。
	day3 := fmDay2.Add(24 * time.Hour)
	svc.SetClock(fmClock(day3))
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		TakenDate: "2026-10-03",
		Lines:     []service.CounterLoadLine{{CubeProductID: sku, QtyKg: d("5")}},
	}, "u-1"); err != nil {
		t.Fatalf("10-03 搬台: %v", err)
	}
	if _, err := svc.RecordPorkCutsStocktake(ctx, "S001", model.RecordPorkCutsStocktakeInput{
		IsComplete: true,
		TakenDate:  "2026-10-03",
		Cuts: []model.CutInput{
			{CutType: model.CutBelly, CubeProductID: sku, ActualRemainKg: d("5")},
		},
	}, "u-1"); err != nil {
		t.Fatalf("10-03 盘点: %v", err)
	}

	st, err := svc.AuditBusinessDays(ctx, "S001", service.BizAuditInput{CutoffDate: "2026-10-02"}, "u-1")
	mustNoErr(t, err, "AuditBusinessDays")
	if st.Replay == nil {
		t.Fatalf("审计结果应带重放信息")
	}
	if !st.Replay.ReplayedCounterLoadKg.Equal(d("5")) {
		t.Errorf("审计后的搬台 5kg 应被重放(一行不丢),got %s", st.Replay.ReplayedCounterLoadKg)
	}
	if st.Replay.ReplayedDays < 1 {
		t.Errorf("应至少重放 1 天(10-03 在截止日之后),got %d", st.Replay.ReplayedDays)
	}
	// 10-03 的结余仍按"20 − 5(搬台) + 5(结转)"算 ⇒ 20kg(没被审计抹掉)。
	rep, err := svc.ComputeGrossMargin(ctx, "S001", day3)
	mustNoErr(t, err, "ComputeGrossMargin(10-03)")
	for _, r := range rep.PerSku {
		if r.CubeProductID != sku {
			continue
		}
		if !r.ClosingKg.Equal(d("20")) {
			t.Errorf("10-03 日终结余应是 20kg(搬台 5 + 结转 5 都算数),got %s", r.ClosingKg)
		}
	}
}
