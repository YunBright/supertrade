// Package service —— pos 域的业务逻辑(开单 / 查单 / 列表 / 退货 / 改价)。
//
// # 三条业务规则
//
//  1. **一笔销售 = 一个本地事务**:
//
//     sales_orders + sale_lines + payments + **一条 outbox 记录** 同事务写入。
//
//     ⇒ 进程在 commit 与 publish 之间被 kill,事件仍在 outbox 里,不会丢
//     (丢一条 sale.paid = 库存不扣 + 积分不加,且**没有任何日志说这件事**)。
//
//  2. **op_id 幂等**:同 op_id 重复提交 ⇒ 返回**首次那张单**(HTTP 200),
//     不新建单、不重复发事件。
//
//  3. **金额自洽**:合计 = Σ 明细金额;找零 = 实收 - 合计;找零不为负。
//     不自洽 ⇒ 409(而不是"先收下再说")—— 收银现场金额不自洽就是**账实不符**。
//
// # 单号
//
// 见 ids.go:SO<yyyymmdd><6 位当天序号>,进程内计数器 + 每日 DB 播种 + 唯一索引兜底。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/pos/model"
	"github.com/YunBright/supertrade/pkg/outbox"
)

// maxIDAttempts 是销售单号冲突时的最大重试次数(跨进程抢号时的兜底)。
const maxIDAttempts = 20

// actor 是审计列的兜底值(拿不到 caller 时用)。
const actor = "system"

// 销售单状态。
const (
	StatusDraft    = "draft"
	StatusPaid     = "paid"
	StatusReturned = "returned"
	StatusVoid     = "void"
)

// errIdempotentReplay 表示"这是重放请求,应当回滚事务并返回首次结果"。
var errIdempotentReplay = errors.New("pos: op_id 重放")

// SalesService 是销售单服务。
type SalesService struct {
	db  *gorm.DB
	now func() time.Time
	ids saleIDGen
	rec *outbox.Recorder
}

// NewSalesService 构造 SalesService。
func NewSalesService(db *gorm.DB) *SalesService {
	return &SalesService{
		db:  db,
		now: func() time.Time { return time.Now() },
		rec: outbox.NewRecorder("pos"),
	}
}

// SetClock 注入时钟(测试用)。
func (s *SalesService) SetClock(fn func() time.Time) {
	if fn != nil {
		s.now = fn
	}
}

// ---- 入参结构 ----

// SaleLineInput 是开单时的一行明细。
type SaleLineInput struct {
	ProductID          string
	ProductName        string
	ProductNo          string
	Barcode            string
	Unit               string
	Qty                decimal.Decimal
	OriginalPriceYuan  decimal.Decimal
	UnitPriceYuan      decimal.Decimal
	DiscountAmountYuan decimal.Decimal
	AmountYuan         decimal.Decimal
	IsExchange         bool
	IsVoid             bool
}

// PaymentInput 是开单时的一条收付。
type PaymentInput struct {
	PayWay              string
	AmountYuan          decimal.Decimal
	ExchangeRate        decimal.Decimal
	ConvertedAmountYuan decimal.Decimal
	RefFlowNo           string
}

// CreateSaleInput 是开单(收款)的入参。
type CreateSaleInput struct {
	// OpID 是幂等键(uuid,必填)。
	OpID string
	// BranchID 是门店。
	BranchID string
	// MemberID 是会员号(可空;空 ⇒ 不计积分)。
	MemberID string
	// ShiftID 是班次(可空)。
	ShiftID string
	// OperatorID 是收银员。
	OperatorID string
	// AuthorizedBy 是越权授权人(可空)。
	AuthorizedBy string
	// Lines 是明细(至少一行)。
	Lines []SaleLineInput
	// Payments 是收付(至少一条)。
	Payments []PaymentInput
	// TotalAmountYuan 是应收合计(必须 = Σ 明细 amount_yuan)。
	TotalAmountYuan decimal.Decimal
	// DiscountAmountYuan 是优惠合计(展示 / 审计用)。
	DiscountAmountYuan decimal.Decimal
	// PaidAmountYuan 是实收。
	PaidAmountYuan decimal.Decimal
	// ChangeAmountYuan 是找零(必须 = 实收 - 合计)。
	ChangeAmountYuan decimal.Decimal
	// CreatedBy 是审计人(JWT sub)。
	CreatedBy string
}

