package service

// internal/fresh-meat/service/pig_update.go —— **修改已录入的猪**。
//
// # 为什么门店需要它（2026-11-03 owner）
//
// 「录入猪，需要可以修改已录入的猪，包括重量和价格。」
//
// 早盘最常见的错：**毛重打错**、**单价记错**。
// ⇒ 以前只能「删了重录」⇒ 但删猪被两条守卫挡着
//   （有分割补录 / 有销售归属 就删不掉）⇒ 录错一头已经卖过的猪
//   ⇒ **只能将错就错** ⇒ 那一头会带歪当天及往后的成本。
//
// ⇒ 这里给一条**改**的路：只改**事实字段**（毛重 / 采购单价），
//   其余（耳标、供应商、到场时间、分割记录）**一律不动** ——
//   那些是身份，不是可以随手改的数。
//
// # 改完触发什么（owner：「需要新增一样触发其它逻辑」）
//
//	① **成本链重算**（recomputeCostSnapshots）
//	   —— 与「录猪」「删猪」**同一条**触发路径（链式 ⇒ 后面的日期跟着变）。
//	② **分割建议重算**（仅当毛重变了）
//	   —— 毛重变了 ⇒ 体重段可能变了 ⇒ 旧的建议是按旧体重段算的 ⇒ 不重算就过期。
//	   —— 只改单价**不动建议**（建议是"切多少 kg"，与价格无关）。
//
// # 守卫
//
//	① 猪必须存在且**属于本店**（否则 ErrWholePigNotFound）。
//	② 毛重 / 单价必须 **> 0**。
//	③ 至少要改一个字段（两个都不传 ⇒ 400，避免"以为改成功了"）。
//
// ⚠️ 2026-11-01 owner：审计结转下线 ⇒ 冻结机制一并下线
//   ⇒ **不再有"截止日已冻结 ⇒ 不许改"** 这条拦截（账面链改为实时可改）。
//
// ⚠️ **已卖出过的猪也能改**：改完链会重算 ⇒ 那几天已归的毛利会跟着变
//   （这是"改事实"的正常后果，与改盘点一个道理）。
//   ⇒ 返回体里带 `recompute_from`（哪个营业日开始受影响）⇒ 前端能提示门店。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"gorm.io/gorm"
)

// UpdateWholePig 改一头**已录入**的猪的毛重 / 采购单价（PUT /whole-pigs/:id）。
func (s *Service) UpdateWholePig(ctx context.Context, branchID, pigID string,
	in model.UpdateWholePigInput, operatorID string) (*model.WholePig, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if strings.TrimSpace(pigID) == "" {
		return nil, fmt.Errorf("%w: pig_id 必填", ErrInvalidInput)
	}
	if in.GrossWeightKg == nil && in.PurchaseUnitPriceYuan == nil {
		return nil, fmt.Errorf("%w: gross_weight_kg 与 purchase_unit_price_yuan 至少要改一个", ErrInvalidInput)
	}
	if in.GrossWeightKg != nil && (in.GrossWeightKg.IsZero() || in.GrossWeightKg.IsNegative()) {
		return nil, fmt.Errorf("%w: gross_weight_kg 必须 > 0", ErrInvalidInput)
	}
	if in.PurchaseUnitPriceYuan != nil &&
		(in.PurchaseUnitPriceYuan.IsZero() || in.PurchaseUnitPriceYuan.IsNegative()) {
		return nil, fmt.Errorf("%w: purchase_unit_price_yuan 必须 > 0", ErrInvalidInput)
	}

	var pig model.WholePig
	err := s.db.WithContext(ctx).
		Where("id = ? AND branch_id = ?", pigID, branchID).
		First(&pig).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: 猪 %s 不存在（或不在这个门店）", ErrWholePigNotFound, pigID)
	}
	if err != nil {
		return nil, fmt.Errorf("query whole_pig: %w", err)
	}

	updates := map[string]any{"updated_at": s.now()}
	weightChanged := false
	if in.GrossWeightKg != nil && !in.GrossWeightKg.Equal(pig.GrossWeightKg) {
		pig.GrossWeightKg = *in.GrossWeightKg
		updates["gross_weight_kg"] = pig.GrossWeightKg
		weightChanged = true
	}
	if in.PurchaseUnitPriceYuan != nil && !in.PurchaseUnitPriceYuan.Equal(pig.PurchaseUnitPriceYuan) {
		pig.PurchaseUnitPriceYuan = *in.PurchaseUnitPriceYuan
		updates["purchase_unit_price_yuan"] = pig.PurchaseUnitPriceYuan
	}
	// 总价 = 毛重 × 单价 ⇒ **两个任一变了都要重算**（不是只改单价时才算）。
	cost := pig.GrossWeightKg.Mul(pig.PurchaseUnitPriceYuan).Round(2)
	pig.PurchaseCostYuan = cost
	updates["purchase_cost_yuan"] = cost

	if err := s.db.WithContext(ctx).
		Model(&model.WholePig{}).
		Where("id = ? AND branch_id = ?", pig.ID, branchID).
		Updates(updates).Error; err != nil {
		return nil, fmt.Errorf("update whole_pig: %w", err)
	}
	s.pubLogger.Info("UpdateWholePig: 已修改猪的重量/单价",
		"branch_id", branchID, "pig_id", pig.ID, "operator_id", operatorID,
		"gross_weight_kg", pig.GrossWeightKg.String(),
		"purchase_unit_price_yuan", pig.PurchaseUnitPriceYuan.String())

	// ---- 成本链重算（与录猪 / 删猪同一条路径）----
	//
	// ⚠️ 失败**不阻断修改**：改猪是"事实录入"，成功就成功；
	//	派生数据没跟上只影响"看数字"，下次任何写动作都会自愈。
	if err := s.recomputeCostSnapshots(ctx, branchID, pig.ArrivedAt); err != nil {
		s.pubLogger.Error("UpdateWholePig: 成本链重算失败(派生数据会旧,下次写动作会自愈)",
			"branch_id", branchID, "pig_id", pig.ID, "err", err)
	}

	// ---- 分割建议重算（仅当毛重变了）----
	//
	// ⚠️ 「只用基线」的猪**不调 LLM**（门店自己选的）⇒ 建议由基线在成本链里现算。
	if weightChanged && pig.AdviceMode != model.AdviceModeBaseline {
		s.kickAdviceGeneration(ctx, pig.ID, branchID)
	}
	return &pig, nil
}
