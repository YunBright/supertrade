// Package service / catalog.go —— 商品选择控件(ProductPicker)的数据服务。
//
// # 本期数据全部来自 cube
//
// catalog 服务还没有本地商品 / 分类 / 供应商主数据(本地表是空的,见
// model/product.go 的说明),所以这一层只做三件事:
//
//  1. 发 cube 查询(商品页 / 分类字典 / 供应商字典 / 按维度计数)
//  2. 在 **Go 侧** join 分类名 / 供应商名
//  3. 把 cube 的行整形成前端契约里的响应体
//
// # 为什么 join 必须在 Go 侧
//
// cube 的 product / category / supplier 都是 **duck** 存储模型,而 cube 的 join
// 引擎只支持 `storage: live` 模型之间的 join —— 所以
// `{"dimensions":["product.id","category.name"]}` 直接 400
// (dimension not found: category.name,实测)。
//
// # 为什么**不按 status 过滤**
//
// 思迅的 product.status 是 "0"~"6" 的状态码,而且**两个门店语义不同**
// (01 店的 status=2 是"精品五花肉""西红柿"这类**正常在售**商品)。
// 按 "status == 1" 过滤会一次性隐藏 01 店 17600 个在售商品。
//
// ⇒ 一律不过滤,status 原样返回,判据留给前端("只有明确的 inactive 才算 inactive")。
//
// # 门店隔离不在这一层
//
// 门店隔离靠 X-Branch-ID → cube-router → branch_cube_sources → 该门店专属 cube
// 实例。一个 cube 实例 = 一家门店,到达 cube 的查询天然只含本店数据,
// 所以这一层的查询里**没有任何 branch 维度**(思迅的 stock.branch_id 是
// branch_no,与本系统 branch_id 是两套无关编码,加了必然失配)。
package service

import (
	"context"
	"log/slog"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cubeclient"
)

// 分页口径(与商品选择控件契约一致)。
const (
	// DefaultPageSize 是商品列表的默认每页条数。
	DefaultPageSize = 50
	// DefaultSearchPageSize 是商品搜索的默认每页条数。
	DefaultSearchPageSize = 20
	// MaxPageSize 是每页条数上限。
	//
	// ⚠️ 上限不是随便定的:page_size 直接落到 cube 的 LIMIT,一个越界值就能
	// 让 cube 把 27k 行商品全部拉进内存。
	MaxPageSize = 200
)

// CatalogService 是商品选择控件的数据服务。
type CatalogService struct {
	cube   cubeclient.Client
	logger *slog.Logger
}

// NewCatalogService 构造 CatalogService。
//
// cube 为 nil 时所有查询返 ErrCubeUnavailable(调用方翻成 503 cube_unavailable)。
func NewCatalogService(cube cubeclient.Client, logger *slog.Logger) *CatalogService {
	if logger == nil {
		logger = slog.Default()
	}
	return &CatalogService{cube: cube, logger: logger}
}

// ErrCubeUnavailable 表示 cube client 未配置。
var ErrCubeUnavailable = errCubeUnavailable{}

type errCubeUnavailable struct{}

func (errCubeUnavailable) Error() string { return "catalog: cube client 未配置" }

// ProductListQuery 是商品列表 / 搜索的查询条件(服务层口径)。
type ProductListQuery struct {
	// Keyword 是子串关键词,命中商品编码或商品名称。空 = 不过滤。
	Keyword string
	// CategoryID / SupplierID 是精确过滤。空 = 不过滤。
	CategoryID string
	SupplierID string
	// SupplierCategoryID 是**供应商类别** id(销售方式:购销/联营/代销/…)。
	//
	// 供应商是两级结构(类别 → 供应商),所以"按某个供应商类别看商品"的语义
	// 是「该类别下的全部供应商」。上层传类别 id,这一层把它展开成
	// 供应商 id 集合,再用 cube 的 `in` 一次查完。
	//
	// ⚠️ `__unclassified__` 是"未分类"哨兵值(见 unclassifiedCategoryID):
	// 某个供应商没有销售方式、或有代码但字典里查不到时,都算"未分类"。
	SupplierCategoryID string
	// Page 从 1 开始;PageSize 是每页条数。
	Page     int
	PageSize int
}

