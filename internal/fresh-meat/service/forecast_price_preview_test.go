package service_test

// 2026-10-31:「按基线预览 / 重算」通道 —— owner 要把 29 日的样本倒推出的
// 「分割基线 + 部位进货单价基线」应用到 30 日,不想再靠「删了重录」。
//
// # 锁的是什么(owner 的原话,不是实现细节)
//
//	① **预览**不写库 ⇒ 录猪前就能看到"基线会给什么";
//	② 预览的建议成本单价 **= 报表同源** ⇒ 录猪页和毛利看板不会打架;
//	③ 预览用**基线口径**(不取 LLM 兜底)⇒ 门店看到的就是"纯基线会给什么";
//	④ 单猪进价缩放:基线是 14.6/kg 的,16/kg 的猪每个部位单价都要按比例上涨;
//	⑤ **断环**:预览 D 日时,单价基线只用 < D 的样本;
//	⑥ **边界②**:各部位合计价值 ≤ 猪的总进货金额(封顶 ⇒ 必须让门店看见);
//	⑦ **重算已录的猪**:切「只用基线」+ 清掉 AI 建议 + 重算成本链;
//	⑧ 切「只用基线」前必须校验该体重段有可用基线(否则这头猪一条建议都没有 → 假短卖);
//
// ⚠️ 这些用例都走**真实 SQLite** + 公开 API ⇒ 引擎接线错了也会红。

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// previewDay 是预览通道用的营业日(2026-10-30 06:00 UTC = 14:00 +08)。
var previewDay = time.Date(2026, 10, 30, 6, 0, 0, 0, time.UTC)

// previewSetup 起服务(时钟停在 10-30)+ 造一条 belly 的**重量基线**。
//
// ⚠️ 重量基线是「建议分割」的来源(没有它 ⇒ 建议分割全空 ⇒ 预览无意义)。
func previewSetup(t *testing.T) (*service.Service, string) {
	t.Helper()
	svc, branch := priceSetup(t)
	// ⚠️ priceSetup 把时钟停在 priceDay(10-29)⇒ 这里挪到 10-30,
	//   让"预览 10-30"符合"基线只用 < 10-30 的样本"这条断环口径。
	svc.SetClock(func() time.Time { return previewDay })
	// 体重段必须和预览的猪一致(100kg ⇒ 见 WeightBandOf)。
	splitBaseline(t, model.CutBelly, service.WeightBandOf(d("100")), "40")
	return svc, branch
}

// ---- ① 预览不写库 ----

// TestPreview_DoesNotWriteAnything 锁「预览是**只读**的」。
//
// ⚠️ owner 的动作是"录猪时先看看基线会给什么"⇒ 预览**绝不能**留下任何痕迹
//   (不能建猪、不能改基线、不能落快照)⇒ 否则门店点一次预览就脏了数据。
func TestPreview_DoesNotWriteAnything(t *testing.T) {
	svc, branch := previewSetup(t)
	// 单价日样本(10-28)⇒ 10-30 预览时可用(断环:只取 < 10-30)。
	priceSample(t, "2026-10-28", model.CutBelly, "10", "200", "20", "14.6")

	before := countPigs(t)
	out, err := svc.PreviewCutPrice(t.Context(), branch, previewDay, d("100"), d("14.6"))
	mustNoErr(t, err, "PreviewCutPrice")
	if len(out.Rows) == 0 {
		t.Fatalf("★ 预览应给出 belly 的建议,got %v", out)
	}
	if got := countPigs(t); got != before {
		t.Errorf("★ 预览**不能**建猪(预览前 %d 行,预览后 %d 行)", before, got)
	}
}

// ---- ② 预览 = 报表同源 ----

