// 本文件锁住商品选择控件(ProductPicker)实际发出的 cube 查询。
//
// # 为什么要有这个文件
//
// 2026-10-09 的教训(见 query_shape_test.go):cubeclientfake 直接返回预置的
// DTO,**根本不走查询构造**,所以 service 层测试(fake)永远测不到"到底有没有
// 去要这个字段"。删掉一个 dimension 不会有任何测试变红,只会让线上静默少一列。
//
// 这里断言的是**发出去的 JSON 字面量** —— measures / dimensions / filters /
// limit / offset / order / total,一条条对着契约文档核。
//
// # 几条不能被"优化"掉的规则
//
//  1. offset > 0 时必须同时有 order(cube 直接 400)。
//  2. order 的 id 必须也在 SELECT 里(cube 直接 400)。
//  3. 计数类查询必须带 total:true,否则响应里的 total 恒为 0,
//     "侧栏 count" 与 "列表 total" 就对不上。
//  4. 搜索的编码/名称子查询必须用 containsCI(cube 的大小写无关算子):
//     contains 大小写敏感,商品名里有拉丁品牌名(洁柔Face)时用户打小写就搜不到。
package cubeclient

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	dapr "github.com/dapr/go-sdk/client"
)

// recordingInvoker 捕获每次 cube 调用的 body,并按调用次序返回预设响应。
type recordingInvoker struct {
	bodies   []map[string]any
	responds [][]map[string]any
	totals   []int
}

func (r *recordingInvoker) InvokeMethodWithContent(_ context.Context, _, _, _ string,
	content *dapr.DataContent) ([]byte, error) {
	var body map[string]any
	if content != nil && len(content.Data) > 0 {
		if err := json.Unmarshal(content.Data, &body); err != nil {
			return nil, err
		}
	}
	r.bodies = append(r.bodies, body)

	rows := []map[string]any{}
	if len(r.responds) > 0 {
		rows = r.responds[0]
		r.responds = r.responds[1:]
	}
	total := 0
	if len(r.totals) > 0 {
		total = r.totals[0]
		r.totals = r.totals[1:]
	}
	return json.Marshal(map[string]any{"data": rows, "total": total})
}

// newRecordingClient 造一个记录 body 的 DaprCubeClient。
func newRecordingClient(inv *recordingInvoker) *DaprCubeClient {
	return NewDaprCubeClient(inv, "supertrade-cube-router", "v1/load")
}

// bodyFilters 取 body 里的 filters(每个是 {member,operator,values})。
func bodyFilters(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["filters"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		fm, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("filter 必须是对象, got %#v", v)
		}
		out = append(out, fm)
	}
	return out
}

// filterOf 找某个 member 的过滤条件(找不到返 nil)。
func filterOf(t *testing.T, body map[string]any, member string) map[string]any {
	t.Helper()
	for _, f := range bodyFilters(t, body) {
		if m, _ := f["member"].(string); m == member {
			return f
		}
	}
	return nil
}

// filterValue 取某个 member 的过滤值(字符串)。
func filterValue(t *testing.T, body map[string]any, member string) string {
	t.Helper()
	f := filterOf(t, body, member)
	if f == nil {
		return ""
	}
	vals, _ := f["values"].([]any)
	if len(vals) == 0 {
		return ""
	}
	s, _ := vals[0].(string)
	return s
}

// ---- 商品页 ----

