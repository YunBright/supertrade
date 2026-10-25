// Package service / promo.go —— 特价档货号登记(**长期配置**)。
//
// # 为什么要这张表
//
// 2026-10-23 实测(佳乐近 14 个完整营业日):特价档是"**价格槽位**",
// 装的是当天滞销的那个部位 —— 成交价**逐日变**:
//
//	03001 特价猪肉5元  名义 5 元 → 实际卖 9 元/斤
//	03002 特价猪肉8元  名义 8 元 → 实际卖 7 元/斤
//	03007 特价猪肉20元 名义 20 元 → 10-13~10-20 卖 15 元、10-21 卖 20 元
//	03390 特价猪肉5Y  名义 4 元 → 卖 3.98 / 4.90 / 4.00
//
// ⇒ **不能静态映射到某个部位**(试过按"当日同价商品"自动反解,匹配上的都是巧合:
// 8 元/斤匹配到的是猪头皮,只是因为该店 8 元/斤到处都是)。
//
// 但"这些货号是特价"这件事是**稳定**的(佳乐 9 个货号长期在售)⇒ 登记一次即可。
//
// # 作用
//
// 登记过的货号,其 POS 销售行**不再被丢弃**:原来 `resolvePigByCubeProduct`
// 查不到部位映射就直接 `continue` 丢整行 ⇒ **金额与重量一起丢**
// (佳乐约 5% 的成交金额、约 10% 的成交重量消失)。
//
// 现在它们归到 `promo` 桶:金额与重量进得来,但**不摊到具体部位**
// ⇒ 毛利报告里单列"特价(未归属)",让这块不精确**可见**而不是静默摊错。
package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"gorm.io/gorm"
)

// PromoSkuInput POST /promo-skus 入参。
type PromoSkuInput struct {
	CubeProductID string `json:"cube_product_id" binding:"required"`
	ProductName   string `json:"product_name"`
	PriceTierText string `json:"price_tier_text"`
	Note          string `json:"note"`
	// Active 可空:不传 = 启用。
	Active *bool `json:"active"`
}

// CreatePromoSku 登记一个特价档货号。
//
// 同 (branch_id, cube_product_id) 重复登记 ⇒ **幂等更新**(不报错),
// 因为"补一条货号说明"是常见的日常操作。
func (s *Service) CreatePromoSku(ctx context.Context, branchID string, in PromoSkuInput) (*model.PromoSku, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	sku := strings.TrimSpace(in.CubeProductID)
	if sku == "" {
		return nil, fmt.Errorf("%w: cube_product_id 必填", ErrInvalidInput)
	}
	active := true
	if in.Active != nil {
		active = *in.Active
	}
	now := s.now()

	var existing model.PromoSku
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND cube_product_id = ?", branchID, sku).
		First(&existing).Error
	switch {
	case err == nil:
		// 幂等更新
		updates := map[string]any{
			"product_name":    strings.TrimSpace(in.ProductName),
			"price_tier_text": strings.TrimSpace(in.PriceTierText),
			"note":            strings.TrimSpace(in.Note),
			"active":          active,
			"updated_at":      now,
		}
		if err := s.db.WithContext(ctx).Model(&existing).Updates(updates).Error; err != nil {
			return nil, fmt.Errorf("update promo_sku: %w", err)
		}
		existing.ProductName = updates["product_name"].(string)
		existing.PriceTierText = updates["price_tier_text"].(string)
		existing.Note = updates["note"].(string)
		existing.Active = active
		existing.UpdatedAt = now
		return &existing, nil
	case err != gorm.ErrRecordNotFound:
		return nil, fmt.Errorf("query promo_sku: %w", err)
	}

	rec := &model.PromoSku{
		ID:            generateID(now, "PSK"),
		BranchID:      branchID,
		CubeProductID: sku,
		ProductName:   strings.TrimSpace(in.ProductName),
		PriceTierText: strings.TrimSpace(in.PriceTierText),
		Note:          strings.TrimSpace(in.Note),
		Active:        active,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.db.WithContext(ctx).Create(rec).Error; err != nil {
		return nil, fmt.Errorf("create promo_sku: %w", err)
	}
	return rec, nil
}

// ListPromoSkus 列某店登记的全部特价档货号。
func (s *Service) ListPromoSkus(ctx context.Context, branchID string) ([]model.PromoSku, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	var rows []model.PromoSku
	if err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("cube_product_id ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query promo_skus: %w", err)
	}
	return rows, nil
}

// DeletePromoSku 停用(软删)一个特价档货号。
//
// 用"停用"而不是物理删除:历史销售行里已经有它的 cut_type=promo 记录,
// 物理删了会让那些行变成孤儿。
func (s *Service) DeletePromoSku(ctx context.Context, branchID, id string) error {
	var rec model.PromoSku
	if err := s.db.WithContext(ctx).First(&rec, "id = ?", id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			// ⚠️ 2026-10-23:原来复用 `ErrBranchCutMappingNotFound`,
			// 于是"停用一个不存在的特价货号"返回的是**部位映射**的措辞
			// (「门店 cut 部位映射不存在」)⇒ 排查会跑去查映射表,方向完全错。
			return ErrPromoSkuNotFound
		}
		return err
	}
	if rec.BranchID != branchID {
		return ErrBranchMismatch
	}
	return s.db.WithContext(ctx).Model(&rec).
		Updates(map[string]any{"active": false, "updated_at": s.now()}).Error
}
