// internal/cubeclient/catalog.go —— 商品选择控件(ProductPicker)所需的 cube 查询。
//
// # 为什么这些查询是"每个端点固定几条",而不是 N+1
//
// cube 的 product / category / supplier 都是 **duck** 存储模型,而 cube 的 join
// 引擎只支持 `storage: live` 模型之间的 join —— 所以
//
//	{"dimensions":["product.id","category.name"]}
//
// 直接 400(dimension not found: category.name,实测)。
//
// ⇒ 分类名 / 供应商名必须在 Go 侧 join,于是每个端点的 cube 查询数是**固定**的:
//
//	/products             3 次(商品页 + 分类字典 + 供应商字典)
//	/products/search?q=   5 次(编码命中 + 编码大写命中 + 名称命中 + 2 个字典)
//	/categories           2 次(分类字典 + 按分类的商品数)
//	/suppliers/summary    2 次(供应商字典 + 按供应商的商品数)
//
// ⚠️ 现有 internal/catalog/handler/product.go 里"每个商品行查一次供应商"是 N+1
// (该文件自己的注释也点了这件事),新端点不照抄那个形状 —— 27k 行商品配 N+1
// 就是一个能把 cube 拖垮的查询。
//
// # 为什么"侧栏 count"与"列表 total"必然相等
//
// 侧栏 count = `SELECT COUNT(*) ... GROUP BY product.category_id`;
// 列表 total = `COUNT(*) OVER()`(同一个 product 模型、同一个过滤条件、只是多一个
// category_id = X 的过滤)。product 表的 id 是主键(primary_key: id),所以
// "按 id 分组"与"按行数"是同一个集合 ⇒ 两个数字必然相等。
//
// ⚠️ 这条相等性**依赖 id 唯一**。如果哪天 product 表出现重复 id(例如按门店
// 拆行),侧栏与列表就会开始对不上 —— 那时必须改成同一套口径重新算。
package cubeclient

import (
	"context"
	"sort"
	"strings"
)

// catalogDictLimit 是字典 / 计数查询的行数上限。
//
// cube 的默认 limit 是 1000;实测分类 186 条 / 供应商 219 条,1000 够用。
// 这里给 5000 是为了留余量:limit 给小了会**静默截断**,表现为"侧栏少几个分类"
// 而且**不报错** —— 这类缺陷最难查。
const catalogDictLimit = 5000

// productPageDimensions 是商品页 SELECT 的维度集合。
//
// 六个维度一起 GROUP BY,粒度 = 一个商品(product.id 是主键,唯一)。
// ⚠️ 不要为了少查一列而删维度:少一个维度就会把不同的商品合并成一行,
// 商品数(count / total)会跟着变小,而"侧栏 count == 列表 total"这条等式
// 也就悄悄不成立了。
func productPageDimensions() []string {
	return []string{"product.id", "product.name", "product.category_id",
		"product.supplier_id", "product.unit", "product.status"}
}

