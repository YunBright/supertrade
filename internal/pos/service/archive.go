// Package service / archive.go —— pos 域的**归档导出 + 批量导入**。
//
// # 端点
//
//	GET  /archive/export?scope=sales_orders|sale_lines|payments
//	POST /archive/import
//
// # 这一域**不在 cube 里**
//
//	cube 只有商品/分类/供应商/条码/价格/促销/库存/会员/会员类别 +
//	销售结算类 model;销售单头/明细/收付是**本域自己产生的**
//	⇒ 读端点永远走本地。
//
//	但导入导出仍按同一套范式走(见 internal/archiveroute):
//	  - 没配 cube 源 ⇒ 导本地 / 写本地;
//	  - 配了 cube 源 ⇒ 导出**空数据** + 导入**拒绝(409 只读)**。
//
// # ⚠️⚠️ 导入 = **数据恢复**,不是业务重放（本域最危险的一处）
//
//	pos 的一笔销售在**同一个事务**里写三张表 + 写一条 `sale.paid` 事件
//	(事务性发件箱)。下游据此:
//	  - inventory 扣库存;
//	  - member 加积分。
//
//	⇒ 如果导入走了 SalesService 的建单路径(或者碰了发件箱),
//	一次"恢复数据"就会**再扣一次库存、再加一次积分**,
//	而且前台看不出来(单据显示正常,只是库存/积分悄悄错了一截)。
//
//	⇒ 所以导入**直接写表**,一行事件都不发:
//	  - 走的是**独立的 ArchiveService**,不碰 SalesService 的 outbox recorder;
//	  - 事务里只 INSERT/UPDATE 三张业务表,outbox 表**一行都不写**;
//	  - 状态机(paid/returned/void)与金额自洽**原样恢复**,不重算、不推进。
//
//	这个"只写数据"的性质由 TestArchiveImport_DoesNotPublishEvents 锁住。
//
// # ⚠️ 导出的字段取舍
//
//	本域三张表**没有凭据列**(没有口令 / 密钥 / 人脸特征)。
//	`authorized_by` 是**越权授权人**(图4/图5 的审计留痕),是业务事实,导出。
package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/archiveroute"
	"github.com/YunBright/supertrade/internal/pos/model"
)

// 归档的资料域(scope)名。
const (
	// ScopeSalesOrders 是销售单头(sales_orders)。
	ScopeSalesOrders = "sales_orders"
	// ScopeSaleLines 是销售明细(sale_lines)。
	ScopeSaleLines = "sale_lines"
	// ScopePayments 是收付流水(payments)。
	ScopePayments = "payments"
)

// 归档端点的守卫 scope(与 auth 的 permissions seed 逐字相同)。
//
// ⚠️ 导出与导入**同档**(pos:sale):导入是"把这家店已经发生过的销售恢复回来",
//
//	与查单是同一档权限;改价(pos:price-override)与退货(pos:refund)另算。
const (
	// ScopeSale 是导出与导入(pos:sale)。
	ScopeSale = "pos:sale"
)

// ArchiveService 是 pos 的归档导出 / 批量导入服务。
//
// ⚠️ **刻意不持有 outbox.Recorder**:导入**绝不能**发 sale.paid 事件
// (见文件头)。
type ArchiveService struct {
	db *gorm.DB
	// resolver 解析"这个门店配了哪个数据源"。
	resolver archiveroute.Resolver
	now      func() time.Time
}

// NewArchiveService 构造 ArchiveService。
func NewArchiveService(db *gorm.DB) *ArchiveService {
	return &ArchiveService{db: db, now: func() time.Time { return time.Now() }}
}

// SetSourceResolver 注入数据源解析器。
func (a *ArchiveService) SetSourceResolver(r archiveroute.Resolver) {
	a.resolver = r
}

// ---- 导出行类型(本域自己的 DTO,不扩 archivedto)----

