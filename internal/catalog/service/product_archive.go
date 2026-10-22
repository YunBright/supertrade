// Package service / product_archive.go —— 商品档案(图1)的读服务。
//
// # 这个端点回答什么
//
// 图1 的商品档案主表 + 「供货信息」页签。与 picker(`/products`)的区别:
//
//	picker      面向**收银员选货**:只要 id / 名称 / 分类 / 单位 / 一个价。
//	product-archive 面向**档案管理**:要品牌、四个价、供货约定、规格。
//
// ⇒ 两者**不能**复用同一批响应结构体(字段集不同),但**形状纪律一样**:
// 本地路径与 cube 路径共用本文件定义的 ProductArchiveItem / SupplierPriceItem。
//
// # 数据来源按「门店配没配数据源」分流(判据唯一:internal/cubesource)
//
//	配了源 ⇒ 走 cube(cube-router → 该门店专属 cube 实例)
//	没配源 ⇒ 走本域 PG(自营店 10)
//	问不到 / 被停用 ⇒ **503**,不回落本地。
//
// ⚠️ **回落本地 = 把一次链路故障显示成"这家店没有商品"** ——
// 这是 2026-10-21 生产事故的形状,禁止回落。
//
// # cube 侧维度契约(**假设**,cube 仓并行实现中)
//
// 本文件用到的 cube model 与成员:
//
//	product       id / name / sub_code / category_id / supplier_id /
//	              unit / spec / status / brand_name
//	              (brand_name 由 cube 侧按 PLAN §2.1 新增,源列 item_brandname)
//	branch_price  item_no / sale_way / supcust_no +
//	              cost_yuan / wholesale_yuan / retail_yuan / member_price_yuan
//	              (新 model,源表 t_pc_branch_price)
//	supplier_item item_no / branch_no / supplier_id / sale_way /
//	              appointed_price_yuan / top_price_yuan /
//	              bottom_price_yuan / last_price_yuan / contract_date
//	              (新 model,源表 t_bd_supcust_item)
//
// ⚠️ 金额成员名取 **FIELD_SPEC §0 的 `<name>_yuan` 规则**,并与 PLAN §3.1
// 的响应字段名逐字一致 —— 响应字段名是冻结的,让 cube 的 canonical 名与它
// 对齐,少一层翻译 ⇒ 少一处可能漂移的地方。
//
// ⚠️ 若 cube 侧最终用了别的成员名,改这里**一处**即可
// (维度清单集中在 productArchiveCubeDims / productArchivePriceDims)。
//
// # ⚠️ is_points 恒 false
//
// PLAN §3.1 的响应里有 `is_points`(是否积分商品),但思迅源库与 cube 侧
// **都没有对应列**(积分商品在另一张表里,本轮没有勘察结论)。
//
// ⇒ 本轮两条路径都返 false,并在此显式写死。
// 宁可"恒 false 且有注释",也不要编一个含义不明的列 ——
// 前端拿 false 去过滤会少显示几行,但拿一个猜出来的真值去过滤会错得更离谱。
package service

import (
	"context"
	"errors"
	"strings"

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead"
)

// ErrLocalArchiveUnavailable 本地库未装配(装配漏了 ⇒ 500,不是"没有商品")。
var ErrLocalArchiveUnavailable = errors.New("catalog: 本地商品档案库未装配")

// ErrProductArchiveReaderMissing cube 读端未装配(配了源却读不了 ⇒ 503)。
var ErrProductArchiveReaderMissing = errors.New("catalog: cube 读端未装配")

// ProductArchiveQuery 是商品档案列表的查询条件。
type ProductArchiveQuery struct {
	// Keyword 命中货号 / 自编码 / 名称(大小写无关)。空 = 不过滤。
	Keyword string
	// CategoryID 按**子树**过滤(该分类 + 它的所有子孙)。空 = 不过滤。
	//
	// ⚠️ 必须是子树而不是精确匹配:两条路径都这样(cube 侧 categorySubtree,
	// 本地侧 localCategorySubtree),否则侧栏 count 与列表 total 会对不上。
	CategoryID string
	// SupplierID 精确过滤。空 = 不过滤。
	SupplierID string
	// Page 从 1 开始;PageSize 是每页条数。
	Page     int
	PageSize int
}

