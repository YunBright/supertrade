// Package service / stocking_display.go —— 正常陈列保有量的维护与落库
// （2026-11-02 用户口径）。
//
// # 口径
//
//	自动生成：审批触发时按历史单批净进货量 P50 写入（Source=auto/pending）。
//	人工修改：后台把该组改成 Source=manual，此后自动生成**永不覆盖**。
//	删除：删掉人工行 ⇒ 下次审批重新按 P50 自动生成。
//
// ⚠️ 陈列量只参与「可售量 = 补货后库存 − 陈列保有量」，
// 与日均消耗、可售上限天数的 180 天学习完全独立 —— 改陈列量不会反过来
// 改变销量口径（用户口径：手动修改陈列数不影响可售天数的自我更新与优化）。
//
// ⚠️ 写权限沿用 product:manage（店长没有该 scope），
// 避免申请人自己把陈列量改大来放宽审批。
package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// StockingDisplayUpdateInput 是人工修改陈列保有量的入参。
type StockingDisplayUpdateInput struct {
	DisplayQty decimal.Decimal
	Note       string
	OperatorID string
}

// ListStockingDisplays 管理后台列表（按代表货号/组键包含匹配 + 可选来源过滤 + 分页）。
func (s *Service) ListStockingDisplays(ctx context.Context, branchID, query, source string, limit, offset int) ([]model.StockingDisplay, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := s.db.WithContext(ctx).Model(&model.StockingDisplay{}).Where("branch_id = ?", branchID)
	if qs := strings.TrimSpace(query); qs != "" {
		like := "%" + qs + "%"
		q = q.Where("product_id ILIKE ? OR product_name ILIKE ? OR group_key ILIKE ?", like, like, like)
	}
	if src := strings.TrimSpace(source); src != "" {
		q = q.Where("source = ?", src)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.StockingDisplay
	if err := q.Order("updated_at DESC").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// UpdateStockingDisplay 人工修改一个商品组的陈列保有量（Source 置 manual）。
func (s *Service) UpdateStockingDisplay(ctx context.Context, branchID, productID string, in StockingDisplayUpdateInput) (*model.StockingDisplay, error) {
	productID = strings.TrimSpace(productID)
	if productID == "" {
		return nil, fmt.Errorf("%w: product_id 必填", ErrInvalidInput)
	}
	if in.DisplayQty.IsNegative() {
		return nil, fmt.Errorf("%w: 陈列保有量不能为负数", ErrInvalidInput)
	}
	var row model.StockingDisplay
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND product_id = ?", branchID, productID).
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return nil, ErrStockingDisplayNotFound
	}
	if err != nil {
		return nil, err
	}
	updates := map[string]any{
		"display_qty": in.DisplayQty.Round(3),
		"source":      model.ProfileSourceManual,
		"note":        truncate(in.Note, 256),
		"operator_id": in.OperatorID,
		"updated_at":  s.now(),
	}
	if err := s.db.WithContext(ctx).Model(&row).Updates(updates).Error; err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).First(&row, "id = ?", row.ID).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

// DeleteStockingDisplay 删除一个商品组的陈列保有量（下次审批自动重算）。
func (s *Service) DeleteStockingDisplay(ctx context.Context, branchID, productID string) error {
	productID = strings.TrimSpace(productID)
	if productID == "" {
		return fmt.Errorf("%w: product_id 必填", ErrInvalidInput)
	}
	res := s.db.WithContext(ctx).
		Where("branch_id = ? AND product_id = ?", branchID, productID).
		Delete(&model.StockingDisplay{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrStockingDisplayNotFound
	}
	return nil
}