// SalesOrderRow 是销售单头的导出行。
type SalesOrderRow struct {
	ID                 string `json:"id"`
	MemberID           string `json:"member_id"`
	ShiftID            string `json:"shift_id"`
	OperatorID         string `json:"operator_id"`
	AuthorizedBy       string `json:"authorized_by"`
	TotalAmountYuan    string `json:"total_amount_yuan"`
	DiscountAmountYuan string `json:"discount_amount_yuan"`
	PaidAmountYuan     string `json:"paid_amount_yuan"`
	ChangeAmountYuan   string `json:"change_amount_yuan"`
	Status             string `json:"status"`
	SoldAt             string `json:"sold_at"`
	OpID               string `json:"op_id"`
	ServerVersion      int64  `json:"server_version"`
	CreatedBy          string `json:"created_by"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
}

// SaleLineRow 是销售明细的导出行。
type SaleLineRow struct {
	ID                 string `json:"id"`
	SaleID             string `json:"sale_id"`
	ProductID          string `json:"product_id"`
	ProductName        string `json:"product_name"`
	ProductNo          string `json:"product_no"`
	Barcode            string `json:"barcode"`
	Unit               string `json:"unit"`
	Qty                string `json:"qty"`
	OriginalPriceYuan  string `json:"original_price_yuan"`
	UnitPriceYuan      string `json:"unit_price_yuan"`
	DiscountAmountYuan string `json:"discount_amount_yuan"`
	AmountYuan         string `json:"amount_yuan"`
	IsExchange         bool   `json:"is_exchange"`
	IsVoid             bool   `json:"is_void"`
	CreatedBy          string `json:"created_by"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
}

// PaymentRow 是收付流水的导出行。
type PaymentRow struct {
	ID                  string `json:"id"`
	SaleID              string `json:"sale_id"`
	PayWay              string `json:"pay_way"`
	AmountYuan          string `json:"amount_yuan"`
	ExchangeRate        string `json:"exchange_rate"`
	ConvertedAmountYuan string `json:"converted_amount_yuan"`
	RefFlowNo           string `json:"ref_flow_no"`
	CreatedBy           string `json:"created_by"`
	CreatedAt           string `json:"created_at"`
	UpdatedAt           string `json:"updated_at"`
}

// ExportResult 是导出的响应体。
//
// ⚠️ 各数组**永远存在**(空时是 `[]`)。
type ExportResult struct {
	Scope    string `json:"scope"`
	BranchID string `json:"branch_id"`
	// External = 该门店配了外部数据源 ⇒ 本次导出**空**。
	External bool `json:"external"`
	// 各 scope 的行;本次只填 scope 指向的那一个。
	SalesOrders []SalesOrderRow `json:"sales_orders"`
	SaleLines   []SaleLineRow   `json:"sale_lines"`
	Payments    []PaymentRow    `json:"payments"`
	// Count 是本次 scope 的行数。
	Count int `json:"count"`
}

// ImportRequest 是导入请求体(与导出响应同形 ⇒ 可往返)。
type ImportRequest struct {
	Scope string `json:"scope"`
	// 各 scope 的行;本次只处理 Scope 指向的那一个。
	SalesOrders []SalesOrderRow `json:"sales_orders,omitempty"`
	SaleLines   []SaleLineRow   `json:"sale_lines,omitempty"`
	Payments    []PaymentRow    `json:"payments,omitempty"`
}

// ImportErrorItem 是单行的失败明细。
type ImportErrorItem struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// ImportResult 是导入结果。
type ImportResult struct {
	Scope    string            `json:"scope"`
	BranchID string            `json:"branch_id"`
	Received int               `json:"received"`
	Inserted int               `json:"inserted"`
	Updated  int               `json:"updated"`
	Skipped  int               `json:"skipped"`
	Errors   []ImportErrorItem `json:"errors,omitempty"`
	Warnings []string          `json:"warnings,omitempty"`
}