// ListFilter 是列表查询条件。
type ListFilter struct {
	// BranchID 是门店(必填;单据按门店隔离)。
	BranchID string
	// From / To 是成交时间区间(左闭右开;nil = 不限)。
	From *time.Time
	To   *time.Time
	// Page 是页码(从 1 开始;<=0 视为 1)。
	Page int
	// PageSize 是每页条数(<=0 默认 20;>100 截到 100)。
	PageSize int
}

// RefundInput 是退货的入参。
type RefundInput struct {
	// OpID 是幂等键(可空 ⇒ 服务端生成 uuid)。
	OpID string
	// SaleID 是被退的销售单。
	SaleID string
	// BranchID 是门店。
	BranchID string
	// OperatorID 是操作人。
	OperatorID string
	// AuthorizedBy 是越权授权人(可空)。
	AuthorizedBy string
	// Reason 是退货原因(可空)。
	Reason string
	// CreatedBy 是审计人。
	CreatedBy string
}

// PriceOverrideInput 是改价的入参。
type PriceOverrideInput struct {
	// SaleID 是销售单。
	SaleID string
	// LineID 是明细 id。
	LineID string
	// BranchID 是门店。
	BranchID string
	// UnitPriceYuan 是新的成交单价(必填,>0)。
	UnitPriceYuan decimal.Decimal
	// AmountYuan 是新的行金额(可空 ⇒ 按 单价 × 数量 重算)。
	AmountYuan decimal.Decimal
	// DiscountAmountYuan 是新的行优惠额(可空 ⇒ 保留原值)。
	DiscountAmountYuan decimal.Decimal
	// HasDiscountAmount 区分"显式传 0"与"没传"。
	HasDiscountAmount bool
	// AuthorizedBy 是越权授权人(可空;有值时写进单头做审计)。
	AuthorizedBy string
	// UpdatedBy 是审计人。
	UpdatedBy string
}

// ---- 开单 ----

// Create 开单(收款):写销售单 + 明细 + 收付 + 一条 sale.paid 事件(同一事务)。
//
// 返回 (单据, 是否重放, error)。
//
// ⚠️ **重放返回首次那张单**,不报错、不新建、不重复发事件。
func (s *SalesService) Create(ctx context.Context, in CreateSaleInput) (*model.SalesOrder, bool, error) {
	if err := validateCreate(in); err != nil {
		return nil, false, err
	}
	// 快速路径:同 op_id 已经开过单 ⇒ 直接返回首次那张单。
	existing, err := s.findByOpID(ctx, in.OpID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, true, nil
	}

	now := s.now().In(bizZone)
	var (
		order   *model.SalesOrder
		payload SalePaidEvent
	)
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		o, lines, replay, err := s.insertOrder(ctx, tx, in, now)
		if err != nil {
			return err
		}
		if replay {
			// 并发的同 op_id 请求赢了 ⇒ 本事务回滚,只返回首次那张单。
			return errIdempotentReplay
		}
		if err := s.insertLines(tx, lines); err != nil {
			return err
		}
		if err := s.insertPayments(tx, o, in.Payments, now); err != nil {
			return err
		}
		payload = buildSalePaidEvent(o, lines, OrderStatusSale, now)
		if err := s.rec.Enqueue(tx, outbox.Event{
			ID:       in.OpID,
			BranchID: o.BranchID,
			Topic:    TopicSalePaid,
			Subject:  o.ID,
			Payload:  payload,
		}); err != nil {
			return err
		}
		order = o
		return nil
	})
	if errors.Is(txErr, errIdempotentReplay) {
		first, err := s.findByOpID(ctx, in.OpID)
		if err != nil {
			return nil, true, err
		}
		if first == nil {
			return nil, true, fmt.Errorf("pos: op_id %s 判定为重放,但查不到首次单据", in.OpID)
		}
		return first, true, nil
	}
	if txErr != nil {
		return nil, false, txErr
	}
	return order, false, nil
}