// productPageQuery 构造商品页的 cube 查询。
//
// 实际发出去的 body(无过滤时):
//
//	{
//	  "measures":   ["product.avg_price_yuan"],
//	  "dimensions": ["product.id","product.name","product.category_id",
//	                 "product.supplier_id","product.unit","product.status"],
//	  "filters":    [],
//	  "limit":      <page_size>,
//	  "offset":     <(page-1)*page_size>,
//	  "order":      [{"id":"product.id","order":"asc"}],
//	  "total":      true
//	}
//
// # 为什么 order 用 product.id
//
//  1. cube 的 offset **强制要求 order**(没给 order 且 offset>0 → 400
//     「offset=N 需要显式 order:分页必须建立在稳定排序上…」)。
//  2. order 的 id 必须**同时在 SELECT 里**,否则 cube 返 400。
//  3. product.id 是主键 ⇒ 排序是**全序且唯一** ⇒ 第 1 页和第 2 页不可能
//     出现同一个商品(这是分页最基本的要求)。
//
// # 为什么 measures 只有 avg_price_yuan
//
//  1. cube 要求至少一个 measure(否则 400 "at least one measure required")。
//  2. price_yuan 走 AVG:product 表按 id 分组后每组只有一行,AVG 在单行上恒等于
//     那一行的值,不存在聚合失真(与 GetProduct 的既有口径一致)。
//  3. 该商品**没维护价**时 AVG 为 NULL ⇒ cube 响应里这个 key 没有值 ⇒
//     后端必须显式写 "price_yuan": null(见 ProductDTO.PriceYuan 的指针语义)。
func productPageQuery(q ProductQuery) CubeQuery {
	cq := CubeQuery{
		Measures:   []string{"product.avg_price_yuan"},
		Dimensions: productPageDimensions(),
		Limit:      q.Limit,
		Offset:     q.Offset,
		Order:      []CubeOrder{{ID: "product.id", Order: "asc"}},
		Total:      true,
	}
	// 分类过滤:**子树优先**。
	//
	// 分类改成树之后,"按某个分类看商品"的语义是「该分类 + 它的所有子孙分类」。
	// 上层把树展开成一组 id 传下来,这里用 cube 的 `in` 算子一次查完
	// (而不是 N 次 equals —— 那会让 cube 把同一张表扫 N 遍,子树可能有
	// 上百个节点)。
	//
	// ⚠️ `in` 的值必须**去重**:分类树里同一个 id 不会出现两次,但上层拼子树时
	// 若把"自己"也拼进去就会重复(重复值不报错,只是 SQL 变长)。
	//
	// ⚠️ CategoryIDs 与 CategoryID **互斥**:两个都加过滤会变成
	// "精确等于 X" ∩ "属于子树" = 精确等于 X —— 子树语义被悄悄吃掉,
	// 表现是"选了一个父分类却只看到它自己的商品"。
	//
	// ⚠️ 判断"要不要加过滤"用 `!= nil`,**不是** `len() > 0`:
	// 空集合的语义是「按这个集合过滤」⇒ **命中 0 行**,
	// 当成"不过滤"会返回**全店商品**(实测门店 00 是 27565 个),
	// 而且**全程不报错**。
	switch {
	case q.CategoryIDs != nil:
		vals := make([]any, 0, len(q.CategoryIDs))
		seen := make(map[string]bool, len(q.CategoryIDs))
		for _, id := range q.CategoryIDs {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			vals = append(vals, id)
		}
		if len(vals) == 0 {
			cq.Filters = append(cq.Filters, CubeFilter{
				Member: "product.category_id", Operator: "equals",
				Values: []any{emptySetSentinel},
			})
		} else {
			cq.Filters = append(cq.Filters, CubeFilter{
				Member: "product.category_id", Operator: "in", Values: vals,
			})
		}
	case q.CategoryID != "":
		cq.Filters = append(cq.Filters, CubeFilter{
			Member: "product.category_id", Operator: "equals", Values: []any{q.CategoryID},
		})
	}

	// 供应商过滤:两种形态,**可以同时存在**(交集语义)。
	//
	// - SupplierIDs:按**供应商类别**过滤时用,一组 id(`in`)
	// - SupplierID:指定单个供应商(`equals`)
	//
	// ⚠️ 与分类那边的 switch **刻意不一样**:
	//
	//	分类必须二选一(CategoryIDs 是子树展开、CategoryID 是精确等于,
	//	两个都加会把子树语义吃掉成"精确等于 X",见上面的注释)。
	//	供应商这两个**不冲突**:"supplier_id=X & supplier_category_id=C" 的
	//	语义是「属于 C 且属于 X」—— X ∈ C 时就是 X 自己的商品,否则为空。
	//	所以两个都加(AND),而不是 switch 掉一个。
	//
	// 生产实测(门店 00,供应商 074 共 2192 个商品):
	// category_id=01 → 229、=02 → 403、=03 → 1560,229+403+1560 = 2192 ✓
	// ⇒ 三个过滤条件是**交集**,不是互斥,也不是"同时发就是空列表"。
	//
	// ⚠️⚠️ 这里判断"要不要加过滤"用的是 `!= nil`,**不是** `len() > 0`。
	//
	// 空集合的语义是「按这个集合过滤」⇒ **命中 0 行**;
	// 当成"不过滤"会返回**全店商品**(实测门店 00 是 27565 个)——
	// 用户点了一个没有供应商的类别(实测"扣率代销"组就是空的),
	// 看到的是整个门店的商品,而**全程不报错**。
	if q.SupplierIDs != nil {
		vals := make([]any, 0, len(q.SupplierIDs))
		seen := make(map[string]bool, len(q.SupplierIDs))
		for _, id := range q.SupplierIDs {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			vals = append(vals, id)
		}
		if len(vals) == 0 {
			// cube 的 `in` 至少要一个值(否则 400),所以空集合用一条恒假条件表达。
			cq.Filters = append(cq.Filters, CubeFilter{
				Member: "product.supplier_id", Operator: "equals",
				Values: []any{emptySetSentinel},
			})
		} else {
			cq.Filters = append(cq.Filters, CubeFilter{
				Member: "product.supplier_id", Operator: "in", Values: vals,
			})
		}
	}
	if q.SupplierID != "" {
		cq.Filters = append(cq.Filters, CubeFilter{
			Member: "product.supplier_id", Operator: "equals", Values: []any{q.SupplierID},
		})
	}
	return cq
}

