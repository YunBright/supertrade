package service_test

// 回归锁:**没录日终实盘 ⇒ 毛利看板不能出现「实盘剩余」**。
//
// 2026-10-26 门店反馈(截图 + 复现步骤):
//   「我没录日终实盘,但是这里却有实盘剩余了。」
//   操作:10-02 录了一头猪 → 去毛利看板看数据 → 发现不对 → 又补录了一头猪
//        → 回来看,这个页面就有实盘剩余值了。
//
// # 根因
//
// 移动加权引擎在**无盘点日**把 ClosingKg 设成 expectedCloseKg(账面推演值)
// (见 moving_average.go 的 closeKg 分支)—— 那是**成本计算**需要的:
// 期末存货金额要进恒等式(期初+入库 = 销售+统收+报损+期末)。
//
// ⚠️ 但毛利看板的「实盘剩余」列直接取 ClosingKg ⇒ 推演值被**冒充成实盘**:
//   · 「实盘剩余」有值(其实是账面推演)
//   · 「差异」恒为 0(推演值 − 推演值 = 0)⇒ 看起来"盘点全对"
// ⇒ 门店按"我明明没盘"看数,却看到一份"实盘"。
//
// 生产现场佐证(店 00 / 2026-10-02):
//   pork_cuts_stocktakes 里 09-28~10-05 **一行都没有**(确实没录实盘)
//   而接口返回 data_source=estimated、is_stocktake_complete=false
//   却 still 给出了 actual_remain_kg == expected_remain_kg、variance_kg == 0。
//
// # 修复口径
//
// 「实盘剩余」与「差异」**只在有真实盘点(is_complete=true)时才有值**;
// 无盘点 ⇒ 留空(omitempty ⇒ JSON 里没有这个键)⇒ 前端显示「—」。
//
// ⚠️ **成本计算不受影响**:ClosingKg 在引擎内部仍按原样参与成本/恒等式。
// 这里只改**报表展示层**。
//
// ⚠️ 更细的一层:is_complete=false 的盘点单(半盘/草稿)**不算实盘**
// (is_complete 的语义见 model.go:「决定 BI 标注 ✓已盘点 / ⚠未盘点数据为推演」)
// ⇒ 口径标 partial、实盘列留空。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// findPerCut 在报表的部位行里按 cut_type 找一条。
func findPerCut(t *testing.T, rows []service.PerCutGrossMargin, ct model.CutType) *service.PerCutGrossMargin {
	t.Helper()
	for i := range rows {
		if rows[i].CutType == ct {
			return &rows[i]
		}
	}
	t.Fatalf("部位 %s 不在 per_cut: %+v", ct, rows)
	return nil
}

// bizLoc 是鲜肉域的营业日界时区。
var bizLoc = time.FixedZone("Asia/Shanghai", 8*3600)

// seedBellyPig 录一头猪 + 单品补录 belly 60kg(= 门店说的「补录了一头猪」),
// 让这一天有**真实产出**(而不是靠 LLM 推演)。
//
// ⚠️ 盘点单的 taken_at 取**服务端时钟**(不是入参)⇒ 必须先把时钟拨到那天。
func seedBellyPig(t *testing.T, svc *service.Service, day time.Time) *model.WholePig {
	t.Helper()
	svc.SetClock(func() time.Time {
		return time.Date(2026, 10, 2, 14, 0, 0, 0, bizLoc)
	})
	pig, err := svc.RecordWholePig(context.Background(), "S001",
		model.RecordWholePigInput{
			EarTag: "EB-ACT", GrossWeightKg: d("100"), PurchaseUnitPriceYuan: d("20"),
			SupplierID: "SUP-1", ArrivedAt: day,
		}, "u-1")
	mustNoErr(t, err, "RecordWholePig")
	// 单品补录:belly 60kg(真实产出)。
	//
	// ⚠️ **SKU 存量只来自单品补录的重量**(pig_cuts),**不是猪的毛重**
	// (毛重只用来摊成本 + 算切分损耗)⇒ 想让账面期末非 0,
	// 补录量必须 **大于** 当天的销量,否则期末就是 0。
	_, err = svc.RecordPigCut(context.Background(), "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutBelly, Barcode: "B-ACT",
		WeightKg: d("60"), ExpiresAt: day.Add(48 * time.Hour), CubeProductID: "P-2001",
	}, "u-1")
	mustNoErr(t, err, "RecordPigCut")
	return pig
}