// ListProducts 必须发出带 order + total 的分页查询。
func TestListProducts_IssuesPagedQueryWithOrderAndTotal(t *testing.T) {
	inv := &recordingInvoker{}
	c := newRecordingClient(inv)

	_, err := c.ListProducts(context.Background(), ProductQuery{Offset: 40, Limit: 20})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	if len(inv.bodies) != 1 {
		t.Fatalf("商品页应只发 1 次 cube 查询,实际 %d 次", len(inv.bodies))
	}
	body := inv.bodies[0]

	if got := body["limit"]; got != float64(20) {
		t.Errorf("limit = %#v, want 20", got)
	}
	if got := body["offset"]; got != float64(40) {
		t.Errorf("offset = %#v, want 40", got)
	}
	if got := body["total"]; got != true {
		t.Errorf("total = %#v, want true(不给 total 时 cube 恒返 0)", got)
	}

	// order 必须存在(offset>0 时 cube 直接 400),且 id 必须也在 SELECT 里。
	order, ok := body["order"].([]any)
	if !ok || len(order) == 0 {
		t.Fatalf("分页查询必须有 order(offset 强制要求 order):%#v", body["order"])
	}
	o0, _ := order[0].(map[string]any)
	if id, _ := o0["id"].(string); id != "product.id" {
		t.Errorf("order.id = %q, want product.id", id)
	}
	if dir, _ := o0["order"].(string); dir != "asc" {
		t.Errorf("order.order = %q, want asc", dir)
	}
	dims := dimsOf(t, body)
	if !hasDim(dims, "product.id") {
		t.Errorf("order 的 id 必须同时在 SELECT 里,dims = %v", dims)
	}

	// 六个维度一个都不能少(少一个就会把不同的商品合并成一行)。
	for _, want := range []string{"product.id", "product.name", "product.category_id",
		"product.supplier_id", "product.unit", "product.status"} {
		if !hasDim(dims, want) {
			t.Errorf("dimensions 缺 %s,实际 %v", want, dims)
		}
	}
}

// 分类 / 供应商过滤必须落到 cube 的 where 上(否则"按分类过滤"是个摆设)。
func TestListProducts_AddsCategoryAndSupplierFilters(t *testing.T) {
	inv := &recordingInvoker{}
	c := newRecordingClient(inv)

	_, err := c.ListProducts(context.Background(), ProductQuery{
		CategoryID: "030601", SupplierID: "019", Limit: 50,
	})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	body := inv.bodies[0]

	if got := filterValue(t, body, "product.category_id"); got != "030601" {
		t.Errorf("category 过滤值 = %q, want 030601", got)
	}
	if got := filterValue(t, body, "product.supplier_id"); got != "019" {
		t.Errorf("supplier 过滤值 = %q, want 019", got)
	}
	f := filterOf(t, body, "product.category_id")
	if op, _ := f["operator"].(string); op != "equals" {
		t.Errorf("category 过滤算子 = %q, want equals", op)
	}
}

// supplierFiltersOf 取出所有作用在某个 member 上的过滤条件(按出现次序)。
//
// ⚠️ 不能只取"第一个":`filterOf` 找到第一个就返回,**看不见重复**。
// 而"同一个过滤被加了两次"正是这里要抓的缺陷。
func supplierFiltersOf(t *testing.T, body map[string]any, member string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, f := range bodyFilters(t, body) {
		if m, _ := f["member"].(string); m == member {
			out = append(out, f)
		}
	}
	return out
}

// 只指定一个供应商时,发出去的 where 里只能有**一条** supplier 过滤。
//
// 2026-10-20 的缺陷:这段代码是 `switch { case SupplierIDs: ... case SupplierID: ... }`
// 后面又跟了一个 `if SupplierID != "" { 追加同一条 equals }` —— 于是只传
// supplier_id 时,同一个 equals 过滤被**加了两次**。
//
// 为什么它躲过了既有测试:`filterOf` / `filterValue` 找到第一个就返回,
// 重复的第二个根本看不见;而重复的过滤在 SQL 语义上是等价的
// (`x = ? AND x = ?` ≡ `x = ?`),结果集一模一样 —— 没有任何"症状"。
//
// 这类缺陷只有靠**数条数**才能发现,不能靠看值。
func TestListProducts_SingleSupplierIDEmitsExactlyOneFilter(t *testing.T) {
	inv := &recordingInvoker{}
	c := newRecordingClient(inv)

	_, err := c.ListProducts(context.Background(), ProductQuery{
		SupplierID: "019", Limit: 50,
	})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}

	got := supplierFiltersOf(t, inv.bodies[0], "product.supplier_id")
	if len(got) != 1 {
		t.Fatalf("只传一个 supplier_id 时 supplier 过滤条数 = %d,want 1 —— %v", len(got), got)
	}
	if op, _ := got[0]["operator"].(string); op != "equals" {
		t.Errorf("算子 = %q, want equals", op)
	}
	if v := filterValue(t, inv.bodies[0], "product.supplier_id"); v != "019" {
		t.Errorf("过滤值 = %q, want 019", v)
	}
}