// normalized 把越界的分页参数拉回合法区间。
//
// ⚠️ 这**不是**校验,是兜底:HTTP 层的越界值必须 400(契约要求),
// 服务层被直接调用时也不该算出负 offset 去打 cube。
func (q ProductListQuery) normalized() ProductListQuery {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = DefaultPageSize
	}
	if q.PageSize > MaxPageSize {
		q.PageSize = MaxPageSize
	}
	return q
}

// ProductItem 是商品行(响应 item)。
//
// 字段与 Flutter 的 domain/product/product_models.dart::Product 一一对应。
//
// # 为什么 price_yuan 是指针
//
// nil = 后端没维护售价;0 = 售价真的是 0 元。两者必须能区分 ——
// 前端把未维护价显示成 ¥0.00 会被仓管当真价贴到货架上,那是会流出到门店的
// 错误,不是显示问题。所以这里用 *float64 且**不加 omitempty**:
// 缺失时也要显式写 "price_yuan": null。
type ProductItem struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Barcode    string `json:"barcode"`
	CategoryID string `json:"category_id"`
	// CategoryName 查不到时为 null —— **不编造**"分类-<id>"这种占位名。
	//
	// 占位名看起来是正常的,于是"这个分类到底叫什么"永远没人去查真实数据。
	CategoryName *string `json:"category_name"`
	SupplierID   string  `json:"supplier_id"`
	// SupplierName 查不到时为 null(理由同 CategoryName)。
	SupplierName *string `json:"supplier_name"`
	// Unit 缺失时为 null。
	Unit      *string  `json:"unit"`
	PriceYuan *float64 `json:"price_yuan"`
	// Status 是思迅的**原始状态码**("0"~"6"),不翻译成 active/inactive。
	//
	// 两个门店对状态码的用法不同(01 店的 status=2 是正常在售商品),
	// 没有可信的映射表 ⇒ 原样透传,判据交给前端。
	Status string `json:"status"`
}

// ProductPage 是商品分页响应。
type ProductPage struct {
	Products []ProductItem `json:"products"`
	// Count 是**本页**实际条数。
	Count int `json:"count"`
	// Page / PageSize 回显请求值。
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
	// Total 是过滤后的总条数(不是本页条数)。
	Total int `json:"total"`
}

// CategoryItem 是分类树的一个节点。
//
// # 为什么是**树**而不是平铺列表
//
// 思迅的分类是**多级**的(思迅 t_bd_item_cls.cls_parent,实测门店 00 有
// 3 个顶级 / 186 个节点 / 最深 3 层:生鲜 → 水果 → 进口水果)。
// 平铺列表没法回答"用户想看'水果'下面有什么",所以按树返回。
//
// # count 与 own_count 的区别
//
//   - Count    = 该节点**含所有子孙**的商品数
//   - OwnCount = **本级直接挂**的商品数
//
// 用户点一个节点时看到的是它的**子树**,所以主数字是 Count;OwnCount 让
// "这一级自己挂了多少"也能看见(不然一个全是中间节点的分支看起来是空的)。
type CategoryItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ParentID string `json:"parent_id"`
	// Count 是该节点**含所有子孙**的商品数。with_counts=false 时为 nil。
	//
	// ⚠️ 这个数与 `/products?category_id=<该节点>` 的 total **必然相等**
	// (同一套"子树"口径),所以"侧栏写 87、点进去只有 12"不会出现。
	Count *int `json:"count,omitempty"`
	// OwnCount 是本级直接挂着的商品数。
	OwnCount *int `json:"own_count,omitempty"`
	// Children 是子节点(按 id 升序)。叶子节点是**空数组而不是 null** ——
	// 前端直接 `.map` 就行,不用判空。
	Children []CategoryItem `json:"children"`
}

