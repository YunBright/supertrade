// 本文件锁住 catalog 服务层的"Go 侧 join + 响应整形"行为。
//
// # 为什么这些断言不是"测实现"
//
// 商品选择控件的契约里最关键的三条是**跨端点一致性**:
//
//	(1) /categories 的 count  ==  /products?category_id=<id> 的 total
//	(2) /suppliers/summary 的 count  ==  /products?supplier_id=<id> 的 total
//	(3) 商品行上的 category_name  ==  /categories 里该 id 的 name
//
// 任何一条不成立,用户看到的就是"侧栏写 87、点进去只有 12"或者
// "分类-030601"这种占位名 —— 而每一个单端点看起来都"正常"。
//
// 这里用 cubeclientfake 做数据源(fake 的语义与真实实现对齐,见
// internal/cubeclient/cubeclientfake/fake.go 的注释)。
package service_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/catalog/service"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubeclient/cubeclientfake"
)

// newSvc 构造一个用 fake cube 的 CatalogService。
func newSvc(t *testing.T) (*service.CatalogService, *cubeclientfake.Client) {
	t.Helper()
	c := cubeclientfake.New()
	return service.NewCatalogService(c, nil), c
}

// flattenCategories 把分类**树**摊平成节点列表(深度优先)。
//
// 分类改成树之后,`ListCategories` 返回的是**顶级节点**组成的树,
// 想按 id 查某个节点必须走树 —— 直接遍历顶层切片会漏掉所有子节点。
func flattenCategories(items []service.CategoryItem) []service.CategoryItem {
	out := make([]service.CategoryItem, 0, len(items))
	var walk func(list []service.CategoryItem)
	walk = func(list []service.CategoryItem) {
		for _, it := range list {
			out = append(out, it)
			walk(it.Children)
		}
	}
	walk(items)
	return out
}

// ---- join ----