// normalized 把越界的分页参数拉回合法区间(兜底,不是校验)。
func (q ProductArchiveQuery) normalized() ProductArchiveQuery {
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

// ProductArchiveItem 是商品档案的一行(PLAN §3.1 的响应项)。
//
// ⚠️ 字段名与顺序以外的**名字**都是冻结契约(前端按它解析)⇒ 不许改名。
//
// # ⚠️ 金额是 **string**,且 nullable
//
//	cost_yuan / wholesale_yuan / retail_yuan / member_price_yuan
//	都是 *string:nil ⇒ JSON null ⇒ "没维护"。
//	⚠️ **null ≠ "0.0000"**:前端把未维护价显示成 ¥0.00 会被仓管当真价
//	贴到货架上(本仓反复强调的约定)。
type ProductArchiveItem struct {
	// ID 是货号(思迅 item_no)。与 ItemNo 同值,两个都给出是为了前端方便。
	ID string `json:"id"`
	// ItemNo 是货号(cube product.id)。
	ItemNo string `json:"item_no"`
	// SubCode 是自编码 / 助记码(思迅 item_subno)。⚠️ 不唯一。
	SubCode string `json:"sub_code"`
	// Name 是商品名。
	Name string `json:"name"`
	// IsPoints 是"是否积分商品"。
	//
	// ⚠️ 本轮**恒 false**:思迅与 cube 侧都没有这一列(见包注释)。
	IsPoints bool `json:"is_points"`
	// CategoryID / CategoryName 是分类。查不到名字时 CategoryName 为 null。
	CategoryID   string  `json:"category_id"`
	CategoryName *string `json:"category_name"`
	// SupplierID / SupplierName 是主供应商。查不到名字时 SupplierName 为 null。
	SupplierID   string  `json:"supplier_id"`
	SupplierName *string `json:"supplier_name"`
	// Spec 是规格(思迅 item_size,实测有 "1*10" 这类脏值 —— 原样存)。
	Spec string `json:"spec"`
	// BrandName 是品牌(思迅 item_brandname,实测覆盖率 100%)。
	//
	// ⚠️ 本地路径恒为 null:本域 `products` 表没有品牌列
	// (见 model/product_price.go 与本文件包注释)。
	BrandName *string `json:"brand_name"`
	// Unit 是基本单位(思迅 unit_no,值即单位名本身)。
	Unit string `json:"unit"`
	// CostYuan / WholesaleYuan / RetailYuan / MemberPriceYuan 是四个价。
	//
	// ⚠️ nil ⇒ null ⇒ **没维护**(不是 0 元)。
	CostYuan        *string `json:"cost_yuan"`
	WholesaleYuan   *string `json:"wholesale_yuan"`
	RetailYuan      *string `json:"retail_yuan"`
	MemberPriceYuan *string `json:"member_price_yuan"`
	// Status 是商品状态(原样透传思迅的状态码,不翻译)。
	Status string `json:"status"`
}

// ProductArchivePage 是商品档案分页响应(PLAN §3.1)。
type ProductArchivePage struct {
	Items []ProductArchiveItem `json:"items"`
	// Count 是**本页**条数。
	Count int `json:"count"`
	// Page / PageSize 回显请求值。
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
	// Total 是过滤后的总条数(不是本页条数)。
	Total int `json:"total"`
	// DataSource 是"cube" / "local"。
	//
	// ⚠️ **这是 PLAN §3.1 之外的附加字段**(冻结契约只列了 items/count/page/
	// page_size/total)。加它的理由:本地路径的价格列可能全是 null,
	// 没有这个标记,调用方无法区分"这家店没维护价"与"这条路查的是别家店"。
	// 前端按固定字段解析,多一个字段不会破契约。
	DataSource string `json:"data_source,omitempty"`
}

// SupplierPriceItem 是「供货信息」的一行(PLAN §3.1 的第二个响应)。
type SupplierPriceItem struct {
	// ItemNo 是货号。
	ItemNo string `json:"item_no"`
	// BranchID / BranchName 是分店。查不到名字时 BranchName 为 null。
	BranchID   string  `json:"branch_id"`
	BranchName *string `json:"branch_name"`
	// SupplierID / SupplierName 是供应商。
	SupplierID   string  `json:"supplier_id"`
	SupplierName *string `json:"supplier_name"`
	// AppointedPriceYuan 是约定价格;TopPriceYuan 最高价;
	// BottomPriceYuan 最低价;LastPriceYuan 最近价。
	//
	// ⚠️ 全部 nullable:nil ⇒ null ⇒ 没维护(不是 0 元)。
	AppointedPriceYuan *string `json:"appointed_price_yuan"`
	TopPriceYuan       *string `json:"top_price_yuan"`
	BottomPriceYuan    *string `json:"bottom_price_yuan"`
	LastPriceYuan      *string `json:"last_price_yuan"`
	// SaleWay 是销售方式代码。
	SaleWay string `json:"sale_way"`
	// ContractDate 是合同有效期(YYYY-MM-DD)。没有则 null。
	ContractDate *string `json:"contract_date"`
}

// SupplierPriceList 是「供货信息」的响应。
type SupplierPriceList struct {
	Items []SupplierPriceItem `json:"items"`
	// Count 是条数。
	Count int `json:"count"`
	// DataSource 见 ProductArchivePage.DataSource。
	DataSource string `json:"data_source,omitempty"`
}

// ---- cube 维度清单(契约假设集中在这里) ----

// productArchiveCubeDims 是商品档案页的 cube 维度。
//
// ⚠️ **不要**在这里少选一个维度:cube 的 dimensions 全部进 GROUP BY,
// 少一个就把不同的商品合并成一行 ⇒ count / total 会跟着变小,
// 而"侧栏 count == 列表 total"这条等式会悄悄不成立。
func productArchiveCubeDims() []string {
	return []string{
		"product.id", "product.name", "product.sub_code", "product.category_id",
		"product.supplier_id", "product.unit", "product.spec", "product.status",
		"product.brand_name",
	}
}

// productArchivePriceDims 是分店价格 model 的维度 + 金额维度。
//
// ⚠️ 金额是**维度**而不是 measure:一行的价是"这个 (商品,供应商,销售方式)
// 的价",不是需要 SUM 的事实值。做成 measure(SUM)会把多行加成一个错数。
func productArchivePriceDims() []string {
	return []string{
		"branch_price.item_no", "branch_price.sale_way", "branch_price.supcust_no",
		"branch_price.cost_yuan", "branch_price.wholesale_yuan",
		"branch_price.retail_yuan", "branch_price.member_price_yuan",
	}
}

// supplierItemDims 是供货信息 model 的维度。
func supplierItemDims() []string {
	return []string{
		"supplier_item.item_no", "supplier_item.branch_no", "supplier_item.supplier_id",
		"supplier_item.sale_way", "supplier_item.appointed_price_yuan",
		"supplier_item.top_price_yuan", "supplier_item.bottom_price_yuan",
		"supplier_item.last_price_yuan", "supplier_item.contract_date",
	}
}

// SetCubeReader 装配 cube 读端(通用查询:model + 维度 + 过滤)。
//
// ⚠️ 生产**必须**装配(cmd/catalog/main.go 里传同一个 *cubeclient.DaprCubeClient):
// 没装 ⇒ 配了数据源的门店的档案查询返 503,而本地路径照常 ——
// 症状是"同一家店,商品档案查不到、但商品选择器能查到"。
func (s *CatalogService) SetCubeReader(r cuberead.Reader) {
	s.reader = r
}

// ListProductArchive 商品档案列表(PLAN §3.1)。
func (s *CatalogService) ListProductArchive(ctx context.Context, q ProductArchiveQuery) (*ProductArchivePage, error) {
	branchID := cubeclient.BranchFromContext(ctx)
	useCube, err := s.useCubeForRead(ctx, branchID)
	if err != nil {
		return nil, err
	}
	q = q.normalized()
	if useCube {
		return s.cubeProductArchive(ctx, branchID, q)
	}
	return s.localProductArchive(ctx, branchID, q)
}

// cubeProductArchive 走 cube 的商品档案列表。
func (s *CatalogService) cubeProductArchive(ctx context.Context, branchID string, q ProductArchiveQuery) (*ProductArchivePage, error) {
	if s.reader == nil {
		return nil, ErrProductArchiveReaderMissing
	}
	// 分类字典**先取**:分类过滤要展开成子树,响应也要用它 join 分类名。
	cats, err := s.cube.ListCategories(ctx)
	if err != nil {
		return nil, err
	}
	cq := cubeclient.CubeQuery{
		Measures:   []string{"product.count"},
		Dimensions: productArchiveCubeDims(),
		Order:      []cubeclient.CubeOrder{{ID: "product.id", Order: "asc"}},
		Limit:      q.PageSize,
		Offset:     (q.Page - 1) * q.PageSize,
		Total:      true,
	}
	if q.Keyword != "" {
		// ⚠️ containsCI(cube 的 contains 是大小写**敏感**的):
		// 思迅商品名里混着拉丁品牌名(洁柔Face / C&S),用户打出小写就搜不到。
		kw := strings.TrimSpace(q.Keyword)
		cq.Filters = append(cq.Filters,
			cuberead.Filter("product.id", cubeclient.OperatorContainsCI, kw),
			cuberead.Filter("product.sub_code", cubeclient.OperatorContainsCI, kw),
			cuberead.Filter("product.name", cubeclient.OperatorContainsCI, kw),
		)
	}
	if q.CategoryID != "" {
		// 子树语义:该分类 + 它的所有子孙(cube 侧同一套 categorySubtree)。
		cq.Filters = append(cq.Filters,
			cuberead.InFilter("product.category_id", categorySubtree(cats.Rows, q.CategoryID)))
	}
	if q.SupplierID != "" {
		cq.Filters = append(cq.Filters,
			cuberead.Filter("product.supplier_id", cubeclient.OperatorEquals, q.SupplierID))
	}
	rows, total, err := s.reader.LoadCubeQueryPage(ctx, "product", cq)
	if err != nil {
		return nil, err
	}
	sups, err := s.cube.ListSuppliers(ctx)
	if err != nil {
		return nil, err
	}
	catNames := map[string]string{}
	for _, c := range cats.Rows {
		catNames[c.ID] = c.Name
	}
	supNames := map[string]string{}
	for _, sp := range sups.Rows {
		supNames[sp.ID] = sp.Name
	}

	items := make([]ProductArchiveItem, 0, len(rows))
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		id := cuberead.Str(r, "product.id")
		ids = append(ids, id)
		items = append(items, ProductArchiveItem{
			ID:         id,
			ItemNo:     id,
			SubCode:    cuberead.Str(r, "product.sub_code"),
			Name:       cuberead.Str(r, "product.name"),
			IsPoints:   false,
			CategoryID: cuberead.Str(r, "product.category_id"),
			SupplierID: cuberead.Str(r, "product.supplier_id"),
			Spec:       cuberead.Str(r, "product.spec"),
			Unit:       cuberead.Str(r, "product.unit"),
			Status:     cuberead.Str(r, "product.status"),
		})
		if n, ok := catNames[items[len(items)-1].CategoryID]; ok {
			items[len(items)-1].CategoryName = strPtr(n)
		}
		if n, ok := supNames[items[len(items)-1].SupplierID]; ok {
			items[len(items)-1].SupplierName = strPtr(n)
		}
		if b := cuberead.Str(r, "product.brand_name"); b != "" {
			items[len(items)-1].BrandName = strPtr(b)
		}
	}

	// 价格:单独一趟(branch_price 是另一个 model)。
	prices, err := s.cubeBranchPrices(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if p, ok := prices[items[i].ItemNo]; ok {
			items[i].CostYuan = p.cost
			items[i].WholesaleYuan = p.wholesale
			items[i].RetailYuan = p.retail
			items[i].MemberPriceYuan = p.member
		}
	}
	return &ProductArchivePage{
		Items:      items,
		Count:      len(items),
		Page:       q.Page,
		PageSize:   q.PageSize,
		Total:      total,
		DataSource: "cube",
	}, nil
}