// emptySetSentinel 是"按空集合过滤"用的恒假值。
//
// cube 的 `in` 算子至少要一个值(否则 400),所以"空集合"没法直接用 in 表达。
// 这里用一个**不可能是真实 id** 的字符串做 equals —— 结果必然 0 行。
//
// ⚠️ 别把它换成"随便一个 id":那会静默返回那个 id 的商品,
// 症状是"点空类别看到了别人的货",比返回全店还难查。
const emptySetSentinel = "__no_match_in_empty_set__"

// ListProducts 分页查商品(不过滤关键词)。
//
// cube 查询见 productPageQuery 的注释。
func (c *DaprCubeClient) ListProducts(ctx context.Context, q ProductQuery) (*ProductPageDTO, error) {
	rows, total, err := c.LoadCubeQueryPage(ctx, "product", productPageQuery(q))
	if err != nil {
		return nil, err
	}
	return &ProductPageDTO{Rows: productRows(rows), Total: total}, nil
}

// SearchProducts 按关键词查商品(命中 product.id 或 product.name)。
//
// # 为什么是 2 次 cube 查询
//
// cube **没有 OR 算子**(可用算子:equals notEquals contains containsCI
// notContains startsWith in notIn gt gte lt lte inDateRange notInDateRange
// onTheDate beforeDate afterDate),所以"编码命中 或 名称命中"必须拆开查,
// 再在 Go 里合并。
//
// # 大小写无关:用 containsCI 算子,不再"再查一次大写"
//
// cube 的 contains 编译成 `RTRIM(col) LIKE '%v%'`,而 DuckDB 的 LIKE **大小写敏感**
// (实测 Face 命中、face 不命中;Z005 命中、z00 不命中)。
// 思迅商品名里混着拉丁品牌名(洁柔Face / C&S / H01-04),商品编码惯例大写
// ("Z005-69") —— 用户在小键盘 / 中文输入法下打出小写是常事,搜不到就等于功能坏掉。
//
// 2026-10-20 起 cube 有了 `containsCI`(列侧 LOWER、模式侧编译期 lower,
// 值仍走参数绑定),编码与名称都用它 ⇒ **一次查询就够**,不再需要
// "原样 q + UPPER(q)"两次查询(那个绕法只覆盖全大写这一种形态)。
//
// # total 是两者之和(上界)
//
// 两个结果集可能有重叠(同一个商品既命中编码又命中名称),所以 total 是上界。
// 前端只用它判断"还有没有下一页",上界可接受;真值要等以后 cube 支持 OR。
func (c *DaprCubeClient) SearchProducts(ctx context.Context, q ProductQuery) (*ProductPageDTO, error) {
	kw := strings.TrimSpace(q.Keyword)
	if kw == "" {
		// 空关键词 = 全量浏览(前端"空关键词 + 全量"走的是 list)。
		return c.ListProducts(ctx, q)
	}
	// 每个子查询都取"够翻到第 N 页"的行数,合并后再在 Go 里切页 ——
	// 因为合并之后才知道真正的第 N 页在哪。
	fetch := q.Offset + q.Limit
	if fetch <= 0 {
		fetch = q.Limit
	}

	type hit struct {
		rows  []ProductDTO
		total int
	}
	var hits []hit

	// (1) 编码命中(大小写无关)
	cq := productPageQuery(q)
	cq.Limit = fetch
	cq.Offset = 0
	cq.Filters = append(cq.Filters, CubeFilter{
		Member: "product.id", Operator: OperatorContainsCI, Values: []any{kw},
	})
	rows, total, err := c.LoadCubeQueryPage(ctx, "product", cq)
	if err != nil {
		return nil, err
	}
	hits = append(hits, hit{rows: productRows(rows), total: total})

	// (2) 名称命中(大小写无关)
	cq = productPageQuery(q)
	cq.Limit = fetch
	cq.Offset = 0
	cq.Filters = append(cq.Filters, CubeFilter{
		Member: "product.name", Operator: OperatorContainsCI, Values: []any{kw},
	})
	rows, total, err = c.LoadCubeQueryPage(ctx, "product", cq)
	if err != nil {
		return nil, err
	}
	hits = append(hits, hit{rows: productRows(rows), total: total})

	// 按 id 去重合并(同一个商品可能同时命中编码与名称)。
	seen := make(map[string]bool)
	merged := make([]ProductDTO, 0, len(hits)*fetch)
	sum := 0
	for _, h := range hits {
		sum += h.total
		for _, r := range h.rows {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			merged = append(merged, r)
		}
	}
	// 合并后按 id 升序排。
	//
	// ⚠️ 为什么不是"编码命中的排前面":编码命中通常是个**很大的**集合
	// (关键词里有数字时可能命中上千个编码),按分组顺序切页会把"名称命中"
	// 整组挤到第 2 页之后 —— 于是"按名称搜"这个最常见的用法会搜不到东西。
	// 按 id 排序后,分页是稳定的,而且"整体 id 靠前"的商品一定在靠前的页里。
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })

	return &ProductPageDTO{Rows: sliceRows(merged, q.Offset, q.Limit), Total: sum}, nil
}

