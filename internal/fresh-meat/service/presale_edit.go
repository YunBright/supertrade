package service

// internal/fresh-meat/service/presale_edit.go —— **预售单价的员工修改**。
//
// # 为什么审计后还能改(2026-10-24 门店口径)
//
// 「取不到也按 0 生成，会经过一轮人工修改，门店审计人负责。」
//
// ⇒ 审计时**取不到售价**的货号会按 **0 元**结转(负毛利照做)⇒
//   门店审计人随后把这些货号的**预期售价**改成真实价 ⇒
//   本期毛利、下一期期初单价、按猪分摊都要跟着重算。
//
// # 与"冻结"的关系(关键)
//
// 门店口径:「审计截止日期和之前的都不能再修改(包括补录)」⇒ 冻结的是**源数据**
// (盘点 / 搬台 / 大宗 / 进猪)与**成本口径快照**。
//
// 而**预售价**是「审计结转」这个**产物**的���一项 —— 它本来就是审计人拍板的
// 「这批货打算按什么价卖掉」⇒ 改它是**修正审计结论**,不是改源数据。
//
// ⇒ 因此:
//
//	sku_cost_daily（冻结的成本口径快照）**不动**;
//	cut_presale_settlements（预售价）**可改**,但每次改都留痕;
//	改完 ⇒ 重算毛利 / 重算按猪分摊 / 重放截止日之后的天 / 校准当前余额。
//
// # 幂等
//
// 同一个价改两次 = 改一次(金额与影响完全一致)⇒ 可安全重试。
// 痕迹表会记两条(旧价相同)⇒ 审计轨迹里看得到"改过两次"。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/gorm/clause"
)


// UpdatePresaleItem 一条要改的预售价。
type UpdatePresaleItem struct {
	// CubeProductID 是要改的货号(= 思迅 item_no)。
	CubeProductID string `json:"cube_product_id" binding:"required"`
	// ExpectedUnitYuan 是**新的预期销售单价**(元/kg)。
	//
	// ⚠️ 允许 0(负毛利照做:门店明确"负毛利也要将本期归属算干净")。
	ExpectedUnitYuan decimal.Decimal `json:"expected_unit_yuan"`
}

// UpdatePresaleInput PUT /stock/presale 入参。
type UpdatePresaleInput struct {
	// BizDate 是**已审计**的结转日("YYYY-MM-DD")。
	BizDate string `json:"biz_date" binding:"required"`
	// Items 是要改的单价(可多行 ⇒ 一次改完)。
	Items []UpdatePresaleItem `json:"items" binding:"required"`
	// Note 是修改说明(留痕用)。
	Note string `json:"note,omitempty"`
}

