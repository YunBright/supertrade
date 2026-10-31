// Package service / receipts.go —— 其他收款与凭证附件（FR-05）。
//
// # 为什么创建即 confirmed
//
//	其他收款（废品变卖 / 押金退回 / 赔偿…）的录入本身是出纳的**人工确认动作**:
//	不像导入那样有"草稿待核对"的中间态。source=import 的批量导入才落 draft。
//
// # 附件存 PG bytea 的边界
//
//	单文件 ≤ CASH_RECON_ATTACHMENT_MAX_BYTES(默认 5MB)、每单 ≤ 5 个;
//	二期换对象存储时新增 storage_key 保留本表做元数据(见 model.go)。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// maxAttachmentsPerReceipt 是每单附件上限。
const maxAttachmentsPerReceipt = 5

// CreateReceiptInput 是其他收款录入的入参。
type CreateReceiptInput struct {
	BranchID string
	BizDate  time.Time
	Amount   decimal.Decimal
	Method   string
	Payer    string
	Reason   string
	Actor    string
}

// receiptMethodValid 判断收款方式是否可用于其他收款。
//
// 找零(change)与未映射(unmapped)不是收款渠道 ⇒ 拒绝,避免把"不知道"记成一种收款方式。
func receiptMethodValid(method string) bool {
	switch method {
	case model.MethodCash, model.MethodWechat, model.MethodBankCard,
		model.MethodStoredValue, model.MethodCredit:
		return true
	}
	return false
}

// CreateReceipt 录入一笔其他收款（创建即 confirmed）。
func (s *Service) CreateReceipt(ctx context.Context, in CreateReceiptInput) (model.ReconOtherReceipt, error) {
	in.BranchID = strings.TrimSpace(in.BranchID)
	in.Method = strings.TrimSpace(in.Method)
	in.Payer = strings.TrimSpace(in.Payer)
	in.Reason = strings.TrimSpace(in.Reason)
	in.Actor = strings.TrimSpace(in.Actor)
	if in.BranchID == "" || in.Actor == "" || in.BizDate.IsZero() {
		return model.ReconOtherReceipt{}, model.ErrInvalidInput
	}
	if !in.Amount.IsPositive() {
		return model.ReconOtherReceipt{}, fmt.Errorf("%w: amount 必须为正数", model.ErrInvalidInput)
	}
	if !receiptMethodValid(in.Method) {
		return model.ReconOtherReceipt{}, fmt.Errorf("%w: method 必须是 cash/wechat/bank_card/stored_value/credit", model.ErrInvalidInput)
	}
	if len(in.Payer) > 255 || len(in.Reason) > 512 {
		return model.ReconOtherReceipt{}, fmt.Errorf("%w: payer/reason 过长", model.ErrInvalidInput)
	}

	day := dayOnly(in.BizDate)
	if err := s.assertNotLocked(ctx, in.BranchID, day, day); err != nil {
		return model.ReconOtherReceipt{}, err
	}

	now := s.now()
	row := model.ReconOtherReceipt{
		ID:          newID(),
		BranchID:    in.BranchID,
		BizDate:     day,
		Amount:      in.Amount,
		Method:      in.Method,
		Payer:       in.Payer,
		Reason:      in.Reason,
		Source:      model.ReceiptSourceManual,
		Status:      model.ReceiptStatusConfirmed,
		ConfirmedBy: in.Actor,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("create recon_other_receipts: %w", err)
		}
		return s.writeAuditTx(ctx, tx, model.ReconAuditLog{
			BranchID:   in.BranchID,
			ObjectType: "other_receipt",
			ObjectID:   row.ID,
			Action:     "create",
			AfterJSON: fmt.Sprintf(`{"biz_date":%q,"amount":%q,"method":%q,"payer":%q,"source":%q}`,
				day.Format("2006-01-02"), in.Amount.String(), in.Method, in.Payer, model.ReceiptSourceManual),
			Reason:    in.Reason,
			Actor:     in.Actor,
			CreatedAt: now,
		})
	})
	if err != nil {
		return model.ReconOtherReceipt{}, err
	}
	return row, nil
}

