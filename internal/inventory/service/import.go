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
	"sort"
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

	// ⚠️⚠️ **分批提交 + 批量 upsert**(2026-10-22)
	//
	// # 为什么要改(生产实测的根因)
	//
	//	原实现是**一个大事务**里逐行「先查后写」23837 行
	//	⇒ 23837 次 SELECT + 23837 次 upsert,全程持有行锁。
	//	⇒ 同步期间**同一个 PG 上的 POS 开单被拖住**
	//	   (sales_orders / sale_lines / payments / outbox 与 stock_levels 同库)
	//	⇒ 症状:「同步库存时 17 秒,超市收银数据上传延迟」。
	//
	//	⚠️ **慢的不是源库**(源库那张表才 23837 行,一次 live 查询几百毫秒),
	//	**慢的是本地 PG 的写** ⇒ 治标要治在这里。
	//
	// # 改法
	//
	//	① **分批提交**(默认 1000 行一批)⇒ 不再持有一个横跨全量的大事务
	//	   ⇒ POS 开单最多等**一批**的时间(几十~几百毫秒),不是十几秒。
	//	② **每批一次 IN 查询**代替"每行一次 SELECT"
	//	   ⇒ SELECT 次数 23837 → 24。
	//	③ **批量 upsert**(CreateInBatches + ON CONFLICT)
	//	   ⇒ INSERT 语句 23837 → 24。
	//
	// # 语义**完全不变**
	//
	//	仍然是「快照覆盖」:导进来的 qty 就是源库此刻的值。
	//	分批只影响**事务边界**,不影响结果
	//	(每批独立提交 ⇒ 中途失败会留下**部分已同步**的状态,
	//	 但下一次同步会把它们补上 ⇒ 自愈)。
	const stockImportBatch = 1000
	for start := 0; start < len(rows); start += stockImportBatch {
		end := start + stockImportBatch
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]

		// ① 先把这批解析成"要写的行"(顺带去重)。
		//    ⚠️ **同批内必须去重**:PG 的 ON CONFLICT 不能对同一行作用两次
		//    ("ON CONFLICT DO UPDATE command cannot affect row a second time")。
		byProduct := make(map[string]model.StockLevel, len(batch))
		for _, row := range batch {
			productID := strings.TrimSpace(row.ProductID)
			if productID == "" {
				skipped++
				continue
			}
			qty, perr := decimal.NewFromString(strings.TrimSpace(row.Qty))
			if perr != nil {
				skipped++
				continue
			}
			avgCost, cerr := decimal.NewFromString(strings.TrimSpace(row.AvgCostYuan))
			if cerr != nil {
				avgCost = decimal.Zero
			}
			byProduct[productID] = model.StockLevel{
				BranchID:    in.TargetBranchID,
				ProductID:   productID,
				Qty:         qty,
				AvgCostYuan: avgCost,
				CreatedBy:   "import",
				UpdatedBy:   "import",
				CreatedAt:   now,
				UpdatedAt:   now,
			}
		}
		if len(byProduct) == 0 {
			continue
		}
		levels := make([]model.StockLevel, 0, len(byProduct))
		for _, lv := range byProduct {
			levels = append(levels, lv)
		}
		// 稳定排序 ⇒ 同一次导入的行序可复现(便于对账/排障)。
		sort.Slice(levels, func(i, j int) bool { return levels[i].ProductID < levels[j].ProductID })

		// ② 这一批单独一个事务(短事务 ⇒ 不挡 POS)。
		batchInserted, batchUpdated, err := s.writeStockBatch(ctx, in.TargetBranchID, levels)
		if err != nil {
			return nil, err
		}
		inserted += batchInserted
		updated += batchUpdated
	}

	rep.ByScope = append(rep.ByScope, StockImportResult{
		Scope:    archivedto.ScopeStock,
		Inserted: inserted,
		Updated:  updated,
		Skipped:  skipped,
	})
	return rep, nil
}

// writeStockBatch 写一批库存快照(一个**短**事务),返回 (新增数, 覆盖数)。
//
// # 为什么单独一个事务
//
//	一个横跨 23837 行的事务会**全程持锁** ⇒ 同一个 PG 上的 POS 开单被拖住
//	(生产实测:同步库存时 17 秒,超市收银数据上传延迟)。
//	⇒ 一批一个事务 ⇒ 锁的窗口只有一批。
//
// # 为什么先查一次"存在性"
//
//	upsert 的 RowsAffected 在"新建"和"覆盖"两种情况下都是 1
//	⇒ 事后查分不出来 ⇒ 必须在写**之前**知道这行在不在。
//	⇒ 但**一次 IN 查询**就够(不必每行一次)。
func (s *StockService) writeStockBatch(ctx context.Context, branchID string, levels []model.StockLevel) (int, int, error) {
	if len(levels) == 0 {
		return 0, 0, nil
	}
	var inserted, updated int
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		existing := stockLevelExistsBatch(tx, branchID, levels)
		res := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "branch_id"},
				{Name: "product_id"},
			},
			DoUpdates: clause.AssignmentColumns([]string{
				"qty", "avg_cost_yuan", "updated_by", "updated_at",
			}),
		}).Create(&levels)
		if res.Error != nil {
			return fmt.Errorf("写库存快照失败: %w", res.Error)
		}
		for _, lv := range levels {
			if existing[lv.ProductID] {
				updated++
			} else {
				inserted++
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return inserted, updated, nil
}

// stockLevelExistsBatch 一次查清这批里哪些商品**已经有**快照行。
//
// 返回的 map 是"已存在"的 product_id 集合。
func stockLevelExistsBatch(tx *gorm.DB, branchID string, levels []model.StockLevel) map[string]bool {
	out := map[string]bool{}
	if len(levels) == 0 {
		return out
	}
	ids := make([]string, 0, len(levels))
	for _, lv := range levels {
		ids = append(ids, lv.ProductID)
	}
	var rows []model.StockLevel
	if err := tx.Model(&model.StockLevel{}).
		Where("branch_id = ? AND product_id IN ?", branchID, ids).
		Find(&rows).Error; err != nil {
		// ⚠️ 查不到就当"全是新建" —— 宁可计数不准,也不能让整批写失败。
		return out
	}
	for _, r := range rows {
		out[r.ProductID] = true
	}
	return out
}
