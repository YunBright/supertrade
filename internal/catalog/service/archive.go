// Package service / archive.go —— 档案导入(批量 upsert)。
//
// # 它是"导入方"
//
// catalog 拥有 products / categories / product_barcodes / suppliers 四张表,
// 所以"把一批档案写进某个门店"天然归它。
//
// 路由:
//
//	POST /archive/import
//	  X-Branch-ID: <目标门店>
//	  body: { source_branch_id, mode, scopes[], data{...} }
//
// # 两种 mode
//
//	incremental —— 已存在**跳过**,只补新的(默认,保守)
//	overwrite   —— 已存在则**覆盖可变列**,不存在的插入
//
// ⚠️ **overwrite 不删目标门店已有的行**。
// "全量替换"(先清空目标再灌)危险得多:中途失败目标就空了。
// 全量替换留给带 plan_token 的预检/执行两段式(见 docs 设计),
// 那里能先告诉用户"将删除 N 条"。
//
// # 幂等
//
// 重复导入同一批数据 ⇒ 结果相同。
// products/categories/suppliers 按 (id, branch_id) upsert;
// item_barcodes 按 **每个商品的条码集合整体替换**
// (源库删掉的条码要跟着消失 —— 只加不减会留下**幽灵条码**,扫出来是错商品)。
//
// # 品名前缀 ⇒ 标记字段
//
// 思迅**没有** `is_exchange` / `is_void` 这两列,它把语义塞在品名里:
// 实测 19 个品名含「换购」、17 个以「（停用）」开头。
// ⇒ 导入时解析成字段并**去掉前缀**,否则 POS 界面上会出现
// 「(换购)远航九江双蒸酒精品500ml」这种脏品名,而且**没法按是否换购过滤**。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/catalog/model"
)

// ArchiveService 是档案导入服务。
type ArchiveService struct {
	db *gorm.DB
	// barcodeSvc 复用商品条码服务(条码是"某商品的集合",要用它的替换语义)。
	barcodeSvc *ProductBarcodeService
	now        func() time.Time
}

// NewArchiveService 构造 ArchiveService。
func NewArchiveService(db *gorm.DB) *ArchiveService {
	return &ArchiveService{
		db:         db,
		barcodeSvc: NewProductBarcodeService(db),
		now:        func() time.Time { return time.Now() },
	}
}

// 导入模式。
const (
	// ModeIncremental 已存在则跳过。
	ModeIncremental = "incremental"
	// ModeOverwrite 已存在则覆盖可变列。
	ModeOverwrite = "overwrite"
)

// 导入 scope(与 cube-router 的导出 scope 一一对应)。
//
// ⚠️ 字面量**不在本包定义** —— 定义在 internal/archivedto,
// 因为导出方(cube-router)与编排方(cubesync)要用的是**同一批名字**。
// 两边各写一份时,改了一边不会编译报错,症状是"导出了 27565 条、导入了 0 条"。
const (
	ScopeCategories   = archivedto.ScopeCategories
	ScopeProducts     = archivedto.ScopeProducts
	ScopeSuppliers    = archivedto.ScopeSuppliers
	ScopeItemBarcodes = archivedto.ScopeItemBarcodes
)

// ---- 请求体 / 响应体的行结构 ----
//
// ⚠️ 这些是**类型别名**而不是新定义:别名让本包的老调用点
// (service.ImportRequest 等)零改动,同时字段名只有 internal/archivedto 一处定义。
//
// 为什么必须共享:JSON 反序列化对**不认识的字段**静默忽略、
// 对**缺失的字段**留零值 ⇒ 字段名拼错的症状是
// "导入报告显示成功 27565 条,但 POS 上商品名全是空的",**没有一行报错**。
type (
	// ImportScopeRow 是单个 scope 的导入结果。
	ImportScopeRow = archivedto.ImportScopeRow
	// ImportErrorItem 是单行的失败明细。
	//
	// ⚠️ **逐行错误不中断整体** —— 一批 27565 行的导入,
	// 因为第 3 行有一条脏数据就整体回滚,等于没导。
	ImportErrorItem = archivedto.ImportErrorItem
	// ImportResult 是导入结果。
	ImportResult = archivedto.ImportResult
	// ImportCategoryRow 是分类行。
	ImportCategoryRow = archivedto.ImportCategoryRow
	// ImportProductRow 是商品行。
	ImportProductRow = archivedto.ImportProductRow
	// ImportSupplierRow 是供应商行。
	ImportSupplierRow = archivedto.ImportSupplierRow
	// ImportBarcodeRow 是商品条码行。
	ImportBarcodeRow = archivedto.ImportBarcodeRow
	// ImportData 是各 scope 的行集合。
	ImportData = archivedto.ImportData
	// ImportRequest 是导入请求。
	ImportRequest = archivedto.ImportRequest
)