// supplier_id 与 supplier_category_id 同时给时是**交集**(AND),不是二选一。
//
// `supplier_category_id` 在服务层被展开成一组供应商 id(走 `in`),
// `supplier_id` 是精确等于(走 `equals`)—— 两个都留着才是
// 「属于该类别 且 属于该供应商」。
//
// ⚠️ 这跟分类那边**刻意相反**:分类的 CategoryIDs(子树)与 CategoryID(精确等于)
//
//	必须二选一,否则子树语义被吃掉成"精确等于 X"。
func TestListProducts_SupplierIDAndSupplierIDsAreIntersected(t *testing.T) {
	inv := &recordingInvoker{}
	c := newRecordingClient(inv)

	_, err := c.ListProducts(context.Background(), ProductQuery{
		SupplierID:  "019",
		SupplierIDs: []string{"019", "023", "038"},
		Limit:       50,
	})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}

	got := supplierFiltersOf(t, inv.bodies[0], "product.supplier_id")
	if len(got) != 2 {
		t.Fatalf("supplier 过滤条数 = %d,want 2(in + equals) —— %v", len(got), got)
	}
	ops := map[string]int{}
	for _, f := range got {
		op, _ := f["operator"].(string)
		ops[op]++
	}
	if ops["in"] != 1 || ops["equals"] != 1 {
		t.Errorf("算子分布 = %v,want in×1 + equals×1", ops)
	}
}

// 空集合的语义是「命中 0 行」,**不是**「不过滤」。
//
// ⚠️ 这是 2026-10-20 生产上查出来的真 bug:
// 用户点一个**没有供应商**的类别(实测门店 00 的"扣率代销"就是空的:
// 销售方式字典有 6 个码,那个门店没有扣率代销的供应商),
// 展开出来的供应商集合是空的 —— 而空集合被当成了"不过滤",
// 于是接口返回了**全店 27565 个商品**,而且**全程不报错**。
//
// 症状极具迷惑性:用户以为"这个类别下有 27565 个商品",
// 而真相是"这个类别下有 0 个商品"。
//
// 这条断言盯的是"过滤**必须存在**",不是"过滤的值对不对"。
func TestListProducts_EmptySupplierSetStillEmitsASupplierFilter(t *testing.T) {
	inv := &recordingInvoker{}
	c := newRecordingClient(inv)

	_, err := c.ListProducts(context.Background(), ProductQuery{
		SupplierIDs: []string{}, // ← 非 nil 的空切片 = "按空集合过滤"
		Limit:       50,
	})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}

	got := supplierFiltersOf(t, inv.bodies[0], "product.supplier_id")
	if len(got) != 1 {
		t.Fatalf("空供应商集合时 supplier 过滤条数 = %d,want 1 —— "+
			"没有过滤就等于返回**全店商品**(实测 27565 个),而且不报错", len(got))
	}
}

// nil 与空切片语义**不同**:nil = 不过滤,空切片 = 命中 0 行。
//
// 这条用例把两者的差别钉死 —— 判据"用 len() > 0 还是用 != nil"
// 决定了空集合会不会被静默当成"不过滤"。
func TestListProducts_NilSupplierSetMeansNoFilter(t *testing.T) {
	inv := &recordingInvoker{}
	c := newRecordingClient(inv)

	_, err := c.ListProducts(context.Background(), ProductQuery{
		SupplierIDs: nil, // ← nil = 没有这个筛选维度
		Limit:       50,
	})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	if got := supplierFiltersOf(t, inv.bodies[0], "product.supplier_id"); len(got) != 0 {
		t.Fatalf("nil 供应商集合时不该有 supplier 过滤,实际 %v", got)
	}
}

