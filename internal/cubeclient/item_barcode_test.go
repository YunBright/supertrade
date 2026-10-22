// 本文件锁住「cube 路径的商品搜索也能按**条形码**命中」这条契约。
//
// # 为什么要在 supertrade 侧解决,而不是改 cube 的 product model
//
//	思迅的商品条码在**另一张模型**上(`item_barcode`,live),
//	而 `product` 是 **duck** 模型(增量刷新的 DuckDB 快照)。
//
//	cube 的 `pkg/cubequery/join.go::checkStorage` 规定:
//	**join 的 base 必须是 storage: live**,被 join 的也必须是 live。
//	⇒ `product`(duck) ← `item_barcode`(live) 这条 1:N 关系**编译不出来**。
//	⇒ 要 join 就得把 product 改成 live ⇒ **每次商品查询直连源库 27567 行**
//	   ⇒ 那才是真正的性能灾难(product 用 DuckDB 快照正是为了避免这个)。
//
//	⇒ 所以走 cube 仓自己在 `item_barcode/schema.yaml` 里写的办法:
//	   **「调用方分两次查,再在 Go 侧拼」**。
//	   本文件锁的就是这个拼法。
//
// # 口径必须与**本地路径**一致
//
//	本地路径(门店 10,本域 PG):子查询反查 `product_barcodes`。
//	cube 路径(门店 00):查 cube 的 `item_barcode` 模型。
//	⇒ **同一个关键词,两条路径必须给出同一批候选**。
//	  否则"门店 10 扫得出、门店 00 扫不出"⇒ 契约就废了。
//
// # 性能(2026-10-22 生产实测)
//
//	item_barcode 只有 **155 行 / 111 个商品**(每商品 1~7 条,孤儿 0)。
//	⇒ 整表缓存 60 秒,Go 侧做子串匹配 ⇒ **关键词不是条形码时 0 次额外查询**。
//	⇒ 门店 00 的搜索延迟(实测 ~880ms)基本不变;
//	   真扫到独立条码时多一趟 `product.id in (...)`(~290ms)。
package cubeclient

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- 工具 ----

// productRow 造一行 cube 的 product 查询结果。
func productRow(id, name string) map[string]any {
	return map[string]any{
		"product.id":           id,
		"product.name":         name,
		"product.category_id":  "C1",
		"product.supplier_id":  "S1",
		"product.unit":         "包",
		"product.status":       "1",
		"product.sub_code":     "SC-" + id,
		"product.avg_price_yuan": 12.5,
	}
}

// barcodeRow 造一行 cube 的 item_barcode 查询结果。
func barcodeRow(barcode, productID string) map[string]any {
	return map[string]any{
		"item_barcode.barcode":   barcode,
		"item_barcode.product_id": productID,
		"item_barcode.count":     1,
	}
}

