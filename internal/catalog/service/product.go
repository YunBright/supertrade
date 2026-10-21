// Package service / product.go —— 商品业务逻辑(本地表 CRUD)。
//
// # 从占位到实装(2026-10-20)
//
// 2026-09 重构时本文件只有一个 `ErrNotImplemented` 占位:表建了、CRUD 没做,
// 全部查询走 cube 代理。
//
// 现在实装,原因是**自营门店(门店 10)没有 cube 源**
// (`branch_cube_sources` 实测只有 00 / 01)。
//
// # 业务规则
//
//   - 同一 (id, branch_id) 唯一;跨门店可重名/重 ID(各门店独立维护)
//   - 软删:Delete 调用 UPDATE deleted_at = NOW();GORM 自动过滤软删行
//   - 字段校验:id / branch_id / name 必填;status 默认 'active'
//
// # 价格不在这张表
//
// 售价/促销归 pricing 服务(有生效期与历史版本)。见 model/product.go 的说明。
//
// 错误:
//
//   - model.ErrProductNotFound      → 404
//   - model.ErrProductAlreadyExists → 409
//   - model.ErrInvalidInput        → 400
//   - 其它 DB error                → 500
package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/catalog/model"
)

// ProductService 是 catalog 的商品服务。
type ProductService struct {
	db  *gorm.DB
	now func() time.Time
}

// NewProductService 构造 ProductService。
func NewProductService(db *gorm.DB) *ProductService {
	return &ProductService{db: db, now: func() time.Time { return time.Now() }}
}

// 分页口径(与商品选择控件契约一致,见 service/catalog.go 的常量)。
const (
	// DefaultProductPageSize 是商品列表的默认每页条数。
	DefaultProductPageSize = 50
	// MaxProductPageSize 是每页条数上限。
	//
	// ⚠️ 上限不是随便定的:本地上限不保护 cube,但保持与 picker 一致,
	// 免得同一个 client 两套分页语义(前端复用同一套分页组件)。
	MaxProductPageSize = 200
)

// LocalProductQuery 是商品列表的查询条件。
type LocalProductQuery struct {
	// Keyword 命中商品 id / 名称 / 条码(任一命中即返回)。
	// 空 = 不过滤。
	Keyword string
	// CategoryID / SupplierID 是精确过滤。空 = 不过滤。
	CategoryID string
	SupplierID string
	// Status 精确过滤。空 = 不过滤。
	//
	// ⚠️ 思迅的 status 是 "0"~"6" 的**原始状态码**,两个门店语义不同
	// (01 店的 status=2 是正常在售商品)⇒ 这里**不翻译**成 active/inactive,
	// 也不做"只返 active"的隐式过滤 —— 判据交给调用方。
	Status string
	// IncludeSpecial 是否带回**换购品 / 停用品**。
	//
	// 默认 false(不返回)。理由见 List 里的注释:换购品的售价多为 .0000
	// 或象征价,混在普通商品里会让收银员扫到"看不见价格"的怪商品。
	// 商品选择器 / 盘点等场景需要它们 ⇒ 显式打开。
	IncludeSpecial bool
	// Page 从 1 开始;PageSize 是每页条数。
	Page     int
	PageSize int
}

// normalized 把越界的分页参数拉回合法区间。
//
// ⚠️ 这**不是**校验,是兜底:HTTP 层的越界值必须 400(契约要求),
// 服务层被直接调用时也不该算出负 offset。
func (q LocalProductQuery) normalized() LocalProductQuery {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = DefaultProductPageSize
	}
	if q.PageSize > MaxProductPageSize {
		q.PageSize = MaxProductPageSize
	}
	return q
}

// CreateProductInput 是 Create 的入参。
type CreateProductInput struct {
	ID         string
	BranchID   string
	Name       string
	CategoryID string
	SupplierID string
	// SubCode 是自编码/助记码(**不唯一**,见 model.Product.SubCode)。
	SubCode string
	// IsExchange / IsVoid 是从思迅品名前缀解析出来的标记。
	IsExchange bool
	IsVoid     bool
	Barcode    string
	Unit       string
	Spec       string
	// Status 可选,默认 "active"。
	Status string
	// CreatedBy 是"谁建的":批量导入填 "import",管理员填 "admin",
	// 收银员建档填其工号。
	//
	// ⚠️ **不记录"数据来自哪个源门店"** —— 基础档案可以从任何地方进来
	// (cube 导出 / Excel 导入 / 手工建档);归属门店由 BranchID 决定。
	CreatedBy string
}