// 分类侧的空集合同理:CategoryIDs 是空切片时也必须发出过滤。
func TestListProducts_EmptyCategorySetStillEmitsACategoryFilter(t *testing.T) {
	inv := &recordingInvoker{}
	c := newRecordingClient(inv)

	_, err := c.ListProducts(context.Background(), ProductQuery{
		CategoryIDs: []string{},
		Limit:       50,
	})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	got := filterOf(t, inv.bodies[0], "product.category_id")
	if got == nil {
		t.Fatalf("空分类集合时必须有 category 过滤,否则返回的是全店商品")
	}
}

// ---- 搜索 ----

// 搜索的三个子查询(货号 / 自编码 / 名称)都必须用 containsCI(大小写无关)。
//
// 2026-10-20 之前这里发 3 次查询(原样 id / 大写 id / 名称),因为 cube 的
// contains 大小写敏感、"再查一次大写"是唯一的绕法 —— 而那个绕法只覆盖
// **全大写**这一种形态("z005-69" 仍然搜不到 "Z005-69")。
// cube 有了 containsCI 之后,一次查询就够。
//
// 2026-10-20 又加了**自编码**那一路(思迅 item_subno):
// 思迅的搜索框是「输入编码、名称或助记码」,而"编码"对它来说是**两个**东西
// (货号 item_no 唯一 / 自编码 item_subno 不唯一)。
// 漏掉这一路 ⇒ 用户搜自编码搜不到那个商品。
func TestSearchProducts_UsesCaseInsensitiveOperator(t *testing.T) {
	inv := &recordingInvoker{}
	c := newRecordingClient(inv)

	_, err := c.SearchProducts(context.Background(), ProductQuery{Keyword: "z005-69", Limit: 20})
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	wantMembers := []string{"product.id", "product.name", "product.sub_code"}
	if len(inv.bodies) != len(wantMembers) {
		t.Fatalf("应发 %d 次查询(id / name / sub_code),实际 %d 次",
			len(wantMembers), len(inv.bodies))
	}
	for i, b := range inv.bodies {
		f := filterOf(t, b, wantMembers[i])
		if f == nil {
			t.Fatalf("第 %d 次查询缺 %s 过滤:%#v", i+1, wantMembers[i], b["filters"])
		}
		if op, _ := f["operator"].(string); op != OperatorContainsCI {
			t.Errorf("第 %d 次查询算子 = %q, want %q(大小写无关)", i+1, op, OperatorContainsCI)
		}
		// 值原样传:lower 由 cube 编译期做,supertrade 不该自己改写。
		if got := filterValue(t, b, wantMembers[i]); got != "z005-69" {
			t.Errorf("第 %d 次查询过滤值 = %q, want z005-69(原样)", i+1, got)
		}
	}
}

// 商品页必须 SELECT `product.sub_code` —— 不然自编码既搜不到也显示不出来。
//
// ⚠️ 少一个维度不会让查询失败,只会让那一列**恒为空**:
// 自编码搜索永远 0 命中、列表里"自编码"那栏永远空。
// 所以这条断言盯的是"维度在 SELECT 里",不是"值对不对"。
func TestListProducts_SelectsSubCodeDimension(t *testing.T) {
	inv := &recordingInvoker{}
	c := newRecordingClient(inv)

	_, err := c.ListProducts(context.Background(), ProductQuery{Limit: 10})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	dims := dimsOf(t, inv.bodies[0])
	if !hasDim(dims, "product.sub_code") {
		t.Errorf("商品页必须 SELECT product.sub_code,实际 %v", dims)
	}
}

// 行里的 SubCode 必须来自 cube 的 product.sub_code(而不是拿 id 顶替)。
//
// ⚠️ 拿 id 顶替是个很隐蔽的错:货号与自编码**长得完全不一样**
// (02000110 vs 6904406383122),用户会拿到一个"看起来像自编码"的货号,
// 然后按它去仓库里找,找不到 —— 而界面上一丝异常都没有。
func TestProductRows_SubCodeComesFromItsOwnColumn(t *testing.T) {
	inv := &recordingInvoker{
		responds: [][]map[string]any{{{
			"product.id":       "02000110",
			"product.name":     "远航九江双蒸酒精品500ml",
			"product.sub_code": "6904406383122",
		}}},
	}
	c := newRecordingClient(inv)

	page, err := c.ListProducts(context.Background(), ProductQuery{Limit: 10})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(page.Rows))
	}
	if page.Rows[0].SubCode != "6904406383122" {
		t.Errorf("SubCode = %q, want 6904406383122", page.Rows[0].SubCode)
	}
	if page.Rows[0].SubCode == page.Rows[0].ID {
		t.Errorf("SubCode 不该被 id 顶替(货号 %q ≠ 自编码)", page.Rows[0].ID)
	}
}