// bodyMeasures 取 body 的 measures。
func bodyMeasures(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, _ := body["measures"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// bodyMember 取 body 的 dimensions。
func bodyMember(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, _ := body["dimensions"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// ---- 匹配口径 ----

// TestBarcodeProductIDs_MatchesContainsCI 锁"Go 侧匹配与 cube 的 containsCI 同口径"。
//
// ⚠️ cube 的 containsCI 编译成 `LOWER(RTRIM(col)) LIKE '%lower(v)%'`
//   ⇒ 列侧去空格、模式侧小写。
//   ⇒ 这里必须做同样的事,否则"同一关键词两条路径答案不同"。
func TestBarcodeProductIDs_MatchesContainsCI(t *testing.T) {
	rows := []ItemBarcodeDTO{
		{Barcode: "6970367140630", ProductID: "02000089"},
		{Barcode: "6970367140920", ProductID: "02000089"},
		{Barcode: "02000116", ProductID: "02000116"},
		{Barcode: "  6908946288185  ", ProductID: "P1"},
	}
	cases := []struct {
		kw   string
		want []string
	}{
		{"6970367140630", []string{"02000089"}},  // 精确
		{"6970367140", []string{"02000089"}},    // 子串
		{"69703671406", []string{"02000089"}},   // 更短的子串
		{"6970367140630", []string{"02000089"}}, // 重复关键词
		{"02000116", []string{"02000116"}},
		{"6908946288185", []string{"P1"}}, // 列侧有空格
		{"690894628818", []string{"P1"}},  // 子串
		{"", nil},                        // 空关键词 ⇒ 不命中
		{"zzz", nil},                     // 不命中
	}
	for _, c := range cases {
		got := barcodeProductIDs(rows, c.kw)
		assert.Equal(t, c.want, got, "关键词 %q 的命中集合不对", c.kw)
	}
}

// TestBarcodeProductIDs_CaseInsensitive 锁"大小写无关"(与 cube containsCI 一致)。
func TestBarcodeProductIDs_CaseInsensitive(t *testing.T) {
	rows := []ItemBarcodeDTO{{Barcode: "Z005-69", ProductID: "P1"}}
	assert.Equal(t, []string{"P1"}, barcodeProductIDs(rows, "z005-69"))
	assert.Equal(t, []string{"P1"}, barcodeProductIDs(rows, "Z005-69"))
}

// ---- 查询形状 ----

// TestSearchProducts_BarcodeHit_AddsProductIDInQuery 锁"条形码命中 ⇒ 多发一趟 id in 查询"。
//
// ⚠️ 这一趟**只在真命中时**发 —— 不命中时查询次数必须还是 3(见下一个用例)。
func TestSearchProducts_BarcodeHit_AddsProductIDInQuery(t *testing.T) {
	inv := &recordingInvoker{}
	// 调用次序:① product.id containsCI ② product.name containsCI
	//          ③ product.sub_code containsCI ④ item_barcode 全表
	//          ⑤ product.id in (条形码命中的货号)
	inv.responds = [][]map[string]any{
		{},                                          // ① 不命中
		{},                                          // ② 不命中
		{},                                          // ③ 不命中
		{barcodeRow("6970367140630", "02000089")},    // ④ 条码表
		{productRow("02000089", "靓婆婆/亚弟")},       // ⑤ 命中
	}
	inv.totals = []int{0, 0, 0, 1, 1}
	c := newRecordingClient(inv)

	page, err := c.SearchProducts(context.Background(), ProductQuery{
		Keyword: "6970367140630", Offset: 0, Limit: 50,
	})
	require.NoError(t, err)
	require.Len(t, page.Rows, 1, "条形码命中的商品必须出现在候选里")
	assert.Equal(t, "02000089", page.Rows[0].ID)

	// ④ 是 item_barcode 模型的一趟。
	require.GreaterOrEqual(t, len(inv.bodies), 5, "应发出 5 趟查询")
	assert.Contains(t, bodyMeasures(t, inv.bodies[3]), "item_barcode.count",
		"第 4 趟必须查 item_barcode 模型")
	assert.Contains(t, bodyMember(t, inv.bodies[3]), "item_barcode.product_id")

	// ⑤ 是 product.id in (货号)。
	assert.Contains(t, bodyMeasures(t, inv.bodies[4]), "product.avg_price_yuan")
	filters := bodyFilters(t, inv.bodies[4])
	require.Len(t, filters, 1)
	assert.Equal(t, "product.id", filters[0]["member"])
	assert.Equal(t, OperatorIn, filters[0]["operator"])
	assert.Equal(t, []any{"02000089"}, filters[0]["values"])
}

// TestSearchProducts_NoBarcodeMatch_NoExtraProductQuery 锁"条形码不命中 ⇒ 不多发 product 查询"。
//
// ⚠️ 这是**性能承诺**:绝大多数搜索(名称/货号/助记码)只发 3 趟 product 查询,
//   条形码那一趟只查那张 155 行的小表(且 60 秒内只查一次)。
func TestSearchProducts_NoBarcodeMatch_NoExtraProductQuery(t *testing.T) {
	inv := &recordingInvoker{}
	inv.responds = [][]map[string]any{
		{productRow("00001", "茄子")}, // ① 货号命中
		{},                           // ② 名称不命中
		{},                           // ③ 自编码不命中
		{barcodeRow("6970367140630", "02000089")}, // ④ 条码表(没有匹配)
	}
	inv.totals = []int{1, 0, 0, 1}
	c := newRecordingClient(inv)

	page, err := c.SearchProducts(context.Background(), ProductQuery{
		Keyword: "00001", Offset: 0, Limit: 50,
	})
	require.NoError(t, err)
	require.Len(t, page.Rows, 1)

	// 4 趟:3 趟 product + 1 趟 item_barcode。**没有第 5 趟**。
	require.Len(t, inv.bodies, 4, "条形码不命中时不该多发 product 查询")
	assert.Contains(t, bodyMeasures(t, inv.bodies[3]), "item_barcode.count")
}

// TestSearchProducts_BarcodeSnapshot_CachedAcrossCalls 锁"条码快照 60 秒内只拉一次"。
//
// ⚠️ 这是**性能承诺**的另一半:条形码表 155 行,但每次搜索都拉一遍就是白白的往返。
func TestSearchProducts_BarcodeSnapshot_CachedAcrossCalls(t *testing.T) {
	inv := &recordingInvoker{}
	// 第一次:3 趟 product + 1 趟 item_barcode(命中) + 1 趟 product in
	inv.responds = [][]map[string]any{
		{}, {}, {},
		{barcodeRow("6970367140630", "02000089")},
		{productRow("02000089", "靓婆婆/亚弟")},
	}
	inv.totals = []int{0, 0, 0, 1, 1}
	c := newRecordingClient(inv)

	_, err := c.SearchProducts(context.Background(), ProductQuery{
		Keyword: "6970367140630", Offset: 0, Limit: 50,
	})
	require.NoError(t, err)
	first := len(inv.bodies)

	// 第二次:快照命中 ⇒ **不再拉 item_barcode** ⇒ 只有 4 趟(3 + id in)。
	inv.responds = [][]map[string]any{
		{}, {}, {},
		{productRow("02000089", "靓婆婆/亚弟")},
	}
	inv.totals = []int{0, 0, 0, 1}
	_, err = c.SearchProducts(context.Background(), ProductQuery{
		Keyword: "6970367140630", Offset: 0, Limit: 50,
	})
	require.NoError(t, err)
	second := len(inv.bodies) - first

	assert.Equal(t, 4, second,
		"快照命中时第二次搜索只应发 4 趟(3 product + 1 id in),不该再拉 item_barcode")
}

// ---- 降级 ----

// TestItemBarcodeSnapshot_FetchFailure_KeepsStaleSnapshot 锁"拉取失败 ⇒ 沿用旧快照"。
//
// ⚠️ "拿不到新数据"**不等于**"没有数据" —— 沿用旧快照(stale-on-error)。
func TestItemBarcodeSnapshot_FetchFailure_KeepsStaleSnapshot(t *testing.T) {
	inv := &recordingInvoker{err: errors.New("cube 不可达")}
	c := newRecordingClient(inv)
	c.barcodes = newItemBarcodeCache(time.Minute, nil)

	// 先塞一份"旧快照"。
	c.barcodes.put([]ItemBarcodeDTO{{Barcode: "6970367140630", ProductID: "02000089"}})

	got := c.itemBarcodeSnapshot(context.Background())
	require.Len(t, got, 1, "拉取失败时应沿用旧快照")
	assert.Equal(t, "02000089", got[0].ProductID)
}

// TestItemBarcodeSnapshot_FetchFailureNoStale_ReturnsNil 锁"没有旧快照 ⇒ 降级为不命中"。
//
// ⚠️ 降级**不能**让整个搜索失败(条形码是附加命中方式),
//   但**必须**有日志(由 logger().Warn 打出)。
func TestItemBarcodeSnapshot_FetchFailureNoStale_ReturnsNil(t *testing.T) {
	inv := &recordingInvoker{err: errors.New("cube 不可达")}
	c := newRecordingClient(inv)
	c.barcodes = newItemBarcodeCache(time.Minute, nil)

	got := c.itemBarcodeSnapshot(context.Background())
	assert.Empty(t, got, "没有旧快照且拉取失败 ⇒ 降级为不命中")
}

// ---- 查询 body 的形状 ----

// TestListItemBarcodes_QueryShape 锁"取条码全表"的查询形状。
//
// ⚠️ limit 给小了会**静默截断**(条码表只增不减,超限的行再也查不到),
//   症状是"某个条码偶尔搜不到"且零报错 ⇒ 必须给足余量。
func TestListItemBarcodes_QueryShape(t *testing.T) {
	inv := &recordingInvoker{}
	inv.responds = [][]map[string]any{{barcodeRow("6970367140630", "02000089")}}
	inv.totals = []int{1}
	c := newRecordingClient(inv)

	rows, err := c.ListItemBarcodes(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "6970367140630", rows[0].Barcode)
	assert.Equal(t, "02000089", rows[0].ProductID)

	require.Len(t, inv.bodies, 1)
	body := inv.bodies[0]
	assert.Contains(t, bodyMeasures(t, body), "item_barcode.count")
	assert.Contains(t, bodyMember(t, body), "item_barcode.barcode")
	assert.Contains(t, bodyMember(t, body), "item_barcode.product_id")
	assert.Equal(t, true, body["total"], "必须带 total:true")
	assert.GreaterOrEqual(t, intOf(t, body["limit"]), 500, "limit 必须给足余量(条码表只增不减)")
	// order 的 id 必须同时在 SELECT 里(cube 直接 400)。
	order, _ := body["order"].([]any)
	require.Len(t, order, 1)
	om, _ := order[0].(map[string]any)
	assert.Equal(t, "item_barcode.barcode", om["id"])
}

// intOf 取 body 里的整数。
func intOf(t *testing.T, v any) int {
	t.Helper()
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	}
	b, err := json.Marshal(v)
	require.NoError(t, err)
	t.Fatalf("不是整数: %s", string(b))
	return 0
}