// insertOrder 写销售单头(带单号冲突重试)。
//
// 返回 (单据, 是否是 op_id 重放, error)。
func (s *SalesService) insertOrder(ctx context.Context, tx *gorm.DB, in CreateSaleInput, now time.Time) (*model.SalesOrder, []model.SaleLine, bool, error) {
	for attempt := 0; attempt < maxIDAttempts; attempt++ {
		id, err := s.ids.next(ctx, tx, now)
		if err != nil {
			return nil, nil, false, err
		}
		row := model.SalesOrder{
			ID:                 id,
			BranchID:           strings.TrimSpace(in.BranchID),
			MemberID:           strings.TrimSpace(in.MemberID),
			ShiftID:            strings.TrimSpace(in.ShiftID),
			OperatorID:         strings.TrimSpace(in.OperatorID),
			AuthorizedBy:       strings.TrimSpace(in.AuthorizedBy),
			TotalAmountYuan:    in.TotalAmountYuan,
			DiscountAmountYuan: in.DiscountAmountYuan,
			PaidAmountYuan:     in.PaidAmountYuan,
			ChangeAmountYuan:   in.ChangeAmountYuan,
			Status:             StatusPaid,
			SoldAt:             now,
			OpID:               strings.TrimSpace(in.OpID),
			CreatedBy:          firstNonEmpty(in.CreatedBy, actor),
			UpdatedBy:          firstNonEmpty(in.CreatedBy, actor),
			CreatedAt:          now,
			UpdatedAt:          now,
		}
		// ⚠️ DoNothing:主键(单号)或 op_id 唯一索引冲突时**跳过**而不是报错。
		//   冲突分两种:op_id 重复(重放)与单号撞了(跨进程抢号)⇒ 下面分别判定。
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if res.Error != nil {
			return nil, nil, false, res.Error
		}
		if res.RowsAffected == 1 {
			return &row, buildLines(&row, in.Lines, now), false, nil
		}
		// 冲突了:先看是不是 op_id 重复(并发同 op_id 赢了)。
		existing, err := s.findByOpIDTx(tx, row.OpID)
		if err != nil {
			return nil, nil, false, err
		}
		if existing != nil {
			return existing, nil, true, nil
		}
		// 单号撞了 ⇒ 下一轮重新取号(进程内计数器已经 +1,不会重复)。
	}
	return nil, nil, false, fmt.Errorf("pos: 销售单号冲突重试 %d 次仍失败", maxIDAttempts)
}

// buildLines 把入参明细装配成 model(行 id = <sale_id>-L<n>)。
func buildLines(o *model.SalesOrder, lines []SaleLineInput, now time.Time) []model.SaleLine {
	out := make([]model.SaleLine, 0, len(lines))
	for i, l := range lines {
		out = append(out, model.SaleLine{
			ID:                 fmt.Sprintf("%s-L%d", o.ID, i+1),
			SaleID:             o.ID,
			BranchID:           o.BranchID,
			ProductID:          strings.TrimSpace(l.ProductID),
			ProductName:        l.ProductName,
			ProductNo:          l.ProductNo,
			Barcode:            l.Barcode,
			Unit:               l.Unit,
			Qty:                l.Qty,
			OriginalPriceYuan:  l.OriginalPriceYuan,
			UnitPriceYuan:      l.UnitPriceYuan,
			DiscountAmountYuan: l.DiscountAmountYuan,
			AmountYuan:         l.AmountYuan,
			IsExchange:         l.IsExchange,
			IsVoid:             l.IsVoid,
			CreatedBy:          o.CreatedBy,
			CreatedAt:          now,
			UpdatedAt:          now,
		})
	}
	return out
}