// Create 新建一个商品(同 (id, branch_id) 已存在 → 409)。
func (s *ProductService) Create(ctx context.Context, in CreateProductInput) (*model.Product, error) {
	if err := validateProductInput(in.ID, in.BranchID, in.Name); err != nil {
		return nil, err
	}
	if in.Status == "" {
		in.Status = "active"
	}
	now := s.now()
	row := &model.Product{
		ID:         in.ID,
		BranchID:   in.BranchID,
		Name:       in.Name,
		CategoryID: in.CategoryID,
		SupplierID: in.SupplierID,
		SubCode:    in.SubCode,
		IsExchange: in.IsExchange,
		IsVoid:     in.IsVoid,
		Barcode:    in.Barcode,
		Unit:       in.Unit,
		Spec:       in.Spec,
		Status:     in.Status,
		CreatedBy:  in.CreatedBy,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		if isUniqueViolation(err) {
			return nil, model.ErrProductAlreadyExists
		}
		return nil, err
	}
	return row, nil
}

// Get 按 (id, branch_id) 查一个;不存在 → ErrProductNotFound。
func (s *ProductService) Get(ctx context.Context, branchID, id string) (*model.Product, error) {
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
	return &row, nil
}

// List 分页查某门店下的商品。
//
// 返回 (rows, total):total 是**过滤后的总条数**,不是本页条数。
func (s *ProductService) List(ctx context.Context, branchID string, q LocalProductQuery) ([]model.Product, int64, error) {
	if branchID == "" {
		return nil, 0, model.ErrInvalidInput
	}
	q = q.normalized()

	tx := s.db.WithContext(ctx).Model(&model.Product{}).Where("branch_id = ?", branchID)
	if q.Keyword != "" {
		like := "%" + q.Keyword + "%"
		// 命中 货号 / 自编码 / 名称 / 条码 任一即返回。
		//
		// ⚠️ 这四个是**不同的东西**(思迅实测,见 model.Product):
		//   货号   = item_no      —— 唯一
		//   自编码 = item_subno   —— **不唯一**(9 组"正常品+换购品"共用)
		//   条码   = item_barcode —— 1 商品 : 1~7 条
		// 思迅的搜索框是「输入编码、名称或助记码(Q)」⇒ 四个都要命中。
		tx = tx.Where(
			"id LIKE ? OR sub_code LIKE ? OR name LIKE ? OR barcode LIKE ?",
			like, like, like, like)
	}
	if q.CategoryID != "" {
		tx = tx.Where("category_id = ?", q.CategoryID)
	}
	if q.SupplierID != "" {
		tx = tx.Where("supplier_id = ?", q.SupplierID)
	}
	if q.Status != "" {
		tx = tx.Where("status = ?", q.Status)
	}
	// 默认**不返回换购品 / 停用品**。
	//
	// 思迅把它们当普通在售商品(实测 19 个换购品的 status 也是 1),
	// 而它们的售价多为 .0000 或象征价(1 元/15 元)⇒ 混在普通商品里会让
	// 收银员扫到"看不见价格"的怪商品。
	// 显式传 include_special=true 才带上(商品选择器/盘点等场景需要)。
	if !q.IncludeSpecial {
		tx = tx.Where("is_exchange = ? AND is_void = ?", false, false)
	}

	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var rows []model.Product
	// ⚠️ **排序必须有稳定 tiebreaker**(name, id)——
	//
	//	只按 name 排序时,同名商品之间的顺序由 DB 决定 ⇒ 不稳定。
	//	客户端是**分页拉全量**(27565 条 = 138 页)⇒ 拉的过程中目录一有变动,
	//	就可能**重复一行或跳过一行**,而且**零报错**。
	//
	//	⇒ 加 id 做 tiebreaker ⇒ 同一份数据任何时候翻页结果都一致。
	if err := tx.Order("name").Order("id").
		Offset((q.Page - 1) * q.PageSize).
		Limit(q.PageSize).
		Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// ExportAll 导出该门店的**全部**商品(离线商品库用)。
//
// # ⚠️ 与 List 的三处差别(都是"离线数据包"逼出来的)
//
//  1. **不分页**:门店 10 有 27470 个商品,`page_size` 上限 200
//     ⇒ 分页拉全量是 **138 次 HTTP 往返**,每趟还要过一次 RBAC。
//     ⇒ 一次给全,是"离线商品库"这件事的前提。
//  2. **含换购品 / 停用品**:List 默认过滤掉它们(见 List 里的注释),
//     而离线商品库缺这两类是**已知缺口** —— 收银员扫到换购品时
//     POS 查不到商品,只能干瞪眼。
//  3. 排序仍是 (name, id) 全序 ⇒ 同一份数据任何时候导出结果一致
//     (客户端可以拿导出的结果做本地全量索引,不需要再排序)。
//
// # ⚠️ 返回的行**不含条码全集**
//
// 条码在 `product_barcodes` 子表(1 商品 : 1~7 条),
// 归组由 ProductBarcodeService.ListByBranch 做 —— 两张表分两个 service,
// 各自守自己的表。
func (s *ProductService) ExportAll(ctx context.Context, branchID string) ([]model.Product, error) {
	if branchID == "" {
		return nil, model.ErrInvalidInput
	}
	var rows []model.Product
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("name").Order("id").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// FindCandidatesByCode 按一个编码找出**候选商品列表**。
//
// # 为什么返回列表而不是单个商品
//
// 一个编码可能命中多个商品(思迅实测):
//
//   - **自编码**(`item_subno`)**不唯一**:9 组自编码对应 2 个货号,
//     全部是「正常品 + 换购品」配对。
//     例:`6904406383122` → `02000110` 远航九江双蒸酒精品500ml(16.50 元)
//     / `02000111` (换购)远航九江双蒸酒精品500ml(5.00 元)。
//   - **条码**(`t_bd_item_barcode.item_barcode`):1 个条码只对应 1 个货号,
//     但**一个货号有 1~7 个条码** ⇒ 反查条码时可能命中多个货号。
//
// ⇒ 扫码收银**不能**假定"一个码对一个商品"。收银员必须能选,
// 或者上层按"换购品只在换购流程出现"的规则自行区分。
//
// 查找顺序(全 OR,去重后按货号排序):
//
//	货号(id) → 自编码(sub_code) → 条码(barcode)
//
// 前两者在 `products` 表上直接查;条码在 `product_barcodes` 表上查(1:N),
// 再回表取商品。
func (s *ProductService) FindCandidatesByCode(ctx context.Context, branchID, code string) ([]model.Product, error) {
	if branchID == "" || strings.TrimSpace(code) == "" {
		return nil, model.ErrInvalidInput
	}
	code = strings.TrimSpace(code)

	seen := map[string]bool{}
	out := []model.Product{}

	// ① 货号 / 自编码(products 表)。
	var direct []model.Product
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND (id = ? OR sub_code = ? OR barcode = ?)", branchID, code, code, code).
		Order("id").
		Find(&direct).Error; err != nil {
		return nil, err
	}
	for _, p := range direct {
		if !seen[p.ID] {
			seen[p.ID] = true
			out = append(out, p)
		}
	}

	// ② 条码表(1 商品 : N 条码 ⇒ 反查可能命中多个货号)。
	var barcodes []model.ProductBarcode
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND barcode = ?", branchID, code).
		Find(&barcodes).Error; err != nil {
		return nil, err
	}
	for _, b := range barcodes {
		if seen[b.ProductID] {
			continue
		}
		p, err := s.Get(ctx, branchID, b.ProductID)
		if err != nil {
			if errors.Is(err, model.ErrProductNotFound) {
				continue
			}
			return nil, err
		}
		seen[p.ID] = true
		out = append(out, *p)
	}

	return out, nil
}

// UpdateProductInput 是 Update 的入参(只更新非空字段)。
type UpdateProductInput struct {
	Name       *string
	CategoryID *string
	SupplierID *string
	// SubCode 是自编码/助记码(不唯一)。
	SubCode *string
	// IsExchange / IsVoid 是从思迅品名前缀解析出来的标记。
	IsExchange *bool
	IsVoid     *bool
	Barcode    *string
	Unit       *string
	Spec       *string
	Status     *string
	UpdatedBy  string
}

// Update 改一个商品;按 (id, branch_id) 定位。
func (s *ProductService) Update(ctx context.Context, branchID, id string, in UpdateProductInput) (*model.Product, error) {
	updates := map[string]any{"updated_at": s.now()}
	if in.Name != nil {
		updates["name"] = *in.Name
	}
	if in.CategoryID != nil {
		updates["category_id"] = *in.CategoryID
	}
	if in.SupplierID != nil {
		updates["supplier_id"] = *in.SupplierID
	}
	if in.SubCode != nil {
		updates["sub_code"] = *in.SubCode
	}
	if in.IsExchange != nil {
		updates["is_exchange"] = *in.IsExchange
	}
	if in.IsVoid != nil {
		updates["is_void"] = *in.IsVoid
	}
	if in.Barcode != nil {
		updates["barcode"] = *in.Barcode
	}
	if in.Unit != nil {
		updates["unit"] = *in.Unit
	}
	if in.Spec != nil {
		updates["spec"] = *in.Spec
	}
	if in.Status != nil {
		updates["status"] = *in.Status
	}
	if in.UpdatedBy != "" {
		updates["updated_by"] = in.UpdatedBy
	}

	res := s.db.WithContext(ctx).
		Model(&model.Product{}).
		Where("id = ? AND branch_id = ?", id, branchID).
		Updates(updates)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, model.ErrProductNotFound
	}
	return s.Get(ctx, branchID, id)
}

// Delete 软删一个商品。
func (s *ProductService) Delete(ctx context.Context, branchID, id string) error {
	res := s.db.WithContext(ctx).
		Model(&model.Product{}).
		Where("id = ? AND branch_id = ?", id, branchID).
		Update("deleted_at", s.now())
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return model.ErrProductNotFound
	}
	return nil
}

// ---- helpers ----

// validateProductInput 校验必填字段。
//
// 独立成函数是为了让"哪个字段缺失"只有一个地方说了算。
func validateProductInput(id, branchID, name string) error {
	if strings.TrimSpace(id) == "" ||
		strings.TrimSpace(branchID) == "" ||
		strings.TrimSpace(name) == "" {
		return model.ErrInvalidInput
	}
	return nil
}