// Export 导出指定 scope 的数据。
func (a *ArchiveService) Export(ctx context.Context, branchID, scope string) (*ExportResult, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", model.ErrInvalidInput)
	}
	scope = strings.TrimSpace(scope)
	switch scope {
	case ScopeSalesOrders, ScopeSaleLines, ScopePayments:
	default:
		return nil, fmt.Errorf("%w: scope=%q 非法", model.ErrInvalidInput, scope)
	}

	mode, err := archiveroute.DecideExport(ctx, a.resolver, branchID)
	if err != nil {
		return nil, err
	}

	out := &ExportResult{
		Scope:       scope,
		BranchID:    branchID,
		External:    mode == archiveroute.ModeExternal,
		SalesOrders: []SalesOrderRow{},
		SaleLines:   []SaleLineRow{},
		Payments:    []PaymentRow{},
	}
	if mode == archiveroute.ModeExternal {
		// ⚠️ 空,**不回落本地**(见文件头)。
		return out, nil
	}

	switch scope {
	case ScopeSalesOrders:
		rows, err := a.exportSalesOrders(ctx, branchID)
		if err != nil {
			return nil, err
		}
		out.SalesOrders = rows
		out.Count = len(rows)
	case ScopeSaleLines:
		rows, err := a.exportSaleLines(ctx, branchID)
		if err != nil {
			return nil, err
		}
		out.SaleLines = rows
		out.Count = len(rows)
	case ScopePayments:
		rows, err := a.exportPayments(ctx, branchID)
		if err != nil {
			return nil, err
		}
		out.Payments = rows
		out.Count = len(rows)
	}
	return out, nil
}