// CategoryList 是分类树响应。
type CategoryList struct {
	// Categories 是**顶级节点**组成的树(不是平铺列表)。
	Categories []CategoryItem `json:"categories"`
	// Total 是**全部**分类节点数(不是顶级节点数)——
	// 前端用它显示"共 N 个分类"。
	Total int `json:"total"`
}

// SupplierItem 是供应商摘要行。
type SupplierItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Count 是该供应商下的商品数。
	Count *int `json:"count,omitempty"`
	// SupplierCategoryID 是供应商类别代码(销售方式:思迅 t_bd_supcust_info.sale_way,
	// cube 维度 supplier.supply_way)。
	//
	// 空 = 该供应商没有归类(实测两个生产库都有值,但字典可能还没同步)。
	SupplierCategoryID string `json:"supplier_category_id,omitempty"`
	// SupplierCategoryName 是类别名。查不到时为 null(**不编造**占位名)。
	SupplierCategoryName *string `json:"supplier_category_name"`
}

// SupplierCategoryItem 是供应商两级结构的**第一层**(供应商类别)。
type SupplierCategoryItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Count 是该类别下所有供应商的商品数之和。
	Count *int `json:"count,omitempty"`
	// Suppliers 是该类别下的供应商(按 id 升序)。
	Suppliers []SupplierItem `json:"suppliers"`
}

// SupplierList 是供应商摘要响应。
type SupplierList struct {
	// Suppliers 是**平铺**的供应商(向后兼容 + 简单场景直接用)。
	Suppliers []SupplierItem `json:"suppliers"`
	// SupplierCategories 是**两级**结构:类别 → 供应商。
	//
	// ⚠️ 未归类的供应商(cust_clsno 为空、或字典里查不到该编号)都进
	// "未分类"这一组(ID 为空串)。**不能丢掉它们** —— 实测门店 00 有
	// 170/219 个供应商没有类别,丢掉的话侧栏就只剩四分之一。
	SupplierCategories []SupplierCategoryItem `json:"supplier_categories"`
	// Total 是供应商总数。
	Total int `json:"total"`
}

// ListProducts 分页商品列表(不过滤关键词)。
func (s *CatalogService) ListProducts(ctx context.Context, q ProductListQuery) (*ProductPage, error) {
	return s.productPage(ctx, q, false)
}

// SearchProducts 关键字商品列表。
//
// 关键词为空时等价于 ListProducts(前端"空关键词 + 全量浏览"走的是 list)。
func (s *CatalogService) SearchProducts(ctx context.Context, q ProductListQuery) (*ProductPage, error) {
	if q.Keyword == "" {
		return s.ListProducts(ctx, q)
	}
	return s.productPage(ctx, q, true)
}