// TestPreview_MatchesReportCostPrice 锁「预览的建议成本单价 = 毛利看板那一行」。
//
// ⚠️ 这是 owner「我这样才能看到效果」的**前提**:
//   录猪页看到的建议单价,必须和毛利看板「成本单价」列的建议行**同一个数**
//   ⇒ 否则门店在两个页面看到两个数 ⇒ 以为系统乱套。
//
// 做法:同一头猪、同一天,分别走「预览」和「报表的建议成本单价」两条路,比数值。
func TestPreview_MatchesReportCostPrice(t *testing.T) {
	svc, branch := previewSetup(t)
	priceSample(t, "2026-10-28", model.CutBelly, "10", "200", "20", "14.6")

	preview, err := svc.PreviewCutPrice(t.Context(), branch, previewDay, d("100"), d("14.6"))
	mustNoErr(t, err, "PreviewCutPrice")

	// 报表口径:同一头猪(同样的重量/进价/建议分割)走 suggestedCutUnitPrices。
	pig := model.WholePig{
		ID:                    "PIG-PREVIEW",
		GrossWeightKg:         d("100"),
		PurchaseUnitPriceYuan: d("14.6"),
		PurchaseCostYuan:      d("100").Mul(d("14.6")),
		AdviceMode:            model.AdviceModeBaseline,
	}
	split := map[string]map[string]decimal.Decimal{
		pig.ID: {string(model.CutBelly): d("40")},
	}
	unit, _, err := service.SuggestedCutUnitPricesForTest(
		t.Context(), svc, branch, []model.WholePig{pig}, split, previewDay)
	mustNoErr(t, err, "SuggestedCutUnitPrices")
	reportPrice := unit[string(model.CutBelly)]

	var previewPrice decimal.Decimal
	found := false
	for _, r := range preview.Rows {
		if r.CutType == model.CutBelly {
			found = true
			if r.SuggestedCostUnitPriceYuan != nil {
				previewPrice = *r.SuggestedCostUnitPriceYuan
			}
		}
	}
	if !found {
		t.Fatalf("★ 预览里应有 belly 行,got %v", preview.Rows)
	}
	if !priceEq(previewPrice, reportPrice) {
		t.Errorf("★ 预览价(%s)必须 = 报表建议价(%s)(同源,不能两个数)",
			previewPrice, reportPrice)
	}
}

// ---- ④ 单猪进价缩放 ----

// TestPreview_ScalesByPigPrice 锁「基线按**这头猪的进价**等比缩放」。
//
// owner 原话:「基线是 14.6/kg 的,那 16/kg 的猪每个部位单价都要按比例上涨」。
//
// 数据:基线单价 20 元/kg(参考猪进价 14.6)
//   ⇒ 预览 14.6/kg 的猪 ⇒ 20 元/kg
//   ⇒ 预览 16.0/kg 的猪 ⇒ 20 × 16/14.6 = **21.9178…** 元/kg
func TestPreview_ScalesByPigPrice(t *testing.T) {
	svc, branch := previewSetup(t)
	priceSample(t, "2026-10-28", model.CutBelly, "10", "200", "20", "14.6")

	base, err := svc.PreviewCutPrice(t.Context(), branch, previewDay, d("100"), d("14.6"))
	mustNoErr(t, err, "PreviewCutPrice")
	up, err := svc.PreviewCutPrice(t.Context(), branch, previewDay, d("100"), d("16"))
	mustNoErr(t, err, "PreviewCutPrice")

	basePrice := previewPriceOf(base, model.CutBelly)
	upPrice := previewPriceOf(up, model.CutBelly)
	if basePrice.IsZero() || upPrice.IsZero() {
		t.Fatalf("★ 应能取到 belly 的建议单价(base=%s up=%s)", basePrice, upPrice)
	}
	if !priceEq(basePrice, d("20")) {
		t.Errorf("★ 14.6/kg 的猪应得基线原价 20,got %s", basePrice)
	}
	want := d("20").Mul(d("16")).Div(d("14.6"))
	if !priceEq(upPrice, want) {
		t.Errorf("★ 16/kg 的猪单价应按比例涨到 %s,got %s", want, upPrice)
	}
}

// ---- ⑤ 断环 ----

