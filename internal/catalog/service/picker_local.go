// Package service / picker_local.go —— 商品选择控件的**本域 PG** 实现。
//
// # 为什么要有这一份
//
// 2026-10-21 生产实测:picker 的 4 条读端点只走 cube ⇒ 自营店(门店 10,
// `branch_cube_sources` 里没有它)调它直接返 `cube 404 not found`,
// 而门店 10 的本地表里明明有 27565 个商品(import 灌的)。
//
// ⇒ 没配数据源的门店必须能读本域 PG。这一份就是那个"本地侧"。
//
// # 响应形状必须与 cube 路径**逐字一致**
//
// 前端 `lib/domain/product/product_models.dart` 依赖这套字段
// (id / name / barcode / sub_code / category_id / category_name /
// supplier_id / supplier_name / unit / price_yuan / status …)。
// ⇒ 两条路径**共用同一批响应结构体**(ProductItem / ProductPage /
// CategoryItem / SupplierItem),只有"数据从哪来"不同。
// **改形状 = 破坏前端**,所以形状不许变。
//
// # 本地侧与 cube 侧的口径对齐(逐条)
//
//	过滤      category_id → **子树**(与 cube 的 categorySubtree 同口径);
//	          supplier_id → 精确;关键字 → id / sub_code / name / 条形码(大小写无关)。
//	          ⚠️ 条形码比 cube 多一种(cube 的 product model 没有 barcode 维度)——
//	            见 localProductPage 的注释。
//	分页      page / page_size ⇒ OFFSET / LIMIT。
//	计数      total = 过滤后的总行数(不是本页条数)。
//	排序      **id 升序**(cube 路径也是 product.id asc)⇒ 两条路径翻页顺序一致。
//
// # price_yuan 为什么恒为 null
//
// 本域 `products` 表**不存价**(价格归 pricing 服务,见 model/product.go)。
// ⇒ 本地路径的 price_yuan 恒为 null(未维护),**不**编造 0 元
// (前端把未维护价显示成 ¥0.00 会被仓管当真价贴到货架上)。
package service

import (
	"context"
	"errors"
	"strings"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/cubeclient"
)

// ErrLocalCatalogUnavailable 本地库未装配(装配漏了 ⇒ 500,不是"没有商品")。
//
// ⚠️ 刻意**不**复用 ErrCubeUnavailable:那个错的含义是"cube 链路不可用",
// 而这里是"catalog 没装本地库"—— 两件事的排查方向完全不同。
var ErrLocalCatalogUnavailable = errors.New("catalog: 本地商品库未装配")