// insertLines 写销售明细。
func (s *SalesService) insertLines(tx *gorm.DB, rows []model.SaleLine) error {
	if len(rows) == 0 {
		return nil
	}
	if err := tx.Create(&rows).Error; err != nil {
		return fmt.Errorf("pos: 写销售明细失败: %w", err)
	}
	return nil
}

// insertPayments 写收付(收付 id = <sale_id>-P<n>)。
func (s *SalesService) insertPayments(tx *gorm.DB, o *model.SalesOrder, pays []PaymentInput, now time.Time) error {
	if len(pays) == 0 {
		return nil
	}
	rows := make([]model.Payment, 0, len(pays))
	for i, p := range pays {
		rows = append(rows, model.Payment{
			ID:                  fmt.Sprintf("%s-P%d", o.ID, i+1),
			SaleID:              o.ID,
			BranchID:            o.BranchID,
			PayWay:              strings.TrimSpace(p.PayWay),
			AmountYuan:          p.AmountYuan,
			ExchangeRate:        p.ExchangeRate,
			ConvertedAmountYuan: p.ConvertedAmountYuan,
			RefFlowNo:           p.RefFlowNo,
			CreatedBy:           o.CreatedBy,
			CreatedAt:           now,
			UpdatedAt:           now,
		})
	}
	if err := tx.Create(&rows).Error; err != nil {
		return fmt.Errorf("pos: 写收付流水失败: %w", err)
	}
	return nil
}

// ---- 查单 / 列表 ----

// Get 查一张销售单(头 + 明细 + 收付)。
//
// ⚠️ 按 (id, branch_id) 查 —— 跨店看不到别人的单。
func (s *SalesService) Get(ctx context.Context, branchID, id string) (*model.SalesOrder, []model.SaleLine, []model.Payment, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, nil, nil, fmt.Errorf("%w: branch_id 不能为空", model.ErrInvalidInput)
	}
	var o model.SalesOrder
	err := s.db.WithContext(ctx).
		Where("id = ? AND branch_id = ?", strings.TrimSpace(id), branchID).
		First(&o).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, nil, model.ErrSaleNotFound
	}
	if err != nil {
		return nil, nil, nil, err
	}
	lines, payments, err := s.loadChildren(ctx, o.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	return &o, lines, payments, nil
}

