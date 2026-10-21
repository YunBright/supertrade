// Package service / types.go —— 会员类别 + 付款方式字典(读侧)。
//
// ⚠️ **会员类别按门店维护,不跨店合并** ——
//
//	思迅两库的字典不同(ysx `2=VIP储值卡` / hbposv7 `wx=微信会员`)
//	⇒ 合并会把"储值卡"和"微信会员"混成一类,POS 下拉里就分不清了。
//
// ⚠️ **付款方式字典来自思迅 `t_bd_payment_info`**(可导入):
//
//	pay_way(char3) / pay_name / rate / is_integral / sort
//	生产在用:RMB / SWX / SZF / LEP / CHG / SUP / SAV,积分支付是 JFP(is_integral=1)。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/member/model"
)

// ListTypes 列某门店的会员类别(按 priority、name 排序)。
func (s *Service) ListTypes(ctx context.Context, branchID string) ([]model.MemberType, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", model.ErrInvalidInput)
	}
	var rows []model.MemberType
	err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("priority ASC, name ASC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// GetType 取某个会员类别。
func (s *Service) GetType(ctx context.Context, branchID, typeID string) (*model.MemberType, error) {
	var row model.MemberType
	err := s.db.WithContext(ctx).
		Where("id = ? AND branch_id = ?", strings.TrimSpace(typeID), strings.TrimSpace(branchID)).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, model.ErrMemberTypeNotFound
		}
		return nil, err
	}
	return &row, nil
}

// ListPaymentWays 列某门店的付款方式字典。
func (s *Service) ListPaymentWays(ctx context.Context, branchID string) ([]model.PaymentWay, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", model.ErrInvalidInput)
	}
	var rows []model.PaymentWay
	err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("sort_order ASC, code ASC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}