// exportSalesOrders 导出销售单头(按 branch_id 过滤)。
func (a *ArchiveService) exportSalesOrders(ctx context.Context, branchID string) ([]SalesOrderRow, error) {
	var rows []model.SalesOrder
	if err := a.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("sold_at desc, id desc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]SalesOrderRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, SalesOrderRow{
			ID:                 r.ID,
			MemberID:           r.MemberID,
			ShiftID:            r.ShiftID,
			OperatorID:         r.OperatorID,
			AuthorizedBy:       r.AuthorizedBy,
			TotalAmountYuan:    r.TotalAmountYuan.String(),
			DiscountAmountYuan: r.DiscountAmountYuan.String(),
			PaidAmountYuan:     r.PaidAmountYuan.String(),
			ChangeAmountYuan:   r.ChangeAmountYuan.String(),
			Status:             r.Status,
			SoldAt:             r.SoldAt.UTC().Format(time.RFC3339),
			OpID:               r.OpID,
			ServerVersion:      r.ServerVersion,
			CreatedBy:          r.CreatedBy,
			CreatedAt:          r.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:          r.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// exportSaleLines 导出销售明细(按 branch_id 过滤)。
func (a *ArchiveService) exportSaleLines(ctx context.Context, branchID string) ([]SaleLineRow, error) {
	var rows []model.SaleLine
	if err := a.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("sale_id asc, id asc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]SaleLineRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, SaleLineRow{
			ID:                 r.ID,
			SaleID:             r.SaleID,
			ProductID:          r.ProductID,
			ProductName:        r.ProductName,
			ProductNo:          r.ProductNo,
			Barcode:            r.Barcode,
			Unit:               r.Unit,
			Qty:                r.Qty.String(),
			OriginalPriceYuan:  r.OriginalPriceYuan.String(),
			UnitPriceYuan:      r.UnitPriceYuan.String(),
			DiscountAmountYuan: r.DiscountAmountYuan.String(),
			AmountYuan:         r.AmountYuan.String(),
			IsExchange:         r.IsExchange,
			IsVoid:             r.IsVoid,
			CreatedBy:          r.CreatedBy,
			CreatedAt:          r.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:          r.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// exportPayments 导出收付流水(按 branch_id 过滤)。
func (a *ArchiveService) exportPayments(ctx context.Context, branchID string) ([]PaymentRow, error) {
	var rows []model.Payment
	if err := a.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("sale_id asc, id asc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]PaymentRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, PaymentRow{
			ID:                  r.ID,
			SaleID:              r.SaleID,
			PayWay:              r.PayWay,
			AmountYuan:          r.AmountYuan.String(),
			ExchangeRate:        r.ExchangeRate.String(),
			ConvertedAmountYuan: r.ConvertedAmountYuan.String(),
			RefFlowNo:           r.RefFlowNo,
			CreatedBy:           r.CreatedBy,
			CreatedAt:           r.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:           r.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// Import 把导出的行**幂等 upsert** 回本域表。
//
// # ⚠️ 这是「数据恢复」,不是「业务重放」
//
//	不调 SalesService.CreateSale(那会写 outbox ⇒ 发 sale.paid ⇒
//	下游**再扣一次库存、再加一次积分**);
//	不重算金额、不校验"合计 = 明细之和"、不推进 paid/returned/void 状态机。
//
//	⇒ 恢复出来的单据停在**它被导出的那个状态**上。
//
// # 幂等
//
//	按**主键** upsert(各表自己的主键)。重复导入同一份归档 ⇒ 行数不变、内容不变。
func (a *ArchiveService) Import(ctx context.Context, branchID string, req ImportRequest) (*ImportResult, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", model.ErrInvalidInput)
	}
	scope := strings.TrimSpace(req.Scope)
	switch scope {
	case ScopeSalesOrders, ScopeSaleLines, ScopePayments:
	default:
		return nil, fmt.Errorf("%w: scope=%q 非法", model.ErrInvalidInput, scope)
	}

	// 判据:配了 cube 源 ⇒ 本域只读(409);解析失败 ⇒ 放行(导入路径自己兜底)。
	mode, err := archiveroute.DecideImport(ctx, a.resolver, branchID)
	if err != nil {
		slog.Warn("pos: 问不到门店数据源,导入按本域处理",
			"branch_id", branchID, "err", err)
		mode = archiveroute.ModeLocal
	}
	if mode == archiveroute.ModeExternal {
		return nil, fmt.Errorf("%w: 门店 %s 的销售数据来自外部数据源,请在原系统操作",
			archiveroute.ErrExternalReadOnly, branchID)
	}

	res := &ImportResult{Scope: scope, BranchID: branchID}
	switch scope {
	case ScopeSalesOrders:
		a.importSalesOrders(ctx, branchID, req.SalesOrders, res)
	case ScopeSaleLines:
		a.importSaleLines(ctx, branchID, req.SaleLines, res)
	case ScopePayments:
		a.importPayments(ctx, branchID, req.Payments, res)
	}
	return res, nil
}

// importSalesOrders 幂等写入销售单头。
//
// ⚠️ **op_id 有唯一索引**:若导入的 op_id 与**另一张单**冲突,
//
//	该行会被数据库拒(唯一约束)⇒ 计入 skipped + errors,
//	而不是静默改掉别人的 op_id。
func (a *ArchiveService) importSalesOrders(ctx context.Context, branchID string, rows []SalesOrderRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.SalesOrder
	if err := a.db.WithContext(ctx).Where("id IN ?", ids).Find(&found).Error; err == nil {
		for _, f := range found {
			existing[f.ID] = true
		}
	}

	for _, r := range rows {
		id := strings.TrimSpace(r.ID)
		if id == "" {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "id 为空"})
			continue
		}
		soldAt, err := time.Parse(time.RFC3339, strings.TrimSpace(r.SoldAt))
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "sold_at 不是 RFC3339 时间"})
			continue
		}
		row := model.SalesOrder{
			ID:                 id,
			BranchID:           branchID,
			MemberID:           r.MemberID,
			ShiftID:            r.ShiftID,
			OperatorID:         r.OperatorID,
			AuthorizedBy:       r.AuthorizedBy,
			TotalAmountYuan:    parseDecimalLoose(r.TotalAmountYuan),
			DiscountAmountYuan: parseDecimalLoose(r.DiscountAmountYuan),
			PaidAmountYuan:     parseDecimalLoose(r.PaidAmountYuan),
			ChangeAmountYuan:   parseDecimalLoose(r.ChangeAmountYuan),
			Status:             r.Status,
			SoldAt:             soldAt,
			OpID:               r.OpID,
			ServerVersion:      r.ServerVersion,
			CreatedBy:          r.CreatedBy,
			CreatedAt:          parseRFC3339(r.CreatedAt, a.now()),
			UpdatedAt:          parseRFC3339(r.UpdatedAt, a.now()),
		}
		err = a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"branch_id", "member_id", "shift_id", "operator_id", "authorized_by",
				"total_amount_yuan", "discount_amount_yuan", "paid_amount_yuan",
				"change_amount_yuan", "status", "sold_at", "server_version",
				// ⚠️ **不更新 created_by / updated_by**:
				//   审计列归本域自己维护,导入不搬别处的审计痕迹。
				"updated_at",
			}),
		}).Create(&row).Error
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: err.Error()})
			continue
		}
		if existing[id] {
			res.Updated++
		} else {
			res.Inserted++
			existing[id] = true
		}
	}
}