// ListCategories 取分类字典(全量)。
//
// cube 查询:
//
//	{
//	  "measures":   ["category.count"],
//	  "dimensions": ["category.id","category.name","category.parent_id"],
//	  "order":      [{"id":"category.id","order":"asc"}],
//	  "limit":      5000,
//	  "total":      true
//	}
func (c *DaprCubeClient) ListCategories(ctx context.Context) (*CategoryListDTO, error) {
	rows, total, err := c.LoadCubeQueryPage(ctx, "category", CubeQuery{
		Measures: []string{"category.count"},
		// parent_id 是分类**树**的边(思迅 t_bd_item_cls.cls_parent)。
		// 少了它就只能拿到一张平铺的分类表 —— 树是上层(supertrade catalog)
		// 用 Go 侧拼的,因为 cube 的 duck 模型之间不能自连接。
		Dimensions: []string{"category.id", "category.name", "category.parent_id"},
		Order:      []CubeOrder{{ID: "category.id", Order: "asc"}},
		Limit:      catalogDictLimit,
		Total:      true,
	})
	if err != nil {
		return nil, err
	}
	out := make([]CategoryDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, CategoryDTO{
			ID:       asStr(row["category.id"]),
			Name:     asStr(row["category.name"]),
			ParentID: asStr(row["category.parent_id"]),
		})
	}
	return &CategoryListDTO{Rows: out, Total: total}, nil
}

// ListSaleWays 取「销售方式」字典(思迅 t_bd_base_code 中 type_no='SW' 的行)。
//
// cube 查询:
//
//	{
//	  "measures":   ["sale_way.count"],
//	  "dimensions": ["sale_way.id", "sale_way.name"],
//	  "order":      [{"id":"sale_way.id","order":"asc"}],
//	  "limit":      5000,
//	  "total":      true
//	}
//
// ⚠️ 这张表在 cube 侧已经**按 type_no='SW' 过滤过**(取数时过滤,不是查询时),
// 所以这里拿到的就是销售方式的 6 行(A 购销 / B 代销 / C 联营 / …)。
// 整张 t_bd_base_code 灌进来的话 code_id 会跨类型重复,primary_key 撞车 ⇒
// 某些行被静默吞掉,id→name 的对应关系是错的。
func (c *DaprCubeClient) ListSaleWays(ctx context.Context) (*SaleWayListDTO, error) {
	rows, total, err := c.LoadCubeQueryPage(ctx, "sale_way", CubeQuery{
		Measures:   []string{"sale_way.count"},
		Dimensions: []string{"sale_way.id", "sale_way.name"},
		Order:      []CubeOrder{{ID: "sale_way.id", Order: "asc"}},
		Limit:      catalogDictLimit,
		Total:      true,
	})
	if err != nil {
		return nil, err
	}
	out := make([]SaleWayDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, SaleWayDTO{
			ID:   asStr(row["sale_way.id"]),
			Name: asStr(row["sale_way.name"]),
		})
	}
	return &SaleWayListDTO{Rows: out, Total: total}, nil
}