// 商品行必须 join 出**真的**分类名 / 供应商名,不能是"分类-<id>"这种占位名。
func TestCatalogService_ListProducts_JoinsRealNames(t *testing.T) {
	svc, _ := newSvc(t)

	page, err := svc.ListProducts(context.Background(), service.ProductListQuery{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	if len(page.Products) == 0 {
		t.Fatalf("商品列表为空")
	}
	for i, p := range page.Products {
		if p.CategoryID != "" {
			if p.CategoryName == nil {
				t.Fatalf("products[%d](%s) 有 category_id=%q 但 category_name 为 null", i, p.ID, p.CategoryID)
			}
			if *p.CategoryName == "分类-"+p.CategoryID {
				t.Errorf("products[%d] 的分类名是占位名 %q", i, *p.CategoryName)
			}
		}
		if p.SupplierID != "" {
			if p.SupplierName == nil {
				t.Fatalf("products[%d](%s) 有 supplier_id=%q 但 supplier_name 为 null", i, p.ID, p.SupplierID)
			}
		}
		// barcode = id(领域事实:item_no 就是条码)。
		if p.Barcode != p.ID {
			t.Errorf("products[%d].barcode=%q 必须等于 id=%q", i, p.Barcode, p.ID)
		}
	}
}

// 字典里查不到的名字必须是 null(而不是编一个"分类-<id>"出来)。
func TestCatalogService_ListProducts_UnknownDictEntryIsNullNotPlaceholder(t *testing.T) {
	svc, fake := newSvc(t)
	// 造一个分类字典里没有的分类 id。
	fake.UpsertProductCategory("P-1001", "CAT-999")

	page, err := svc.ListProducts(context.Background(), service.ProductListQuery{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	var hit *service.ProductItem
	for i := range page.Products {
		if page.Products[i].ID == "P-1001" {
			hit = &page.Products[i]
		}
	}
	if hit == nil {
		t.Fatalf("没找到 P-1001")
	}
	if hit.CategoryName != nil {
		t.Errorf("字典里没有的分类必须给 null,实际 %q", *hit.CategoryName)
	}
}

// ---- 价格:null ≠ 0 ----

// 未维护售价(null)与 0 元必须能区分 —— 前端会把 null 显示成「未维护售价」。
func TestCatalogService_ListProducts_PriceNullDistinctFromZero(t *testing.T) {
	svc, fake := newSvc(t)
	zero := decimal.Zero
	fake.UpsertProductPrice("P-1002", nil) // 未维护
	fake.UpsertProductPrice("P-1003", &zero)

	page, err := svc.ListProducts(context.Background(), service.ProductListQuery{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	byID := map[string]service.ProductItem{}
	for _, p := range page.Products {
		byID[p.ID] = p
	}
	noPrice, ok := byID["P-1002"]
	if !ok {
		t.Fatalf("没找到 P-1002")
	}
	if noPrice.PriceYuan != nil {
		t.Errorf("未维护售价必须给 null,实际 %v", *noPrice.PriceYuan)
	}
	zeroPrice, ok := byID["P-1003"]
	if !ok {
		t.Fatalf("没找到 P-1003")
	}
	if zeroPrice.PriceYuan == nil {
		t.Fatalf("0 元售价必须保留为非 null")
	}
	if *zeroPrice.PriceYuan != 0 {
		t.Errorf("0 元售价 = %v, want 0", *zeroPrice.PriceYuan)
	}
}

// ---- 跨端点一致性 ----

// 分类字典的 count 必须等于 /products?category_id=<id> 的 total。
//
// 侧栏写「五花(87)」而点进去只有 12 个商品,用户会认为是数据错了。
func TestCatalogService_CategoryCountMatchesProductListTotal(t *testing.T) {
	svc, _ := newSvc(t)

	cats, err := svc.ListCategories(context.Background(), true)
	if err != nil {
		t.Fatalf("ListCategories: %v", err)
	}
	if len(cats.Categories) == 0 {
		t.Fatalf("分类字典为空")
	}
	// 遍历**整棵树**的节点:顶层节点只是树根,子节点同样是"某个分类",
	// 它们的 count 也必须与 /products?category_id=<id> 的 total 相等。
	for _, c := range flattenCategories(cats.Categories) {
		if c.Count == nil {
			t.Fatalf("with_counts=true 时每个分类都必须带 count")
		}
		page, err := svc.ListProducts(context.Background(), service.ProductListQuery{
			CategoryID: c.ID, Page: 1, PageSize: 1,
		})
		if err != nil {
			t.Fatalf("ListProducts(category_id=%s): %v", c.ID, err)
		}
		if page.Total != *c.Count {
			t.Errorf("分类 %q 的 count=%d,但 /products?category_id=%q 的 total=%d",
				c.ID, *c.Count, c.ID, page.Total)
		}
	}
}

// 供应商摘要的 count 必须等于 /products?supplier_id=<id> 的 total。
func TestCatalogService_SupplierCountMatchesProductListTotal(t *testing.T) {
	svc, _ := newSvc(t)

	sups, err := svc.ListSuppliers(context.Background())
	if err != nil {
		t.Fatalf("ListSuppliers: %v", err)
	}
	if len(sups.Suppliers) == 0 {
		t.Fatalf("供应商摘要为空")
	}
	for _, s := range sups.Suppliers {
		if s.Count == nil {
			t.Fatalf("供应商摘要必须带 count")
		}
		page, err := svc.ListProducts(context.Background(), service.ProductListQuery{
			SupplierID: s.ID, Page: 1, PageSize: 1,
		})
		if err != nil {
			t.Fatalf("ListProducts(supplier_id=%s): %v", s.ID, err)
		}
		if page.Total != *s.Count {
			t.Errorf("供应商 %q 的 count=%d,但 /products?supplier_id=%q 的 total=%d",
				s.ID, *s.Count, s.ID, page.Total)
		}
	}
}

// 商品行上的名字必须与字典里的名字**逐字相同**(前端不再二次请求)。
func TestCatalogService_ProductNamesMatchDictionaries(t *testing.T) {
	svc, _ := newSvc(t)

	page, err := svc.ListProducts(context.Background(), service.ProductListQuery{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	cats, err := svc.ListCategories(context.Background(), false)
	if err != nil {
		t.Fatalf("ListCategories: %v", err)
	}
	sups, err := svc.ListSuppliers(context.Background())
	if err != nil {
		t.Fatalf("ListSuppliers: %v", err)
	}
	catNames := map[string]string{}
	for _, c := range flattenCategories(cats.Categories) {
		catNames[c.ID] = c.Name
	}
	supNames := map[string]string{}
	for _, s := range sups.Suppliers {
		supNames[s.ID] = s.Name
	}
	checked := 0
	for _, p := range page.Products {
		if p.CategoryID != "" {
			name, ok := catNames[p.CategoryID]
			if !ok {
				t.Errorf("商品 %s 的 category_id=%q 不在分类字典里", p.ID, p.CategoryID)
				continue
			}
			if p.CategoryName == nil || *p.CategoryName != name {
				t.Errorf("商品 %s 的 category_name 与字典不一致", p.ID)
			}
			checked++
		}
		if p.SupplierID != "" {
			name, ok := supNames[p.SupplierID]
			if !ok {
				t.Errorf("商品 %s 的 supplier_id=%q 不在供应商字典里", p.ID, p.SupplierID)
				continue
			}
			if p.SupplierName == nil || *p.SupplierName != name {
				t.Errorf("商品 %s 的 supplier_name 与字典不一致", p.ID)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Errorf("没有任何商品名可校验")
	}
}

// ---- 搜索 ----

// 搜索必须按 id / 名称命中,且结果按 id 去重。
func TestCatalogService_SearchProducts_MatchesIDAndName(t *testing.T) {
	svc, _ := newSvc(t)

	byName, err := svc.SearchProducts(context.Background(), service.ProductListQuery{
		Keyword: "可口", Page: 1, PageSize: 50,
	})
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	if len(byName.Products) == 0 {
		t.Fatalf("按名称搜不到任何商品")
	}

	byID, err := svc.SearchProducts(context.Background(), service.ProductListQuery{
		Keyword: "P-1001", Page: 1, PageSize: 50,
	})
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	found := false
	seen := map[string]int{}
	for _, p := range byID.Products {
		seen[p.ID]++
		if p.ID == "P-1001" {
			found = true
		}
	}
	if !found {
		t.Errorf("按编码搜不到 P-1001:%+v", byID.Products)
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("商品 %s 重复出现 %d 次(必须按 id 去重)", id, n)
		}
	}
}

// 编码搜索**大小写无关**:小写编码要能搜到大写编码。
func TestCatalogService_SearchProducts_EncodingIsCaseInsensitive(t *testing.T) {
	svc, _ := newSvc(t)

	page, err := svc.SearchProducts(context.Background(), service.ProductListQuery{
		Keyword: "p-1001", Page: 1, PageSize: 50,
	})
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	found := false
	for _, p := range page.Products {
		if p.ID == "P-1001" {
			found = true
		}
	}
	if !found {
		t.Errorf("小写编码搜不到大写编码 P-1001:%+v", page.Products)
	}
}

// 空关键词 = 全量浏览(与 list 等价)。
func TestCatalogService_SearchProducts_EmptyKeywordEqualsList(t *testing.T) {
	svc, _ := newSvc(t)

	list, err := svc.ListProducts(context.Background(), service.ProductListQuery{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	search, err := svc.SearchProducts(context.Background(), service.ProductListQuery{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	if len(list.Products) != len(search.Products) {
		t.Errorf("空关键词搜索应等价于列表:%d vs %d", len(list.Products), len(search.Products))
	}
	if search.Total != list.Total {
		t.Errorf("空关键词搜索的 total 应等于列表的 total:%d vs %d", search.Total, list.Total)
	}
}

// 按**自编码**搜索必须能命中 —— 而且可能命中**多个**。
//
// ⚠️ 自编码(item_subno)**不唯一**:思迅实测 ysx 27565 个商品里 9 个自编码
// 对应 2 个货号、hbposv7 44623 个里 72 个,全部是「正常品 + 换购品」配对。
// ⇒ 搜自编码返回**候选列表**是正常结果,不能"只取第一条"。
//
// 这条用例照抄思迅实测的一对:6904406383122 → 02000110 / 02000111。
func TestCatalogService_SearchProducts_MatchesSubCode(t *testing.T) {
	svc, _ := newSvc(t)

	page, err := svc.SearchProducts(context.Background(), service.ProductListQuery{
		Keyword: "6904406383122", Page: 1, PageSize: 50,
	})
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	if len(page.Products) == 0 {
		t.Fatalf("按自编码搜不到任何商品 —— 用户扫了自编码却搜不到")
	}
	// 自编码不唯一 ⇒ 至少要能列出 2 个候选。
	if len(page.Products) < 2 {
		t.Errorf("按自编码只搜到 %d 条,应 >= 2(自编码不唯一):%+v",
			len(page.Products), page.Products)
	}
	for _, p := range page.Products {
		if p.SubCode != "6904406383122" {
			t.Errorf("商品 %s 的 sub_code = %q,不该出现在自编码搜索结果里",
				p.ID, p.SubCode)
		}
	}
}

// 商品响应必须带 **sub_code**(自编码)—— 不带的话前端那栏永远空着。
//
// ⚠️ 别用 id 顶替:货号(02000110)与自编码(6904406383122)**长得完全不一样**,
// 顶替了用户会拿到一个"看起来像自编码"的货号,
// 然后拿着它去仓库里找,找不到 —— 而界面上一丝异常都没有。
func TestCatalogService_ProductItemCarriesSubCode(t *testing.T) {
	svc, _ := newSvc(t)

	page, err := svc.ListProducts(context.Background(), service.ProductListQuery{
		Page: 1, PageSize: 50,
	})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	checked := 0
	for _, p := range page.Products {
		if p.SubCode == "" {
			continue
		}
		checked++
		if p.SubCode == p.ID {
			t.Errorf("商品 %s 的 sub_code 被 id 顶替了(货号 ≠ 自编码)", p.ID)
		}
	}
	if checked == 0 {
		t.Fatalf("没有任何商品带 sub_code —— 响应里这一列根本没出来")
	}
}

// ---- 老路径兼容 ----

// 老路径(?barcode=)必须保留老字段(product_id / product_name / category),
// 同时带上新字段 —— Flutter 的 POP 价签流读的就是老字段名。
func TestCatalogService_SearchProductsByBarcode_KeepsLegacyFields(t *testing.T) {
	svc, _ := newSvc(t)

	page, err := svc.SearchProductsByBarcode(context.Background(), "6901234567890", "S001", 10)
	if err != nil {
		t.Fatalf("SearchProductsByBarcode: %v", err)
	}
	if len(page.Products) != 1 {
		t.Fatalf("应命中 1 条,实际 %d", len(page.Products))
	}
	p := page.Products[0]
	if p.ProductID != "P-1001" {
		t.Errorf("product_id = %q, want P-1001", p.ProductID)
	}
	if p.ProductName == "" {
		t.Errorf("product_name 不能为空")
	}
	if p.Category == "" {
		t.Errorf("category(老别名)不能为空")
	}
	if p.ID != p.ProductID || p.Name != p.ProductName {
		t.Errorf("新字段与老别名必须同源:id=%q product_id=%q name=%q product_name=%q",
			p.ID, p.ProductID, p.Name, p.ProductName)
	}
	if p.Barcode == "" {
		t.Errorf("barcode 不能为空")
	}
	if p.StockQty == nil {
		t.Errorf("老路径必须保留 stock_qty(它做库存查询)")
	}
	if p.AvgCostYuan == nil {
		t.Errorf("老路径必须保留 avg_cost_yuan")
	}
	// 老路径的响应壳也必须保留。
	if page.Meta.BranchID != "S001" {
		t.Errorf("meta.branch_id = %q, want S001", page.Meta.BranchID)
	}
	if page.Meta.BarcodeQuery != "6901234567890" {
		t.Errorf("meta.barcode_query = %q", page.Meta.BarcodeQuery)
	}
}

// ---- 分类树(2026-10-20) ----

// 分类必须按 parent_id 组成**树**,子节点挂在父节点下。
//
// fake 的分类字典是:
//
//	CAT-01 饮料        (顶级)
//	CAT-02 生鲜        (顶级)
//	  └ CAT-03 生鲜蔬果 (CAT-02 的子节点)
func TestCatalogService_CategoriesAreATree(t *testing.T) {
	svc, _ := newSvc(t)

	cats, err := svc.ListCategories(context.Background(), true)
	if err != nil {
		t.Fatalf("ListCategories: %v", err)
	}
	// 顶层只有两个节点(CAT-01 / CAT-02)—— CAT-03 是子节点,不该在顶层。
	if len(cats.Categories) != 2 {
		t.Fatalf("顶层节点数 = %d, want 2(CAT-01 / CAT-02)", len(cats.Categories))
	}
	// Total 是**全部**节点数,不是顶层数。
	if cats.Total != 3 {
		t.Errorf("Total = %d, want 3(全部节点数)", cats.Total)
	}
	byID := map[string]service.CategoryItem{}
	for _, c := range flattenCategories(cats.Categories) {
		byID[c.ID] = c
	}
	child, ok := byID["CAT-03"]
	if !ok {
		t.Fatalf("树里找不到子节点 CAT-03")
	}
	if child.ParentID != "CAT-02" {
		t.Errorf("CAT-03 的 parent_id = %q, want CAT-02", child.ParentID)
	}
}

// 节点的 count 含所有子孙,own_count 只算本级 —— 两者必须能分开。
func TestCatalogService_CategoryCountIncludesDescendants(t *testing.T) {
	svc, _ := newSvc(t)

	cats, err := svc.ListCategories(context.Background(), true)
	if err != nil {
		t.Fatalf("ListCategories: %v", err)
	}
	byID := map[string]service.CategoryItem{}
	for _, c := range flattenCategories(cats.Categories) {
		byID[c.ID] = c
	}
	// CAT-02:本级 1 个(P-2001)+ 子节点 CAT-03 的 1 个(P-3001)= 2
	node := byID["CAT-02"]
	if node.OwnCount == nil || *node.OwnCount != 1 {
		t.Fatalf("CAT-02 的 own_count = %v, want 1", node.OwnCount)
	}
	if node.Count == nil || *node.Count != 2 {
		t.Fatalf("CAT-02 的 count(含子孙) = %v, want 2", node.Count)
	}
	if len(node.Children) != 1 || node.Children[0].ID != "CAT-03" {
		t.Fatalf("CAT-02 的子节点 = %+v, want [CAT-03]", node.Children)
	}
}

// 按分类过滤 = 该分类**及其所有子孙**。
//
// 这是"分类是树"这个改动的核心语义:用户在树上点"生鲜",看到的是
// 生鲜 + 生鲜蔬果 的全部商品,而不是只有生鲜本级的那一个。
func TestCatalogService_CategoryFilterExpandsToSubtree(t *testing.T) {
	svc, _ := newSvc(t)

	page, err := svc.ListProducts(context.Background(), service.ProductListQuery{
		CategoryID: "CAT-02", Page: 1, PageSize: 50,
	})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("按 CAT-02 过滤的 total = %d, want 2(含子节点 CAT-03)", page.Total)
	}
	ids := map[string]bool{}
	for _, p := range page.Products {
		ids[p.ID] = true
	}
	if !ids["P-2001"] || !ids["P-3001"] {
		t.Errorf("按 CAT-02 过滤应同时含 P-2001 与 P-3001,实际 %+v", ids)
	}
}

// 叶子节点的过滤就是精确匹配(子树只有它自己)。
func TestCatalogService_CategoryFilterOnLeafIsExact(t *testing.T) {
	svc, _ := newSvc(t)

	page, err := svc.ListProducts(context.Background(), service.ProductListQuery{
		CategoryID: "CAT-03", Page: 1, PageSize: 50,
	})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("按叶子 CAT-03 过滤的 total = %d, want 1", page.Total)
	}
}

// ---- 供应商两级(2026-10-20) ----

// 供应商必须按类别分成两级;没有类别的供应商进"未分类",**不能丢**。
//
// ⚠️ fake 的「销售方式」字典给的是**生产实测的那 6 行**
// (A 购销 / B 代销 / C 联营 / …),不是空的 —— 所以这条断言盯的是
// "按真实字典分组",不是"字典空了就全塞未分类"。
func TestCatalogService_SuppliersAreTwoLevelBySaleWay(t *testing.T) {
	svc, _ := newSvc(t)

	sups, err := svc.ListSuppliers(context.Background())
	if err != nil {
		t.Fatalf("ListSuppliers: %v", err)
	}
	if len(sups.Suppliers) == 0 {
		t.Fatalf("供应商摘要为空")
	}
	if len(sups.SupplierCategories) == 0 {
		t.Fatalf("供应商两级结构为空")
	}

	// 每个供应商**恰好**出现在一个组里 —— 既不能重复,也不能丢。
	seen := map[string]int{}
	groupOf := map[string]string{}
	for _, g := range sups.SupplierCategories {
		if g.Name == "" {
			t.Errorf("供应商类别组 %q 没有名字", g.ID)
		}
		for _, s := range g.Suppliers {
			seen[s.ID]++
			groupOf[s.ID] = g.Name
		}
	}
	for _, s := range sups.Suppliers {
		if seen[s.ID] != 1 {
			t.Errorf("供应商 %s 出现在 %d 个类别组里,want 1", s.ID, seen[s.ID])
		}
	}
	if len(sups.SupplierCategories) < 2 {
		t.Fatalf("类别组数 = %d,want >= 2(销售方式有多个取值)", len(sups.SupplierCategories))
	}

	// 组名必须是**真的**销售方式名字(购销/联营/…),
	// 不能是"类别-<id>"这种占位名,也不能是代码本身(A/B/C)。
	realNames := map[string]bool{"购销": true, "代销": true, "联营": true,
		"租赁": true, "自产": true, "扣率代销": true}
	found := 0
	for _, g := range sups.SupplierCategories {
		if g.ID == "__unclassified__" {
			continue
		}
		if !realNames[g.Name] {
			t.Errorf("类别组 %q 的名字 = %q,不是销售方式名字", g.ID, g.Name)
			continue
		}
		found++
	}
	if found == 0 {
		t.Errorf("没有任何一组用上真实销售方式名字: %+v", sups.SupplierCategories)
	}
}

// 字典里存在、但**一个供应商都没有**的类别组不返回。
//
// 实测门店 00:销售方式字典有 6 个代码(购销/代销/联营/租赁/自产/扣率代销),
// 但那个门店**没有"扣率代销"**的供应商。返回空组的话,App 的两级侧栏里
// 会出现一个点进去什么都没有的分类头 —— 死路,而且用户会以为
// "系统里有这个分类但没供应商"。
//
// ⚠️ 反过来:**有供应商但商品数为 0** 的组要留着(实测"租赁"/"自产"
// 各 1 个供应商、0 个商品)—— 供应商本身存在,藏起来才是真的丢数据。
func TestCatalogService_DropsEmptySupplierCategoryGroups(t *testing.T) {
	svc, c := newSvc(t)
	// 显式造一个"有供应商但没商品"的类别,确认它不被当成空组丢掉。
	c.SetSaleWays([]cubeclient.SaleWayDTO{
		{ID: "A", Name: "购销"},
		{ID: "C", Name: "联营"},
		{ID: "D", Name: "租赁"},
	})
	c.SetSupplier(cubeclient.SupplierDTO{ID: "SUP-NOPROD", Name: "没有商品的供应商", SupplyWay: "D"})

	sups, err := svc.ListSuppliers(context.Background())
	if err != nil {
		t.Fatalf("ListSuppliers: %v", err)
	}
	for _, g := range sups.SupplierCategories {
		if len(g.Suppliers) == 0 {
			t.Errorf("类别组 %q(%s)没有任何供应商,它不该被返回", g.ID, g.Name)
		}
	}
	// 字典里的 B(代销)没人用 ⇒ 不该出现。
	for _, g := range sups.SupplierCategories {
		if g.Name == "代销" {
			t.Errorf("『代销』这个组一个供应商都没有,不该返回")
		}
	}
	// 有供应商但 0 商品的组要留着。
	found := false
	for _, g := range sups.SupplierCategories {
		if g.Name == "租赁" {
			found = true
			if g.Count == nil || *g.Count != 0 {
				t.Errorf("『租赁』组的 count = %v,want 0", g.Count)
			}
		}
	}
	if !found {
		t.Errorf("『租赁』组(1 个供应商、0 个商品)被丢掉了: %+v", sups.SupplierCategories)
	}
}

// 按供应商类别过滤、而该类别**一个供应商都没有**时 ⇒ 结果必须是 0 行。
//
// ⚠️ 2026-10-20 生产真 bug:类别展开成空集后被当成了"不过滤",
// 接口返回了**全店 27565 个商品**,而且全程不报错。
// (实测门店 00 的"扣率代销"就是空的:字典有 6 个码,那个门店没有扣率代销的供应商。)
//
// 用户点一个空类别看到整个门店的商品 —— 极具迷惑性,
// 所以这条断言钉在服务层:空集 ⇒ 0 行。
func TestCatalogService_FilterByEmptySupplierCategoryReturnsNoRows(t *testing.T) {
	svc, _ := newSvc(t)

	page, err := svc.ListProducts(context.Background(), service.ProductListQuery{
		SupplierCategoryID: "Z-NO-SUCH-CATEGORY",
		Page:               1,
		PageSize:           50,
	})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	if page.Total != 0 {
		t.Fatalf("空类别的 total = %d,want 0 —— 空集被当成了'不过滤',"+
			"于是用户点一个空类别看到的是全店商品", page.Total)
	}
	if len(page.Products) != 0 {
		t.Errorf("空类别的商品行数 = %d,want 0", len(page.Products))
	}
}

// 字典为空 ⇒ **所有**供应商都进"未分类",而且一个都不能丢。
//
// 这是降级路径:类别名查不到时,供应商本身还在(前端侧栏不会空白)。
func TestCatalogService_EmptySaleWayDictionaryPutsEveryoneUnclassified(t *testing.T) {
	svc, c := newSvc(t)
	c.SetSaleWays(nil)

	sups, err := svc.ListSuppliers(context.Background())
	if err != nil {
		t.Fatalf("ListSuppliers: %v", err)
	}
	if len(sups.SupplierCategories) != 1 {
		t.Fatalf("类别组数 = %d, want 1(字典为空 ⇒ 全进未分类)", len(sups.SupplierCategories))
	}
	g := sups.SupplierCategories[0]
	if g.Name != "未分类" {
		t.Errorf("未归类组的 name = %q, want 未分类", g.Name)
	}
	if len(g.Suppliers) != len(sups.Suppliers) {
		t.Errorf("未分类组只有 %d 个供应商,但平铺列表有 %d 个:有的供应商被丢了",
			len(g.Suppliers), len(sups.Suppliers))
	}
	// 每组都要带 count(该组供应商的商品数之和)。
	if g.Count == nil {
		t.Errorf("供应商类别组必须带 count")
	}
}

// 字典里**没有**某个代码(源库加了新销售方式,字典还没同步)
// ⇒ 那些供应商进"未分类",不能凭空建一个组,也不能丢。
func TestCatalogService_UnknownSaleWayCodeFallsIntoUnclassified(t *testing.T) {
	svc, c := newSvc(t)
	// 只留 A(购销);把 SUP-002 的代码改成字典里没有的 "Z"。
	c.SetSaleWays([]cubeclient.SaleWayDTO{{ID: "A", Name: "购销"}})
	c.SetSupplier(cubeclient.SupplierDTO{ID: "SUP-002", Name: "农夫山泉代理", SupplyWay: "Z"})

	sups, err := svc.ListSuppliers(context.Background())
	if err != nil {
		t.Fatalf("ListSuppliers: %v", err)
	}
	var unclassified []string
	for _, g := range sups.SupplierCategories {
		if g.ID != "__unclassified__" {
			continue
		}
		for _, s := range g.Suppliers {
			unclassified = append(unclassified, s.ID)
		}
	}
	if len(unclassified) == 0 {
		t.Fatalf("字典外的代码没有被归入未分类: %+v", sups.SupplierCategories)
	}
	sawZ := false
	for _, id := range unclassified {
		if id == "SUP-002" {
			sawZ = true
		}
	}
	if !sawZ {
		t.Errorf("SUP-002(代码 Z 不在字典里)应该在未分类组,实际未分类组 = %v", unclassified)
	}
}
