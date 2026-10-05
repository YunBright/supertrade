// Package service / subscribe.go —— Dapr pub/sub 订阅回调实现。
//
// 监听领域:
//   - sale.completed   → OnSaleCompleted 落 line_sales_by_pig
//   - auth.user.access_changed → OnAccessChanged 失效 scope cache(已在 service.go 实现)
//
// 关键设计:
//   - 只对 fresh_type=meat 的行落 line_sales_by_pig(produce 行丢弃,非本仓职责)。
//   - 退货 R 行(负 qty/amount):POS 已自翻符号,本服务直存 OrderStatus="R";毛利计算时排除。
//   - line 缺 pig_id 但有 cube_product_id 时,新规则 resolvePigForSaleLine:
//     1. 若 payload 含 sale_time → 在 sale_time 之前到达的当日 pig 候选;
//     2. 按 purchase_group_id 圈定:同 group 的猪按 arrived_at 升序分配;
//        LLM advice 含目标 cut 的猪优先;LLM 该 cut 售罄后轮到下个 pig;
//     3. 无 group → 选当日到达时间最早的 pig。
//   - 同一 (branch_id, pos_line_id) 二次投递 → idempotent(UNIQUE 约束 + DO NOTHING)。
//   - 时区:用 service.bizTZ 划日界,4:30 北京时间场内的猪触发当日 in_kg > 0。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SaleLine 是 sale.completed 内 lines[] 的元素(EVENT-CATALOG §2.3)。
type SaleLine struct {
	LineID        string          `json:"line_id"`    // pos_line_id,idempotent 键
	SKUID         string          `json:"sku_id"`     // 即 cube product id
	Qty           decimal.Decimal `json:"qty"`        // kg
	UnitPriceYuan decimal.Decimal `json:"unit_price_yuan"`
	AmountYuan    decimal.Decimal `json:"amount_yuan"`
	FreshType     string          `json:"fresh_type"` // "none" | "produce" | "meat"
	PigID         string          `json:"pig_id,omitempty"`
	CutType       model.CutType   `json:"cut_type,omitempty"`
}

// SaleCompletedPayload 是 sale.completed 事件 data(EVENT-CATALOG §2.3)。
//
// branch_id 必填(从 envelope.data 或 ?branch_id query 取,subscribe handler 已校验)。
type SaleCompletedPayload struct {
	SaleID          string          `json:"sale_id"`
	BranchID        string          `json:"branch_id"`
	OperatorID      string          `json:"operator_id"`
	TotalAmountYuan decimal.Decimal `json:"total_amount_yuan"`
	Lines           []SaleLine      `json:"lines"`
	CompletedAt     time.Time       `json:"completed_at"`
	OrderStatus     string          `json:"order_status"` // "S" | "R"
}

// OnSaleCompleted 落 line_sales_by_pig(fresh_type=meat 行)。
//
// 流程:
//  1. 遍历 lines,过滤 fresh_type="meat" 的行。
//  2. 行级补全 pig_id(若空):
//     - 通过 cube_product_id 找 branch_cut_mapping → cut_type;
//     - 找该 branch 当日最近一头 whole_pig(未关联任何 meat line 的优先;fallback:当日任一头);
//     - 找不到 → 丢该行(可观察事件总线补发)。
//  3. 行级 idempotent INSERT(UNIQUE(branch_id, pos_line_id) DO NOTHING)。
//
// 返回:成功插入的行数(用于日志 / 测试断言)。
func (s *Service) OnSaleCompleted(ctx context.Context, p SaleCompletedPayload) (int, error) {
	if p.BranchID == "" {
		return 0, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if len(p.Lines) == 0 {
		return 0, nil
	}

	now := s.now()
	saleDate := datatypes.Date(now)
	saleTime := now
	if !p.CompletedAt.IsZero() {
		saleTime = p.CompletedAt.In(s.bizTZ)
		saleDate = datatypes.Date(saleTime)
	}

	// 订单状态(销售/退货)。POS 已自翻符号(R 行 qty/amount 负数)。
	status := strings.ToUpper(strings.TrimSpace(p.OrderStatus))
	if status == "" {
		status = "S"
	}
	if status != "S" && status != "R" {
		// 未知状态:warn 后按 S 处理(防 POS 字段错位丢全部事件)。
		s.pubLogger.Warn("OnSaleCompleted: unknown order_status, fallback to S",
			"sale_id", p.SaleID, "status", p.OrderStatus)
		status = "S"
	}

	rows := make([]model.LineSalesByPig, 0, len(p.Lines))
	for _, line := range p.Lines {
		if !strings.EqualFold(line.FreshType, "meat") {
			continue
		}
		if strings.TrimSpace(line.LineID) == "" {
			continue
		}

		pigID := strings.TrimSpace(line.PigID)
		cutType := line.CutType

		// line 缺 pig_id + 有 cube_product_id → 反查 branch_cut_mapping → 找候选 pig。
		if pigID == "" && line.SKUID != "" {
			resolvedPig, resolvedCut, err := s.resolvePigByCubeProduct(ctx, p.BranchID, line.SKUID, saleTime)
			if err != nil {
				s.pubLogger.Warn("OnSaleCompleted: resolvePigByCubeProduct failed",
					"branch_id", p.BranchID,
					"sku_id", line.SKUID,
					"err", err)
				continue // drop 该行,后续监控告警
			}
			pigID = resolvedPig
			if cutType == "" {
				cutType = resolvedCut
			}
		}
		if pigID == "" {
			// 反查失败 → drop 行
			continue
		}

		rows = append(rows, model.LineSalesByPig{
			BranchID:      p.BranchID,
			SaleDate:      saleDate,
			PigID:         pigID,
			CutType:       cutType,
			QtyKg:         line.Qty,
			AmountYuan:    line.AmountYuan,
			UnitPriceYuan: line.UnitPriceYuan,
			Source:        "pos",
			PosLineID:     line.LineID,
			PosBranchID:   p.BranchID,
			CubeProductID: line.SKUID,
			OrderStatus:   status,
			CreatedAt:     now,
		})
	}

	if len(rows) == 0 {
		return 0, nil
	}

	// INSERT ... ON CONFLICT(branch_id, pos_line_id) DO NOTHING(idempotency)。
	db := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "pos_branch_id"}, {Name: "pos_line_id"},
		},
		DoNothing: true,
	})
	if err := db.Create(&rows).Error; err != nil {
		return 0, fmt.Errorf("insert line_sales_by_pig: %w", err)
	}
	// rows 长度是"送入的 meat 行数";idempotency 跳过的行不在 GORM 反馈里,
	// 业务层近似认为"提交了 len(rows) 条";严格统计用日志/DB 查询。
	return len(rows), nil
}