// ListSuppliers 取供应商字典(全量)。
//
// cube 查询:
//
//	{
//	  "measures":   ["supplier.count"],
//	  "dimensions": ["supplier.id","supplier.name","supplier.type",
//	                 "supplier.cust_flag","supplier.status","supplier.supply_way"],
//	  "order":      [{"id":"supplier.id","order":"asc"}],
//	  "limit":      5000,
//	  "total":      true
//	}
//
// ⚠️ 本期**不过滤** cust_flag / type:picker 回答的是"这家店在卖什么",
// 客户条目(C)也挂在商品上。cust_flag 带出来是为了以后能收窄
// (S=供应商 / C=客户 / A=其他)。
//
// ⚠️ supply_way(销售方式代码)是**供应商两级结构的第一层**,
// 名字在 sale_way model 里(见 ListSaleWays)。
func (c *DaprCubeClient) ListSuppliers(ctx context.Context) (*SupplierListDTO, error) {
	rows, total, err := c.LoadCubeQueryPage(ctx, "supplier", CubeQuery{
		Measures: []string{"supplier.count"},
		Dimensions: []string{"supplier.id", "supplier.name", "supplier.type",
			"supplier.cust_flag", "supplier.status", "supplier.supply_way"},
		Order: []CubeOrder{{ID: "supplier.id", Order: "asc"}},
		Limit: catalogDictLimit,
		Total: true,
	})
	if err != nil {
		return nil, err
	}
	out := make([]SupplierDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, SupplierDTO{
			ID:        asStr(row["supplier.id"]),
			Name:      asStr(row["supplier.name"]),
			Type:      asStr(row["supplier.type"]),
			CustFlag:  asStr(row["supplier.cust_flag"]),
			Status:    asStr(row["supplier.status"]),
			SupplyWay: asStr(row["supplier.supply_way"]),
		})
	}
	return &SupplierListDTO{Rows: out, Total: total}, nil
}

// CountProductsByCategory 返回「每个分类下的商品数」。
//
// cube 查询:
//
//	{
//	  "measures":   ["product.count"],
//	  "dimensions": ["product.category_id"],
//	  "order":      [{"id":"product.category_id","order":"asc"}],
//	  "limit":      5000,
//	  "total":      true
//	}
//
// product.count = COUNT(*),按 category_id 分组 ⇒ 每行的 count 就是该分类下的
// 商品数,与 `/products?category_id=X` 的 total 必然相等(同一个表、同一口径)。
//
// 没有分类的商品(空 category_id)不进 map:字典里不存在空 id 这个分类,
// 计入它只会让"侧栏 count"多出一个对不上的条目。
func (c *DaprCubeClient) CountProductsByCategory(ctx context.Context) (map[string]int, error) {
	rows, _, err := c.LoadCubeQueryPage(ctx, "product", CubeQuery{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.category_id"},
		Order:      []CubeOrder{{ID: "product.category_id", Order: "asc"}},
		Limit:      catalogDictLimit,
		Total:      true,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, row := range rows {
		id := asStr(row["product.category_id"])
		if id == "" {
			continue
		}
		out[id] = asInt(row["product.count"])
	}
	return out, nil
}

// CountProductsBySupplier 返回「每个供应商下的商品数」。
//
// cube 查询:
//
//	{
//	  "measures":   ["product.count"],
//	  "dimensions": ["product.supplier_id"],
//	  "order":      [{"id":"product.supplier_id","order":"asc"}],
//	  "limit":      5000,
//	  "total":      true
//	}
//
// 口径与 CountProductsByCategory 一致:与 `/products?supplier_id=X` 的 total 相等。
func (c *DaprCubeClient) CountProductsBySupplier(ctx context.Context) (map[string]int, error) {
	rows, _, err := c.LoadCubeQueryPage(ctx, "product", CubeQuery{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.supplier_id"},
		Order:      []CubeOrder{{ID: "product.supplier_id", Order: "asc"}},
		Limit:      catalogDictLimit,
		Total:      true,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, row := range rows {
		id := asStr(row["product.supplier_id"])
		if id == "" {
			continue
		}
		out[id] = asInt(row["product.count"])
	}
	return out, nil
}

// productRows 把 cube 的原始行转成 ProductDTO。
//
// PriceYuan 走指针:nil = cube 没给售价(未维护),与"售价就是 0 元"是两件事 ——
// 前端要能把两者区分开(否则未维护价会被印成 ¥0.00 流到货架上)。
func productRows(rows []map[string]any) []ProductDTO {
	out := make([]ProductDTO, 0, len(rows))
	for _, row := range rows {
		id := asStr(row["product.id"])
		out = append(out, ProductDTO{
			ID:         id,
			Name:       asStr(row["product.name"]),
			CategoryID: asStr(row["product.category_id"]),
			SupplierID: asStr(row["product.supplier_id"]),
			Unit:       asStr(row["product.unit"]),
			Status:     asStr(row["product.status"]),
			PriceYuan:  asDecimalPtr(row["product.avg_price_yuan"]),
			// Barcode = id(领域事实:思迅 item_no 就是条码)。
			Barcode: id,
		})
	}
	return out
}

// sliceRows 切出 [offset, offset+limit) 这一段(越界安全)。
func sliceRows(rows []ProductDTO, offset, limit int) []ProductDTO {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(rows) {
		return []ProductDTO{}
	}
	end := len(rows)
	if limit > 0 && offset+limit < len(rows) {
		end = offset + limit
	}
	return rows[offset:end]
}