// localProductPage 走本域 PG 的商品分页(没配数据源的门店)。
//
// ⚠️ 与 cube 路径的差别只有一处:本地一次查询就把 id / sub_code / name
// 三个命中合并了(用 OR),所以 total 是**精确值**;
// cube 侧没有 OR 算子,只能查三次再合并 ⇒ total 是三者之和(上界)。
// 前端只用 total 判断"还有没有下一页" ⇒ 精确值是它的上界,可接受。
func (s *CatalogService) localProductPage(ctx context.Context, branchID string, q ProductListQuery) (*ProductPage, error) {
	if s.db == nil {
		return nil, ErrLocalCatalogUnavailable
	}
	if branchID == "" {
		return nil, model.ErrInvalidInput
	}
	q = q.normalized()

	tx := s.db.WithContext(ctx).Model(&model.Product{}).Where("branch_id = ?", branchID)

	// 关键字:命中**货号 / 自编码 / 名称 / 条形码**四种(思迅的商品标识体系)。
	//
	// ⚠️ 用 LOWER(...) LIKE 而不是 ILIKE:PG 的 LIKE 大小写敏感,
	// 而 SQLite 没有 ILIKE ⇒ LOWER() 两边都能跑,行为一致。
	//
	// # ⚠️ **条形码在另一张表上**(`product_barcodes`,一品多码)
	//
	//	`products.barcode` 是"主条码"**冗余列**,生产实测**大面积为空**
	//	(门店 10:27568 行全空)—— 领域事实是"思迅 item_no 就是条码"
	//	(见 localProductItems:`Barcode = r.ID`)。
	//	真正的商品条码在 `t_bd_item_barcode` ⇒ 同步后落在 `product_barcodes`。
	//
	//	⇒ 只在 `products` 表上做 LIKE 是**扫不到条形码**的:
	//	  2026-10-22 生产实测,商品 `02000089` 的条形码 `6970367140630`
	//	  在 `products/search` 里 count=0 ⇒ **POS 上扫这个码搜不出来**。
	//
	//	⇒ 必须用子查询反查 `product_barcodes`。
	//
	// # ⚠️ 四种标识的**唯一性**不同 ⇒ 命中数可能 > 1
	//
	//	自编码(思迅不唯一:9 组"正常品+换购品"共用)、条形码(一品多码:1 商品 1~7 条)
	//	⇒ 端点必须返回**候选列表**(它已经是 candidates[] 了)⇒ 收银员要能选。
	//
	// # ⚠️ cube 路径(配了数据源的门店)目前**只有三种**
	//
	//	cube 的 product model 没有 barcode 维度 ⇒ 门店 00 走 cube 时扫不出条形码。
	//	⇒ POS 实际跑在门店 10(本地路径)⇒ 不影响收银;
	//	  要与 cube 对齐得改 cube 仓的 product model(加 barcode 维度 + join)。
	if q.Keyword != "" {
		like := "%" + strings.ToLower(q.Keyword) + "%"
		tx = tx.Where(
			"(LOWER(id) LIKE ? OR LOWER(sub_code) LIKE ? OR LOWER(name) LIKE ? OR "+
				"id IN (SELECT product_id FROM product_barcodes "+
				"WHERE branch_id = ? AND deleted_at IS NULL AND LOWER(barcode) LIKE ?))",
			like, like, like, branchID, like)
	}
	if q.CategoryID != "" {
		ids, err := s.localCategorySubtree(ctx, branchID, q.CategoryID)
		if err != nil {
			return nil, err
		}
		// ⚠️ 空集合 ⇒ 命中 0 行(不是"不过滤"):
		// 当成不过滤会返回**全店商品**,而且全程不报错。
		tx = tx.Where("category_id IN ?", ids)
	}
	if q.SupplierID != "" {
		tx = tx.Where("supplier_id = ?", q.SupplierID)
	}
	if q.SupplierCategoryID != "" {
		ids, err := s.localSupplierIDsByCategory(ctx, branchID, q.SupplierCategoryID)
		if err != nil {
			return nil, err
		}
		tx = tx.Where("supplier_id IN ?", ids)
	}

	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, err
	}
	var rows []model.Product
	// ⚠️ 排序 = id 升序(cube 路径也是 product.id asc)⇒ 两条路径翻页顺序一致,
	// 且 id 是主键 ⇒ 全序 ⇒ 翻页不会重复 / 漏行。
	if err := tx.Order("id").
		Offset((q.Page - 1) * q.PageSize).
		Limit(q.PageSize).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	items, err := s.localProductItems(ctx, branchID, rows)
	if err != nil {
		return nil, err
	}
	return &ProductPage{
		Products: items,
		Count:    len(items),
		Page:     q.Page,
		PageSize: q.PageSize,
		Total:    int(total),
	}, nil
}

// localProductItems 把本域商品行组装成 picker 的响应项。
//
// ⚠️ barcode **强制等于 id**(与 cube 路径同款,见 service/catalog.go 的 joinProduct):
// 领域事实是"思迅 item_no 就是条码",前端拿它当扫码键。
// ⇒ 两条路径的 barcode 口径必须一样,否则同一个商品在列表里扫不出来。
func (s *CatalogService) localProductItems(ctx context.Context, branchID string, rows []model.Product) ([]ProductItem, error) {
	catNames, err := s.localCategoryNames(ctx, branchID)
	if err != nil {
		return nil, err
	}
	supNames, err := s.localSupplierNames(ctx, branchID)
	if err != nil {
		return nil, err
	}
	out := make([]ProductItem, 0, len(rows))
	for _, r := range rows {
		item := ProductItem{
			ID:         r.ID,
			Name:       r.Name,
			Barcode:    r.ID,
			SubCode:    r.SubCode,
			CategoryID: r.CategoryID,
			SupplierID: r.SupplierID,
			Unit:       strPtr(r.Unit),
			// ⚠️ 本域不存价 ⇒ 恒为 null(未维护 ≠ 0 元)。
			PriceYuan: nil,
			Status:    r.Status,
		}
		if n, ok := catNames[r.CategoryID]; ok {
			item.CategoryName = &n
		}
		if n, ok := supNames[r.SupplierID]; ok {
			item.SupplierName = &n
		}
		out = append(out, item)
	}
	return out, nil
}