// List 列销售单(按门店 + 成交时间区间,倒序 + 分页)。
func (s *SalesService) List(ctx context.Context, f ListFilter) ([]model.SalesOrder, int64, error) {
	if strings.TrimSpace(f.BranchID) == "" {
		return nil, 0, fmt.Errorf("%w: branch_id 不能为空", model.ErrInvalidInput)
	}
	q := s.db.WithContext(ctx).Model(&model.SalesOrder{}).
		Where("branch_id = ?", strings.TrimSpace(f.BranchID))
	if f.From != nil {
		q = q.Where("sold_at >= ?", *f.From)
	}
	if f.To != nil {
		q = q.Where("sold_at < ?", *f.To)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	page, size := NormalizePage(f.Page, f.PageSize)
	var rows []model.SalesOrder
	err := q.Order("sold_at DESC, id DESC").
		Offset((page - 1) * size).
		Limit(size).
		Find(&rows).Error
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// loadChildren 取单据的明细与收付。
func (s *SalesService) loadChildren(ctx context.Context, saleID string) ([]model.SaleLine, []model.Payment, error) {
	var lines []model.SaleLine
	if err := s.db.WithContext(ctx).
		Where("sale_id = ?", saleID).Order("id asc").Find(&lines).Error; err != nil {
		return nil, nil, err
	}
	var pays []model.Payment
	if err := s.db.WithContext(ctx).
		Where("sale_id = ?", saleID).Order("id asc").Find(&pays).Error; err != nil {
		return nil, nil, err
	}
	return lines, pays, nil
}

// Children 取单据的明细与收付(handler 组装响应用)。
func (s *SalesService) Children(ctx context.Context, saleID string) ([]model.SaleLine, []model.Payment, error) {
	return s.loadChildren(ctx, saleID)
}

// 分页默认值。
const (
	// DefaultPageSize 是默认每页条数。
	DefaultPageSize = 20
	// MaxPageSize 是每页条数上限。
	MaxPageSize = 100
)

// NormalizePage 归一化分页参数(page 从 1 开始;size 默认 20、上限 100)。
func NormalizePage(page, size int) (int, int) {
	if page <= 0 {
		page = 1
	}
	if size <= 0 {
		size = DefaultPageSize
	}
	if size > MaxPageSize {
		size = MaxPageSize
	}
	return page, size
}

// ---- 退货 ----

// Refund 退货:单据置 returned + 写负数收付 + 发一条 order_status="R" 的 sale.paid。
//
// 返回 (单据, 是否重放, error)。
//
// ⚠️ 退货**复用 sale.paid topic**,用 order_status="R" 区分;
//
//	事件里 qty 与金额**取负**。
func (s *SalesService) Refund(ctx context.Context, in RefundInput) (*model.SalesOrder, bool, error) {
	if strings.TrimSpace(in.SaleID) == "" {
		return nil, false, fmt.Errorf("%w: sale_id 不能为空", model.ErrInvalidInput)
	}
	if strings.TrimSpace(in.BranchID) == "" {
		return nil, false, fmt.Errorf("%w: branch_id 不能为空", model.ErrInvalidInput)
	}
	opID := strings.TrimSpace(in.OpID)
	if opID == "" {
		opID = uuid.NewString()
	}
	now := s.now().In(bizZone)
	var order *model.SalesOrder
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 幂等:同一 op_id 已经退过 ⇒ 回滚并返回首次结果。
		done, err := s.refundAlreadyDone(tx, opID)
		if err != nil {
			return err
		}
		if done {
			return errIdempotentReplay
		}
		var o model.SalesOrder
		err = tx.Where("id = ? AND branch_id = ?", strings.TrimSpace(in.SaleID), strings.TrimSpace(in.BranchID)).
			First(&o).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.ErrSaleNotFound
		}
		if err != nil {
			return err
		}
		if o.Status == StatusReturned {
			return model.ErrSaleAlreadyReturned
		}
		var lines []model.SaleLine
		if err := tx.Where("sale_id = ?", o.ID).Order("id asc").Find(&lines).Error; err != nil {
			return err
		}
		// 只取**原单**收付(id = <sale_id>-P<n>),排除退货自己写的 <sale_id>-RF<n>。
		var pays []model.Payment
		if err := tx.Where("sale_id = ? AND id LIKE ?", o.ID, o.ID+"-P%").
			Order("id asc").Find(&pays).Error; err != nil {
			return err
		}
		by := firstNonEmpty(in.CreatedBy, actor)
		if err := tx.Model(&model.SalesOrder{}).Where("id = ?", o.ID).
			Updates(map[string]any{
				"status":     StatusReturned,
				"updated_by": by,
				"updated_at": now,
			}).Error; err != nil {
			return err
		}
		o.Status = StatusReturned
		o.UpdatedBy = by
		o.UpdatedAt = now
		// 退款收付(负金额)。
		if len(pays) > 0 {
			refundPays := make([]model.Payment, 0, len(pays))
			for i, p := range pays {
				refundPays = append(refundPays, model.Payment{
					ID:                  fmt.Sprintf("%s-RF%d", o.ID, i+1),
					SaleID:              o.ID,
					BranchID:            o.BranchID,
					PayWay:              p.PayWay,
					AmountYuan:          p.AmountYuan.Neg(),
					ExchangeRate:        p.ExchangeRate,
					ConvertedAmountYuan: p.ConvertedAmountYuan.Neg(),
					RefFlowNo:           firstNonEmpty(p.RefFlowNo, p.ID),
					CreatedBy:           by,
					CreatedAt:           now,
					UpdatedAt:           now,
				})
			}
			if err := tx.Create(&refundPays).Error; err != nil {
				return fmt.Errorf("pos: 写退款收付失败: %w", err)
			}
		}
		payload := buildRefundEvent(&o, lines, opID, in, now)
		if err := s.rec.Enqueue(tx, outbox.Event{
			ID:       opID,
			BranchID: o.BranchID,
			Topic:    TopicSalePaid,
			Subject:  o.ID,
			Payload:  payload,
		}); err != nil {
			return err
		}
		order = &o
		return nil
	})
	if errors.Is(txErr, errIdempotentReplay) {
		first, _, _, err := s.Get(ctx, in.BranchID, in.SaleID)
		if err != nil {
			return nil, true, err
		}
		return first, true, nil
	}
	if txErr != nil {
		return nil, false, txErr
	}
	return order, false, nil
}