// Import 执行一次导入。
func (s *ArchiveService) Import(ctx context.Context, branchID string, req ImportRequest) (*ImportResult, error) {
	if branchID == "" {
		return nil, model.ErrInvalidInput
	}
	mode := req.Mode
	if mode == "" {
		mode = ModeIncremental
	}
	if mode != ModeIncremental && mode != ModeOverwrite {
		return nil, fmt.Errorf("%w: mode=%q 非法(只能是 %s / %s)",
			model.ErrInvalidInput, mode, ModeIncremental, ModeOverwrite)
	}

	res := &ImportResult{
		SourceBranchID: req.SourceBranchID,
		TargetBranchID: branchID,
		Mode:           mode,
	}

	want := map[string]bool{}
	for _, sc := range req.Scopes {
		want[sc] = true
	}
	enabled := func(sc string) bool {
		return len(want) == 0 || want[sc]
	}

	// 顺序有讲究:先分类与供应商(商品的引用对象),再商品,最后条码
	// (条码引用商品)。这样"引用不存在"的问题在最后一步才暴露。
	if enabled(ScopeCategories) && len(req.Data.Categories) > 0 {
		row := s.importCategories(ctx, branchID, req.Data.Categories, mode)
		res.ByScope = append(res.ByScope, row)
	}
	if enabled(ScopeSuppliers) && len(req.Data.Suppliers) > 0 {
		row := s.importSuppliers(ctx, branchID, req.Data.Suppliers, mode)
		res.ByScope = append(res.ByScope, row)
	}
	if enabled(ScopeProducts) && len(req.Data.Products) > 0 {
		row := s.importProducts(ctx, branchID, req.Data.Products, mode)
		res.ByScope = append(res.ByScope, row)
	}
	if enabled(ScopeItemBarcodes) && len(req.Data.ItemBarcodes) > 0 {
		row := s.importItemBarcodes(ctx, branchID, req.Data.ItemBarcodes, mode)
		res.ByScope = append(res.ByScope, row)
	}

	// 价格没接住 ⇒ 必须说出来。
	//
	// ⚠️ 静默丢价格是最坏的一种丢:导入报告显示"成功 27565 条",
	// 而 POS 扫码出来全是 0 元,排查方向会跑到别处去。
	if enabled(ScopeProducts) && len(req.Data.Products) > 0 {
		hasPrice := false
		for _, p := range req.Data.Products {
			if p.PriceYuan != "" {
				hasPrice = true
				break
			}
		}
		if hasPrice {
			res.Warnings = append(res.Warnings,
				"price_yuan / cost_yuan **未落库**:价格归 pricing 服务(price_lists),"+
					"catalog.products 不存价(避免两个真相源)。"+
					"接入 pricing 前,门店 10 的商品在本表里没有售价。")
		}
	}
	return res, nil
}

// importCategories 导入分类。
func (s *ArchiveService) importCategories(ctx context.Context, branchID string,
	rows []ImportCategoryRow, mode string) ImportScopeRow {

	out := ImportScopeRow{Scope: ScopeCategories, Received: len(rows)}
	for _, r := range rows {
		if r.ID == "" || r.Name == "" {
			out.Skipped++
			continue
		}
		exists, err := s.exists(ctx, &model.Category{}, branchID, r.ID)
		if err != nil {
			out.Skipped++
			continue
		}
		if exists && mode == ModeIncremental {
			out.Skipped++
			continue
		}
		now := s.now()
		row := &model.Category{
			ID:        r.ID,
			BranchID:  branchID,
			Name:      r.Name,
			ParentID:  r.ParentID,
			SortOrder: 0,
			Status:    "active",
			CreatedBy: importActor,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if exists {
			// ⚠️ 只更新**可变列**;created_by / created_at 不动(那是审计线索)。
			err = s.db.WithContext(ctx).
				Model(&model.Category{}).
				Where("id = ? AND branch_id = ?", r.ID, branchID).
				Updates(map[string]any{
					"name":       r.Name,
					"parent_id":  r.ParentID,
					"status":     "active",
					"updated_by": importActor,
					"updated_at": now,
				}).Error
			if err != nil {
				out.Skipped++
				continue
			}
			out.Updated++
			continue
		}
		if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
			out.Skipped++
			continue
		}
		out.Inserted++
	}
	return out
}