// archivePrices 是一个商品的四价(指针 = 没维护时 nil)。
type archivePrices struct {
	cost      *string
	wholesale *string
	retail    *string
	member    *string
}

// cubeBranchPrices 批量取一页商品的分店价格。
//
// ⚠️ **同一商品可能有多行**(不同供应商 / 销售方式)⇒ 取**第一个非空**的价,
// 而不是求和(求和会把"这个商品有 3 个供应商的价"变成 3 倍价)。
func (s *CatalogService) cubeBranchPrices(ctx context.Context, ids []string) (map[string]archivePrices, error) {
	out := map[string]archivePrices{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, _, err := s.reader.LoadCubeQueryPage(ctx, "branch_price", cubeclient.CubeQuery{
		Measures:   []string{"branch_price.count"},
		Dimensions: productArchivePriceDims(),
		Filters:    []cubeclient.CubeFilter{cuberead.InFilter("branch_price.item_no", ids)},
		Limit:      len(ids) * 4,
	})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		id := cuberead.Str(r, "branch_price.item_no")
		if id == "" {
			continue
		}
		p, ok := out[id]
		if !ok {
			p = archivePrices{
				cost:      cuberead.MoneyPtr(cuberead.DecPtr(r, "branch_price.cost_yuan")),
				wholesale: cuberead.MoneyPtr(cuberead.DecPtr(r, "branch_price.wholesale_yuan")),
				retail:    cuberead.MoneyPtr(cuberead.DecPtr(r, "branch_price.retail_yuan")),
				member:    cuberead.MoneyPtr(cuberead.DecPtr(r, "branch_price.member_price_yuan")),
			}
			out[id] = p
			continue
		}
		// 已有行:补上**缺**的那一个价(不覆盖已取到的)。
		if p.cost == nil {
			p.cost = cuberead.MoneyPtr(cuberead.DecPtr(r, "branch_price.cost_yuan"))
		}
		if p.wholesale == nil {
			p.wholesale = cuberead.MoneyPtr(cuberead.DecPtr(r, "branch_price.wholesale_yuan"))
		}
		if p.retail == nil {
			p.retail = cuberead.MoneyPtr(cuberead.DecPtr(r, "branch_price.retail_yuan"))
		}
		if p.member == nil {
			p.member = cuberead.MoneyPtr(cuberead.DecPtr(r, "branch_price.member_price_yuan"))
		}
		out[id] = p
	}
	return out, nil
}