// refundAlreadyDone 判断该 op_id 是否已经退过(发件箱里有同 id 的行)。
func (s *SalesService) refundAlreadyDone(tx *gorm.DB, opID string) (bool, error) {
	var n int64
	err := tx.Model(&outbox.Outbox{}).Where("id = ?", s.rec.RowID(opID)).Count(&n).Error
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ---- 改价 ----

// PriceOverride 改价(越权授权后可放行)。
//
// ⚠️ **不发事件**:sale.paid 已经投递,再发一次会让下游**重复扣库存 / 重复加积分**。
// 改价只改本地单据 + 审计(authorized_by 落在单头上)。
func (s *SalesService) PriceOverride(ctx context.Context, in PriceOverrideInput) (*model.SalesOrder, error) {
	if strings.TrimSpace(in.SaleID) == "" || strings.TrimSpace(in.LineID) == "" {
		return nil, fmt.Errorf("%w: sale_id / line_id 不能为空", model.ErrInvalidInput)
	}
	if strings.TrimSpace(in.BranchID) == "" {
		return nil, fmt.Errorf("%w: branch_id 不能为空", model.ErrInvalidInput)
	}
	if in.UnitPriceYuan.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("%w: unit_price_yuan 必须大于 0", model.ErrInvalidInput)
	}
	now := s.now().In(bizZone)
	var out *model.SalesOrder
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var o model.SalesOrder
		err := tx.Where("id = ? AND branch_id = ?", strings.TrimSpace(in.SaleID), strings.TrimSpace(in.BranchID)).
			First(&o).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.ErrSaleNotFound
		}
		if err != nil {
			return err
		}
		var line model.SaleLine
		err = tx.Where("id = ? AND sale_id = ?", strings.TrimSpace(in.LineID), o.ID).First(&line).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.ErrLineNotFound
		}
		if err != nil {
			return err
		}
		amount := in.AmountYuan
		if amount.IsZero() {
			amount = in.UnitPriceYuan.Mul(line.Qty)
		}
		discount := line.DiscountAmountYuan
		if in.HasDiscountAmount {
			discount = in.DiscountAmountYuan
		}
		by := firstNonEmpty(in.UpdatedBy, actor)
		lineUpdates := map[string]any{
			"unit_price_yuan":      in.UnitPriceYuan,
			"amount_yuan":          amount,
			"discount_amount_yuan": discount,
			"updated_at":           now,
		}
		if err := tx.Model(&model.SaleLine{}).Where("id = ?", line.ID).
			Updates(lineUpdates).Error; err != nil {
			return err
		}
		// 重算单头合计(找零与实收不动 —— 钱已经收付过了)。
		var lines []model.SaleLine
		if err := tx.Where("sale_id = ?", o.ID).Order("id asc").Find(&lines).Error; err != nil {
			return err
		}
		total := decimal.Zero
		disc := decimal.Zero
		for _, l := range lines {
			amount := l.AmountYuan
			discount := l.DiscountAmountYuan
			if l.ID == line.ID {
				amount = lineUpdates["amount_yuan"].(decimal.Decimal)
				discount = lineUpdates["discount_amount_yuan"].(decimal.Decimal)
			}
			total = total.Add(amount)
			disc = disc.Add(discount)
		}
		orderUpdates := map[string]any{
			"total_amount_yuan":    total,
			"discount_amount_yuan": disc,
			"updated_by":           by,
			"updated_at":           now,
		}
		auth := strings.TrimSpace(in.AuthorizedBy)
		if auth != "" {
			orderUpdates["authorized_by"] = auth
		}
		if err := tx.Model(&model.SalesOrder{}).Where("id = ?", o.ID).
			Updates(orderUpdates).Error; err != nil {
			return err
		}
		o.TotalAmountYuan = total
		o.DiscountAmountYuan = disc
		if auth != "" {
			o.AuthorizedBy = auth
		}
		o.UpdatedBy = by
		o.UpdatedAt = now
		out = &o
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- 事件装配 ----

