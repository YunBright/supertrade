package fact

import (
	"strings"
	"testing"
)

func hasStock(qty, min float64) StockRecord {
	return StockRecord{ItemNo: "X", Quantity: qty, MinQty: min, HasRow: true}
}

// ⚠️ 本文件是整个产品**最不能出错**的逻辑。
//
// 归因判错的代价是真实的经营损失：
//   把缺货说成滞销 → 店长砍掉一个爆款 → 永久失去这个品
//   把整体下滑说成单品问题 → 店长砍掉一堆本该补货的品
//
// 所以每条规则都配了**反证用例**：不只测"该判成什么"，
// 也测"不该判成什么"（比如有货时绝不能判缺货）。
func TestAttributeCauses(t *testing.T) {
	cases := []struct {
		name      string
		in        AttributionInput
		wantCause DeclineCause
		// wantEvidence 必须包含的关键词 —— 证据要能让人核对
		wantEvidence string
	}{
		{
			// 核心场景：卖不动其实是因为没货
			name: "缺货优先于一切",
			in: AttributionInput{
				Product:    TopProduct{ItemNo: "A", Amount: 0, DeltaPct: -100},
				PrevAmount: 500, HasPrev: true,
				Stock:       hasStock(0, 12),
				// 注意：品类也在跌。缺货**仍然**优先。
				CategoryChangePct: -30, HasCategoryChange: true,
			},
			wantCause:    CauseStockout,
			wantEvidence: "缺货",
		},
		{
			name: "低库存也算非正常下滑",
			in: AttributionInput{
				Product:    TopProduct{ItemNo: "A", Amount: 20, DeltaPct: -60},
				PrevAmount: 50, HasPrev: true,
				Stock:       hasStock(3, 12),
			},
			wantCause:    CauseLowStock,
			wantEvidence: "安全库存",
		},
		{
			// 关键对照：有货 + 品类同步大跌 ⇒ 不是单品问题
			name: "品类同步下滑判为整体下滑",
			in: AttributionInput{
				Product:    TopProduct{ItemNo: "A", Amount: 40, DeltaPct: -60},
				PrevAmount: 100, HasPrev: true,
				Stock:       hasStock(80, 12),
				CategoryChangePct: -25, HasCategoryChange: true,
			},
			wantCause:    CauseMarketWide,
			wantEvidence: "整体环境",
		},
		{
			name: "有货且品类稳定才判需求下降",
			in: AttributionInput{
				Product:    TopProduct{ItemNo: "A", Amount: 40, DeltaPct: -60},
				PrevAmount: 100, HasPrev: true,
				Stock:       hasStock(80, 12),
				CategoryChangePct: 3, HasCategoryChange: true,
			},
			wantCause:    CauseDemandDrop,
			wantEvidence: "换供应商",
		},
		{
			// 反证：新商品没有上期数据，不能判任何下降
			name: "新品不下结论",
			in: AttributionInput{
				Product: TopProduct{ItemNo: "NEW", Amount: 100, DeltaPct: 0},
				Stock:   hasStock(50, 10),
			},
			wantCause:    CauseNewOrUnknown,
			wantEvidence: "上期无销售数据",
		},
		{
			// 反证：没跌就不归因
			name: "没跌不归因",
			in: AttributionInput{
				Product:    TopProduct{ItemNo: "A", Amount: 120, DeltaPct: 20},
				PrevAmount: 100, HasPrev: true,
				Stock:       hasStock(80, 12),
			},
			wantCause: CauseNone,
		},
		{
			// 反证：查不到库存 ≠ 缺货。
			// 这是最容易犯的错：HasRow=false 时 Quantity 是 0，
			// 若直接按数值判会得出"库存为 0 ⇒ 缺货"，
			// 于是一个从未纳入库存系统的临时摊位商品会被标成断货，
			// 店长去补一批根本不该管的货。
			name: "查不到库存记录不判缺货",
			in: AttributionInput{
				Product:    TopProduct{ItemNo: "A", Amount: 10, DeltaPct: -80},
				PrevAmount: 50, HasPrev: true,
				Stock:       StockRecord{ItemNo: "A", Quantity: 0, HasRow: false},
			},
			wantCause: CauseDemandDrop,
		},
		{
			// 反证：门店没设安全库存线时，全部低库存的判定会失效。
			name: "未设安全库存不算低库存",
			in: AttributionInput{
				Product:    TopProduct{ItemNo: "A", Amount: 10, DeltaPct: -80},
				PrevAmount: 50, HasPrev: true,
				Stock:       hasStock(1, 0), // 有库存但 min_qty=0
			},
			wantCause: CauseDemandDrop,
		},
		{
			// 边界：跌 19% 不算"下降"（阈值 -20%）
			name: "小幅跌幅不归因",
			in: AttributionInput{
				Product:    TopProduct{ItemNo: "A", Amount: 82, DeltaPct: -18},
				PrevAmount: 100, HasPrev: true,
				Stock:       hasStock(80, 12),
			},
			wantCause: CauseNone,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, evidence := Attribute(c.in)
			if got != c.wantCause {
				t.Fatalf("归因 = %q, 期望 %q（证据:%s）", got, c.wantCause, evidence)
			}
			if c.wantEvidence != "" && !strings.Contains(evidence, c.wantEvidence) {
				t.Fatalf("证据 %q 未包含 %q", evidence, c.wantEvidence)
			}
		})
	}
}