// localProductArchive 走本域 PG 的商品档案列表。
//
// ⚠️ 本域 `products` 表**不存价**(价格归 pricing 服务,见 model/product.go)
// ⇒ 四价来自 `product_branch_prices` 表;**该表里没有的价 ⇒ null**(未维护 ≠ 0 元)。
//
// ⚠️ 本域没有品牌列 ⇒ brand_name 恒 null(见本文件包注释)。
func (s *CatalogService) localProductArchive(ctx context.Context, branchID string, q ProductArchiveQuery) (*ProductArchivePage, error) {
	if s.db == nil {
		return nil, ErrLocalArchiveUnavailable
	}
	tx := s.db.WithContext(ctx).Model(&model.Product{}).Where("branch_id = ?", branchID)
	if q.Keyword != "" {
		like := "%" + strings.ToLower(strings.TrimSpace(q.Keyword)) + "%"
		// ⚠️ **必须自己加括号**:SQL 里 AND 的优先级高于 OR
		// ⇒ 不加括号会变成
		//   branch_id = '10' AND id LIKE ? OR sub_code LIKE ? OR name LIKE ?
		// ⇒ **别的门店**的行也会被带出来(而且全程不报错)。
		tx = tx.Where(
			"(LOWER(id) LIKE ? OR LOWER(sub_code) LIKE ? OR LOWER(name) LIKE ?)",
			like, like, like)
	}
	if q.CategoryID != "" {
		ids, err := s.localCategorySubtree(ctx, branchID, q.CategoryID)
		if err != nil {
			return nil, err
		}
		// ⚠️ 空集合 ⇒ 命中 0 行(不是"不过滤")。
		tx = tx.Where("category_id IN ?", ids)
	}
	if q.SupplierID != "" {
		tx = tx.Where("supplier_id = ?", q.SupplierID)
	}
	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, err
	}
	var rows []model.Product
	if err := tx.Order("id").
		Offset((q.Page - 1) * q.PageSize).
		Limit(q.PageSize).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	catNames, err := s.localCategoryNames(ctx, branchID)
	if err != nil {
		return nil, err
	}
	supNames, err := s.localSupplierNames(ctx, branchID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	prices, err := s.localBranchPrices(ctx, branchID, ids)
	if err != nil {
		return nil, err
	}
	items := make([]ProductArchiveItem, 0, len(rows))
	for _, r := range rows {
		item := ProductArchiveItem{
			ID:         r.ID,
			ItemNo:     r.ID,
			SubCode:    r.SubCode,
			Name:       r.Name,
			IsPoints:   false,
			CategoryID: r.CategoryID,
			SupplierID: r.SupplierID,
			Spec:       r.Spec,
			Unit:       r.Unit,
			Status:     r.Status,
		}
		if n, ok := catNames[r.CategoryID]; ok {
			item.CategoryName = strPtr(n)
		}
		if n, ok := supNames[r.SupplierID]; ok {
			item.SupplierName = strPtr(n)
		}
		if p, ok := prices[r.ID]; ok {
			item.CostYuan = p.cost
			item.WholesaleYuan = p.wholesale
			item.RetailYuan = p.retail
			item.MemberPriceYuan = p.member
		}
		items = append(items, item)
	}
	return &ProductArchivePage{
		Items:      items,
		Count:      len(items),
		Page:       q.Page,
		PageSize:   q.PageSize,
		Total:      int(total),
		DataSource: "local",
	}, nil
}