// 不得再有"大写变体"查询 —— containsCI 已经是大小写无关的,
// 再查一次 UPPER 是多余的一次 cube 往返。
func TestSearchProducts_NoUpperCaseVariantQuery(t *testing.T) {
	inv := &recordingInvoker{}
	c := newRecordingClient(inv)

	_, err := c.SearchProducts(context.Background(), ProductQuery{Keyword: "z005-69", Limit: 20})
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	for i, b := range inv.bodies {
		if v := filterValue(t, b, "product.id"); v == "Z005-69" {
			t.Fatalf("第 %d 次查询仍是大写变体(%q):containsCI 已大小写无关,不该再查一次",
				i+1, v)
		}
	}
}

// 搜索结果的 total 是各子查询 total 之和(上界);去重后按 id 升序。
func TestSearchProducts_MergesDedupesAndSumsTotal(t *testing.T) {
	inv := &recordingInvoker{
		// 第 1 次:id 命中 2 条;第 2 次:名称命中 1 条(与第 1 次重叠一条)。
		responds: [][]map[string]any{
			{{"product.id": "Z005-69", "product.name": "五花肉"}, {"product.id": "Z005-70", "product.name": "五花肉(2)"}},
			{{"product.id": "Z005-69", "product.name": "五花肉"}, {"product.id": "A-001", "product.name": "五花肉"}},
		},
		totals: []int{2, 2},
	}
	c := newRecordingClient(inv)

	page, err := c.SearchProducts(context.Background(), ProductQuery{Keyword: "z005", Limit: 20})
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	// 去重后 3 个商品(A-001 / Z005-69 / Z005-70)。
	if len(page.Rows) != 3 {
		t.Fatalf("去重后应 3 条,实际 %d:%+v", len(page.Rows), page.Rows)
	}
	// 按 id 升序。
	if page.Rows[0].ID != "A-001" || page.Rows[1].ID != "Z005-69" || page.Rows[2].ID != "Z005-70" {
		t.Errorf("合并后应按 id 升序,实际 %+v", page.Rows)
	}
	// total = 两者之和(上界)。
	if page.Total != 4 {
		t.Errorf("total = %d, want 4(两个子查询 total 之和)", page.Total)
	}
}

// 空关键词 = 全量浏览,只发 1 次查询。
func TestSearchProducts_EmptyKeywordFallsBackToList(t *testing.T) {
	inv := &recordingInvoker{}
	c := newRecordingClient(inv)

	_, err := c.SearchProducts(context.Background(), ProductQuery{Keyword: "  ", Limit: 20})
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	if len(inv.bodies) != 1 {
		t.Fatalf("空关键词应只发 1 次查询,实际 %d 次", len(inv.bodies))
	}
	if len(bodyFilters(t, inv.bodies[0])) != 0 {
		t.Errorf("空关键词不应有过滤条件:%#v", inv.bodies[0]["filters"])
	}
}

// ---- 字典 / 计数 ----

// 分类字典查询必须带上 name(占位名"分类-<id>"就是缺这一列造成的)。
func TestListCategories_IssuesDictionaryQuery(t *testing.T) {
	inv := &recordingInvoker{}
	c := newRecordingClient(inv)

	if _, err := c.ListCategories(context.Background()); err != nil {
		t.Fatalf("ListCategories: %v", err)
	}
	if len(inv.bodies) != 1 {
		t.Fatalf("分类字典应只发 1 次查询,实际 %d 次", len(inv.bodies))
	}
	body := inv.bodies[0]
	dims := dimsOf(t, body)
	for _, want := range []string{"category.id", "category.name"} {
		if !hasDim(dims, want) {
			t.Errorf("分类字典 dimensions 缺 %s,实际 %v", want, dims)
		}
	}
	if got := body["total"]; got != true {
		t.Errorf("total = %#v, want true", got)
	}
}

