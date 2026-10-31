// Package service / payments.go —— 付款登记与核销（FR-07，计划 §11）。
//
// # 数据来源
//
//	审批侧: settlement-auditor 的月度总表(direction=payment 且 pay_state=paid)
//	        ⇒ 懒同步/懒补齐成本域 recon_payment_records(source=wecom_approval)
//	手工侧: 出纳直接登记(source=manual)
//
// # 核销为什么不删银行流水
//
//	核销只是"这笔付款与哪条到账流水对上了"的**关联事实**;
//	删流水等于把银行事实抹掉(红线:银行流水导入即事实)。
//	因此核销只写 verified/bank_flow_id,原始流水行保持不动。
//
// # 跨域只读
//
//	读 approval_monthly_items 用 model.CrossMonthlyItem(显式 TableName),
//	**永不写**该表(见 model/crossdomain.go)。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// 跨域常量（字面值与 settlement-auditor 的约定一致，但**不 import** 其包）。
const (
	// crossDirectionPayment 是 approval_items.direction 的付款项取值。
	crossDirectionPayment = "payment"
	// crossPayStatePaid / Unpaid 是 approval_monthly_items.pay_state 的取值。
	crossPayStatePaid   = "paid"
	crossPayStateUnpaid = "unpaid"
)

// crossExcludedInstanceStatuses 是"没有付过款 / 不应计入应付"的审批终态。
//
// human_rejected 与 cancelled 一样是终态：不会再付款，不能留在应付台账里。
var crossExcludedInstanceStatuses = []string{"rejected", "failed", "cancelled", "human_rejected"}

// verifyAmountTolerance 是核销允许的金额差（核定口径：≤ 0.01 元）。
var verifyAmountTolerance = decimal.NewFromFloat(0.01)

// paymentChannelAliases 是核销时的渠道等价关系（付款方式 ↔ 银行流水渠道）。
//
// 银行流水 channel 是导入文本（"bank"/"wechat"/"CMB"…），付款登记 method 是
// 标准枚举 ⇒ 只对**语义等价**的几组放行，其余要求字面一致。
var paymentChannelAliases = map[string][]string{
	model.MethodBankCard: {"bank", "bank_card", "transfer"},
	model.MethodWechat:   {"wechat", "wxpay", "wechat_pay"},
	model.MethodCash:     {"cash"},
}

// SyncPaymentsResult 是懒同步的统计。
type SyncPaymentsResult struct {
	Scanned  int `json:"scanned"`
	Inserted int `json:"inserted"`
}

// SyncPayments 把已支付的审批项同步到付款台账（幂等）。
//
// 只新增缺失行：已存在的记录保持 verified/supplier 等人工字段不变，
// 不会被同步覆盖（审批侧金额变更也不覆盖已登记台账）。
func (s *Service) SyncPayments(ctx context.Context, branchID string) (SyncPaymentsResult, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return SyncPaymentsResult{}, model.ErrInvalidInput
	}
	var items []model.CrossMonthlyItem
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND direction = ? AND pay_state = ? AND instance_status NOT IN ?",
			branchID, crossDirectionPayment, crossPayStatePaid, crossExcludedInstanceStatuses).
		Order("created_at, id").
		Find(&items).Error
	if err != nil {
		return SyncPaymentsResult{}, fmt.Errorf("read approval_monthly_items: %w", err)
	}

	now := s.now()
	res := SyncPaymentsResult{Scanned: len(items)}
	for _, it := range items {
		row := model.ReconPaymentRecord{
			ID:           newID(),
			BranchID:     branchID,
			Source:       model.PaymentSourceWecom,
			SourceID:     it.ItemID,
			SupplierName: strings.TrimSpace(it.ItemType),
			Amount:       it.AmountYuan,
			RequestDate:  dayPtr(it.CreatedAt),
			PayDate:      dateOfPtr(it.DecidedAt),
			Note:         truncate(it.Remark, 512),
			Verified:     false,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		tx := s.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "branch_id"}, {Name: "source"}, {Name: "source_id"}},
			DoNothing: true,
		}).Create(&row)
		if tx.Error != nil {
			return res, fmt.Errorf("upsert recon_payment_records: %w", tx.Error)
		}
		res.Inserted += int(tx.RowsAffected)
	}
	return res, nil
}

// dayPtr 取日期指针（UTC 零点）。
func dayPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	d := dayOnly(t)
	return &d
}

// dateOfPtr 把可空时间转成日期指针。
func dateOfPtr(t *time.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	d := dayOnly(*t)
	return &d
}

// RegisterPaymentInput 是手工付款登记的入参。
type RegisterPaymentInput struct {
	BranchID     string
	SupplierName string
	Amount       decimal.Decimal
	RequestDate  *time.Time
	PayDate      *time.Time
	Method       string
	Note         string
	Actor        string
}