// productPage 是列表 / 搜索的共同实现。
//
// cube 查询数固定:商品页(1 或 3 次)+ 分类字典 + 供应商字典。
func (s *CatalogService) productPage(ctx context.Context, q ProductListQuery, search bool) (*ProductPage, error) {
	if s.cube == nil {
		return nil, ErrCubeUnavailable
	}
	q = q.normalized()

	// 分类字典**先取**:商品查询要用它把"选中的分类"展开成子树,
	// 响应也要用它 join 分类名。
	cats, err := s.cube.ListCategories(ctx)
	if err != nil {
		return nil, err
	}

	cq := cubeclient.ProductQuery{
		Keyword:    q.Keyword,
		SupplierID: q.SupplierID,
		Offset:     (q.Page - 1) * q.PageSize,
		Limit:      q.PageSize,
	}
	// ⚠️ 分类过滤的语义是「该分类 + 它的所有子孙分类」(分类是树)。
	// 展开成 id 集合后用 cube 的 `in` 一次查完。
	//
	// 这样 `/products?category_id=01` 与分类树里 01 节点的 count 必然相等 ——
	// 两边是同一套"子树"口径,不会出现"侧栏写 2450、点进去只有 35"。
	if q.CategoryID != "" {
		cq.CategoryIDs = categorySubtree(cats.Rows, q.CategoryID)
	}

	// 供应商类别 → 该类别下的供应商 id 集合。
	//
	// 类别 = **销售方式**(思迅 t_bd_supcust_info.sale_way → cube supplier.supply_way,
	// 字典 t_bd_base_code 的 type_no='SW':购销/代销/联营/租赁/自产/扣率代销)。
	//
	// ⚠️ 类别字典**只取一次**:它是"类别 → 名字",供应商字典才给"供应商 → 类别代码"。
	// 在循环里查字典就是 N+1(219 个供应商 = 219 次 cube 查询)。
	if q.SupplierCategoryID != "" {
		sups, err := s.cube.ListSuppliers(ctx)
		if err != nil {
			return nil, err
		}
		ways, err := s.cube.ListSaleWays(ctx)
		if err != nil {
			// 类别字典拿不到 ⇒ 只能按代码精确匹配(未分类组就选不出来)。
			s.logger.Warn("catalog: 取供应商类别字典失败,按类别过滤退化为精确匹配",
				"err", err)
			ways = &cubeclient.SaleWayListDTO{}
		}
		known := make(map[string]bool, len(ways.Rows))
		for _, t := range ways.Rows {
			known[t.ID] = true
		}
		ids := make([]string, 0, len(sups.Rows))
		for _, sp := range sups.Rows {
			cls := sp.SupplyWay
			// 未归类:销售方式为空;或者有代码但字典里查不到
			// (新加的代码字典还没同步)—— 都算"未分类"。
			isUnclassified := cls == "" || !known[cls]
			if (q.SupplierCategoryID == unclassifiedCategoryID && isUnclassified) ||
				(q.SupplierCategoryID != unclassifiedCategoryID &&
					cls == q.SupplierCategoryID) {
				ids = append(ids, sp.ID)
			}
		}
		cq.SupplierIDs = ids
	}

	var (
		page *cubeclient.ProductPageDTO
	)
	if search {
		page, err = s.cube.SearchProducts(ctx, cq)
	} else {
		page, err = s.cube.ListProducts(ctx, cq)
	}
	if err != nil {
		return nil, err
	}
	sups, err := s.cube.ListSuppliers(ctx)
	if err != nil {
		return nil, err
	}
	items := joinProducts(page.Rows, cats.Rows, sups.Rows)
	return &ProductPage{
		Products: items,
		Count:    len(items),
		Page:     q.Page,
		PageSize: q.PageSize,
		Total:    page.Total,
	}, nil
}

// ListCategories 分类**树**。
//
// withCounts=true 时每个节点带 count(含所有子孙)与 own_count(本级)。
//
// ⚠️ 节点 count 与 `/products?category_id=<该节点>` 的 total **必然相等**
// (同一套"子树"口径),所以"侧栏写 87、点进去只有 12"这种自相矛盾不会出现。
func (s *CatalogService) ListCategories(ctx context.Context, withCounts bool) (*CategoryList, error) {
	if s.cube == nil {
		return nil, ErrCubeUnavailable
	}
	cats, err := s.cube.ListCategories(ctx)
	if err != nil {
		return nil, err
	}
	var counts map[string]int
	if withCounts {
		if counts, err = s.cube.CountProductsByCategory(ctx); err != nil {
			return nil, err
		}
	}
	return &CategoryList{
		Categories: buildCategoryTree(cats.Rows, counts),
		Total:      cats.Total,
	}, nil
}