// ListReceipts 返回区间内的其他收款（可按收款方式过滤）。
func (s *Service) ListReceipts(ctx context.Context, branchID string, from, to time.Time, method string, limit, offset int) ([]model.ReconOtherReceipt, int64, error) {
	branchID = strings.TrimSpace(branchID)
	method = strings.TrimSpace(method)
	if err := s.validateRange(branchID, from, to); err != nil {
		return nil, 0, err
	}
	if method != "" && !receiptMethodValid(method) {
		return nil, 0, fmt.Errorf("%w: 未知收款方式 %q", model.ErrInvalidInput, method)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	db := s.db.WithContext(ctx).Model(&model.ReconOtherReceipt{}).
		Where("branch_id = ? AND biz_date >= ? AND biz_date <= ?", branchID, dayOnly(from), dayOnly(to))
	if method != "" {
		db = db.Where("method = ?", method)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	rows := make([]model.ReconOtherReceipt, 0, limit)
	if err := db.Order("biz_date DESC, created_at DESC").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// GetReceipt 取单笔其他收款（必须属于该门店）。
func (s *Service) GetReceipt(ctx context.Context, branchID, id string) (model.ReconOtherReceipt, error) {
	branchID = strings.TrimSpace(branchID)
	id = strings.TrimSpace(id)
	if branchID == "" || id == "" {
		return model.ReconOtherReceipt{}, model.ErrInvalidInput
	}
	var row model.ReconOtherReceipt
	err := s.db.WithContext(ctx).Where("branch_id = ? AND id = ?", branchID, id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.ReconOtherReceipt{}, model.ErrNotFound
	}
	if err != nil {
		return model.ReconOtherReceipt{}, err
	}
	return row, nil
}

// AddAttachmentInput 是上传收款凭证的入参。
type AddAttachmentInput struct {
	BranchID    string
	ReceiptID   string
	FileName    string
	ContentType string
	Data        []byte
	Actor       string
}

// AddAttachment 给其他收款上传凭证（≤5MB、每单 ≤5 个）。
func (s *Service) AddAttachment(ctx context.Context, in AddAttachmentInput) (model.ReconAttachment, error) {
	in.BranchID = strings.TrimSpace(in.BranchID)
	in.ReceiptID = strings.TrimSpace(in.ReceiptID)
	in.FileName = strings.TrimSpace(in.FileName)
	in.ContentType = strings.TrimSpace(in.ContentType)
	if in.BranchID == "" || in.ReceiptID == "" || in.Actor == "" {
		return model.ReconAttachment{}, model.ErrInvalidInput
	}
	if len(in.FileName) > 255 || len(in.ContentType) > 64 {
		return model.ReconAttachment{}, fmt.Errorf("%w: file_name/content_type 过长", model.ErrInvalidInput)
	}
	if len(in.Data) == 0 {
		return model.ReconAttachment{}, fmt.Errorf("%w: 附件内容为空", model.ErrInvalidInput)
	}
	if int64(len(in.Data)) > s.cfg.AttachmentMaxBytes {
		return model.ReconAttachment{}, fmt.Errorf("%w: 附件超过 %d 字节", model.ErrInvalidInput, s.cfg.AttachmentMaxBytes)
	}

	receipt, err := s.GetReceipt(ctx, in.BranchID, in.ReceiptID)
	if err != nil {
		return model.ReconAttachment{}, err
	}
	// 冻结期内的收款不允许再补凭证:冻结后凭证是审核快照的一部分。
	if err := s.assertNotLocked(ctx, in.BranchID, receipt.BizDate, receipt.BizDate); err != nil {
		return model.ReconAttachment{}, err
	}

	var count int64
	if err := s.db.WithContext(ctx).Model(&model.ReconAttachment{}).
		Where("receipt_id = ?", in.ReceiptID).Count(&count).Error; err != nil {
		return model.ReconAttachment{}, err
	}
	if count >= maxAttachmentsPerReceipt {
		return model.ReconAttachment{}, fmt.Errorf("%w: 每单最多 %d 个附件", model.ErrInvalidInput, maxAttachmentsPerReceipt)
	}

	now := s.now()
	row := model.ReconAttachment{
		ID:          newID(),
		ReceiptID:   in.ReceiptID,
		FileName:    in.FileName,
		ContentType: in.ContentType,
		SizeBytes:   int64(len(in.Data)),
		Data:        in.Data,
		CreatedAt:   now,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return model.ReconAttachment{}, fmt.Errorf("create recon_attachments: %w", err)
	}
	_ = s.writeAudit(ctx, model.ReconAuditLog{
		BranchID:   in.BranchID,
		ObjectType: "other_receipt",
		ObjectID:   in.ReceiptID,
		Action:     "attach",
		AfterJSON:  fmt.Sprintf(`{"attachment_id":%q,"file_name":%q,"size_bytes":%d}`, row.ID, row.FileName, row.SizeBytes),
		Actor:      in.Actor,
		CreatedAt:  now,
	})
	return row, nil
}

// ListAttachments 返回某单的附件元数据（不含二进制）。
func (s *Service) ListAttachments(ctx context.Context, branchID, receiptID string) ([]model.ReconAttachment, error) {
	if _, err := s.GetReceipt(ctx, branchID, receiptID); err != nil {
		return nil, err
	}
	rows := make([]model.ReconAttachment, 0)
	if err := s.db.WithContext(ctx).Select(
		"id, receipt_id, file_name, content_type, size_bytes, created_at").
		Where("receipt_id = ?", receiptID).
		Order("created_at, id").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// GetAttachment 取附件（含二进制），校验所属收款属于该门店。
func (s *Service) GetAttachment(ctx context.Context, branchID, id string) (model.ReconAttachment, error) {
	branchID = strings.TrimSpace(branchID)
	id = strings.TrimSpace(id)
	if branchID == "" || id == "" {
		return model.ReconAttachment{}, model.ErrInvalidInput
	}
	var row model.ReconAttachment
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.ReconAttachment{}, model.ErrNotFound
	}
	if err != nil {
		return model.ReconAttachment{}, err
	}
	if _, err := s.GetReceipt(ctx, branchID, row.ReceiptID); err != nil {
		return model.ReconAttachment{}, err
	}
	return row, nil
}
