// Package service / scope_push.go —— 把"这个货号是猪肉部位"推给通用盘点的守卫。
//
// # 为什么需要推送
//
// 通用盘点(`stocktake`)必须**不盘**猪肉 —— 猪肉由 fresh-meat 域按部位盘。
// 两边各盘一次 ⇒ 两个数、没人说得清哪个对,而失败是**静默**的。
//
// 而"哪些货号是猪肉"的**唯一事实源**就是 fresh-meat 的部位映射表
// (`branch_cut_mappings`)。若让 stocktake 另维护一份清单 ⇒ **两份真相必然漂移**。
//
// 两个服务共用同一个 PG ⇒ 由 fresh-meat(权威方)把结论**推送**给
// stocktake 的排除域配置表(`stocktake_excluded_scopes`,scope_kind='product')。
// stocktake 只读自己那张表 ⇒ 读路径上没有跨服务耦合。
//
// # 写失败不回滚映射
//
// 排除域是**守卫**,不是主流程:
//   - 写失败 ⇒ 通用盘点暂时会 fail open(放行)⇒ 猪肉可能被盘两次(可见、可发现);
//   - 若因此回滚映射 ⇒ 映射这个业务事实就丢了(不可发现)。
// ⇒ 前者更安全。写失败只记日志。
package service

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm/clause"
)

// scopeExcludedRow 通用盘点排除域配置表的一行(本服务**只写不读**)。
type scopeExcludedRow struct {
	ID         string    `gorm:"column:id;type:varchar(64);primaryKey"`
	BranchID   string    `gorm:"column:branch_id;type:varchar(64);not null"`
	ScopeKind  string    `gorm:"column:scope_kind;type:varchar(16);not null"`
	ScopeValue string    `gorm:"column:scope_value;type:varchar(64);not null"`
	Note       string    `gorm:"column:note;type:varchar(255)"`
	CreatedAt  time.Time `gorm:"column:created_at;not null"`
	UpdatedAt  time.Time `gorm:"column:updated_at;not null"`
}

// TableName 显式表名(通用盘点服务的配置表)。
func (scopeExcludedRow) TableName() string { return "stocktake_excluded_scopes" }

const (
	// scopeKindProduct = 按货号排除(与 stocktake 侧 model.ScopeKindProduct 同形;
	// 这里不 import 另一个服务的 model 包 —— 那是**它的**表,不是本服务的模型)。
	scopeKindProduct = "product"
	// scopeExclusionNote 是给操作员看的一句话(拒绝时原样进错误消息)。
	scopeExclusionNote = "猪肉请用鲜肉盘点(生鲜肉盘点)"
)

// pushScopeExclusion 推送/撤销一个货号的排除域配置。
//
// active=true  ⇒ 该货号是猪肉部位 ⇒ 通用盘点必须拒收
// active=false ⇒ 撤销(映射被删了)
func (s *Service) pushScopeExclusion(ctx context.Context, branchID, cubeProductID string, active bool) {
	if branchID == "" || cubeProductID == "" {
		return
	}
	now := s.now()
	if !active {
		// 撤销:删掉那一行(不存在也不算错)。
		if err := s.db.WithContext(ctx).
			Where("branch_id = ? AND scope_kind = ? AND scope_value = ?",
				branchID, scopeKindProduct, cubeProductID).
			Delete(&scopeExcludedRow{}).Error; err != nil {
			s.pubLogger.Warn("pushScopeExclusion: delete failed",
				"branch_id", branchID, "cube_product_id", cubeProductID, "err", err)
		}
		return
	}
	rec := scopeExcludedRow{
		ID:         scopeExclusionID(now, branchID, cubeProductID),
		BranchID:   branchID,
		ScopeKind:  scopeKindProduct,
		ScopeValue: cubeProductID,
		Note:       scopeExclusionNote,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "branch_id"}, {Name: "scope_kind"}, {Name: "scope_value"}},
		DoUpdates: clause.Assignments(map[string]any{
			"note":       scopeExclusionNote,
			"updated_at": now,
		}),
	}).Create(&rec).Error; err != nil {
		// ⚠️ 表可能还不存在(通用盘点服务没起)⇒ 记日志、不回滚映射。
		s.pubLogger.Warn("pushScopeExclusion: upsert failed(通用盘点的排除域表可能还没建)",
			"branch_id", branchID, "cube_product_id", cubeProductID, "err", err)
	}
}

// scopeExclusionID 生成稳定的行 ID(同一条排除项反复推送得到同一个 ID)。
func scopeExclusionID(now time.Time, branchID, cubeProductID string) string {
	return fmt.Sprintf("SES%s%s%s", now.UTC().Format("20060102"), branchID, cubeProductID)
}