// stocktakeMode 这一天有没有盘点单、是不是完整的。
type stocktakeMode int

const (
	noStocktake        stocktakeMode = iota // 当天没有任何盘点单
	incompleteStocktake                      // 有盘点单,但 is_complete=false(半盘/草稿)
	completeStocktake                        // 有盘点单且 is_complete=true
)

// seedGrossMarginScenario 造一天"有猪、有单品补录、有销售"的场景,
// 按 mode 录盘点单,返回当天的毛利报表。
//
// ⚠️ 每个场景用**独立的内存 SQLite**(setupTestService 每次新开)⇒ 场景之间互不污染。
func seedGrossMarginScenario(t *testing.T, svc *service.Service, mode stocktakeMode) *service.GrossMarginReport {
	t.Helper()
	day := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)

	_, err := svc.CreateBranchCutMapping(context.Background(), "S001",
		model.CreateBranchCutMappingInput{CutType: model.CutBelly, CubeProductID: "P-2001"})
	mustNoErr(t, err, "CreateBranchCutMapping belly")

	pig := seedBellyPig(t, svc, day)

	// 卖 20kg(补录 60kg ⇒ 账面期末 = 40kg)
	_, err = svc.OnSaleCompleted(context.Background(), service.SaleCompletedPayload{
		SaleID: "SO-1", BranchID: "S001", CompletedAt: day.Add(time.Hour),
		Lines: []service.SaleLine{
			{LineID: "L-1", SKUID: "P-2001", FreshType: "meat",
				Qty: d("20"), AmountYuan: d("600"), PigID: pig.ID, CutType: model.CutBelly},
		},
	})
	mustNoErr(t, err, "OnSaleCompleted")

	if mode != noStocktake {
		// 实盘 70kg(账面 40kg ⇒ 差异 +30)。
		_, err = svc.RecordPorkCutsStocktake(context.Background(), "S001",
			model.RecordPorkCutsStocktakeInput{
				IsComplete: mode == completeStocktake,
				Cuts: []model.CutInput{
					{CutType: model.CutBelly, CubeProductID: "P-2001", ActualRemainKg: d("70")},
				},
			}, "u-1")
		mustNoErr(t, err, "RecordPorkCutsStocktake")
	}

	report, err := svc.ComputeGrossMargin(context.Background(), "S001", day)
	mustNoErr(t, err, "ComputeGrossMargin")
	return report
}

// TestGrossMargin_NoStocktake_NoActualRemain 核心回归:
//
// 无盘点单 ⇒ 「实盘剩余」必须为空、且不能出现"差异=0"这种假的"对上了"。
func TestGrossMargin_NoStocktake_NoActualRemain(t *testing.T) {
	svc, _ := setupTestService(t)
	report := seedGrossMarginScenario(t, svc, noStocktake)

	// 口径标注必须说"这是推演"。
	if report.DataSource != "estimated" {
		t.Errorf("data_source = %q, want estimated(当日无盘点单)", report.DataSource)
	}
	if report.IsStocktakeComplete {
		t.Errorf("is_stocktake_complete 应为 false(当日无盘点单)")
	}

	belly := findPerCut(t, report.PerCut, model.CutBelly)

	// ⭐ 核心断言:「实盘剩余」必须为空(nil ⇒ JSON 里没有这个键)。
	if belly.ActualRemainKg != nil {
		t.Errorf("★ 无盘点日不该有实盘剩余: actual_remain_kg = %s(want 空)", *belly.ActualRemainKg)
	}
	// ⭐ 「差异」也不能是 0 —— 那是"推演−推演"的假账,看着像"盘点全对"。
	if belly.VarianceKg != nil {
		t.Errorf("★ 无盘点日不该有差异: variance_kg = %s(want 空)", *belly.VarianceKg)
	}
	// 「应有」照常有值(那是账面推演,门店要看的"账上该有多少")。
	if belly.ExpectedRemainKg.IsZero() {
		t.Errorf("expected_remain_kg 不该为 0(账面推演仍要展示)")
	}
}