// UpdatePresale 改**已审计**那一天的预售单价。
//
// 返回改完之后的完整预售视图(按部位 / 按 SKU / 按猪)。
func (s *Service) UpdatePresale(ctx context.Context, branchID string, in UpdatePresaleInput, operatorID string) (*PresaleView, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if operatorID == "" {
		return nil, fmt.Errorf("%w: operator_id 必填", ErrInvalidInput)
	}
	loc := s.bizTZOrUTC()
	cutoff, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(in.BizDate), loc)
	if err != nil {
		return nil, fmt.Errorf("%w: biz_date 必须是 YYYY-MM-DD(收到 %q)", ErrInvalidInput, in.BizDate)
	}
	if len(in.Items) == 0 {
		return nil, fmt.Errorf("%w: items 不能为空", ErrInvalidInput)
	}
	// ⚠️ 只能改**已结转**的那一天(最近一次审计的截止日):
	//   没有审计过 ⇒ 根本没有预售结转行可改。
	lastCutoff, err := s.auditCutoff(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if lastCutoff == "" {
		return nil, fmt.Errorf("%w: %s 还没有审计结转,没有可改的预售结转", ErrInvalidInput, in.BizDate)
	}
	if in.BizDate != lastCutoff {
		return nil, fmt.Errorf("%w: 只能改最近一次审计结转(%s)的预售价,收到 %s",
			ErrInvalidInput, lastCutoff, in.BizDate)
	}

	dayStart, dayEnd := s.businessDayBounds(cutoff)
	var rows []model.CutPresale
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, dayStart, dayEnd).
		Order("cube_product_id ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query cut_presale_settlements(update): %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: %s 没有预售结转行(该营业日可能没有结余)", ErrInvalidInput, in.BizDate)
	}
	bySku := map[string]model.CutPresale{}
	for _, r := range rows {
		bySku[r.CubeProductID] = r
	}

	now := s.now()
	bizDate := datatypesDate(cutoff.In(loc))
	for _, item := range in.Items {
		sku := strings.TrimSpace(item.CubeProductID)
		if sku == "" {
			return nil, fmt.Errorf("%w: items[].cube_product_id 必填", ErrInvalidInput)
		}
		if item.ExpectedUnitYuan.IsNegative() {
			return nil, fmt.Errorf("%w: %s 的预期单价不能为负(收到 %s)", ErrInvalidInput, sku, item.ExpectedUnitYuan)
		}
		cur, ok := bySku[sku]
		if !ok {
			return nil, fmt.Errorf("%w: %s 在 %s 没有预售结转行(改不了)", ErrInvalidInput, sku, in.BizDate)
		}
		oldUnit := cur.ExpectedUnitYuan
		newUnit := item.ExpectedUnitYuan
		newRevenue := cur.QtyKg.Mul(newUnit)
		newMargin := newRevenue.Sub(cur.CostAmountYuan)
		if err := s.db.WithContext(ctx).Model(&model.CutPresale{}).
			Where("branch_id = ? AND cube_product_id = ? AND biz_date >= ? AND biz_date < ?",
				branchID, sku, dayStart, dayEnd).
			Updates(map[string]any{
				"expected_unit_yuan":    newUnit,
				"expected_revenue_yuan": newRevenue,
				"margin_yuan":           newMargin,
				"price_source":          PresalePriceSourceManual,
				"note":                  strings.TrimSpace(in.Note),
				"updated_at":            now,
			}).Error; err != nil {
			return nil, fmt.Errorf("update cut_presale_settlements %s: %w", sku, err)
		}
		// 留痕(append-only):旧价 → 新价、谁改的、为什么。
		edit := &model.CutPresaleEdit{
			ID:            CutPresaleEditID(branchID, in.BizDate, sku, now),
			BranchID:      branchID,
			BizDate:       bizDate,
			CubeProductID: sku,
			OldUnitYuan:   oldUnit,
			NewUnitYuan:   newUnit,
			OperatorID:    operatorID,
			Note:          strings.TrimSpace(in.Note),
			CreatedAt:     now,
		}
		if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.Assignments(map[string]any{
				"old_unit_yuan": edit.OldUnitYuan,
				"new_unit_yuan": edit.NewUnitYuan,
				"operator_id":   edit.OperatorID,
				"note":          edit.Note,
			}),
		}).Create(edit).Error; err != nil {
			return nil, fmt.Errorf("insert cut_presale_edits %s: %w", edit.ID, err)
		}
	}

	// 重新归猪(金额变了)+ 重放截止日之后的天 + 校准当前余额。
	plan, err := s.settledPresalePlan(ctx, branchID, cutoff)
	if err != nil {
		return nil, err
	}
	if err := s.writePigAllocations(ctx, branchID, cutoff, plan); err != nil {
		return nil, err
	}
	// ⚠️ 改完预售价 ⇒ 截止日的**结转金额**变了(预期售价口径)
	//   ⇒ 重落那天的成本日快照 ⇒ 下一期的期初单价跟着改
	//   (门店口径:「改完要重算:本期毛利 / 按猪分摊 / 下一期期初单价 / 当前余额」)。
	if _, err := s.persistCutCostDaily(ctx, branchID, cutoff, false); err != nil {
		return nil, fmt.Errorf("改价后重落成本快照: %w", err)
	}
	if _, err := s.replayAfterAudit(ctx, branchID, cutoff); err != nil {
		s.pubLogger.Error("修改预售价后重放失败",
			"branch_id", branchID, "biz_date", in.BizDate, "err", err)
	}
	return s.ListPresale(ctx, branchID, cutoff)
}