// ListSuppliers 供应商**两级**摘要(类别 → 供应商)。
//
// count 与 `/products?supplier_id=X` 的 total 必然相等(口径同上)。
func (s *CatalogService) ListSuppliers(ctx context.Context) (*SupplierList, error) {
	if s.cube == nil {
		return nil, ErrCubeUnavailable
	}
	sups, err := s.cube.ListSuppliers(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := s.cube.CountProductsBySupplier(ctx)
	if err != nil {
		return nil, err
	}
	// 类别字典查不到**不算失败**:那只是"没有类别名"这一层信息缺失,
	// 供应商本身还在。字典拿不到时所有供应商都进"未分类"。
	ways, err := s.cube.ListSaleWays(ctx)
	if err != nil {
		s.logger.Warn("catalog: 取供应商类别字典失败,供应商全部归入未分类", "err", err)
		ways = &cubeclient.SaleWayListDTO{}
	}

	flat := make([]SupplierItem, 0, len(sups.Rows))
	for _, sp := range sups.Rows {
		n := counts[sp.ID]
		item := SupplierItem{ID: sp.ID, Name: sp.Name, Count: &n}
		if sp.SupplyWay != "" {
			item.SupplierCategoryID = sp.SupplyWay
			if name, ok := saleWayName(ways.Rows, sp.SupplyWay); ok {
				item.SupplierCategoryName = &name
			}
		}
		flat = append(flat, item)
	}
	return &SupplierList{
		Suppliers:           flat,
		SupplierCategories: buildSupplierCategories(flat, ways.Rows),
		Total:               sups.Total,
	}, nil
}

// saleWayName 在「销售方式」字典里查名字。
func saleWayName(ways []cubeclient.SaleWayDTO, id string) (string, bool) {
	for _, t := range ways {
		if t.ID == id {
			return t.Name, true
		}
	}
	return "", false
}

// 未归类供应商的组 id / 名字。
//
// ⚠️ id **不是空串**:空串在 query 参数里与"没传"分不开
// (`c.Query` 两者都返 "")—— 于是"按未分类过滤"永远表达不出来。
// 用一个显式哨兵值,前端拿到就能原样回传给后端。
const (
	unclassifiedCategoryID   = "__unclassified__"
	unclassifiedCategoryName = "未分类"
)

// buildSupplierCategories 把平铺的供应商按类别分组(两级结构的第一层)。
//
// 类别 = 销售方式(购销/代销/联营/租赁/自产/扣率代销)。
//
// ⚠️ 三条不能丢数据的规则:
//
//  1. 字典里**没有**的类别代码 → 归到"未分类"(不能凭空建一个组,
//     也不能把供应商丢掉);
//  2. 没有类别代码的供应商 → 归到"未分类";
//  3. 字典本身是空的(字典查失败 / 源库没同步)→ **所有**供应商都进"未分类",
//     仍然返回一个非空的列表(前端才不至于显示空白)。
func buildSupplierCategories(flat []SupplierItem, ways []cubeclient.SaleWayDTO) []SupplierCategoryItem {
	// 先按字典顺序建组(空的先建,后面统一追加未分类的)。
	groups := make([]*SupplierCategoryItem, 0, len(ways)+1)
	byID := make(map[string]*SupplierCategoryItem, len(ways)+1)
	for _, t := range ways {
		g := &SupplierCategoryItem{
			ID:        t.ID,
			Name:      t.Name,
			Suppliers: []SupplierItem{},
		}
		groups = append(groups, g)
		byID[t.ID] = g
	}
	unclassified := &SupplierCategoryItem{
		ID:        unclassifiedCategoryID,
		Name:      unclassifiedCategoryName,
		Suppliers: []SupplierItem{},
	}
	groups = append(groups, unclassified)

	for _, s := range flat {
		g, ok := byID[s.SupplierCategoryID]
		if !ok || s.SupplierCategoryID == "" {
			g = unclassified
		}
		g.Suppliers = append(g.Suppliers, s)
	}
	// 每组的 count = 该组供应商的商品数之和。
	//
	// ⚠️ **没有任何供应商的组不返回**。
	//
	// 字典里有 6 个销售方式(购销/代销/联营/租赁/自产/扣率代销),但一个门店
	// 未必每种都有(实测门店 00 没有"扣率代销")。返回空组的话,App 的两级
	// 侧栏里会出现一个**点进去什么都没有**的分类头 —— 那是死路,
	// 而且用户会以为"系统里有这个分类但没供应商"。
	//
	// ⚠️ 只丢"**零供应商**"的组。**有供应商但商品数为 0** 的组要留着
	// (实测门店 00 的"租赁"/"自产"各 1 个供应商、0 个商品):
	// 供应商本身存在,藏起来才是真的丢数据。
	out := make([]SupplierCategoryItem, 0, len(groups))
	for _, g := range groups {
		if len(g.Suppliers) == 0 {
			continue
		}
		sum := 0
		for _, s := range g.Suppliers {
			if s.Count != nil {
				sum += *s.Count
			}
		}
		n := sum
		g.Count = &n
		out = append(out, *g)
	}
	return out
}

// catNode 是建树过程中的中间节点(用指针,值拷贝会丢子树)。
type catNode struct {
	item     CategoryItem
	children []*catNode
}

// buildCategoryTree 把平铺的分类字典拼成树,并算好每个节点的 count。
//
// counts 是"每个分类**本级**的商品数"(来自 cube 的按 category_id 分组计数)。
// 节点的 count = 本级 + 全部子孙(自底向上累加)。
//
// ⚠️ 两条不能丢数据的规则:
//
//  1. 父节点在字典里**不存在**(数据不一致)→ 该节点当**顶级**处理,
//     不能让它凭空消失;
//  2. 父链成环(A→B→A)→ 递归会无限循环。建树时按"已访问"集合切断,
//     成环的节点降级成顶级。
func buildCategoryTree(cats []cubeclient.CategoryDTO, counts map[string]int) []CategoryItem {
	nodes := make(map[string]*catNode, len(cats))
	for _, c := range cats {
		n := counts[c.ID]
		nodes[c.ID] = &catNode{item: CategoryItem{
			ID:       c.ID,
			Name:     c.Name,
			ParentID: c.ParentID,
			OwnCount: &n,
			Children: []CategoryItem{},
		}}
	}

	var roots []*catNode
	for _, c := range cats {
		n := nodes[c.ID]
		p, ok := nodes[c.ParentID]
		if !ok || c.ParentID == "" || c.ParentID == c.ID {
			roots = append(roots, n)
			continue
		}
		p.children = append(p.children, n)
	}

	// 自底向上算 count;顺带把成环的边切断(访问过的节点不再往上挂)。
	visiting := make(map[string]bool, len(nodes))
	var build func(n *catNode) (CategoryItem, int)
	build = func(n *catNode) (CategoryItem, int) {
		item := n.item
		total := 0
		if n.item.OwnCount != nil {
			total = *n.item.OwnCount
		}
		for _, ch := range n.children {
			if visiting[ch.item.ID] {
				// 成环:这个子节点已经算过了,不再重复累加。
				continue
			}
			visiting[n.item.ID] = true
			ci, ct := build(ch)
			delete(visiting, n.item.ID)
			total += ct
			item.Children = append(item.Children, ci)
		}
		cnt := total
		item.Count = &cnt
		return item, total
	}

	out := make([]CategoryItem, 0, len(roots))
	for _, r := range roots {
		item, _ := build(r)
		out = append(out, item)
	}
	return out
}

// categorySubtree 返回某分类**及其所有子孙**的 id 集合。
//
// 用途:"按某个分类看商品"的语义是"这个分类 + 它下面的所有分类"。
//
// ⚠️ 成环保护:父链可能是脏数据(A→B→A),不切断就会无限循环。
func categorySubtree(cats []cubeclient.CategoryDTO, rootID string) []string {
	if rootID == "" {
		return nil
	}
	children := make(map[string][]string, len(cats))
	exists := make(map[string]bool, len(cats))
	for _, c := range cats {
		exists[c.ID] = true
		if c.ParentID != "" && c.ParentID != c.ID {
			children[c.ParentID] = append(children[c.ParentID], c.ID)
		}
	}
	if !exists[rootID] {
		// 字典里没有这个分类(前端传了个不存在的 id)→ 退化成精确匹配。
		return []string{rootID}
	}
	out := []string{}
	seen := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
		for _, ch := range children[id] {
			walk(ch)
		}
	}
	walk(rootID)
	return out
}