// 供应商字典必须带 cust_flag(以后要按它收窄供应商口径)。
func TestListSuppliers_IssuesDictionaryQuery(t *testing.T) {
	inv := &recordingInvoker{}
	c := newRecordingClient(inv)

	if _, err := c.ListSuppliers(context.Background()); err != nil {
		t.Fatalf("ListSuppliers: %v", err)
	}
	body := inv.bodies[0]
	dims := dimsOf(t, body)
	for _, want := range []string{"supplier.id", "supplier.name", "supplier.cust_flag"} {
		if !hasDim(dims, want) {
			t.Errorf("供应商字典 dimensions 缺 %s,实际 %v", want, dims)
		}
	}
}

// 按分类计数必须用 product 模型 + COUNT(*),而且不能带任何过滤
// (带过滤就会让"侧栏 count"与"列表 total"口径不一致)。
func TestCountProductsByCategory_IssuesUnfilteredCountQuery(t *testing.T) {
	inv := &recordingInvoker{
		responds: [][]map[string]any{
			{{"product.category_id": "01", "product.count": 35},
				{"product.category_id": "0101", "product.count": 218}},
		},
	}
	c := newRecordingClient(inv)

	counts, err := c.CountProductsByCategory(context.Background())
	if err != nil {
		t.Fatalf("CountProductsByCategory: %v", err)
	}
	if len(inv.bodies) != 1 {
		t.Fatalf("按分类计数应只发 1 次查询,实际 %d 次", len(inv.bodies))
	}
	body := inv.bodies[0]
	if len(bodyFilters(t, body)) != 0 {
		t.Errorf("计数查询不得带过滤(否则与列表 total 口径不一致):%#v", body["filters"])
	}
	dims := dimsOf(t, body)
	if !hasDim(dims, "product.category_id") {
		t.Errorf("计数查询必须按 product.category_id 分组,dims = %v", dims)
	}
	raw, ok := body["measures"].([]any)
	if !ok || len(raw) == 0 || raw[0] != "product.count" {
		t.Errorf("计数查询的 measure 必须是 product.count,实际 %#v", body["measures"])
	}
	if counts["01"] != 35 || counts["0101"] != 218 {
		t.Errorf("counts = %v", counts)
	}
}

// 按供应商计数同理。
func TestCountProductsBySupplier_IssuesUnfilteredCountQuery(t *testing.T) {
	inv := &recordingInvoker{
		responds: [][]map[string]any{
			{{"product.supplier_id": "001", "product.count": 125}},
		},
	}
	c := newRecordingClient(inv)

	counts, err := c.CountProductsBySupplier(context.Background())
	if err != nil {
		t.Fatalf("CountProductsBySupplier: %v", err)
	}
	if counts["001"] != 125 {
		t.Errorf("counts = %v", counts)
	}
	dims := dimsOf(t, inv.bodies[0])
	if !hasDim(dims, "product.supplier_id") {
		t.Errorf("计数查询必须按 product.supplier_id 分组,dims = %v", dims)
	}
}

// ---- total 的传递 ----