// TestPreview_AsOfBreaksCircularDependency 锁「预览 D 日时,单价基线只用 **< D** 的样本」。
//
// ⚠️ 这是 owner 点名的「数据依赖循环问题」的**结构性**解法:
//   倒推单价需要当日毛利率 ⇒ 毛利率来自成本链 ⇒ 若基线把**当天**样本算进去,
//   就成了「今天的毛利率 → 今天的单价 → 今天的毛利率」。
//
// ⚠️ 方向别搞反(owner 2026-10-31 的原话):
//   「我打算用 29 日的样本倒推出的基线应用到 30 日去」
//   ⇒ **29 日的样本必须参与** 30 日的预览(只有 30 日自己的样本不许参与)。
//
// 断言:预览 10-30 时,10-29 的样本生效;10-30 当天那份荒谬样本**不**生效。
func TestPreview_AsOfBreaksCircularDependency(t *testing.T) {
	svc, branch := previewSetup(t)
	// 10-29 的样本(owner 要用的那份):20 元/kg。
	priceSample(t, "2026-10-29", model.CutBelly, "10", "200", "20", "14.6")
	// ⚠️ 10-30 **当天**的样本给 999 元/kg(荒谬)⇒ 若它混进来,预览价会离谱。
	priceSample(t, "2026-10-30", model.CutBelly, "10", "9990", "999", "14.6")

	out, err := svc.PreviewCutPrice(t.Context(), branch, previewDay, d("100"), d("14.6"))
	mustNoErr(t, err, "PreviewCutPrice")
	got := previewPriceOf(out, model.CutBelly)
	if !priceEq(got, d("20")) {
		t.Fatalf("★ 预览 10-30 应只生效 10-29 的样本(20),当天(10-30)的样本不许混进来,got %s", got)
	}
}

// ---- ⑥ 边界②:封顶 ----

// TestPreview_CapsAtPigPurchaseCost 锁 owner 给的第二条边界(在预览里也生效)。
//
// 猪 10kg @ 10 元/kg ⇒ 总进货金额 **100 元**;
// 基线单价 1000 元/kg × 建议分割 8kg = 8000 元 ⇒ **远超** 100 元
// ⇒ 必须封顶:单价缩到 100/8 = **12.5 元/kg**;
// 且 **capped 标志必须是 true** ⇒ 门店看得见"系统帮我砍过了"。
func TestPreview_CapsAtPigPurchaseCost(t *testing.T) {
	svc, branch := previewSetup(t)
	priceSample(t, "2026-10-28", model.CutBelly, "10", "10000", "1000", "10")
	// ⚠️ 重量基线改成 8kg(否则 40kg × 12.5 会先被猪重截断,测不到封顶)。
	splitBaseline(t, model.CutBelly, service.WeightBandOf(d("10")), "8")

	out, err := svc.PreviewCutPrice(t.Context(), branch, previewDay, d("10"), d("10"))
	mustNoErr(t, err, "PreviewCutPrice")
	got := previewPriceOf(out, model.CutBelly)
	if !priceEq(got, d("12.5")) {
		t.Errorf("★ 合计价值超过猪的总进货金额 ⇒ 单价应封顶到 12.5(100/8),got %s", got)
	}
	if !out.Capped {
		t.Errorf("★ 发生了封顶,capped 标志必须是 true(门店要看得见),got false")
	}
	if out.TotalValueYuan.GreaterThan(d("100")) {
		t.Errorf("★ 合计价值 %s 超过了猪的总进货金额 100", out.TotalValueYuan)
	}
}

// ---- ⑦⑧ 重算已录的猪 ----

// TestApplyBaseline_SetsModeAndClearsAdvice 锁「切『只用基线』的落库效果」。
//
// 断言:
//   - advice_mode = baseline
//   - llm_advice_json 被清空(AI 建议不再参与)
//   - advice_status = none(前端显示"没有 AI 建议")
//   - data_source = stub
func TestApplyBaseline_SetsModeAndClearsAdvice(t *testing.T) {
	svc, branch := previewSetup(t)
	priceSample(t, "2026-10-28", model.CutBelly, "10", "200", "20", "14.6")
	pig := pricePig(t, "PIG-1", "100", "14.6", previewDay)
	// ⚠️ 给它一份 LLM 建议(模拟"这头猪已经有 AI 建议了")。
	mustNoErr(t, testDBSingleton.Model(&model.WholePig{}).Where("id = ?", pig.ID).
		Update("llm_advice_json", []byte(`{"cuts":[{"cut_type":"belly","expected_kg":55}]}`)).Error,
		"写 llm_advice_json")

	preview, err := svc.ApplyBaselineToPig(t.Context(), branch, pig.ID, "baseline")
	mustNoErr(t, err, "ApplyBaselineToPig")
	if preview.AdviceMode != model.AdviceModeBaseline {
		t.Errorf("★ 返回的 advice_mode 应是 baseline,got %q", preview.AdviceMode)
	}

	got := reloadPig(t, svc, branch, pig.ID)
	if got.AdviceMode != model.AdviceModeBaseline {
		t.Errorf("★ 库里 advice_mode 应是 baseline,got %q", got.AdviceMode)
	}
	if len(got.LLMAdviceJSON) != 0 {
		t.Errorf("★ 切成基线后 AI 建议应被清空(免得门店以为 AI 还在生效),got %s", got.LLMAdviceJSON)
	}
	if got.AdviceStatus != model.AdviceStatusNone {
		t.Errorf("★ advice_status 应是 none,got %q", got.AdviceStatus)
	}
	if string(got.DataSource) != string(model.DataSourceStub) {
		t.Errorf("★ data_source 应是 stub,got %q", got.DataSource)
	}
}

