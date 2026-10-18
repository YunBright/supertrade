package salesdaily

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrNotFound 表示该门店该营业日没有快照。
//
// ⚠️ 它与"采集过但当天零销售"必须分得开：
//
//	后者会写下一行金额全 0 的 DailyTotals；前者连行都没有。
//	调用方（推送）据此决定"回退查 cube"还是"如实报告今天没生意"，
//	混为一谈会让"没采集"被当成"没生意"报给店长。
var ErrNotFound = errors.New("salesdaily: 该营业日没有销售快照")

// Store 是每日事实包的持久化仓库。
type Store struct {
	db  *gorm.DB
	now func() time.Time
}

// NewStore 构造。now 为 nil 时用 time.Now（生产固定，测试注入）。
func NewStore(db *gorm.DB, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, now: now}
}

// Save 幂等地写入一个营业日的完整快照。
//
// # 为什么是"整日替换"而不是 upsert 各子表
//
// 采集会跑两次（22:45 首次、次日 01:00 校正）。若只 upsert 不删除，
// 那么**第一次采到、第二次消失**的品类/供应商会永远留在库里 ——
// 表现为"某个商品明明今天没卖，却一直出现在月报里"。
//
// → 所以子表一律「先按 (branch, date) 删干净，再整体插入」。
//
//	整日替换的代价是每天写几十~几千行，对这个量级完全可以接受；
//	而"残留"是那种零报错、事后极难定位的问题。
//
// # 事务
//
// 四张表在**同一个事务**里写：不这样做会出现"总额已更新、明细还是旧的"，
// 而月报正好会把两者放在一起算 —— 那是一个看起来完全正常的错数字。
func (s *Store) Save(ctx context.Context, snap Snapshot, source CollectSource) error {
	if s == nil || s.db == nil {
		return errors.New("salesdaily: store 未初始化")
	}
	if snap.BranchID == "" || snap.OperDate == "" {
		return fmt.Errorf("salesdaily: branch_id 与 oper_date 必填（收到 %q/%q）", snap.BranchID, snap.OperDate)
	}

	collectedAt := s.now().Format(time.RFC3339)
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		t := DailyTotals{
			BranchID: snap.BranchID, OperDate: snap.OperDate,
			NetAmount: snap.Totals.NetAmount, NetQty: snap.Totals.NetQty,
			RefCost: snap.Totals.RefCost, RefMargin: snap.Totals.RefMargin,
			RefMarginPct: snap.Totals.RefMarginPct, CostCoverage: snap.Totals.CostCoverage,
			MarginAvailable: snap.Totals.MarginAvailable,
			TotalRows:       snap.Totals.TotalRows, CostedRows: snap.Totals.CostedRows,
			Source: string(source), CollectedAt: collectedAt,
		}
		// OnConflict 覆盖而不是追加：同一 (branch, date) 两次采集只留最新一次。
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "branch_id"}, {Name: "oper_date"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"net_amount", "net_qty", "ref_cost", "ref_margin", "ref_margin_pct",
				"cost_coverage", "margin_available", "total_rows", "costed_rows",
				"source", "collected_at",
			}),
		}).Create(&t).Error; err != nil {
			return fmt.Errorf("salesdaily: 写 totals: %w", err)
		}

		// ---- 子表：整日替换 ----
		if err := tx.Where("branch_id = ? AND oper_date = ?", snap.BranchID, snap.OperDate).
			Delete(&DailyCategory{}).Error; err != nil {
			return fmt.Errorf("salesdaily: 清 categories: %w", err)
		}
		if len(snap.Categories) > 0 {
			rows := make([]DailyCategory, 0, len(snap.Categories))
			for _, c := range snap.Categories {
				// 空类目名无法构成主键，也没法在报表里展示 —— 直接跳过。
				// ⚠️ 但**不能**因此推断总额也少了这一块：
				//   总额来自独立的无分组查询，不经过这张表。
				if c.Name == "" {
					continue
				}
				rows = append(rows, DailyCategory{
					BranchID: snap.BranchID, OperDate: snap.OperDate, Name: c.Name,
					Amount: c.Amount, Qty: c.Qty, Cost: c.Cost,
					CostedRow: c.CostedRow, TotalRow: c.TotalRow,
				})
			}
			if err := tx.Create(&rows).Error; err != nil {
				return fmt.Errorf("salesdaily: 写 categories: %w", err)
			}
		}

		if err := tx.Where("branch_id = ? AND oper_date = ?", snap.BranchID, snap.OperDate).
			Delete(&DailySupplier{}).Error; err != nil {
			return fmt.Errorf("salesdaily: 清 suppliers: %w", err)
		}
		if len(snap.Suppliers) > 0 {
			rows := make([]DailySupplier, 0, len(snap.Suppliers))
			for _, u := range snap.Suppliers {
				if u.Code == "" {
					continue
				}
				top := u.TopItems
				if top == nil {
					top = []TopItemRow{}
				}
				b, err := json.Marshal(top)
				if err != nil {
					return fmt.Errorf("salesdaily: 编码供应商 %s 的商品: %w", u.Code, err)
				}
				rows = append(rows, DailySupplier{
					BranchID: snap.BranchID, OperDate: snap.OperDate, Code: u.Code,
					Name: u.Name, Amount: u.Amount, Qty: u.Qty,
					ItemCount: u.ItemCount, TopItems: string(b),
				})
			}
			if len(rows) > 0 {
				if err := tx.Create(&rows).Error; err != nil {
					return fmt.Errorf("salesdaily: 写 suppliers: %w", err)
				}
			}
		}

		if err := tx.Where("branch_id = ? AND oper_date = ?", snap.BranchID, snap.OperDate).
			Delete(&DailyProduct{}).Error; err != nil {
			return fmt.Errorf("salesdaily: 清 products: %w", err)
		}
		if len(snap.Products) > 0 {
			rows := make([]DailyProduct, 0, len(snap.Products))
			for _, p := range snap.Products {
				if p.ItemNo == "" {
					continue
				}
				rows = append(rows, DailyProduct{
					BranchID: snap.BranchID, OperDate: snap.OperDate, ItemNo: p.ItemNo,
					Name: p.Name, Category: p.Category, Amount: p.Amount, Qty: p.Qty,
				})
			}
			if len(rows) > 0 {
				if err := tx.Create(&rows).Error; err != nil {
					return fmt.Errorf("salesdaily: 写 products: %w", err)
				}
			}
		}
		return nil
	})
}