// TestGrossMargin_IncompleteStocktake_NoActualRemain 锁 is_complete=false 的盘点单
// **不算**实盘(它的语义见 model.go:「决定 BI 标注 ✓已盘点 / ⚠未盘点数据为推演」)。
//
// ⚠️ 之前 loadSkuCounterAndCold 只查"当天有没有任何盘点单"
// ⇒ is_complete=false 的单被当成完整实盘 ⇒ 报表标"已盘点"却给的是推演值。
func TestGrossMargin_IncompleteStocktake_NoActualRemain(t *testing.T) {
	svc, _ := setupTestService(t)
	report := seedGrossMarginScenario(t, svc, incompleteStocktake)

	// 口径必须是 partial(有单但不完整),不能是 actual。
	if report.DataSource != "partial" {
		t.Errorf("★ is_complete=false 的盘点单不该标 actual: data_source = %q, want partial", report.DataSource)
	}
	if report.IsStocktakeComplete {
		t.Errorf("★ is_complete=false ⇒ is_stocktake_complete 应为 false")
	}

	belly := findPerCut(t, report.PerCut, model.CutBelly)
	if belly.ActualRemainKg != nil {
		t.Errorf("★ 非完整盘点不该进实盘剩余: actual_remain_kg = %s(want 空)", *belly.ActualRemainKg)
	}
	if belly.VarianceKg != nil {
		t.Errorf("★ 非完整盘点不该有差异: variance_kg = %s(want 空)", *belly.VarianceKg)
	}
}

// TestGrossMargin_StocktakeFlagOnlyChangesLabel 锁本次修复的**边界**:
//
// ⭐ 有盘点单时,「完整 / 半盘」只改**标注**(data_source)与**实盘列**,
//   **一个金额都不许变**(销售成本 / 应有 / 毛利 / 对账全部一致)。
//
// ⚠️ 这是"只改报表展示层,不动成本链"的机器可检版本:
//   如果哪天有人把 hasStocktake 跟 is_complete 绑在一起
//   (半盘日退化成推演期末),这个用例立刻红。
func TestGrossMargin_StocktakeFlagOnlyChangesLabel(t *testing.T) {
	svcPartial, _ := setupTestService(t)
	repPartial := seedGrossMarginScenario(t, svcPartial, incompleteStocktake)

	svcFull, _ := setupTestService(t)
	repFull := seedGrossMarginScenario(t, svcFull, completeStocktake)

	if repPartial.DataSource != "partial" || repFull.DataSource != "actual" {
		t.Fatalf("口径不对: partial=%q full=%q", repPartial.DataSource, repFull.DataSource)
	}

	p := findPerCut(t, repPartial.PerCut, model.CutBelly)
	f := findPerCut(t, repFull.PerCut, model.CutBelly)

	// 金额必须逐项相等。
	if !p.SalesCostYuan.Equal(f.SalesCostYuan) {
		t.Errorf("★ 半盘 vs 完整:销售成本变了 %s vs %s(不该变)",
			p.SalesCostYuan, f.SalesCostYuan)
	}
	if !p.ExpectedRemainKg.Equal(f.ExpectedRemainKg) {
		t.Errorf("★ 半盘 vs 完整:应有剩余变了 %s vs %s(不该变)",
			p.ExpectedRemainKg, f.ExpectedRemainKg)
	}
	if !p.GrossMarginYuan.Equal(f.GrossMarginYuan) {
		t.Errorf("★ 半盘 vs 完整:毛利变了 %s vs %s(不该变)",
			p.GrossMarginYuan, f.GrossMarginYuan)
	}
	if !repPartial.Reconciliation.DiffYuan.Equal(repFull.Reconciliation.DiffYuan) {
		t.Errorf("★ 半盘 vs 完整:对账差额变了 %s vs %s(不该变)",
			repPartial.Reconciliation.DiffYuan, repFull.Reconciliation.DiffYuan)
	}

	// 完整盘点 ⇒ 实盘列必须有值(别把功能改没了)。
	if f.ActualRemainKg == nil {
		t.Fatalf("★ 完整盘点时实盘剩余必须有值(现在是空)")
	}
	if !f.ActualRemainKg.IsPositive() {
		t.Errorf("★ 完整盘点时实盘剩余必须有值: actual_remain_kg = %s", *f.ActualRemainKg)
	}
}