// localCategoryNames 取本域分类字典(id → name)。
func (s *CatalogService) localCategoryNames(ctx context.Context, branchID string) (map[string]string, error) {
	var rows []model.Category
	if err := s.db.WithContext(ctx).Model(&model.Category{}).
		Where("branch_id = ?", branchID).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Name
	}
	return out, nil
}

// localSupplierNames 取本域供应商字典(id → name)。
func (s *CatalogService) localSupplierNames(ctx context.Context, branchID string) (map[string]string, error) {
	var rows []model.Supplier
	if err := s.db.WithContext(ctx).Model(&model.Supplier{}).
		Where("branch_id = ? AND deleted_at IS NULL", branchID).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Name
	}
	return out, nil
}

// localCategorySubtree 返回某分类**及其所有子孙**的 id 集合。
//
// ⚠️ 复用 CategoryService.SubtreeIDs(它与 cube 侧的 categorySubtree 同口径,
// 且带成环保护)⇒ 两条路径的"按分类看商品"语义完全一致。
func (s *CatalogService) localCategorySubtree(ctx context.Context, branchID, rootID string) ([]string, error) {
	ids, err := NewCategoryService(s.db).SubtreeIDs(ctx, branchID, rootID)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		// 字典里没有这个分类 ⇒ 退化成精确匹配(cube 侧同款)。
		return []string{rootID}, nil
	}
	return ids, nil
}

// localSupplierIDsByCategory 把"供应商类别"展开成供应商 id 集合。
//
// ⚠️ 本域 `suppliers` 表**没有销售方式列**(供应商类别是思迅侧的概念,
// 只在 cube 那边有)⇒ 本地供应商一律算"未分类"。
//
//	__unclassified__ ⇒ 本域全部供应商(与 cube 的降级路径同款)
//	具体类别 id      ⇒ 空集合 ⇒ 命中 0 行
//
// ⚠️ 空集合当成"不过滤"会返回**全店商品**,而且全程不报错
// (实测门店 00 的"扣率代销"组就是空的)。
func (s *CatalogService) localSupplierIDsByCategory(ctx context.Context, branchID, categoryID string) ([]string, error) {
	if categoryID != unclassifiedCategoryID {
		return []string{}, nil
	}
	var rows []model.Supplier
	if err := s.db.WithContext(ctx).Model(&model.Supplier{}).
		Where("branch_id = ? AND deleted_at IS NULL", branchID).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids, nil
}

// localCategoryList 本域分类**树**(与 cube 路径同一套建树逻辑)。
//
// ⚠️ 复用 buildCategoryTree:父节点缺失 ⇒ 当顶级;父链成环 ⇒ 切断。
// 两边用同一份实现 ⇒ 两条路径的树结构与边界处理**逐字一致**。
func (s *CatalogService) localCategoryList(ctx context.Context, branchID string, withCounts bool) (*CategoryList, error) {
	if s.db == nil {
		return nil, ErrLocalCatalogUnavailable
	}
	var rows []model.Category
	if err := s.db.WithContext(ctx).Model(&model.Category{}).
		Where("branch_id = ?", branchID).
		Order("id").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	// 本域分类行 → 字典行(只为复用同一套建树逻辑,不改任何字段)。
	dict := make([]cubeclient.CategoryDTO, 0, len(rows))
	for _, r := range rows {
		dict = append(dict, cubeclient.CategoryDTO{ID: r.ID, Name: r.Name, ParentID: r.ParentID})
	}
	var counts map[string]int
	var err error
	if withCounts {
		if counts, err = s.localCountProductsByCategory(ctx, branchID); err != nil {
			return nil, err
		}
	}
	return &CategoryList{
		Categories: buildCategoryTree(dict, counts),
		Total:      len(dict),
	}, nil
}

// localCountProductsByCategory 返回「每个分类下的商品数」(本级)。
//
// ⚠️ 口径与 cube 的 CountProductsByCategory 一致:
// 只统计**本级直接挂**的商品(子树的合计由 buildCategoryTree 自底向上算),
// 没有分类的商品(空 category_id)不进 map。
func (s *CatalogService) localCountProductsByCategory(ctx context.Context, branchID string) (map[string]int, error) {
	var rows []struct {
		CategoryID string `gorm:"column:category_id"`
		N          int64  `gorm:"column:n"`
	}
	if err := s.db.WithContext(ctx).Model(&model.Product{}).
		Where("branch_id = ?", branchID).
		Select("category_id, COUNT(*) AS n").
		Group("category_id").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		if r.CategoryID == "" {
			continue
		}
		out[r.CategoryID] = int(r.N)
	}
	return out, nil
}