// importSuppliers 导入供应商。
func (s *ArchiveService) importSuppliers(ctx context.Context, branchID string,
	rows []ImportSupplierRow, mode string) ImportScopeRow {

	out := ImportScopeRow{Scope: ScopeSuppliers, Received: len(rows)}
	for _, r := range rows {
		if r.ID == "" || r.Name == "" {
			out.Skipped++
			continue
		}
		exists, err := s.exists(ctx, &model.Supplier{}, branchID, r.ID)
		if err != nil {
			out.Skipped++
			continue
		}
		if exists && mode == ModeIncremental {
			out.Skipped++
			continue
		}
		now := s.now()
		typ := r.Type
		if typ == "" {
			typ = "0" // 供应商(客户是 "1")
		}
		status := r.Status
		if status == "" {
			status = "active"
		}
		if exists {
			err = s.db.WithContext(ctx).
				Model(&model.Supplier{}).
				Where("id = ? AND branch_id = ?", r.ID, branchID).
				Updates(map[string]any{
					"name":       r.Name,
					"type":       typ,
					"contact":    r.Contact,
					"phone":      r.Phone,
					"email":      r.Email,
					"address":    r.Address,
					"status":     status,
					"updated_by": importActor,
					"updated_at": now,
				}).Error
			if err != nil {
				out.Skipped++
				continue
			}
			out.Updated++
			continue
		}
		row := &model.Supplier{
			ID:        r.ID,
			BranchID:  branchID,
			Name:      r.Name,
			Type:      typ,
			Contact:   r.Contact,
			Phone:     r.Phone,
			Email:     r.Email,
			Address:   r.Address,
			Status:    status,
			CreatedBy: importActor,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
			out.Skipped++
			continue
		}
		out.Inserted++
	}
	return out
}

// importProducts 导入商品。
//
// ⚠️ 品名前缀在这里被解析成 is_exchange / is_void 并从名字里去掉。
func (s *ArchiveService) importProducts(ctx context.Context, branchID string,
	rows []ImportProductRow, mode string) ImportScopeRow {

	out := ImportScopeRow{Scope: ScopeProducts, Received: len(rows)}
	for _, r := range rows {
		if r.ID == "" || r.Name == "" {
			out.Skipped++
			continue
		}
		cleanName, isExchange, isVoid := parseProductName(r.Name)

		exists, err := s.exists(ctx, &model.Product{}, branchID, r.ID)
		if err != nil {
			out.Skipped++
			continue
		}
		if exists && mode == ModeIncremental {
			out.Skipped++
			continue
		}
		now := s.now()
		status := r.Status
		if status == "" {
			status = "active"
		}
		if exists {
			err = s.db.WithContext(ctx).
				Model(&model.Product{}).
				Where("id = ? AND branch_id = ?", r.ID, branchID).
				Updates(map[string]any{
					"name":        cleanName,
					"category_id": r.CategoryID,
					"supplier_id": r.SupplierID,
					"sub_code":    r.SubCode,
					"is_exchange": isExchange,
					"is_void":     isVoid,
					"unit":        r.Unit,
					"spec":        r.Spec,
					"status":      status,
					"updated_by":  importActor,
					"updated_at":  now,
				}).Error
			if err != nil {
				out.Skipped++
				continue
			}
			out.Updated++
			continue
		}
		row := &model.Product{
			ID:         r.ID,
			BranchID:   branchID,
			Name:       cleanName,
			CategoryID: r.CategoryID,
			SupplierID: r.SupplierID,
			SubCode:    r.SubCode,
			IsExchange: isExchange,
			IsVoid:     isVoid,
			Unit:       r.Unit,
			Spec:       r.Spec,
			Status:     status,
			CreatedBy:  importActor,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
			out.Skipped++
			continue
		}
		out.Inserted++
	}
	return out
}