// TestGrossMargin_CompleteStocktake_StillShowsActual 回归的反面:
// 有完整盘点时,「实盘剩余」与「差异」**必须照常有值**(别把功能改没了)。
func TestGrossMargin_CompleteStocktake_StillShowsActual(t *testing.T) {
	svc, _ := setupTestService(t)
	report := seedGrossMarginScenario(t, svc, completeStocktake)

	if report.DataSource != "actual" {
		t.Errorf("data_source = %q, want actual", report.DataSource)
	}
	if !report.IsStocktakeComplete {
		t.Errorf("is_stocktake_complete 应为 true")
	}

	belly := findPerCut(t, report.PerCut, model.CutBelly)
	if belly.ActualRemainKg == nil {
		t.Fatalf("★ 完整盘点时实盘剩余必须有值(现在是空)")
	}
	if !belly.ActualRemainKg.IsPositive() {
		t.Errorf("★ 完整盘点时实盘剩余必须有值: actual_remain_kg = %s", *belly.ActualRemainKg)
	}
	if belly.VarianceKg == nil {
		t.Fatalf("★ 完整盘点时差异必须有值(现在是空)")
	}
	// 差异 = 实盘 − 应有 ⇒ 必须能算出**非零**的差异(否则又变成"假对上了")。
	if belly.VarianceKg.IsZero() && !belly.ExpectedRemainKg.Equal(*belly.ActualRemainKg) {
		t.Errorf("★ 差异应为 实盘(%s) − 应有(%s) = %s,不该是 0",
			*belly.ActualRemainKg, belly.ExpectedRemainKg, *belly.VarianceKg)
	}
	// 恒等式:差异 = 实盘 − 应有(防止把两者搞反)。
	if !belly.VarianceKg.Equal(belly.ActualRemainKg.Sub(belly.ExpectedRemainKg)) {
		t.Errorf("variance = %s, want %s(实盘 − 应有)",
			*belly.VarianceKg, belly.ActualRemainKg.Sub(belly.ExpectedRemainKg))
	}
}