// RegisterPayment 手工登记一笔付款（source=manual）。
func (s *Service) RegisterPayment(ctx context.Context, in RegisterPaymentInput) (model.ReconPaymentRecord, error) {
	in.BranchID = strings.TrimSpace(in.BranchID)
	in.SupplierName = strings.TrimSpace(in.SupplierName)
	in.Method = strings.TrimSpace(in.Method)
	in.Note = strings.TrimSpace(in.Note)
	in.Actor = strings.TrimSpace(in.Actor)
	if in.BranchID == "" || in.Actor == "" {
		return model.ReconPaymentRecord{}, model.ErrInvalidInput
	}
	if !in.Amount.IsPositive() {
		return model.ReconPaymentRecord{}, fmt.Errorf("%w: amount 必须为正数", model.ErrInvalidInput)
	}
	if in.Method != "" && !paymentMethodValid(in.Method) {
		return model.ReconPaymentRecord{}, fmt.Errorf("%w: 未知付款方式 %q", model.ErrInvalidInput, in.Method)
	}
	if len(in.SupplierName) > 255 || len(in.Note) > 512 {
		return model.ReconPaymentRecord{}, fmt.Errorf("%w: supplier_name/note 过长", model.ErrInvalidInput)
	}
	for _, d := range []*time.Time{in.RequestDate, in.PayDate} {
		if d == nil || d.IsZero() {
			continue
		}
		if err := s.assertNotLocked(ctx, in.BranchID, *d, *d); err != nil {
			return model.ReconPaymentRecord{}, err
		}
	}

	now := s.now()
	row := model.ReconPaymentRecord{
		ID:           newID(),
		BranchID:     in.BranchID,
		Source:       model.PaymentSourceManual,
		SourceID:     newID(),
		SupplierName: in.SupplierName,
		Amount:       in.Amount,
		RequestDate:  dateOfPtr(in.RequestDate),
		PayDate:      dateOfPtr(in.PayDate),
		Method:       in.Method,
		Note:         in.Note,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("create recon_payment_records: %w", err)
		}
		return s.writeAuditTx(ctx, tx, model.ReconAuditLog{
			BranchID:   in.BranchID,
			ObjectType: "payment",
			ObjectID:   row.ID,
			Action:     "create",
			AfterJSON:  fmt.Sprintf(`{"supplier_name":%q,"amount":%q,"method":%q,"source":%q}`, in.SupplierName, in.Amount.String(), in.Method, model.PaymentSourceManual),
			Reason:     in.Note,
			Actor:      in.Actor,
			CreatedAt:  now,
		})
	})
	if err != nil {
		return model.ReconPaymentRecord{}, err
	}
	return row, nil
}

// paymentMethodValid 判断手工登记的付款方式（change/unmapped 不是付款渠道）。
func paymentMethodValid(method string) bool {
	switch method {
	case model.MethodCash, model.MethodWechat, model.MethodBankCard,
		model.MethodStoredValue, model.MethodCredit:
		return true
	}
	return false
}

// VerifyPaymentInput 是核销入参。
type VerifyPaymentInput struct {
	BranchID   string
	PaymentID  string
	BankFlowID string
	Note       string
	Actor      string
}