// TestApplyBaseline_RejectsWhenNoBaselineForBand 锁⑧:切「只用基线」前必须校验体重段。
//
// ⚠️ baseline 模式 = **基线没覆盖的部位没有建议** ⇒ 若这个体重段压根没有基线,
//   这头猪就一条建议都没有 ⇒ 当天产出凭空少一整头猪
//   ⇒ 到处是「缺口 / 短卖 / 幻影搭赠」⇒ 门店会以为系统算错了。
//
// ⇒ 必须 **400 拦住**,并说清"先补基线,或改用自动"。
func TestApplyBaseline_RejectsWhenNoBaselineForBand(t *testing.T) {
	svc, branch := priceSetup(t)
	// ⚠️ 故意**不造**重量基线 ⇒ 该体重段没有可用基线。
	pig := pricePig(t, "PIG-1", "100", "14.6", previewDay)

	_, err := svc.ApplyBaselineToPig(t.Context(), branch, pig.ID, "baseline")
	if err == nil {
		t.Fatalf("★ 没有可用基线时切成「只用基线」必须被拒绝(否则这头猪一条建议都没有)")
	}
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("★ 应是 ErrInvalidInput,got %v", err)
	}
	// 断言错误文案点名了"基线"(门店要知道去做什么)。
	if !strings.Contains(err.Error(), "基线") {
		t.Errorf("★ 错误文案应点名「基线」(告诉门店去补基线),got %q", err.Error())
	}
	// ⚠️ 被拒 ⇒ **不能**改库。
	got := reloadPig(t, svc, branch, pig.ID)
	if got.AdviceMode == model.AdviceModeBaseline {
		t.Errorf("★ 被拒绝后不该改库(advice_mode 仍是 baseline)")
	}
}

// TestApplyBaseline_RejectsCrossBranch 锁「不能改别店的猪」。
func TestApplyBaseline_RejectsCrossBranch(t *testing.T) {
	svc, _ := previewSetup(t)
	pig := pricePig(t, "PIG-1", "100", "14.6", previewDay)

	_, err := svc.ApplyBaselineToPig(t.Context(), "S999", pig.ID, "baseline")
	if !errors.Is(err, service.ErrBranchMismatch) {
		t.Errorf("★ 跨店操作应是 ErrBranchMismatch,got %v", err)
	}
}

// TestApplyBaseline_RejectsUnknownMode 锁「mode 只能是 baseline / auto」。
func TestApplyBaseline_RejectsUnknownMode(t *testing.T) {
	svc, branch := previewSetup(t)
	pig := pricePig(t, "PIG-1", "100", "14.6", previewDay)

	_, err := svc.ApplyBaselineToPig(t.Context(), branch, pig.ID, "llm")
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("★ 非法 mode 应是 ErrInvalidInput,got %v", err)
	}
}

// ---- 工具 ----

// previewPriceOf 取预览里某部位的建议成本单价(取不到 ⇒ 0)。
func previewPriceOf(p *service.CutPricePreview, cut model.CutType) decimal.Decimal {
	for _, r := range p.Rows {
		if r.CutType == cut && r.SuggestedCostUnitPriceYuan != nil {
			return *r.SuggestedCostUnitPriceYuan
		}
	}
	return decimal.Zero
}

// countPigs 数库里有多少头猪(用来锁"预览不写库")。
func countPigs(t *testing.T) int64 {
	t.Helper()
	var n int64
	mustNoErr(t, testDBSingleton.Model(&model.WholePig{}).Count(&n).Error, "count whole_pigs")
	return n
}
