// Package service / stocking_settings.go —— 商品档位的维护
// （2026-11-02 用户口径）。
//
// # 口径
//
//	product_profiles  可售上限天数（档位）与保质期；自动按思迅类别生成，人工可改。
//
// 与陈列保有量一样：审批触发时自动生成（Source=auto/pending），
// 人工改过（Source=manual）后自动生成不再覆盖；删除即回到自动。
//
// ⚠️ 旧的「一次进货下限」（stocking_floors）已并入陈列保有量，不再维护。
package service

import (
	"context"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// ProductProfileUpdateInput 是人工修改商品经营属性的入参。
type ProductProfileUpdateInput struct {
	CapDays       int
	ShelfLifeDays int
	Note          string
	OperatorID    string
}

// ListProductProfiles 管理后台列表（按货号/名称包含匹配 + 可选来源过滤 + 分页）。
func (s *Service) ListProductProfiles(ctx context.Context, branchID, query, source string, limit, offset int) ([]model.ProductProfile, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := s.db.WithContext(ctx).Model(&model.ProductProfile{}).Where("branch_id = ?", branchID)
	if qs := strings.TrimSpace(query); qs != "" {
		like := "%" + qs + "%"
		q = q.Where("product_id ILIKE ? OR product_name ILIKE ?", like, like)
	}
	if src := strings.TrimSpace(source); src != "" {
		q = q.Where("source = ?", src)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.ProductProfile
	if err := q.Order("updated_at DESC").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// UpdateProductProfile 人工修改可售上限天数/保质期（Source 置 manual）。
func (s *Service) UpdateProductProfile(ctx context.Context, branchID, productID string, in ProductProfileUpdateInput) (*model.ProductProfile, error) {
	productID = strings.TrimSpace(productID)
	if productID == "" {
		return nil, fmt.Errorf("%w: product_id 必填", ErrInvalidInput)
	}
	if in.CapDays < 1 || in.CapDays > 730 {
		return nil, fmt.Errorf("%w: 可售上限天数必须在 1~730 天之间", ErrInvalidInput)
	}
	if in.ShelfLifeDays < 0 || in.ShelfLifeDays > 3650 {
		return nil, fmt.Errorf("%w: 保质期必须在 0~3650 天之间", ErrInvalidInput)
	}
	var row model.ProductProfile
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND product_id = ?", branchID, productID).
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return nil, ErrStockingProfileNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Model(&row).Updates(map[string]any{
		"cap_days":        in.CapDays,
		"shelf_life_days": in.ShelfLifeDays,
		"source":          model.ProfileSourceManual,
		"note":            truncate(in.Note, 256),
		"operator_id":     in.OperatorID,
		"updated_at":      s.now(),
	}).Error; err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).First(&row, "id = ?", row.ID).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

// DeleteProductProfile 删除人工档位（下次审批自动重算）。
func (s *Service) DeleteProductProfile(ctx context.Context, branchID, productID string) error {
	productID = strings.TrimSpace(productID)
	if productID == "" {
		return fmt.Errorf("%w: product_id 必填", ErrInvalidInput)
	}
	res := s.db.WithContext(ctx).
		Where("branch_id = ? AND product_id = ?", branchID, productID).
		Delete(&model.ProductProfile{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrStockingProfileNotFound
	}
	return nil
}