// Get 读某门店某营业日的完整快照。
//
// 不存在时返回 ErrNotFound（见其注释：与"当天零销售"是两件事）。
func (s *Store) Get(ctx context.Context, branchID, operDate string) (Snapshot, error) {
	if s == nil || s.db == nil {
		return Snapshot{}, errors.New("salesdaily: store 未初始化")
	}
	snap := Snapshot{BranchID: branchID, OperDate: operDate}

	var t DailyTotals
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND oper_date = ?", branchID, operDate).
		First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Snapshot{}, fmt.Errorf("%w: %s %s", ErrNotFound, branchID, operDate)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("salesdaily: 读 totals: %w", err)
	}
	// 只读字段（见 Snapshot 的注释）：它们是"这一行什么时候、被谁写的"。
	snap.Source = CollectSource(t.Source)
	snap.CollectedAt = t.CollectedAt
	snap.Totals = TotalsRow{
		NetAmount: t.NetAmount, NetQty: t.NetQty, RefCost: t.RefCost,
		RefMargin: t.RefMargin, RefMarginPct: t.RefMarginPct,
		CostCoverage: t.CostCoverage, MarginAvailable: t.MarginAvailable,
		TotalRows: t.TotalRows, CostedRows: t.CostedRows,
	}

	var cats []DailyCategory
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND oper_date = ?", branchID, operDate).
		Order("amount desc").Find(&cats).Error; err != nil {
		return Snapshot{}, fmt.Errorf("salesdaily: 读 categories: %w", err)
	}
	for _, c := range cats {
		snap.Categories = append(snap.Categories, CategoryRow{
			Name: c.Name, Amount: c.Amount, Qty: c.Qty, Cost: c.Cost,
			CostedRow: c.CostedRow, TotalRow: c.TotalRow,
		})
	}

	var sups []DailySupplier
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND oper_date = ?", branchID, operDate).
		Order("amount desc").Find(&sups).Error; err != nil {
		return Snapshot{}, fmt.Errorf("salesdaily: 读 suppliers: %w", err)
	}
	for _, u := range sups {
		row := SupplierRow{
			Code: u.Code, Name: u.Name, Amount: u.Amount, Qty: u.Qty,
			ItemCount: u.ItemCount,
		}
		if u.TopItems != "" {
			var items []TopItemRow
			if err := json.Unmarshal([]byte(u.TopItems), &items); err != nil {
				return Snapshot{}, fmt.Errorf("salesdaily: 解码供应商 %s 的商品: %w", u.Code, err)
			}
			row.TopItems = items
		}
		snap.Suppliers = append(snap.Suppliers, row)
	}

	var prods []DailyProduct
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND oper_date = ?", branchID, operDate).
		Order("amount desc").Find(&prods).Error; err != nil {
		return Snapshot{}, fmt.Errorf("salesdaily: 读 products: %w", err)
	}
	for _, p := range prods {
		snap.Products = append(snap.Products, ProductRow{
			ItemNo: p.ItemNo, Name: p.Name, Category: p.Category,
			Amount: p.Amount, Qty: p.Qty,
		})
	}
	return snap, nil
}

// Meta 是某天的快照元信息（不读明细，供列表/存在性判断用）。
type Meta struct {
	OperDate    string
	NetAmount   float64
	Source      CollectSource
	CollectedAt string
}