// LegacyProductItem 是 ?barcode= 老路径的响应项。
//
// # 为什么它是新字段的**超集**
//
// Flutter 的 POP 价签流(lib/infra/pop/pop_repository_impl.dart)与老 catalog
// 仓库(lib/infra/catalog/catalog_repository_impl.dart)读的是老字段名:
//
//	product_id / product_name / unit / id / name
//
// 后端一旦改名,它们不会报错,只会**静默拿到 null** —— 价签上就印出空品名。
//
// ⇒ 这里同时给新字段(id / name / barcode / category_id / category_name /
// supplier_id / supplier_name / unit / price_yuan / status)与老别名
// (product_id / product_name / category),两边同时存在,谁读都有值。
type LegacyProductItem struct {
	ProductItem
	// ProductID / ProductName / Category 是老字段别名。
	ProductID   string `json:"product_id"`
	ProductName string `json:"product_name"`
	Category    string `json:"category"`
	// StockQty / AvgCostYuan 只在老路径有(那条路径会做一次库存查询)。
	//
	// 类型保持 decimal.Decimal:它序列化出来是**数字或字符串**(取决于指数),
	// 改类型就等于改了线上契约。
	StockQty    *decimal.Decimal `json:"stock_qty,omitempty"`
	AvgCostYuan *decimal.Decimal `json:"avg_cost_yuan,omitempty"`
}