// localCountProductsBySupplier 返回「每个供应商下的商品数」(本域)。
func (s *CatalogService) localCountProductsBySupplier(ctx context.Context, branchID string) (map[string]int, error) {
	var rows []struct {
		SupplierID string `gorm:"column:supplier_id"`
		N          int64  `gorm:"column:n"`
	}
	if err := s.db.WithContext(ctx).Model(&model.Product{}).
		Where("branch_id = ?", branchID).
		Select("supplier_id, COUNT(*) AS n").
		Group("supplier_id").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]int, len(rows))
	for _, r := range rows {
		if r.SupplierID == "" {
			continue
		}
		out[r.SupplierID] = int(r.N)
	}
	return out, nil
}

// localSupplierList 本域供应商摘要(两级结构)。
//
// ⚠️ 本域没有销售方式 ⇒ 全部供应商进「未分类」组
// (cube 侧的降级路径同款:字典查不到时所有供应商都进"未分类")。
func (s *CatalogService) localSupplierList(ctx context.Context, branchID string) (*SupplierList, error) {
	if s.db == nil {
		return nil, ErrLocalCatalogUnavailable
	}
	var rows []model.Supplier
	if err := s.db.WithContext(ctx).Model(&model.Supplier{}).
		Where("branch_id = ? AND deleted_at IS NULL", branchID).
		Order("id").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	counts, err := s.localCountProductsBySupplier(ctx, branchID)
	if err != nil {
		return nil, err
	}
	flat := make([]SupplierItem, 0, len(rows))
	for _, r := range rows {
		n := counts[r.ID]
		flat = append(flat, SupplierItem{ID: r.ID, Name: r.Name, Count: &n})
	}
	return &SupplierList{
		Suppliers: flat,
		// ⚠️ ways 传 nil ⇒ 所有供应商进「未分类」组(buildSupplierCategories 的降级路径)。
		SupplierCategories: buildSupplierCategories(flat, nil),
		Total:              len(flat),
	}, nil
}

// localGetProduct 取本域单个商品,组装成与 cube 路径**同形**的 DTO。
//
// ⚠️ 返回 cubeclient.ProductDTO 是为了**响应形状逐字一致**:
// getProduct 端点的响应体一直是这个 DTO,前端按它解析。
func (s *CatalogService) localGetProduct(ctx context.Context, branchID, id string) (*cubeclient.ProductDTO, error) {
	if s.db == nil {
		return nil, ErrLocalCatalogUnavailable
	}
	var row model.Product
	err := s.db.WithContext(ctx).
		Where("id = ? AND branch_id = ?", id, branchID).
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return nil, model.ErrProductNotFound
	}
	if err != nil {
		return nil, err
	}
	return &cubeclient.ProductDTO{
		ID:         row.ID,
		Name:       row.Name,
		CategoryID: row.CategoryID,
		SupplierID: row.SupplierID,
		Unit:       row.Unit,
		Spec:       row.Spec,
		Status:     row.Status,
		// ⚠️ barcode = id(与 cube 路径的 GetProduct 同款:领域事实 item_no == barcode)。
		Barcode: row.ID,
		SubCode: row.SubCode,
		// ⚠️ 本域不存价 ⇒ nil(未维护 ≠ 0 元)。
		PriceYuan: nil,
	}, nil
}

// localSearchProductsByBarcode 本域按条码查商品(老路径 ?barcode=)。
//
// ⚠️ 与 cube 路径的差别:cube 侧会顺带查一次库存(stock_qty / avg_cost_yuan),
// 而**库存归 inventory 服务**,本域没有库存表
// ⇒ 本地路径不返库存字段(它们是 omitempty ⇒ 直接不出现)。
func (s *CatalogService) localSearchProductsByBarcode(ctx context.Context, barcode, branchID string, limit int) (*LegacyProductPage, error) {
	if s.db == nil {
		return nil, ErrLocalCatalogUnavailable
	}
	rows, err := NewProductService(s.db).FindCandidatesByCode(ctx, branchID, barcode)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	items, err := s.localProductItems(ctx, branchID, rows)
	if err != nil {
		return nil, err
	}
	products := make([]LegacyProductItem, 0, len(items))
	for _, it := range items {
		products = append(products, LegacyProductItem{
			ProductItem: it,
			ProductID:   it.ID,
			ProductName: it.Name,
			Category:    it.CategoryID,
		})
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
