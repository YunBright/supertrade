// Package service / product_barcode.go —— 商品条码业务逻辑(1 商品 : N 条码)。
//
// # 为什么单独一张表
//
// 思迅的条码在独立表 `t_bd_item_barcode`,**同一商品可以有多条码**
// (实测 `6914782121881` 有两条:草莓味 / 桔子味)。
// 单列 `products.barcode` 装不下 ⇒ 全集在本表,`products.barcode` 只冗余主条码。
//
// 详见 model/product_barcode.go 的注释(那里有实测数据)。
//
// # 扫码查商品
//
// FindByBarcode 是 POS 扫码的主路径:
//
//	扫到的条码 → 本表查 (branch_id, barcode) → product_id → 商品
//
// 查不到再退到 `products.barcode`(冗余的主条码,多数商品只有一条)。
//
// 错误:
//
//   - model.ErrBarcodeNotFound → 404
//   - model.ErrInvalidInput    → 400
package service

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/catalog/model"
)

// ProductBarcodeService 是 catalog 的商品条码服务。
type ProductBarcodeService struct {
	db  *gorm.DB
	now func() time.Time
}

// NewProductBarcodeService 构造 ProductBarcodeService。
func NewProductBarcodeService(db *gorm.DB) *ProductBarcodeService {
	return &ProductBarcodeService{db: db, now: func() time.Time { return time.Now() }}
}

// BarcodeInput 是单条条码的入参。
type BarcodeInput struct {
	ProductID string
	BranchID  string
	Barcode   string
	// IsPrimary 标记主条码(冗余进 products.barcode)。
	IsPrimary bool
	// Label 是条码说明(口味/规格变体)。
	Label string
}

// Upsert 写一条条码(同 (product_id, branch_id, barcode) 幂等覆盖)。
func (s *ProductBarcodeService) Upsert(ctx context.Context, in BarcodeInput) (*model.ProductBarcode, error) {
	if in.ProductID == "" || in.BranchID == "" || strings.TrimSpace(in.Barcode) == "" {
		return nil, model.ErrInvalidInput
	}
	now := s.now()
	row := &model.ProductBarcode{
		ProductID: in.ProductID,
		BranchID:  in.BranchID,
		Barcode:   strings.TrimSpace(in.Barcode),
		IsPrimary: in.IsPrimary,
		Label:     in.Label,
		CreatedBy: "system",
		CreatedAt: now,
		UpdatedAt: now,
	}
	// ⚠️ 用 OnConflict 只更新**可变列**,不碰 created_by / created_at:
	// 重跑导入时不该把"谁建的"改掉(那会丢审计线索)。
	//
	// ⚠️ 冲突键是三列复合 PK(product_id, branch_id, barcode)——
	// 单靠 barcode 冲突会把**别的商品**的同条码行也改掉。
	err := s.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "product_id"}, {Name: "branch_id"}, {Name: "barcode"},
			},
			DoUpdates: clause.Assignments(map[string]any{
				"is_primary": row.IsPrimary,
				"label":      row.Label,
				"updated_at": now,
			}),
		}).
		Create(row).Error
	if err != nil {
		return nil, err
	}
	return row, nil
}

// ReplaceForProduct 用给定集合**整体替换**某商品的条码。
//
// 用于导入:一个商品的条码集合是权威的(源库给了几条就是几条),
// 源库里删掉的条码也要跟着消失 —— 增量式"只加不减"会留下幽灵条码。
//
// 返回 (写入条数, 删除条数)。
func (s *ProductBarcodeService) ReplaceForProduct(ctx context.Context, branchID, productID string, in []BarcodeInput) (int, int, error) {
	if branchID == "" || productID == "" {
		return 0, 0, model.ErrInvalidInput
	}
	kept := make([]string, 0, len(in))
	for _, b := range in {
		if strings.TrimSpace(b.Barcode) == "" {
			continue
		}
		kept = append(kept, strings.TrimSpace(b.Barcode))
	}

	// ① 先删掉不在集合里的(软删,保留审计)。
	del := s.db.WithContext(ctx).
		Model(&model.ProductBarcode{}).
		Where("branch_id = ? AND product_id = ?", branchID, productID)
	if len(kept) > 0 {
		del = del.Where("barcode NOT IN ?", kept)
	}
	res := del.Update("deleted_at", s.now())
	if res.Error != nil {
		return 0, 0, res.Error
	}
	removed := int(res.RowsAffected)

	// ② 再写集合里的。
	written := 0
	for _, b := range in {
		if _, err := s.Upsert(ctx, BarcodeInput{
			ProductID: productID,
			BranchID:  branchID,
			Barcode:   b.Barcode,
			IsPrimary: b.IsPrimary,
			Label:     b.Label,
		}); err != nil {
			return written, removed, err
		}
		written++
	}
	return written, removed, nil
}

// ListByProduct 列出某商品的全部条码。
func (s *ProductBarcodeService) ListByProduct(ctx context.Context, branchID, productID string) ([]model.ProductBarcode, error) {
	if branchID == "" || productID == "" {
		return nil, model.ErrInvalidInput
	}
	var rows []model.ProductBarcode
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND product_id = ?", branchID, productID).
		Order("is_primary desc, barcode").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ListByBranch 列出该门店**全部**条码,按 product_id 归组。
//
// # 为什么要"一次全量"
//
// 离线商品库需要每个商品的**全部**条码(1 商品 : 1~7 条),
// 逐个商品调 ListByProduct 是 N 次往返(27470 个商品 = 27470 次)。
//
// # 返回值语义
//
// 返回 map[product_id][]barcode:
//
//   - **没有条码的商品不在 map 里**(调用方据此回落到 `products.id`)
//   - 同一商品的条码按 (is_primary desc, barcode) 排 —— 主条码在前
//
// ⚠️ 软删行由 GORM 自动过滤(`gorm.DeletedAt`)⇒ 删掉的条码不会回来。
func (s *ProductBarcodeService) ListByBranch(ctx context.Context, branchID string) (map[string][]string, error) {
	out := map[string][]string{}
	if branchID == "" {
		return nil, model.ErrInvalidInput
	}
	var rows []model.ProductBarcode
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("is_primary desc, barcode").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		code := strings.TrimSpace(r.Barcode)
		if code == "" {
			continue
		}
		out[r.ProductID] = append(out[r.ProductID], code)
	}
	return out, nil
}

// FindByBarcode 按条码找商品 id。
//
// 只查条码表(全集)。查不到时调用方可以再退到 `products.barcode`。
func (s *ProductBarcodeService) FindByBarcode(ctx context.Context, branchID, barcode string) (string, error) {
	if branchID == "" || strings.TrimSpace(barcode) == "" {
		return "", model.ErrInvalidInput
	}
	var row model.ProductBarcode
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND barcode = ?", branchID, strings.TrimSpace(barcode)).
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return "", model.ErrBarcodeNotFound
	}
	if err != nil {
		return "", err
	}
	return row.ProductID, nil
}

// Delete 软删一条条码。
func (s *ProductBarcodeService) Delete(ctx context.Context, branchID, productID, barcode string) error {
	res := s.db.WithContext(ctx).
		Model(&model.ProductBarcode{}).
		Where("branch_id = ? AND product_id = ? AND barcode = ?", branchID, productID, barcode).
		Update("deleted_at", s.now())
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return model.ErrBarcodeNotFound
	}
	return nil
}