// localBranchPrices 取本域 PG 的分店价格。
//
// ⚠️ 同一个商品可能有多行(不同供应商 / 销售方式)⇒ 取第一个非空的价,
// 与 cube 路径同口径(不求和)。
func (s *CatalogService) localBranchPrices(ctx context.Context, branchID string, ids []string) (map[string]archivePrices, error) {
	out := map[string]archivePrices{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []model.ProductBranchPrice
	if err := s.db.WithContext(ctx).Model(&model.ProductBranchPrice{}).
		Where("branch_id = ? AND product_id IN ?", branchID, ids).
		Order("product_id, supplier_id, sale_way").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		p, ok := out[r.ProductID]
		if !ok {
			p = archivePrices{
				cost:      cuberead.MoneyPtr(r.CostYuan),
				wholesale: cuberead.MoneyPtr(r.WholesaleYuan),
				retail:    cuberead.MoneyPtr(r.RetailYuan),
				member:    cuberead.MoneyPtr(r.MemberPriceYuan),
			}
			out[r.ProductID] = p
			continue
		}
		if p.cost == nil {
			p.cost = cuberead.MoneyPtr(r.CostYuan)
		}
		if p.wholesale == nil {
			p.wholesale = cuberead.MoneyPtr(r.WholesaleYuan)
		}
		if p.retail == nil {
			p.retail = cuberead.MoneyPtr(r.RetailYuan)
		}
		if p.member == nil {
			p.member = cuberead.MoneyPtr(r.MemberPriceYuan)
		}
		out[r.ProductID] = p
	}
	return out, nil
}