// TestGrossMargin_ActualRemain_JSONKeyMustDisappear 锁"**JSON 里这个键真的消失**"。
//
// ⚠️⚠️ 这是本次修复里**最容易再犯**的一个坑(2026-10-26 实测):
//
//	decimal.Decimal 是 **struct** ⇒ encoding/json 的 omitempty 对 struct **无效**
//	⇒ 用值类型时,零值照样序列化成 "actual_remain_kg":"0"
//	⇒ 前端拿到 0 ⇒ 显示"实盘剩余 0" ⇒ 门店以为"我盘出来是 0 kg"(还是假实盘!)
//
// ⇒ 必须用**指针**:nil ⇒ 整个键缺失 ⇒ 前端 parseOptionalDecimal ⇒ null ⇒ 显示「—」。
//
// ⇒ 这个用例直接查**序列化后的 JSON**,不查 Go 层的值
// (Go 层是 nil 不代表 JSON 里没这个键)。
func TestGrossMargin_ActualRemain_JSONKeyMustDisappear(t *testing.T) {
	perCutJSONKeys := func(t *testing.T, report *service.GrossMarginReport) []map[string]any {
		t.Helper()
		b, err := json.Marshal(report)
		mustNoErr(t, err, "marshal report")
		var m map[string]any
		mustNoErr(t, json.Unmarshal(b, &m), "unmarshal report")
		rows, ok := m["per_cut"].([]any)
		if !ok {
			t.Fatalf("per_cut 不是数组: %v", m["per_cut"])
		}
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			r, _ := row.(map[string]any)
			out = append(out, r)
		}
		return out
	}

	// ---- 无盘点日:两个键都**不该存在** ----
	svcNone, _ := setupTestService(t)
	repNone := seedGrossMarginScenario(t, svcNone, noStocktake)
	for _, r := range perCutJSONKeys(t, repNone) {
		if v, ok := r["actual_remain_kg"]; ok {
			t.Errorf("★ 无盘点日不该有 actual_remain_kg 键: %v", v)
		}
		if v, ok := r["variance_kg"]; ok {
			t.Errorf("★ 无盘点日不该有 variance_kg 键: %v", v)
		}
		// 「应有」必须有(它是账面推演,任何口径下都要给)。
		if _, ok := r["expected_remain_kg"]; !ok {
			t.Errorf("★ expected_remain_kg 该一直在: %v", r)
		}
	}

	// ---- 半盘日:同样不该有 ----
	svcPartial, _ := setupTestService(t)
	repPartial := seedGrossMarginScenario(t, svcPartial, incompleteStocktake)
	for _, r := range perCutJSONKeys(t, repPartial) {
		if v, ok := r["actual_remain_kg"]; ok {
			t.Errorf("★ 半盘日不该有 actual_remain_kg 键: %v", v)
		}
		if v, ok := r["variance_kg"]; ok {
			t.Errorf("★ 半盘日不该有 variance_kg 键: %v", v)
		}
	}

	// ---- 完整盘点日:两个键必须**在**(别把功能改没了) ----
	svcFull, _ := setupTestService(t)
	repFull := seedGrossMarginScenario(t, svcFull, completeStocktake)
	for _, r := range perCutJSONKeys(t, repFull) {
		if _, ok := r["actual_remain_kg"]; !ok {
			t.Errorf("★ 完整盘点日该有 actual_remain_kg 键: %v", r)
		}
		if _, ok := r["variance_kg"]; !ok {
			t.Errorf("★ 完整盘点日该有 variance_kg 键: %v", r)
		}
	}
}

// TestGrossMargin_NoStocktake_CostUnchanged 锁住"只改展示层,不动成本":
// 无盘点日的**销售成本 / 毛利**必须与修复前一致
// (引擎内部 ClosingKg 仍按账面推演参与成本计算)。
//
// ⚠️ 这是本次修复的**安全边界**:展示层不填实盘,成本照算。
func TestGrossMargin_NoStocktake_CostUnchanged(t *testing.T) {
	svc, _ := setupTestService(t)
	report := seedGrossMarginScenario(t, svc, noStocktake)

	belly := findPerCut(t, report.PerCut, model.CutBelly)

	// 成本必须非零(若为 0 说明"期末被清零"了 ⇒ 毛利虚高 100%)。
	if belly.SalesCostYuan.IsZero() {
		t.Errorf("★ 无盘点日的销售成本不该为 0(否则毛利虚高)")
	}
	// 毛利 = 收入 − 销售成本(此处无统收/报损)。
	wantGM := d("600").Sub(belly.SalesCostYuan)
	if !belly.GrossMarginYuan.Equal(wantGM) {
		t.Errorf("belly gm = %s, want %s(收入 600 − 销售成本 %s)",
			belly.GrossMarginYuan, wantGM, belly.SalesCostYuan)
	}
	// 对账恒等式:差额必须为 0(成本链没被改坏)。
	if !report.Reconciliation.DiffYuan.IsZero() {
		t.Errorf("★ 对账差额 = %s, want 0(成本链被改坏了)", report.Reconciliation.DiffYuan)
	}
	// decimal 精度护栏:确保用的是 decimal 而不是 float。
	if decimal.Zero.Equal(belly.SalesCostYuan) {
		t.Errorf("销售成本比较应走 decimal")
	}
}
