// Package service / import.go —— 库存快照的**导入**（一次性归档同步用）。
//
// # 它是什么
//
// 把某门店在**同步时刻**的库存快照写进 `inventory.stock_levels`。
//
// # ⚠️ 时效语义（2026-10-21 用户确认）
//
// 「库存同步只同步**至同步时的时间**的库存，回放也只回放该同步时间之后的收银记录」。
//
// ⇒ 导入的是**快照**，不是历史轨迹：
//   - cube 的 `stock` model 是 **live**（透传思迅 `t_im_branch_stock` 的当前值）
//     ⇒ 导出来天然就是"同步那一刻"的库存，**不需要**任何时间过滤；
//   - 导入是**幂等 upsert**（按 `(branch_id, product_id)`）
//     ⇒ 重复导入同一份快照不会产生第二行；
//   - 库存**数量允许为负**（卖超了 / 盘点亏损）——
//     ⚠️ **不做夹逼**：夹到 0 会把"超卖"从数据里抹掉，现场再也查不出原因。
//
// # ⚠️ **有源门店只读**
//
// 门店在 cube-router 里配了数据源 ⇒ 它的库存在外部系统里 ⇒ 本域只读，
// 导入一律 409（见 EnsureLocalSource）。
package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/inventory/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// StockImportResult 是单个 scope 的导入结果。
type StockImportResult struct {
	Scope    string `json:"scope"`
	Inserted int    `json:"inserted"`
	Updated  int    `json:"updated"`
	Skipped  int    `json:"skipped"`
	Deleted  int    `json:"deleted"`
}

// StockImportReport 是一次库存导入的整体结果。
type StockImportReport struct {
	SourceBranchID string              `json:"source_branch_id"`
	TargetBranchID string              `json:"target_branch_id"`
	Mode           string              `json:"mode"`
	ByScope        []StockImportResult `json:"by_scope"`
	Warnings       []string            `json:"warnings,omitempty"`
	Errors         []string            `json:"errors,omitempty"`
}

// StockImportInput 是导入的入参。
type StockImportInput struct {
	// SourceBranchID 是**来源门店**（仅用于审计与报告；数据本身由 cube 决定）。
	SourceBranchID string
	// TargetBranchID 是**目标门店**（= 请求的 X-Branch-ID）。
	TargetBranchID string
	// Mode 是 incremental / overwrite。
	Mode string
	// Data 是导入的行。
	Data archivedto.ImportData
}

// ImportStock 导入库存快照。
//
// ⚠️ **目标门店只从 header 取**（调用方传进来的）——
// body 里带一份门店就能跨店写入，这是**跨店写**的口子。
//
// ⚠️ **有源门店只读**（2026-10-21）：门店配了数据源 ⇒ 它的库存在外部系统里
// ⇒ 本域 PG 里那份"导入的库存"**没人看**（前台查的是 cube）。
// 不拒的话：导入报告显示"成功 23833 条"，而门店的库存根本没变
// ⇒ 账实不符，且**没有任何一行报错**。
func (s *StockService) ImportStock(ctx context.Context, in StockImportInput) (*StockImportReport, error) {
	if in.TargetBranchID == "" {
		return nil, model.ErrInvalidInput
	}
	// ⚠️ **闸门在写之前**：有源 ⇒ 409（不写一行）。
	//   解析失败（问不到路由）⇒ 放行（导入路径自己兜底，见 EnsureLocalSource）。
	if err := s.EnsureLocalSource(ctx, in.TargetBranchID); err != nil {
		return nil, err
	}
	mode := strings.TrimSpace(in.Mode)
	if mode == "" {
		mode = "incremental"
	}
	if mode != "incremental" && mode != "overwrite" {
		return nil, model.ErrInvalidInput
	}

	rep := &StockImportReport{
		SourceBranchID: in.SourceBranchID,
		TargetBranchID: in.TargetBranchID,
		Mode:           mode,
	}

	rows := in.Data.Stocks
	if len(rows) == 0 {
		rep.ByScope = append(rep.ByScope, StockImportResult{Scope: archivedto.ScopeStock})
		return rep, nil
	}

	now := s.now()
	var inserted, updated, skipped int

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, row := range rows {
			productID := strings.TrimSpace(row.ProductID)
			if productID == "" {
				skipped++
				continue
			}
			qty, err := decimal.NewFromString(strings.TrimSpace(row.Qty))
			if err != nil {
				skipped++
				continue
			}
			avgCost, err := decimal.NewFromString(strings.TrimSpace(row.AvgCostYuan))
			if err != nil {
				avgCost = decimal.Zero
			}

			// ⚠️ **先查后写**：upsert 的 RowsAffected 在"新建"和"覆盖"两种情况下
			//   都是 1 ⇒ 事后查已经分不出来了 ⇒ 必须在写**之前**判断这行在不在。
			isNew := !stockLevelExists(tx, in.TargetBranchID, productID)

			level := model.StockLevel{
				BranchID:    in.TargetBranchID,
				ProductID:   productID,
				Qty:         qty,
				AvgCostYuan: avgCost,
				CreatedBy:   "import",
				UpdatedBy:   "import",
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			res := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{
					{Name: "branch_id"},
					{Name: "product_id"},
				},
				DoUpdates: clause.AssignmentColumns([]string{
					"qty", "avg_cost_yuan", "updated_by", "updated_at",
				}),
			}).Create(&level)
			if res.Error != nil {
				return fmt.Errorf("写库存快照失败: %w", res.Error)
			}
			if isNew {
				inserted++
			} else {
				updated++
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	rep.ByScope = append(rep.ByScope, StockImportResult{
		Scope:    archivedto.ScopeStock,
		Inserted: inserted,
		Updated:  updated,
		Skipped:  skipped,
	})
	return rep, nil
}

// stockLevelExists 判断该门店该商品的快照行在不在。
func stockLevelExists(tx *gorm.DB, branchID, productID string) bool {
	var n int64
	if err := tx.Model(&model.StockLevel{}).
		Where("branch_id = ? AND product_id = ?", branchID, productID).
		Count(&n).Error; err != nil {
		return false
	}
	return n > 0
}
