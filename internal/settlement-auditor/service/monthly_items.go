// Package service / monthly_items.go —— 企微审批月度总表（计划 §9）。
//
// # 写点（单一写方，全部在本服务）
//
//	storeItems           落明细的**同一事务**里写月度行（带解析出的 year_month）
//	OnStatusChanged      同步 sp_status / instance_status / pay_state / decided_at
//	EnsureMonthlyBackfill 某门店首次读取时回填全部历史（只跑一次，按版本号）
//	RebuildMonthlyItems  改正则/解析口径后按当前数据全量重建
//
// # 读点
//
//	countHistoricalMonthlyItems 改为查本表（索引 branch_id + item_type + year_month），
//	不再实时 JOIN approval_items + approval_instances。
//
// # 解析不出年月的行仍然参与判据
//
// `parse_status=unmatched` 的行**不计数但会让审核拒绝**（fail-closed）：
// 静默跳过等于把可能重复的付款从统计里抹掉。
package service

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 备注里的「所属年月」写法（依次尝试）。
//
//	2026年9月 / 2026-09 / 2026/9 / 9月
//
// ⚠️ 这里只解析通用写法；门店配置的正则（audit_regex_patterns）仍用于
// **本单**判定，历史行以本表的 year_month 为准（见 rebuild 的说明）。
var yearMonthPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(\d{4})\s*年\s*(\d{1,2})\s*月`),
	regexp.MustCompile(`(\d{4})\s*[-/]\s*(\d{1,2})`),
	regexp.MustCompile(`(\d{1,2})\s*月`),
}

// parseYearMonth 从备注解析所属年月，返回 yyyy-mm；解析失败 ⇒ ok=false。
//
// 没写年份 ⇒ 取默认年份（该审批单的创建年份），与既有口径一致：
// 9 月的水电费一般在 10 月付，按审批单年份补足才不会落到错误窗口。
func parseYearMonth(remark string, defaultYear int) (string, bool) {
	text := strings.TrimSpace(remark)
	for i, re := range yearMonthPatterns {
		m := re.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		if i == 0 || i == 1 {
			y, err1 := strconv.Atoi(m[1])
			mo, err2 := strconv.Atoi(m[2])
			if err1 != nil || err2 != nil || mo < 1 || mo > 12 {
				continue
			}
			return fmt.Sprintf("%04d-%02d", y, mo), true
		}
		mo, err := strconv.Atoi(m[1])
		if err != nil || mo < 1 || mo > 12 {
			continue
		}
		if defaultYear <= 0 {
			continue
		}
		return fmt.Sprintf("%04d-%02d", defaultYear, mo), true
	}
	return "", false
}

// monthlyPayState 把 sp_status 映射成付款状态。
//
//	2 审批通过（办理人已处理、企微流程结束）/ 10 已支付 ⇒ paid
//	1 审批中 / 3 驳回 / 4 撤销 / 6 通过后撤销 ⇒ unpaid
//	其它（0 未提交）⇒ 空（保持"未知"，不冒充 unpaid）
//
// ⚠️ 付款审批是「审批人 → 办理人」两段，办理人处理完本单付款后企微才把
// 整单置为 sp_status=2（企微不保证另有 10 的「已支付」回调）。所以 2 就是
// **已付款**：资金对账同步后默认「确认匹配」。把 2 当未付款会让所有已
// 完成终审的付款单永久显示未付款。
func monthlyPayState(spStatus int) string {
	switch spStatus {
	case SpStatusApproved, SpStatusPaid:
		return model.PayStatePaid
	case SpStatusApproving, SpStatusRejected, SpStatusCancelled, SpStatusCancelAfterOK:
		return model.PayStateUnpaid
	default:
		return ""
	}
}

// monthlyItemOf 把一条明细投影成月度行。
func monthlyItemOf(it model.ApprovalItem, instCreated time.Time, instanceStatus string,
	spStatus int, spNo string, now time.Time,
) model.ApprovalMonthlyItem {
	created := instCreated
	if created.IsZero() {
		created = it.CreatedAt
	}
	yearMonth, ok := parseYearMonth(it.Remark, created.Year())
	parseStatus := model.MonthlyParseUnmatched
	if ok {
		parseStatus = model.MonthlyParseOK
	}
	return model.ApprovalMonthlyItem{
		ID:             uuid.NewString(),
		BranchID:       it.BranchID,
		ApprovalID:     it.ApprovalID,
		ItemID:         it.ID,
		Direction:      it.Direction,
		ItemType:       it.ItemType,
		Remark:         it.Remark,
		AmountYuan:     it.AmountYuan,
		YearMonth:      yearMonth,
		ParseStatus:    parseStatus,
		SpStatus:       spStatus,
		InstanceStatus: instanceStatus,
		PayState:       monthlyPayState(spStatus),
		SpNo:           spNo,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

// writeMonthlyItemsTx 在落明细的同一事务里写月度行。
//
// 已存在（重跑/重发）⇒ 按 (branch,approval,item) 覆盖派生字段，不新增行。
func writeMonthlyItemsTx(tx *gorm.DB, ap *model.ApprovalInstance, items []model.ApprovalItem, now time.Time) error {
	if len(items) == 0 {
		return nil
	}
	rows := make([]model.ApprovalMonthlyItem, 0, len(items))
	for _, it := range items {
		rows = append(rows, monthlyItemOf(it, ap.CreatedAt, ap.Status, ap.SpStatus, ap.SpNo, now))
	}
	err := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "branch_id"}, {Name: "approval_id"}, {Name: "item_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"direction", "item_type", "remark", "amount_yuan",
			"year_month", "parse_status", "sp_status", "instance_status",
			"pay_state", "sp_no", "updated_at",
		}),
	}).Create(&rows).Error
	if err != nil {
		return fmt.Errorf("write approval_monthly_items: %w", err)
	}
	return nil
}

// syncMonthlyStatusTx 把审批状态变更同步到该审批的全部月度行。
func syncMonthlyStatusTx(tx *gorm.DB, approvalID string, spStatus int, instanceStatus string,
	decidedAt *time.Time, now time.Time,
) error {
	updates := map[string]any{
		"sp_status":       spStatus,
		"instance_status": instanceStatus,
		"pay_state":       monthlyPayState(spStatus),
		"updated_at":      now,
	}
	if decidedAt != nil {
		updates["decided_at"] = *decidedAt
	}
	return tx.Model(&model.ApprovalMonthlyItem{}).
		Where("approval_id = ?", approvalID).
		Updates(updates).Error
}

// decidedAtOf 返回需要写入 decided_at 的时刻（仅企微终态）。
func decidedAtOf(spStatus int, now time.Time) *time.Time {
	switch spStatus {
	case SpStatusApproved, SpStatusRejected, SpStatusCancelled, SpStatusCancelAfterOK:
		t := now
		return &t
	default:
		return nil
	}
}

// EnsureMonthlyBackfill 保证该门店的历史明细已回填进月度总表（幂等、只跑一次）。
//
// 用 branch 级标记 + 版本号：解析口径升级时把 MonthlyBackfillVersion +1，
// 门店下一次读取会重新回填。回填本身是 upsert，重复执行不会产生重复行。
func (s *Service) EnsureMonthlyBackfill(ctx context.Context, branchID string) error {
	if branchID == "" {
		return fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	var marker model.ApprovalMonthlyBackfill
	err := s.db.WithContext(ctx).First(&marker, "branch_id = ?", branchID).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return fmt.Errorf("read monthly backfill marker: %w", err)
	}
	if err == nil && marker.Version == model.MonthlyBackfillVersion {
		return nil
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := s.now()
		claim := model.ApprovalMonthlyBackfill{
			BranchID: branchID, Version: model.MonthlyBackfillVersion, DoneAt: now,
		}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&claim)
		if res.Error != nil {
			return fmt.Errorf("claim monthly backfill: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			// 并发者已持标记：若版本已是当前，说明它已完成（或正在完成）回填。
			var cur model.ApprovalMonthlyBackfill
			if err := tx.First(&cur, "branch_id = ?", branchID).Error; err != nil {
				return err
			}
			if cur.Version == model.MonthlyBackfillVersion {
				return nil
			}
			if err := tx.Model(&model.ApprovalMonthlyBackfill{}).
				Where("branch_id = ?", branchID).
				Updates(map[string]any{"version": model.MonthlyBackfillVersion, "done_at": now}).Error; err != nil {
				return err
			}
		}
		return backfillMonthlyTx(tx, branchID, now)
	})
}

// backfillMonthlyTx 按页把某门店的历史明细回填进月度总表。
//
// 每页按 approval_id 批量取实例（避免逐行查询造成 N+1），
// 写入用 ON CONFLICT DO UPDATE ⇒ 重跑安全。
//
// ⚠️ 分页用 offset 而不是 `id > lastID`：approval_items 的 id 由写入方生成，
// 历史/夹具里可能存在空 id 或重复 id —— 游标分页在那种数据上会**原地打转**
// （lastID 不前进 ⇒ 永远查同一页），直接把审核流水线挂死。
func backfillMonthlyTx(tx *gorm.DB, branchID string, now time.Time) error {
	const pageSize = 200
	for offset := 0; ; offset += pageSize {
		var items []model.ApprovalItem
		if err := tx.Where("branch_id = ?", branchID).
			Order("created_at ASC").Offset(offset).Limit(pageSize).
			Find(&items).Error; err != nil {
			return fmt.Errorf("scan approval_items: %w", err)
		}
		if len(items) == 0 {
			return nil
		}

		ids := make([]string, 0, len(items))
		seen := map[string]bool{}
		for _, it := range items {
			if !seen[it.ApprovalID] {
				seen[it.ApprovalID] = true
				ids = append(ids, it.ApprovalID)
			}
		}
		var insts []model.ApprovalInstance
		if err := tx.Where("id IN ?", ids).Find(&insts).Error; err != nil {
			return fmt.Errorf("load approval instances: %w", err)
		}
		byID := make(map[string]model.ApprovalInstance, len(insts))
		for _, in := range insts {
			byID[in.ID] = in
		}

		rows := make([]model.ApprovalMonthlyItem, 0, len(items))
		for _, it := range items {
			in := byID[it.ApprovalID]
			rows = append(rows, monthlyItemOf(it, in.CreatedAt, in.Status, in.SpStatus, in.SpNo, now))
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "branch_id"}, {Name: "approval_id"}, {Name: "item_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"direction", "item_type", "remark", "amount_yuan",
				"year_month", "parse_status", "sp_status", "instance_status",
				"pay_state", "sp_no", "updated_at",
			}),
		}).Create(&rows).Error; err != nil {
			return fmt.Errorf("backfill approval_monthly_items: %w", err)
		}
	}
}

// RebuildMonthlyItems 按当前解析口径全量重建某门店的 year_month / parse_status。
//
// 供「改正则/补写法」后使用：只重算派生列，不动审批状态。
// 返回重算行数。
func (s *Service) RebuildMonthlyItems(ctx context.Context, branchID string) (int64, error) {
	if branchID == "" {
		return 0, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if err := s.EnsureMonthlyBackfill(ctx, branchID); err != nil {
		return 0, err
	}
	const pageSize = 500
	var total int64
	for offset := 0; ; offset += pageSize {
		var rows []model.ApprovalMonthlyItem
		if err := s.db.WithContext(ctx).Where("branch_id = ?", branchID).
			Order("created_at ASC").Offset(offset).Limit(pageSize).
			Find(&rows).Error; err != nil {
			return total, fmt.Errorf("scan approval_monthly_items: %w", err)
		}
		if len(rows) == 0 {
			return total, nil
		}

		// 默认年份取审批单创建年份：月度行不存创建时间，按 approval_id 批量补。
		ids := make([]string, 0, len(rows))
		seen := map[string]bool{}
		for _, r := range rows {
			if !seen[r.ApprovalID] {
				seen[r.ApprovalID] = true
				ids = append(ids, r.ApprovalID)
			}
		}
		var insts []model.ApprovalInstance
		if err := s.db.WithContext(ctx).Where("id IN ?", ids).Find(&insts).Error; err != nil {
			return total, fmt.Errorf("load approval instances: %w", err)
		}
		yearOf := make(map[string]int, len(insts))
		for _, in := range insts {
			yearOf[in.ID] = in.CreatedAt.Year()
		}

		now := s.now()
		for _, r := range rows {
			defaultYear := yearOf[r.ApprovalID]
			if defaultYear <= 0 {
				defaultYear = r.CreatedAt.Year()
			}
			yearMonth, ok := parseYearMonth(r.Remark, defaultYear)
			parseStatus := model.MonthlyParseUnmatched
			if ok {
				parseStatus = model.MonthlyParseOK
			}
			if err := s.db.WithContext(ctx).Model(&model.ApprovalMonthlyItem{}).
				Where("id = ?", r.ID).
				Updates(map[string]any{
					"year_month": yearMonth, "parse_status": parseStatus, "updated_at": now,
				}).Error; err != nil {
				return total, fmt.Errorf("rebuild monthly item %s: %w", r.ID, err)
			}
			total++
		}
	}
}
