package service

// internal/fresh-meat/service/pig_delete.go —— **删掉录错了的猪**。
//
// # 为什么门店需要它
//
// 早盘最常见的错：**多录一头** / **毛重打错**。
// ⇒ 没有删除 ⇒ 只能"将错就错" ⇒ 那一头会进成本池
// ⇒ 当天毛利全歪，而且**没人知道是哪头猪的锅**。
//
// # 什么时候能删
//
//	① 没有**分割补录**（pig_cuts）—— 有的话删了会留下孤儿分割行；
//	② 没有**销售归属**（line_sales_by_pigs）—— 有的话收入会归到不存在的猪；
//	③ 归属日**不在已审计结转的期间**里（冻结不可逆，见 assertNotAudited）。
//
// # 删了之后
//
//	- 营业日状态：这天再没有猪 ⇒ 退回 `no_pig`（状态机平时只进不退，删除是例外）；
//	- 成本链：重算那天的成本日快照（链式 ⇒ 后面的日期跟着变）。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"gorm.io/gorm"
)

// DeleteWholePig 删掉一头**录错了**的猪（DELETE /whole-pigs/:id）。
func (s *Service) DeleteWholePig(ctx context.Context, branchID, pigID string) error {
	if branchID == "" {
		return fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if strings.TrimSpace(pigID) == "" {
		return fmt.Errorf("%w: pig_id 必填", ErrInvalidInput)
	}
	var pig model.WholePig
	err := s.db.WithContext(ctx).
		Where("id = ? AND branch_id = ?", pigID, branchID).
		First(&pig).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("%w: 猪 %s 不存在（或不在这个门店）", ErrWholePigNotFound, pigID)
	}
	if err != nil {
		return fmt.Errorf("query whole_pig: %w", err)
	}
	// 冻结检查：已审计结转的期间不能改（含删）。
	if err := s.assertNotAudited(ctx, branchID, pig.ArrivedAt, "进猪"); err != nil {
		return err
	}
	// ① 有分割补录 ⇒ 不许删（会留下孤儿分割行）。
	var cuts int64
	if err := s.db.WithContext(ctx).Model(&model.PigCut{}).
		Where("pig_id = ?", pig.ID).Count(&cuts).Error; err != nil {
		return fmt.Errorf("count pig_cuts: %w", err)
	}
	if cuts > 0 {
		return fmt.Errorf("%w: 这头猪已经分割补录了（%d 条）—— 删了会留下对不上的分割行", ErrInvalidInput, cuts)
	}
	// ② 有销售归属 ⇒ 不许删（收入会归到不存在的猪）。
	var sales int64
	if err := s.db.WithContext(ctx).Model(&model.LineSalesByPig{}).
		Where("pig_id = ?", pig.ID).Count(&sales).Error; err != nil {
		return fmt.Errorf("count line_sales_by_pigs: %w", err)
	}
	if sales > 0 {
		return fmt.Errorf("%w: 这头猪已经产生销售归属（%d 条）—— 不能删", ErrInvalidInput, sales)
	}
	if err := s.db.WithContext(ctx).
		Delete(&model.WholePig{}, "id = ? AND branch_id = ?", pig.ID, branchID).Error; err != nil {
		return fmt.Errorf("delete whole_pig: %w", err)
	}
	// 营业日状态：这天再没有猪 ⇒ 退回 no_pig（删除是"状态只进不退"的例外）。
	if err := s.markBusinessDayPigDeleted(ctx, branchID, pig.ArrivedAt); err != nil {
		s.pubLogger.Error("DeleteWholePig: 营业日状态回退失败(状态落后,下次动作会自愈)",
			"branch_id", branchID, "pig_id", pig.ID, "err", err)
	}
	// 成本链变了 ⇒ 重算那天的成本日快照（链式 ⇒ 后面的日期跟着变）。
	if err := s.recomputeCostSnapshots(ctx, branchID, pig.ArrivedAt); err != nil {
		s.pubLogger.Error("DeleteWholePig: 结余快照重算失败",
			"branch_id", branchID, "pig_id", pig.ID, "err", err)
	}
	return nil
}

// markBusinessDayPigDeleted 删了猪之后把营业日状态退回"没进猪"。
//
// ⚠️ **状态机平时只进不退**（见 mergeBusinessDay）—— 这里是唯一的例外：
//   猪被删了，"已进猪"就不成立了 ⇒ 门店在每日作业里看到的 ① 步骤
//   必须回到"未做"（否则门店以为已经录过了）。
//
// ⚠️ 只回退**进猪**相关的字段：盘点/搬台的痕迹**留着**
//   （那些动作确实发生过）。
func (s *Service) markBusinessDayPigDeleted(ctx context.Context, branchID string, bizDate time.Time) error {
	day := bizDate.In(s.bizTZOrUTC())
	start, end := s.businessDayBounds(day)
	// 这天还有别的猪吗？⇒ 还有就不动状态（只把计数减一）。
	var remain int64
	if err := s.db.WithContext(ctx).Model(&model.WholePig{}).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, start, end).
		Count(&remain).Error; err != nil {
		return fmt.Errorf("count whole_pigs(after delete): %w", err)
	}
	updates := map[string]any{"updated_at": s.now()}
	if remain == 0 {
		updates["has_pig"] = false
		updates["pig_count"] = 0
		updates["status"] = BizDayStatusNoPig
	} else {
		updates["pig_count"] = remain
	}
	return s.db.WithContext(ctx).Model(&model.BusinessDay{}).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, start, end).
		Updates(updates).Error
}
