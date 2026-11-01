package service

// internal/fresh-meat/service/audit_history.go —— **审计历史只读**(2026-11-01 owner 决策)。
//
// # 为什么审计端点下线了，这两个函数还留着
//
// 2026-11-01 owner 拍板：审计结转端点(`/stock/audit`)整条退役 —— 连同「冻结」语义一起下线。
// 但**历史 `biz_audits` 行不能跟着消失**，因为还有两条读路径依赖它：
//
// ① 余额链基线(`chainBase`)与日终收盘(`closingForDay`)以「最近一次审计截止日」作为链的起点，
//    老店的余额链就是从那一行快照接着往后推的 ⇒ 拿不到锚点，链会退化成从开业日重算。
// ② 期初结转清理(`stock_openings.go`)要用同一个截止日判断「哪些旧账可以清、哪些是链内数据」。
//
// ⇒ 所以：`model.BizAudit` 模型 / `biz_audits` 表保留(只读 + purge 清理)，
//   但「写审计」与「冻结校验」两条路径随端点一起退役。
//
// # 与旧实现的差别
//
// 旧实现在同一文件里还有 `assertNotAudited`(写操作前的冻结闸门)。
// 它已随端点一起删除 —— 现在**任何**写入都不会再被「截止日已冻结」拦下，
// 这是 owner 明确要的语义变更(账面链改为「实时可改」)。

import (
	"context"
	"fmt"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"gorm.io/gorm"
)

// lastAudit 取该店最近一次审计(按截止日倒序第一条)。
//
// ⚠️ 2026-11-01 owner：审计写入口已下线 ⇒ 这里退化为**纯历史只读**查询 ——
// 只回答「这家店历史上最后一次审计到哪天」，不再产生任何冻结效果。
func (s *Service) lastAudit(ctx context.Context, branchID string) (*model.BizAudit, error) {
	var a model.BizAudit
	err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("cutoff_date DESC").
		First(&a).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query biz_audits: %w", err)
	}
	return &a, nil
}

// auditCutoff 该店历史上最后一次审计的截止日(空字符串 = 从没审计过)。
//
// ⚠️ 2026-11-01 owner：审计端点下线 ⇒「冻结」语义已消失；
// 这里保留的 cutoff 只作为**余额链基线锚点**与**期初结转清理水位线**使用，
// 不再用于拦截任何写操作。
func (s *Service) auditCutoff(ctx context.Context, branchID string) (string, error) {
	a, err := s.lastAudit(ctx, branchID)
	if err != nil {
		return "", err
	}
	if a == nil {
		return "", nil
	}
	return time.Time(a.CutoffDate).In(s.bizTZOrUTC()).Format("2006-01-02"), nil
}