// LegacyMeta 是老路径的 meta 段(保留字段,不改语义)。
type LegacyMeta struct {
	BarcodeQuery string `json:"barcode_query"`
	BranchID     string `json:"branch_id"`
}

// LegacyProductPage 是 ?barcode= 老路径的响应体。
//
// 保留了老字段 products / count / meta,并**追加** page / page_size / total
// (新契约的字段)。老客户端只读 products / count,不受影响。
type LegacyProductPage struct {
	Products []LegacyProductItem `json:"products"`
	Count    int                 `json:"count"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"page_size"`
	Total    int                 `json:"total"`
	Meta     LegacyMeta          `json:"meta"`
}

// SearchProductsByBarcode 是 ?barcode= 老路径:按条码查商品 + 合并该门店库存。
//
// 与老行为一致的地方:
//
//   - 做**一次**库存查询(所以 stock_qty / avg_cost_yuan 有值)
//   - 响应体保留 products / count / meta
//
// 与老行为不同的地方:商品项多给了新字段(id / name / barcode / category_id /
// category_name / supplier_id / supplier_name / status),并且**不再**逐行查
// 供应商(老代码那是个 N+1,而且查的是"名称包含供应商 id",几乎永远查不到 ——
// 于是 supplier_name 基本恒为空)。
//
// 字典查询失败时**不**让整个请求失败:老路径的核心数据是"商品 + 库存",
// 名字查不到就留 null(前端会显示空分类名,但价签该印的品名/单位照样印)。
func (s *CatalogService) SearchProductsByBarcode(ctx context.Context, barcode, branchID string, limit int) (*LegacyProductPage, error) {
	if s.cube == nil {
		return nil, ErrCubeUnavailable
	}
	rows, err := s.cube.SearchProductsByBarcode(ctx, barcode, branchID, limit)
	if err != nil {
		return nil, err
	}
	// 字典是"锦上添花":查不到名字不影响商品本身。
	cats, sups := s.dictionariesBestEffort(ctx)

	// 字典索引只建一次:老路径也要 join 分类名 / 供应商名。
	products := make([]LegacyProductItem, 0, len(rows))
	catNames := categoryNameIndex(cats)
	supNames := supplierNameIndex(sups)
	for _, r := range rows {
		if r.Product == nil {
			continue
		}
		legacy := LegacyProductItem{
			ProductItem: joinProduct(*r.Product, catNames, supNames),
			ProductID:   r.Product.ID,
			ProductName: r.Product.Name,
			Category:    r.Product.CategoryID,
		}
		if r.Stock != nil {
			qty := r.Stock.Quantity
			cost := r.Stock.AvgCostYuan
			legacy.StockQty = &qty
			legacy.AvgCostYuan = &cost
		}
		products = append(products, legacy)
	}
	return &LegacyProductPage{
		Products: products,
		Count:    len(products),
		Page:     1,
		PageSize: limit,
		Total:    len(products),
		Meta:     LegacyMeta{BarcodeQuery: barcode, BranchID: branchID},
	}, nil
}