// importItemBarcodes 导入商品条码。
//
// ⚠️ **按商品整体替换**:同一批次里某个 product_id 的条码集合是权威的,
// 源库删掉的条码要跟着消失(只加不减 ⇒ 留下**幽灵条码**,扫出来是错商品)。
func (s *ArchiveService) importItemBarcodes(ctx context.Context, branchID string,
	rows []ImportBarcodeRow, mode string) ImportScopeRow {

	out := ImportScopeRow{Scope: ScopeItemBarcodes, Received: len(rows)}
	if mode == ModeIncremental {
		// 增量模式**只加不改**:每个条码独立 upsert。
		for _, r := range rows {
			if r.Barcode == "" || r.ProductID == "" {
				out.Skipped++
				continue
			}
			_, err := s.barcodeSvc.Upsert(ctx, BarcodeInput{
				ProductID: r.ProductID,
				BranchID:  branchID,
				Barcode:   r.Barcode,
				Label:     r.Label,
			})
			if err != nil {
				out.Skipped++
				continue
			}
			out.Inserted++
		}
		return out
	}

	// 覆盖模式:按 product_id 分组,每组整体替换。
	byProduct := map[string][]ImportBarcodeRow{}
	var order []string
	for _, r := range rows {
		if r.Barcode == "" || r.ProductID == "" {
			out.Skipped++
			continue
		}
		if _, seen := byProduct[r.ProductID]; !seen {
			order = append(order, r.ProductID)
		}
		byProduct[r.ProductID] = append(byProduct[r.ProductID], r)
	}
	for _, pid := range order {
		group := byProduct[pid]
		inputs := make([]BarcodeInput, 0, len(group))
		for _, g := range group {
			inputs = append(inputs, BarcodeInput{
				ProductID: pid,
				BranchID:  branchID,
				Barcode:   g.Barcode,
				Label:     g.Label,
			})
		}
		written, removed, err := s.barcodeSvc.ReplaceForProduct(ctx, branchID, pid, inputs)
		if err != nil {
			out.Skipped += len(group)
			continue
		}
		out.Updated += written
		out.Deleted += removed
	}
	return out
}

// importActor 是导入写入的 created_by / updated_by。
//
// ⚠️ **不记录"数据来自哪个源门店"** —— 基础档案可以从任何地方进来
// (cube 导出 / Excel 导入 / 手工建档);归属门店由 branch_id 决定。
const importActor = "import"

// exists 判断 (id, branch_id) 是否已存在。
func (s *ArchiveService) exists(ctx context.Context, m any, branchID, id string) (bool, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(m).
		Where("id = ? AND branch_id = ?", id, branchID).
		Count(&n).Error
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// parseProductName 解析品名前缀,返回 (清洗后的名字, 是否换购品, 是否停用品)。
//
// # 前缀清单(实测)
//
//	「(换购)」「（换购）」或名字里含「换购」  → 换购品
//	「（停用）」或名字里含「停用」              → 停用品
//
// ⚠️ **只去掉已知的语义前缀**,别的括号前缀(如「（礼盒装）」)保留 ——
// 那是商品的正式名字的一部分,去掉就改了名字。
//
// # 为什么要清洗
//
// POS 界面上出现「(换购)远航九江双蒸酒精品500ml」这种品名,
// 用户会以为系统坏了;而且**没法按"是否换购"过滤** ——
// 换购品 19 个,售价多为 .0000 或象征价,混在普通商品里会让人扫到"看不见价格"的怪商品。
func parseProductName(name string) (clean string, isExchange, isVoid bool) {
	clean = strings.TrimSpace(name)
	// 换购:名字里任意位置含「换购」。
	if strings.Contains(clean, "换购") {
		isExchange = true
		clean = stripLeadingParenToken(clean, "换购")
	}
	// 停用:名字里任意位置含「停用」。
	if strings.Contains(clean, "停用") {
		isVoid = true
		clean = stripLeadingParenToken(clean, "停用")
	}
	return strings.TrimSpace(clean), isExchange, isVoid
}

// stripLeadingParenToken 去掉名字**开头**的「(token)」/「（token）」。
//
// ⚠️ 只处理开头:中间的括号是名字的一部分(如「和成天下100（15元换购）」里
// 括号在尾部,去掉会改掉商品名的语义)。
func stripLeadingParenToken(name, token string) string {
	for _, open := range []string{"(", "（"} {
		close := ")"
		if open == "（" {
			close = "）"
		}
		prefix := open + token + close
		if strings.HasPrefix(name, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(name, prefix))
		}
	}
	return name
}

// ErrArchiveUnsupported 是导入端点遇到不认识的东西。
var ErrArchiveUnsupported = errors.New("catalog: archive 导入参数不支持")