// 归因优先用传入的 PrevAmount 重算环比，而不是信 TopProduct.DeltaPct。
//
// 理由：上层组装 TopProduct 时可能用了不同口径（按天平均 vs 合计），
// 两处不一致会让"报表上显示跌 15%、归因却说跌 60%"，
// 店长会认为报告自相矛盾。
func TestAttributeRecomputesPctFromPrevAmount(t *testing.T) {
	_, evidence := Attribute(AttributionInput{
		Product:    TopProduct{ItemNo: "A", Amount: 40, DeltaPct: -99}, // 故意给错的 pct
		PrevAmount: 100, HasPrev: true,
		Stock:      hasStock(80, 12),
	})
	// 用 PrevAmount=100、Amount=40 重算应为 -60%
	if !strings.Contains(evidence, "60.0") {
		t.Fatalf("证据应基于重算后的 -60.0%%，实际 %q", evidence)
	}
	if strings.Contains(evidence, "99") {
		t.Fatalf("证据不应采用传入的 DeltaPct: %q", evidence)
	}
}

// 「卖得好但断货」才值得补货；从来没卖过的断货不值得补。
//
// 这是 P0 价值的核心：让店长早上就知道该订什么。
// 把没卖过的断货商品也塞进补货清单，会浪费他的订货时间。
func TestBuildStockoutsOnlyIncludesItemsThatSold(t *testing.T) {
	products := []TopProduct{
		{ItemNo: "SOLD", Name: "西红柿", Amount: 300, PrevAmount: 400},
		{ItemNo: "NEVER", Name: "从没卖过的临时品", Amount: 0, PrevAmount: 0},
		{ItemNo: "PREV", Name: "上期爆款今断货", Amount: 0, PrevAmount: 800},
		{ItemNo: "PLENTY", Name: "库存充足", Amount: 500, PrevAmount: 500},
		{ItemNo: "NOSTOCK", Name: "无库存记录", Amount: 200, PrevAmount: 200},
	}
	stocks := map[string]StockRecord{
		"SOLD":    hasStock(0, 12),
		"NEVER":   hasStock(0, 5),
		"PREV":    hasStock(0, 20),
		"PLENTY":  {ItemNo: "PLENTY", Quantity: 100, MinQty: 10, HasRow: true},
		// NOSTOCK 故意不在 map 里 ⇒ 查不到
	}

	got := BuildStockouts(products, stocks)
	gotNos := make([]string, 0, len(got))
	for _, g := range got {
		gotNos = append(gotNos, g.ItemNo)
	}
	// 同为断货时，按「本期+上期合计」降序：
	//   PREV = 0 + 800 = 800，应排在 SOLD = 300 + 400 = 700 之前。
	// 用的是**两期合计**而不是只看本期 —— 一个上期卖 800、本期归零的断货品
	// 显然比一个两期都平稳在 300 的更值得补货。
	want := []string{"PREV", "SOLD"}
	if len(gotNos) != len(want) {
		t.Fatalf("补货清单 = %v, 期望 %v（NEVER 从没卖过、NOSTOCK 无库存记录、PLENTY 库存充足都应排除）",
			gotNos, want)
	}
	for i := range want {
		if gotNos[i] != want[i] {
			t.Fatalf("补货清单顺序 = %v, 期望 %v", gotNos, want)
		}
	}
}

// 库存状态判定：查不到 ≠ 为零。
func TestClassifyStock(t *testing.T) {
	cases := []struct {
		name string
		rec  StockRecord
		want StockStatus
	}{
		{"无记录", StockRecord{HasRow: false}, StockNone},
		{"有记录且为 0", hasStock(0, 12), StockOut},
		{"有记录为负数视为 0", hasStock(-3, 12), StockOut},
		{"低于安全线", hasStock(5, 12), StockLow},
		{"等于安全线算正常", hasStock(12, 12), StockOK},
		{"高于安全线", hasStock(50, 12), StockOK},
		{"未设安全线的低库存仍算正常", hasStock(1, 0), StockOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyStock(c.rec); got != c.want {
				t.Fatalf("ClassifyStock = %q, 期望 %q", got, c.want)
			}
		})
	}
}

// TopN 必须夹住上限 —— 配置写错成 1000 会把整年商品塞进 prompt。
func TestTopNIsClamped(t *testing.T) {
	if got := TopN(0); got != DefaultTopN {
		t.Errorf("TopN(0) = %d, 期望 %d", got, DefaultTopN)
	}
	if got := TopN(-5); got != DefaultTopN {
		t.Errorf("TopN(负数) = %d, 期望 %d", got, DefaultTopN)
	}
	if got := TopN(5); got != 5 {
		t.Errorf("TopN(5) = %d, 期望 5", got)
	}
	if got := TopN(1000); got != MaxTopN {
		t.Errorf("TopN(1000) = %d, 期望被夹到 %d（防 token 成本失控）", got, MaxTopN)
	}
}
