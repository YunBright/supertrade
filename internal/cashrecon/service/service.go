// Package service 是 cash-recon 的业务装配层。
//
// # 与 engine 的边界
//
//	engine  → 纯函数匹配(无 DB / 无 time.Now / 无 rand)
//	service → 取数、校验、落库、审计日志
//
// 把"取数"放在 service 而不是 engine,是这个域最关键的一条:
// 收银流水**不落本地库**(有 cube 源查 cube / 无源读 transaction 的 pay_flows),
// 而匹配必须能在 dry-run 下跑 —— 预览与落库共用 engine,只是 service 不写库。
package service

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cashrecon/engine"
	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// maxRangeDays 是单次查询/匹配允许的最大区间。
//
// 按**逐日**聚合设计(单日 ≤ 20 行),31 天是一次月度对账的自然上限;
// 再大只会把 cube 查询放大成慢查询,而收益是"一次看三个月"这种没人用的视图。
const maxRangeDays = 31

// Service 是 cash-recon 的业务单例。
type Service struct {
	db *gorm.DB
}

// New 构造 service。
func New(db *gorm.DB) *Service {
	return &Service{db: db}
}

// PreviewInput 是 dry-run 的入参。
type PreviewInput struct {
	BranchID    string
	BizDateFrom time.Time
	BizDateTo   time.Time
}

// Preview 组装引擎输入并返回匹配结果,**零写库**。
//
// S0 只落引擎骨架:收银流水与银行流水接入在 S1/S2(此时输入为空集合,
// 返回的 Summary 全零)。接口形状先定下来,是为了让前端与契约测试
// 从第一天起就对着最终形状写。
func (s *Service) Preview(ctx context.Context, in PreviewInput) (engine.Output, error) {
	if err := validateRange(in.BranchID, in.BizDateFrom, in.BizDateTo); err != nil {
		return engine.Output{}, err
	}
	_ = ctx
	return engine.Run(engine.Input{
		BranchID:    in.BranchID,
		BizDateFrom: in.BizDateFrom,
		BizDateTo:   in.BizDateTo,
	}), nil
}

// ListBatches 返回门店的批次列表(按创建时间倒序)。
func (s *Service) ListBatches(ctx context.Context, branchID string, limit int) ([]model.ReconBatch, error) {
	if branchID == "" {
		return nil, model.ErrInvalidInput
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var rows []model.ReconBatch
	err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("created_at DESC").
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// GetBatch 按 id 取单个批次(必须属于该门店)。
func (s *Service) GetBatch(ctx context.Context, branchID, id string) (model.ReconBatch, error) {
	if branchID == "" || id == "" {
		return model.ReconBatch{}, model.ErrInvalidInput
	}
	var row model.ReconBatch
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND id = ?", branchID, id).
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return model.ReconBatch{}, model.ErrNotFound
	}
	if err != nil {
		return model.ReconBatch{}, err
	}
	return row, nil
}

// validateRange 校验门店 + 日期区间。
//
// ⚠️ 区间上限写死在这里、不是只在前端拦:cube 查询的成本由**后端**承担,
// 前端拦不住的调用方(脚本/契约测试)照样能把区间拉到仨月。
func validateRange(branchID string, from, to time.Time) error {
	if branchID == "" {
		return model.ErrInvalidInput
	}
	if from.IsZero() || to.IsZero() {
		return model.ErrInvalidInput
	}
	from = time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	to = time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	if to.Before(from) {
		return model.ErrInvalidInput
	}
	if int(to.Sub(from).Hours()/24)+1 > maxRangeDays {
		return model.ErrRangeTooLarge
	}
	return nil
}