// LoadCubeQueryPage 必须把响应顶层的 total 交给调用方。
//
// 不给 total:true 时 cube 恒返 0 —— 那是"没问",不是"总共 0 条"。
func TestLoadCubeQueryPage_SurfacesTopLevelTotal(t *testing.T) {
	inv := &recordingInvoker{
		responds: [][]map[string]any{{{"product.id": "P-1"}}},
		totals:   []int{27565},
	}
	c := newRecordingClient(inv)

	rows, total, err := c.LoadCubeQueryPage(context.Background(), "product", CubeQuery{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.id"},
		Limit:      1,
		Total:      true,
	})
	if err != nil {
		t.Fatalf("LoadCubeQueryPage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if total != 27565 {
		t.Errorf("total = %d, want 27565", total)
	}
}

// 老方法 LoadCubeQuery 的行为不变(只返 data),既有调用方零影响。
func TestLoadCubeQuery_StillReturnsRowsOnly(t *testing.T) {
	inv := &recordingInvoker{
		responds: [][]map[string]any{{{"product.id": "P-1"}}},
		totals:   []int{7},
	}
	c := newRecordingClient(inv)

	rows, err := c.LoadCubeQuery(context.Background(), "product", CubeQuery{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.id"},
		Limit:      1,
	})
	if err != nil {
		t.Fatalf("LoadCubeQuery: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
}

// 没有 total:true 时,请求体里**不能**出现 total 字段(避免改变既有查询的语义)。
func TestLoadCubeQuery_OmitsTotalWhenNotRequested(t *testing.T) {
	inv := &recordingInvoker{}
	c := newRecordingClient(inv)

	if _, err := c.LoadCubeQuery(context.Background(), "product", CubeQuery{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.id"},
		Limit:      1,
	}); err != nil {
		t.Fatalf("LoadCubeQuery: %v", err)
	}
	body := inv.bodies[0]
	if _, ok := body["total"]; ok {
		t.Errorf("没要 total 时请求体不该带 total 字段:%#v", body["total"])
	}
	if _, ok := body["offset"]; ok {
		t.Errorf("没要 offset 时请求体不该带 offset 字段:%#v", body["offset"])
	}
	if _, ok := body["order"]; ok {
		t.Errorf("没要 order 时请求体不该带 order 字段:%#v", body["order"])
	}
}

// 商品行的 barcode 必须等于 id(领域事实:item_no 就是条码)。
func TestProductRows_BarcodeEqualsID(t *testing.T) {
	inv := &recordingInvoker{
		responds: [][]map[string]any{{{"product.id": "6957583900828", "product.name": "凤派XL码抽纸8包"}}},
	}
	c := newRecordingClient(inv)

	page, err := c.ListProducts(context.Background(), ProductQuery{Limit: 10})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	if len(page.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(page.Rows))
	}
	if page.Rows[0].Barcode != page.Rows[0].ID {
		t.Errorf("barcode(%q) 必须等于 id(%q)", page.Rows[0].Barcode, page.Rows[0].ID)
	}
}

// 未维护售价的商品:响应里没有 avg_price_yuan 这个 key ⇒ PriceYuan 必须是 nil
// (nil = 未维护,与"售价是 0 元"是两件事)。
func TestProductRows_PriceNilWhenAbsent(t *testing.T) {
	inv := &recordingInvoker{
		responds: [][]map[string]any{
			{
				{"product.id": "P-1"},                              // 没有价
				{"product.id": "P-2", "product.avg_price_yuan": 0}, // 真的是 0 元
			},
		},
	}
	c := newRecordingClient(inv)

	page, err := c.ListProducts(context.Background(), ProductQuery{Limit: 10})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	if page.Rows[0].PriceYuan != nil {
		t.Errorf("未维护价必须给 nil,实际 %v", page.Rows[0].PriceYuan)
	}
	if page.Rows[1].PriceYuan == nil {
		t.Fatalf("0 元必须保留为非 nil(前端要区分 null 与 0)")
	}
	if !page.Rows[1].PriceYuan.IsZero() {
		t.Errorf("0 元商品的价 = %v, want 0", page.Rows[1].PriceYuan)
	}
}

// 商品行里不能出现 status 的翻译(必须原样返回思迅状态码)。
func TestProductRows_StatusIsRawCode(t *testing.T) {
	inv := &recordingInvoker{
		responds: [][]map[string]any{{{"product.id": "P-1", "product.status": "2"}}},
	}
	c := newRecordingClient(inv)

	page, err := c.ListProducts(context.Background(), ProductQuery{Limit: 10})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	if page.Rows[0].Status != "2" {
		t.Errorf("status = %q, want \"2\"(必须原样返回思迅状态码)", page.Rows[0].Status)
	}
	if strings.Contains(strings.ToLower(page.Rows[0].Status), "active") {
		t.Errorf("status 不得被翻译成 active/inactive")
	}
}