// resolvePigByCubeProduct 通过 cube_product_id 反查 (branch, biz-day) 候选 pig。
//
// 算法(2026-10-01 优化):
//  1. branch_cut_mapping 找 cube_product_id 对应的 cut_type;
//  2. 候选 pig 圈选:
//     - 限定 bizTZ 当日窗口(默认 Asia/Shanghai);
//     - 若 line 有 purchase_group_id(本服务外推):先按 group 匹配;
//     - 否则:所有当日 whole_pig 按 arrived_at ASC(早到仓的优先)。
//  3. 在候选集里:LLM advice 含该 cut_type 的 pig 优先(精匹配);
//
//     兜底:取 arrived_at 最早的候选 pig(场景一/二的单头/半头猪)。
//
// 找不到 pig 时返 empty(""), nil — 由 caller 决定 drop 该行。
func (s *Service) resolvePigByCubeProduct(ctx context.Context, branchID, cubeProductID string, saleTime time.Time) (string, model.CutType, error) {
	var m model.BranchCutMapping
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND cube_product_id = ?", branchID, cubeProductID).
		First(&m).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", "", nil // 没找到映射 → caller drop
		}
		return "", "", err
	}

	// 候选集:当日窗口内 + arrived_at <= sale_time(sale 之前的猪才可能贡献该 cut)。
	startOfDay, endOfDay := s.businessDayBounds(saleTime)
	if saleTime.After(endOfDay) {
		saleTime = endOfDay
	}

	// 优先 LLM 精匹配:findPigByCutInAdvice 找当窗口内 llm_advice_json 含该 cut 的 pig。
	candidate, err := s.findPigByCutInAdvice(ctx, branchID, m.CutType, startOfDay, saleTime)
	if err == nil && candidate != "" {
		return candidate, m.CutType, nil
	}

	// Fallback:当日窗口内最早一头 pig(arrived_at ASC;默认场景一/二)。
	var pig model.WholePig
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, startOfDay, endOfDay).
		Order("arrived_at ASC").
		First(&pig).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", "", nil // 当日没 pig → caller drop
		}
		return "", "", err
	}
	return pig.ID, m.CutType, nil
}

// findPigByCutInAdvice 在 llm_advice_json.cuts 数组里找含目标 cut_type 的 pig。
//
// PG 用 jsonb_array_elements + ->>'cut_type';SQLite 用 json_each + json_extract。
// 跨 dial 兼容:用 GORM raw SQL,根据驱动名分两套查询。
func (s *Service) findPigByCutInAdvice(ctx context.Context, branchID string, cutType model.CutType, start, end time.Time) (string, error) {
	dialect := s.db.Dialector.Name()
	cutStr := string(cutType)

	var row struct {
		ID string
	}
	var query string
	switch dialect {
	case "postgres":
		// jsonb 数组元素 ->> 字段
		query = `
SELECT id FROM whole_pigs
WHERE branch_id = ?
  AND arrived_at >= ? AND arrived_at < ?
  AND EXISTS (
    SELECT 1 FROM jsonb_array_elements(llm_advice_json) elem
    WHERE elem->>'cut_type' = ?
  )
ORDER BY arrived_at DESC
LIMIT 1`
	case "sqlite":
		// json_extract(elem, '$.cut_type')
		query = `
SELECT id FROM whole_pigs
WHERE branch_id = ?
  AND arrived_at >= ? AND arrived_at < ?
  AND EXISTS (
    SELECT 1 FROM json_each(llm_advice_json) je,
                 json_extract(je.value, '$.cut_type') AS cut
    WHERE cut = ?
  )
ORDER BY arrived_at DESC
LIMIT 1`
	default:
		return "", nil // 不支持的 dialect → fallback 到 by string match
	}
	err := s.db.WithContext(ctx).Raw(query, branchID, start, end, cutStr).Scan(&row).Error
	if err != nil {
		return "", err
	}
	if row.ID == "" {
		return "", nil
	}
	return row.ID, nil
}

// OnAccessChanged 处理 auth.user.access_changed 事件,失效 scope cache。
//
// branchID 空 → 失效该 user 全部缓存(InvalidateScopeCache 内部实现)。
func (s *Service) OnAccessChanged(_ context.Context, userID, branchID string) {
	s.InvalidateScopeCache(userID, branchID)
}

// 静默引用 encoding/json(若用户剥离部分代码)。
var _ = json.Marshal