// dictionariesBestEffort 取分类 / 供应商字典;失败时返空字典 + 记日志。
func (s *CatalogService) dictionariesBestEffort(ctx context.Context) ([]cubeclient.CategoryDTO, []cubeclient.SupplierDTO) {
	var cats []cubeclient.CategoryDTO
	var sups []cubeclient.SupplierDTO
	if c, err := s.cube.ListCategories(ctx); err != nil {
		s.logger.Warn("catalog: 取分类字典失败,商品名留空", "err", err)
	} else {
		cats = c.Rows
	}
	if sp, err := s.cube.ListSuppliers(ctx); err != nil {
		s.logger.Warn("catalog: 取供应商字典失败,供应商名留空", "err", err)
	} else {
		sups = sp.Rows
	}
	return cats, sups
}

// joinProducts 把 cube 的商品行与分类 / 供应商字典 join 起来。
//
// 字典里查不到时给 null(**不编造**"分类-<id>"这种占位名)。
func joinProducts(rows []cubeclient.ProductDTO, cats []cubeclient.CategoryDTO, sups []cubeclient.SupplierDTO) []ProductItem {
	catNames := categoryNameIndex(cats)
	supNames := supplierNameIndex(sups)
	out := make([]ProductItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, joinProduct(r, catNames, supNames))
	}
	return out
}

// categoryNameIndex 把分类字典收成 id → name 的索引。
func categoryNameIndex(cats []cubeclient.CategoryDTO) map[string]string {
	out := make(map[string]string, len(cats))
	for _, c := range cats {
		out[c.ID] = c.Name
	}
	return out
}

// supplierNameIndex 把供应商字典收成 id → name 的索引。
func supplierNameIndex(sups []cubeclient.SupplierDTO) map[string]string {
	out := make(map[string]string, len(sups))
	for _, s := range sups {
		out[s.ID] = s.Name
	}
	return out
}

// joinProduct 把一行商品 join 上分类名 / 供应商名。
func joinProduct(r cubeclient.ProductDTO, catNames, supNames map[string]string) ProductItem {
	item := ProductItem{
		ID:   r.ID,
		Name: r.Name,
		// barcode = id(领域事实:思迅 item_no 就是条码,实测
		// 6957583900828 → item_no 同值)。
		//
		// 这里**直接用 id**而不是 DTO 里的 Barcode 字段:契约要求
		// barcode == id,前端拿它当扫码 key。让响应层强制这条等式,
		// 比"信任上游字段恰好相等"稳 —— 上游哪天多映射了一列真正的
		// EAN 条码,前端就会同时看到两个不同的条码。
		Barcode:    r.ID,
		CategoryID: r.CategoryID,
		SupplierID: r.SupplierID,
		Unit:       strPtr(r.Unit),
		PriceYuan:  pricePtr(r.PriceYuan),
		Status:     r.Status,
	}
	if r.CategoryID != "" {
		if n, ok := catNames[r.CategoryID]; ok {
			item.CategoryName = &n
		}
	}
	if r.SupplierID != "" {
		if n, ok := supNames[r.SupplierID]; ok {
			item.SupplierName = &n
		}
	}
	return item
}

// strPtr 空串 → nil(缺失的语义),非空 → 指针。
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// pricePtr nil(未维护价)→ nil;0 元 → 0(两者必须能区分)。
func pricePtr(d *decimal.Decimal) *float64 {
	if d == nil {
		return nil
	}
	f := d.InexactFloat64()
	return &f
}