// ListMetas 列出 [from, to] 闭区间内**已有快照**的日期，按日期升序。
//
// ⚠️ 返回的日期可能**少于**区间内的自然日数 —— 采集漏掉的那天不会出现在这里。
//
//	调用方若把"缺失"当成"零销售"，月报就会凭空少掉一天而无人察觉。
//	所以本函数**不**补齐缺失日期，由调用方自己决定怎么表达缺口。
func (s *Store) ListMetas(ctx context.Context, branchID, from, to string) ([]Meta, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("salesdaily: store 未初始化")
	}
	var rows []DailyTotals
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND oper_date >= ? AND oper_date <= ?", branchID, from, to).
		Order("oper_date asc").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("salesdaily: 列区间元信息: %w", err)
	}
	out := make([]Meta, 0, len(rows))
	for _, r := range rows {
		out = append(out, Meta{
			OperDate: r.OperDate, NetAmount: r.NetAmount,
			Source: CollectSource(r.Source), CollectedAt: r.CollectedAt,
		})
	}
	return out, nil
}

// FirstCollectedDate 返回该门店**最早**有快照的营业日。
//
// # 存在的理由：区分两种完全不同的"缺数据"
//
//	① **历史还没攒够**：落库改造上线前的那些天，库里永远不会有。
//	   该做的是等，或者接受"这个月覆盖不全"。
//	② **中间漏了几天**：采集 job 某几天失败了。
//	   该做的是去查那几天的采集日志/源库连通性。
//
// 两者在 `ListMetas` 的结果里**长得一模一样**（都是"那几天没有行"），
// 而处置完全相反 —— 不区分的话，运营会去查采集配置，
// 而真正的问题只是"这个功能上周才上线"。
func (s *Store) FirstCollectedDate(ctx context.Context, branchID string) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, errors.New("salesdaily: store 未初始化")
	}
	var first sql.NullString
	if err := s.db.WithContext(ctx).
		Model(&DailyTotals{}).
		Where("branch_id = ?", branchID).
		Select("MIN(oper_date)").Scan(&first).Error; err != nil {
		return "", false, fmt.Errorf("salesdaily: 查最早采集日: %w", err)
	}
	if !first.Valid || strings.TrimSpace(first.String) == "" {
		return "", false, nil
	}
	return first.String, true, nil
}

// AggregateRange 把 [from, to] 内的**每日总额**相加。
//
// ⚠️ 缺失的日期会被静默跳过 —— 调用方必须同时用 ListMetas 数一遍天数，
//
//	否则"采集漏了一天"就会变成"月报少了一天营业额"且零报错。
//	本函数返回的 coveredDays 就是为此准备的。
type AggregateRange struct {
	Totals      TotalsRow
	CoveredDays int
	MissingDays int
}

// AggregateTotals 聚合区间内每天的大盘总额。
//
// # ⚠️ 为什么不直接返回"正确总额"了事
//
// 求和本身很简单，**难的是缺口**：区间有 30 天而只采到 28 天时，
// 这个和是 28 天的。把它当 30 天的报出去，店长会以为营业额跌了两成。
// 所以返回体里必须带 covered/missing，让调用方决定
// 是"照报 + 标注缺 N 天"还是"拒绝出数"。
func (s *Store) AggregateTotals(ctx context.Context, branchID, from, to string) (AggregateRange, error) {
	var rows []DailyTotals
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND oper_date >= ? AND oper_date <= ?", branchID, from, to).
		Order("oper_date asc").Find(&rows).Error
	if err != nil {
		return AggregateRange{}, fmt.Errorf("salesdaily: 聚合区间: %w", err)
	}

	// ⚠️ 复用 sumTotals 而不是在这里再写一遍求和：
	//
	//	LoadRange 算的是同一件事，两处各写一份迟早漂移，
	//	而漂移的表现是"按区间读出来的总额"与"逐日加起来"不一致 ——
	//	一个看起来完全正常的错数字。
	var agg TotalsRow
	if len(rows) > 0 {
		agg = sumTotals(rows)
	}

	days, err := daysBetween(from, to)
	if err != nil {
		return AggregateRange{}, err
	}
	return AggregateRange{
		Totals:      agg,
		CoveredDays: len(rows),
		MissingDays: days - len(rows),
	}, nil
}

// minCostCoverage 与 fact.MinCostCoverage 保持同一阈值。
//
// ⚠️ 这里是**独立定义**的常量（与 fact/salesagg 各有一份）：
//
//	存储层不该依赖 fact 包（那是报表组装层），而阈值一旦不一致，
//	"能不能显示毛利"就会随读取路径变化 —— 同一份数据，
//	从 PG 聚合出来的和现查 cube 得到的可能一个显示一个不显示。
const minCostCoverage = 0.5

// daysBetween 返回闭区间内的自然日数（含首尾）。
func daysBetween(from, to string) (int, error) {
	f, err := time.Parse("2006-01-02", from)
	if err != nil {
		return 0, fmt.Errorf("salesdaily: 解析 from %q: %w", from, err)
	}
	t, err := time.Parse("2006-01-02", to)
	if err != nil {
		return 0, fmt.Errorf("salesdaily: 解析 to %q: %w", to, err)
	}
	if t.Before(f) {
		return 0, fmt.Errorf("salesdaily: 区间倒置 %s > %s", from, to)
	}
	return int(t.Sub(f).Hours()/24) + 1, nil
}