// VerifyPayment 把付款台账与一条银行流水核销（渠道一致 + 金额差 ≤ 0.01）。
//
// ⚠️ 只标记 verified 并关联 bank_flow_id，**不动**银行流水行（红线：导入即事实）。
func (s *Service) VerifyPayment(ctx context.Context, in VerifyPaymentInput) (model.ReconPaymentRecord, error) {
	in.BranchID = strings.TrimSpace(in.BranchID)
	in.PaymentID = strings.TrimSpace(in.PaymentID)
	in.BankFlowID = strings.TrimSpace(in.BankFlowID)
	in.Note = strings.TrimSpace(in.Note)
	in.Actor = strings.TrimSpace(in.Actor)
	if in.BranchID == "" || in.PaymentID == "" || in.BankFlowID == "" || in.Actor == "" {
		return model.ReconPaymentRecord{}, model.ErrInvalidInput
	}

	now := s.now()
	var out model.ReconPaymentRecord
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var pay model.ReconPaymentRecord
		err := tx.Where("branch_id = ? AND id = ?", in.BranchID, in.PaymentID).First(&pay).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.ErrNotFound
		}
		if err != nil {
			return err
		}
		if pay.Verified {
			return fmt.Errorf("%w: 该付款已核销", model.ErrInvalidInput)
		}
		beforeBankFlowID := pay.BankFlowID
		var flow model.ReconBankFlow
		err = tx.Where("branch_id = ? AND id = ?", in.BranchID, in.BankFlowID).First(&flow).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.ErrNotFound
		}
		if err != nil {
			return err
		}

		// 金额比对按**含手续费的毛额**：手续费必须单列，不能为了凑平把它吞掉。
		gross := flow.Amount.Add(flow.Fee)
		if pay.Amount.Sub(gross).Abs().Cmp(verifyAmountTolerance) > 0 {
			return fmt.Errorf("%w: 付款 %s 与流水毛额 %s 差额超过 %s",
				model.ErrInvalidInput, pay.Amount.String(), gross.String(), verifyAmountTolerance.String())
		}
		if err := assertChannelMatches(pay.Method, flow.Channel); err != nil {
			return err
		}

		updates := map[string]any{
			"verified":     true,
			"verified_by":  in.Actor,
			"bank_flow_id": flow.ID,
			"updated_at":   now,
		}
		if pay.Method == "" && flow.Channel != "" {
			updates["method"] = flow.Channel
			pay.Method = flow.Channel
		}
		if pay.PayDate == nil {
			vd := dayOnly(flow.ValueDate)
			updates["pay_date"] = vd
			pay.PayDate = &vd
		}
		if in.Note != "" {
			updates["note"] = truncate(in.Note, 512)
			pay.Note = truncate(in.Note, 512)
		}
		if err := tx.Model(&model.ReconPaymentRecord{}).
			Where("branch_id = ? AND id = ?", in.BranchID, in.PaymentID).
			Updates(updates).Error; err != nil {
			return err
		}

		pay.Verified = true
		pay.VerifiedBy = in.Actor
		pay.BankFlowID = flow.ID
		pay.UpdatedAt = now
		out = pay

		return s.writeAuditTx(ctx, tx, model.ReconAuditLog{
			BranchID:   in.BranchID,
			ObjectType: "payment",
			ObjectID:   pay.ID,
			Action:     "verify",
			BeforeJSON: fmt.Sprintf(`{"verified":false,"bank_flow_id":%q}`, beforeBankFlowID),
			AfterJSON:  fmt.Sprintf(`{"verified":true,"bank_flow_id":%q,"bank_amount":%q,"bank_fee":%q}`, flow.ID, flow.Amount.String(), flow.Fee.String()),
			Reason:     in.Note,
			Actor:      in.Actor,
			CreatedAt:  now,
		})
	})
	if err != nil {
		return model.ReconPaymentRecord{}, err
	}
	return out, nil
}

// assertChannelMatches 校验付款方式与银行流水渠道一致（未知方式时只要求非空）。
func assertChannelMatches(method, channel string) error {
	method = strings.ToLower(strings.TrimSpace(method))
	channel = strings.ToLower(strings.TrimSpace(channel))
	if method == "" || channel == "" {
		// 审批自动生成的记录没有 method，导入流水确有 channel 才能核销。
		if method == "" && channel == "" {
			return fmt.Errorf("%w: 银行流水缺少渠道", model.ErrInvalidInput)
		}
		return nil
	}
	if method == channel {
		return nil
	}
	for _, alias := range paymentChannelAliases[method] {
		if channel == alias {
			return nil
		}
	}
	return fmt.Errorf("%w: 付款方式 %q 与流水渠道 %q 不一致", model.ErrInvalidInput, method, channel)
}

// PaymentQuery 是付款台账的筛选条件。
type PaymentQuery struct {
	BranchID string
	Source   string
	Verified *bool
	Limit    int
	Offset   int
}

// ListPayments 返回付款台账。
func (s *Service) ListPayments(ctx context.Context, q PaymentQuery) ([]model.ReconPaymentRecord, int64, error) {
	q.BranchID = strings.TrimSpace(q.BranchID)
	q.Source = strings.TrimSpace(q.Source)
	if q.BranchID == "" {
		return nil, 0, model.ErrInvalidInput
	}
	if q.Source != "" && q.Source != model.PaymentSourceWecom && q.Source != model.PaymentSourceManual {
		return nil, 0, fmt.Errorf("%w: 未知 source %q", model.ErrInvalidInput, q.Source)
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 200 {
		q.Limit = 200
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	db := s.db.WithContext(ctx).Model(&model.ReconPaymentRecord{}).Where("branch_id = ?", q.BranchID)
	if q.Source != "" {
		db = db.Where("source = ?", q.Source)
	}
	if q.Verified != nil {
		db = db.Where("verified = ?", *q.Verified)
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	rows := make([]model.ReconPaymentRecord, 0, q.Limit)
	if err := db.Order("created_at DESC, id").Limit(q.Limit).Offset(q.Offset).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// GetPayment 取单笔付款台账。
func (s *Service) GetPayment(ctx context.Context, branchID, id string) (model.ReconPaymentRecord, error) {
	branchID = strings.TrimSpace(branchID)
	id = strings.TrimSpace(id)
	if branchID == "" || id == "" {
		return model.ReconPaymentRecord{}, model.ErrInvalidInput
	}
	var row model.ReconPaymentRecord
	err := s.db.WithContext(ctx).Where("branch_id = ? AND id = ?", branchID, id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.ReconPaymentRecord{}, model.ErrNotFound
	}
	if err != nil {
		return model.ReconPaymentRecord{}, err
	}
	return row, nil
}