// buildSalePaidEvent 装配 sale.paid 事件(销售)。
func buildSalePaidEvent(o *model.SalesOrder, lines []model.SaleLine, status string, at time.Time) SalePaidEvent {
	ev := SalePaidEvent{
		OpID:            o.OpID,
		SaleID:          o.ID,
		BranchID:        o.BranchID,
		MemberID:        o.MemberID,
		OperatorID:      o.OperatorID,
		AuthorizedBy:    o.AuthorizedBy,
		TotalAmountYuan: money(o.TotalAmountYuan),
		PaidAt:          at.Format(time.RFC3339),
		OrderStatus:     status,
		Lines:           make([]SalePaidLine, 0, len(lines)),
	}
	for _, l := range lines {
		ev.Lines = append(ev.Lines, SalePaidLine{
			LineID:        l.ID,
			ProductID:     l.ProductID,
			Qty:           l.Qty.String(),
			UnitPriceYuan: money(l.UnitPriceYuan),
			AmountYuan:    money(l.AmountYuan),
		})
	}
	return ev
}

// buildRefundEvent 装配退货事件(order_status="R",qty 与金额取负)。
func buildRefundEvent(o *model.SalesOrder, lines []model.SaleLine, opID string, in RefundInput, at time.Time) SalePaidEvent {
	operator := strings.TrimSpace(in.OperatorID)
	if operator == "" {
		operator = o.OperatorID
	}
	ev := SalePaidEvent{
		OpID:            opID,
		SaleID:          o.ID,
		BranchID:        o.BranchID,
		MemberID:        o.MemberID,
		OperatorID:      operator,
		AuthorizedBy:    strings.TrimSpace(in.AuthorizedBy),
		TotalAmountYuan: money(o.TotalAmountYuan.Neg()),
		PaidAt:          at.Format(time.RFC3339),
		OrderStatus:     OrderStatusRefund,
		Lines:           make([]SalePaidLine, 0, len(lines)),
	}
	for _, l := range lines {
		ev.Lines = append(ev.Lines, SalePaidLine{
			LineID:        l.ID,
			ProductID:     l.ProductID,
			Qty:           l.Qty.Neg().String(),
			UnitPriceYuan: money(l.UnitPriceYuan),
			AmountYuan:    money(l.AmountYuan.Neg()),
		})
	}
	return ev
}

// ---- 校验与工具 ----

