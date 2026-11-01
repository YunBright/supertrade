package service

// internal/fresh-meat/service/presale_units.go —— **预期售价读取**(账面链遗留只读)。
//
// # 为什么要留这一小块
//
// 2026-11-01 owner：预期毛利(预售结转)端点(`/stock/presale`)整条退役，
// 但**账面期末重估**必须继续按「该营业日的预期售价」把结余重估一次
// (`cut_cost.go` 的重估分支)—— 否则结余会被压回成本口径，账面毛利失真。
//
// ⇒ 这里只保留**读**的那一半：从历史 `cut_presale_settlements` 取该营业日 per-SKU 的预期售价。
//   没有预售结转的日子 ⇒ 空 map ⇒ 期末仍按成本口径(行为不变)。
//
// # 边界
//
// 只读历史表，**不写**。预售结转的写入路径(`settlePresale` / `writePigAllocations`)
// 已随 `presale.go` 一起删除 ⇒ 表 `cut_presale_settlements` 之后只会是历史存量。

import (
	"context"
	"fmt"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
)

// loadPresaleUnits 取某营业日的**预期销售单价**(per-SKU)。
//
// 用途:毛利引擎算该营业日的期末估值时,用它把结余**重估**到预期售价口径。
//
// ⚠️ 没有预售结转的日子 ⇒ 空 map ⇒ 期末仍按成本口径(行为不变)。
func (s *Service) loadPresaleUnits(ctx context.Context, branchID string, bizDate time.Time) (map[string]decimal.Decimal, error) {
	start, end := s.businessDayBounds(bizDate)
	var rows []model.CutPresale
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, start, end).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query cut_presale_settlements: %w", err)
	}
	out := map[string]decimal.Decimal{}
	for _, r := range rows {
		out[r.CubeProductID] = r.ExpectedUnitYuan
	}
	return out, nil
}