// importSaleLines 幂等写入销售明细。
func (a *ArchiveService) importSaleLines(ctx context.Context, branchID string, rows []SaleLineRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.SaleLine
	if err := a.db.WithContext(ctx).Where("id IN ?", ids).Find(&found).Error; err == nil {
		for _, f := range found {
			existing[f.ID] = true
		}
	}

	for _, r := range rows {
		id := strings.TrimSpace(r.ID)
		saleID := strings.TrimSpace(r.SaleID)
		if id == "" || saleID == "" {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "id / sale_id 必填"})
			continue
		}
		row := model.SaleLine{
			ID:                 id,
			SaleID:             saleID,
			BranchID:           branchID,
			ProductID:          r.ProductID,
			ProductName:        r.ProductName,
			ProductNo:          r.ProductNo,
			Barcode:            r.Barcode,
			Unit:               r.Unit,
			Qty:                parseDecimalLoose(r.Qty),
			OriginalPriceYuan:  parseDecimalLoose(r.OriginalPriceYuan),
			UnitPriceYuan:      parseDecimalLoose(r.UnitPriceYuan),
			DiscountAmountYuan: parseDecimalLoose(r.DiscountAmountYuan),
			AmountYuan:         parseDecimalLoose(r.AmountYuan),
			IsExchange:         r.IsExchange,
			IsVoid:             r.IsVoid,
			CreatedBy:          r.CreatedBy,
			CreatedAt:          parseRFC3339(r.CreatedAt, a.now()),
			UpdatedAt:          parseRFC3339(r.UpdatedAt, a.now()),
		}
		err := a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"sale_id", "branch_id", "product_id", "product_name", "product_no",
				"barcode", "unit", "qty", "original_price_yuan", "unit_price_yuan",
				"discount_amount_yuan", "amount_yuan", "is_exchange", "is_void",
				"created_by", "updated_at",
			}),
		}).Create(&row).Error
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: err.Error()})
			continue
		}
		if existing[id] {
			res.Updated++
		} else {
			res.Inserted++
			existing[id] = true
		}
	}
}

// importPayments 幂等写入收付流水。
func (a *ArchiveService) importPayments(ctx context.Context, branchID string, rows []PaymentRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.Payment
	if err := a.db.WithContext(ctx).Where("id IN ?", ids).Find(&found).Error; err == nil {
		for _, f := range found {
			existing[f.ID] = true
		}
	}

	for _, r := range rows {
		id := strings.TrimSpace(r.ID)
		saleID := strings.TrimSpace(r.SaleID)
		if id == "" || saleID == "" {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "id / sale_id 必填"})
			continue
		}
		row := model.Payment{
			ID:                  id,
			SaleID:              saleID,
			BranchID:            branchID,
			PayWay:              r.PayWay,
			AmountYuan:          parseDecimalLoose(r.AmountYuan),
			ExchangeRate:        parseDecimalLoose(r.ExchangeRate),
			ConvertedAmountYuan: parseDecimalLoose(r.ConvertedAmountYuan),
			RefFlowNo:           r.RefFlowNo,
			CreatedBy:           r.CreatedBy,
			CreatedAt:           parseRFC3339(r.CreatedAt, a.now()),
			UpdatedAt:           parseRFC3339(r.UpdatedAt, a.now()),
		}
		err := a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"sale_id", "branch_id", "pay_way", "amount_yuan", "exchange_rate",
				"converted_amount_yuan", "ref_flow_no", "created_by", "updated_at",
			}),
		}).Create(&row).Error
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: err.Error()})
			continue
		}
		if existing[id] {
			res.Updated++
		} else {
			res.Inserted++
			existing[id] = true
		}
	}
}

// ---- 小工具 ----

// parseDecimalLoose 解析数量 / 金额字符串;解析不了按 0。
func parseDecimalLoose(s string) decimal.Decimal {
	t := strings.TrimSpace(s)
	if t == "" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(t)
	if err != nil {
		return decimal.Zero
	}
	return d
}

// parseRFC3339 解析时间;解析不了用 def。
func parseRFC3339(s string, def time.Time) time.Time {
	t := strings.TrimSpace(s)
	if t == "" {
		return def
	}
	parsed, err := time.Parse(time.RFC3339, t)
	if err != nil {
		return def
	}
	return parsed
}