// validateCreate 校验开单入参。
//
// ⚠️ **金额不自洽 ⇒ 409**(不是 400):它是**账实不符**,不是请求格式错。
func validateCreate(in CreateSaleInput) error {
	if strings.TrimSpace(in.OpID) == "" {
		return fmt.Errorf("%w: op_id 必填(幂等键)", model.ErrInvalidInput)
	}
	if strings.TrimSpace(in.BranchID) == "" {
		return fmt.Errorf("%w: branch_id 必填", model.ErrInvalidInput)
	}
	if strings.TrimSpace(in.OperatorID) == "" {
		return fmt.Errorf("%w: operator_id 必填", model.ErrInvalidInput)
	}
	if len(in.Lines) == 0 {
		return fmt.Errorf("%w: lines 不能为空", model.ErrInvalidInput)
	}
	if len(in.Payments) == 0 {
		return fmt.Errorf("%w: payments 不能为空", model.ErrInvalidInput)
	}
	for i, l := range in.Lines {
		if strings.TrimSpace(l.ProductID) == "" {
			return fmt.Errorf("%w: lines[%d].product_id 不能为空", model.ErrInvalidInput, i)
		}
		if l.Qty.LessThanOrEqual(decimal.Zero) {
			return fmt.Errorf("%w: lines[%d].qty 必须大于 0", model.ErrInvalidInput, i)
		}
		if l.AmountYuan.IsNegative() {
			return fmt.Errorf("%w: lines[%d].amount_yuan 不能为负", model.ErrInvalidInput, i)
		}
	}
	for i, p := range in.Payments {
		if strings.TrimSpace(p.PayWay) == "" {
			return fmt.Errorf("%w: payments[%d].pay_way 不能为空", model.ErrInvalidInput, i)
		}
		if p.AmountYuan.IsNegative() {
			return fmt.Errorf("%w: payments[%d].amount_yuan 不能为负", model.ErrInvalidInput, i)
		}
	}
	// 合计 = Σ 明细金额。
	sum := decimal.Zero
	for _, l := range in.Lines {
		sum = sum.Add(l.AmountYuan)
	}
	if !sum.Equal(in.TotalAmountYuan) {
		return fmt.Errorf("%w: total_amount_yuan(%s) ≠ Σ 明细 amount_yuan(%s)",
			model.ErrAmountMismatch, in.TotalAmountYuan.String(), sum.String())
	}
	// 找零 = 实收 - 合计。
	wantChange := in.PaidAmountYuan.Sub(in.TotalAmountYuan)
	if !wantChange.Equal(in.ChangeAmountYuan) {
		return fmt.Errorf("%w: change_amount_yuan(%s) ≠ paid_amount_yuan(%s) - total_amount_yuan(%s)",
			model.ErrAmountMismatch, in.ChangeAmountYuan.String(),
			in.PaidAmountYuan.String(), in.TotalAmountYuan.String())
	}
	if in.ChangeAmountYuan.IsNegative() {
		return fmt.Errorf("%w: 实收不足(找零为负 %s)", model.ErrAmountMismatch, in.ChangeAmountYuan.String())
	}
	return nil
}

// findByOpID 按 op_id 查销售单(查不到返 nil,nil)。
func (s *SalesService) findByOpID(ctx context.Context, opID string) (*model.SalesOrder, error) {
	var row model.SalesOrder
	err := s.db.WithContext(ctx).Where("op_id = ?", strings.TrimSpace(opID)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// findByOpIDTx 同 findByOpID,但用事务里的句柄。
func (s *SalesService) findByOpIDTx(tx *gorm.DB, opID string) (*model.SalesOrder, error) {
	var row model.SalesOrder
	err := tx.Where("op_id = ?", strings.TrimSpace(opID)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// money 把金额格式化成**字符串**(2 位小数,元)。
//
// ⚠️ 不用 float、不用 JSON number —— Dart 侧 jsonDecode 会变 double。
func money(d decimal.Decimal) string {
	return d.StringFixed(2)
}

// firstNonEmpty 取第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