// ListSupplierPrices 取某个商品的「供货信息」(PLAN §3.1 的第二个端点)。
func (s *CatalogService) ListSupplierPrices(ctx context.Context, productID string) (*SupplierPriceList, error) {
	branchID := cubeclient.BranchFromContext(ctx)
	useCube, err := s.useCubeForRead(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if useCube {
		return s.cubeSupplierPrices(ctx, productID)
	}
	return s.localSupplierPrices(ctx, branchID, productID)
}

// cubeSupplierPrices 走 cube 的供货信息。
func (s *CatalogService) cubeSupplierPrices(ctx context.Context, productID string) (*SupplierPriceList, error) {
	if s.reader == nil {
		return nil, ErrProductArchiveReaderMissing
	}
	rows, _, err := s.reader.LoadCubeQueryPage(ctx, "supplier_item", cubeclient.CubeQuery{
		Measures:   []string{"supplier_item.count"},
		Dimensions: supplierItemDims(),
		Filters: []cubeclient.CubeFilter{
			cuberead.Filter("supplier_item.item_no", cubeclient.OperatorEquals, productID),
		},
		Limit: 200,
	})
	if err != nil {
		return nil, err
	}
	// 供应商名:另发一趟字典查询(cube 的 live↔duck 不许 join,
	// 见 internal/salesagg/cubeq 的说明)。
	supNames := map[string]string{}
	if sups, err := s.cube.ListSuppliers(ctx); err == nil {
		for _, sp := range sups.Rows {
			supNames[sp.ID] = sp.Name
		}
	} else {
		s.logger.Warn("catalog: 取供应商字典失败,供货信息里的供应商名留空", "err", err)
	}
	items := make([]SupplierPriceItem, 0, len(rows))
	for _, r := range rows {
		it := SupplierPriceItem{
			ItemNo:      cuberead.Str(r, "supplier_item.item_no"),
			BranchID:    cuberead.Str(r, "supplier_item.branch_no"),
			SupplierID:  cuberead.Str(r, "supplier_item.supplier_id"),
			SaleWay:     cuberead.Str(r, "supplier_item.sale_way"),
			AppointedPriceYuan: cuberead.MoneyPtr(
				cuberead.DecPtr(r, "supplier_item.appointed_price_yuan")),
			TopPriceYuan: cuberead.MoneyPtr(
				cuberead.DecPtr(r, "supplier_item.top_price_yuan")),
			BottomPriceYuan: cuberead.MoneyPtr(
				cuberead.DecPtr(r, "supplier_item.bottom_price_yuan")),
			LastPriceYuan: cuberead.MoneyPtr(
				cuberead.DecPtr(r, "supplier_item.last_price_yuan")),
		}
		if n, ok := supNames[it.SupplierID]; ok {
			it.SupplierName = strPtr(n)
		}
		if d := cuberead.Date(cuberead.Str(r, "supplier_item.contract_date")); d != "" {
			it.ContractDate = strPtr(d)
		}
		items = append(items, it)
	}
	return &SupplierPriceList{Items: items, Count: len(items), DataSource: "cube"}, nil
}

// localSupplierPrices 走本域 PG 的供货信息。
//
// ⚠️ 本地路径查 `product_supplier_prices` 表。该表**本轮没有写端点**
// ⇒ 现在查出来是空 —— 这是**诚实的空**(表在、列在、查询在),
// 而不是"查不到"的假象。
func (s *CatalogService) localSupplierPrices(ctx context.Context, branchID, productID string) (*SupplierPriceList, error) {
	if s.db == nil {
		return nil, ErrLocalArchiveUnavailable
	}
	var rows []model.ProductSupplierPrice
	if err := s.db.WithContext(ctx).Model(&model.ProductSupplierPrice{}).
		Where("branch_id = ? AND product_id = ?", branchID, productID).
		Order("supplier_id, sale_way").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	supNames, err := s.localSupplierNames(ctx, branchID)
	if err != nil {
		return nil, err
	}
	items := make([]SupplierPriceItem, 0, len(rows))
	for _, r := range rows {
		it := SupplierPriceItem{
			ItemNo:     r.ProductID,
			BranchID:   r.BranchID,
			SupplierID: r.SupplierID,
			SaleWay:    r.SaleWay,
			AppointedPriceYuan: cuberead.MoneyPtr(r.AppointedPriceYuan),
			TopPriceYuan:       cuberead.MoneyPtr(r.TopPriceYuan),
			BottomPriceYuan:    cuberead.MoneyPtr(r.BottomPriceYuan),
			LastPriceYuan:      cuberead.MoneyPtr(r.LastPriceYuan),
		}
		if n, ok := supNames[r.SupplierID]; ok {
			it.SupplierName = strPtr(n)
		}
		if r.ContractDate != nil {
			it.ContractDate = strPtr(r.ContractDate.Format("2006-01-02"))
		}
		items = append(items, it)
	}
	return &SupplierPriceList{Items: items, Count: len(items), DataSource: "local"}, nil
}