// settledPresalePlan 从**已落库的**预售行重建一份 plan(改价后重算用)。
func (s *Service) settledPresalePlan(ctx context.Context, branchID string, cutoff time.Time) (*PresaleView, error) {
	dayStart, dayEnd := s.businessDayBounds(cutoff)
	var rows []model.CutPresale
	if err := s.db.WithContext(ctx).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, dayStart, dayEnd).
		Order("cube_product_id ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query cut_presale_settlements(plan): %w", err)
	}
	plan := &PresaleView{
		BranchID: branchID,
		BizDate:  cutoff.In(s.bizTZOrUTC()).Format("2006-01-02"),
		Rows:     []PresaleCutRow{},
		Note:     PresaleNote,
	}
	byCut := map[model.CutType][]PresaleSkuRow{}
	order := []model.CutType{}
	for _, r := range rows {
		skuRow := PresaleSkuRow{
			CubeProductID:      r.CubeProductID,
			CutType:            r.CutType,
			QtyKg:              r.QtyKg,
			CostUnitYuan:       r.CostUnitYuan,
			CostAmountYuan:     r.CostAmountYuan,
			ExpectedUnitYuan:   r.ExpectedUnitYuan,
			ExpectedRevenueYuan: r.ExpectedRevenueYuan,
			MarginYuan:         r.MarginYuan,
			PriceSource:        PresalePriceSourceFromWire(r.PriceSource),
		}
		if _, ok := byCut[r.CutType]; !ok {
			order = append(order, r.CutType)
		}
		byCut[r.CutType] = append(byCut[r.CutType], skuRow)
		plan.TotalQtyKg = plan.TotalQtyKg.Add(r.QtyKg)
		plan.TotalCostAmountYuan = plan.TotalCostAmountYuan.Add(r.CostAmountYuan)
		plan.TotalExpectedRevenueYuan = plan.TotalExpectedRevenueYuan.Add(r.ExpectedRevenueYuan)
		plan.TotalMarginYuan = plan.TotalMarginYuan.Add(r.MarginYuan)
	}
	for _, cut := range order {
		skus := byCut[cut]
		agg := PresaleCutRow{
			CutType:  cut,
			CutLabel: model.CutLabel(cut),
			Skus:     skus,
		}
		for _, r := range skus {
			agg.QtyKg = agg.QtyKg.Add(r.QtyKg)
			agg.CostAmountYuan = agg.CostAmountYuan.Add(r.CostAmountYuan)
			agg.ExpectedRevenueYuan = agg.ExpectedRevenueYuan.Add(r.ExpectedRevenueYuan)
			agg.MarginYuan = agg.MarginYuan.Add(r.MarginYuan)
		}
		if agg.QtyKg.IsPositive() {
			agg.ExpectedUnitYuan = agg.ExpectedRevenueYuan.Div(agg.QtyKg)
		}
		plan.Rows = append(plan.Rows, agg)
	}
	return plan, nil
}

// PresalePriceSourceFromWire wire 值 → 枚举(未知值回退 "missing")。
func PresalePriceSourceFromWire(wire string) string {
	switch wire {
	case PresalePriceSourcePromo:
		return PresalePriceSourcePromo
	case PresalePriceSourceRetail:
		return PresalePriceSourceRetail
	case PresalePriceSourceZero:
		return PresalePriceSourceZero
	case PresalePriceSourceMissing:
		return PresalePriceSourceMissing
	case PresalePriceSourceManual:
		return PresalePriceSourceManual
	default:
		return PresalePriceSourceMissing
	}
}
