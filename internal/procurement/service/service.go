// Package service —— 采购收货域(图5)的读服务。
//
// # 本轮**只读**
//
// PLAN §3.4 明写「只读(单据列表 + 单据详情)」⇒ 本包不实现任何写操作。
//
// # 数据来源按「门店配没配数据源」分流(判据唯一:internal/cubesource)
//
//	配了源 ⇒ 走 cube(purchase_sheet / purchase_sheet_line)
//	没配源 ⇒ 走本域 PG(purchase_sheets / purchase_sheet_lines)
//	问不到 / 被停用 ⇒ **503**,不回落本地。
//
// # ⚠️ 两条路径**共用同一批响应结构体**
//
// (model.PurchaseSheetItem / model.PurchaseSheetLine)⇒ 形状不会漂移。
//
// # ⚠️ 金额符号(采购侧 vs 结算侧)
//
//	采购侧 t_pm_sheet_master 的 RO 退货单是**正数**(实测 ysx 3565 张合计
//	+175.2 万),结算侧 t_fm_recpay_detail 的 RO 是**负数**(实测 -171.7 万)。
//	直接对比 ⇒ 差一个符号、约 346 万。
//
// ⇒ 头行的 `total_net_amount_yuan` 取 cube 的 total_net_amount_yuan
// (cube 已把 RO 取负,与结算侧同口径);`total_amount_yuan` 是原样金额。
//
// # ⚠️ cube 侧维度契约(**假设**,cube 仓已有该 model,本仓按其 schema 对齐)
//
// purchase_sheet(源表 t_pm_sheet_master):
//
//	id / supplier_id / document_type / approve_status / order_status /
//	branch_id / pay_way / currency / created_at / finished_at /
//	operator_id / confirmer / remark
//	measure: total_amount_yuan / total_net_amount_yuan
//
// purchase_sheet_line(源表 t_pm_sheet_detail):
//
//	id / sheet_id / product_id / valid_at
//	measure: total_amount_yuan / total_received_quantity /
//	         total_order_quantity / avg_valid_price_yuan
//
// 商品名与单位走 cube `product` 字典(duck,单独一趟查询)。
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/procurement/model"
)

// 分页口径。
const (
	// DefaultPageSize 是采购单列表的默认每页条数。
	DefaultPageSize = 50
	// MaxPageSize 是每页条数上限(page_size 直接落到 cube 的 LIMIT)。
	MaxPageSize = 200
)

// Service 是采购收货域的服务。
type Service struct {
	db      *gorm.DB
	sources cubesource.Resolver
	reader  cuberead.Reader
	logger  *slog.Logger
}

// New 构造 Service。
func New(db *gorm.DB, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{db: db, logger: logger}
}

// SetCubeSource 注入数据源解析器与 cube 读端。
//
// ⚠️ **两个都注入**才走 cube 路径(理由同 transaction / pricing)。
func (s *Service) SetCubeSource(resolver cubesource.Resolver, reader cuberead.Reader) {
	if s == nil {
		return
	}
	s.sources = resolver
	s.reader = reader
}

// readPath 判定该门店的采购单该走哪条路(问不到 / 停用 ⇒ 503,不回落本地)。
func (s *Service) readPath(ctx context.Context, branchID string) (bool, error) {
	useCube, err := cubesource.Decide(ctx, s.sources, branchID)
	if err == nil {
		if useCube && s.reader == nil {
			return false, fmt.Errorf("%w: procurement 缺少 cube 读端", model.ErrCubeSourceUnavailable)
		}
		return useCube, nil
	}
	switch {
	case errors.Is(err, cubesource.ErrSourceDisabled):
		return false, fmt.Errorf("%w: 门店 %s 的数据源已停用",
			model.ErrCubeSourceDisabled, strings.TrimSpace(branchID))
	case errors.Is(err, cubesource.ErrSourceUnavailable):
		return false, fmt.Errorf("%w: %v", model.ErrCubeSourceUnavailable, err)
	default:
		return false, err
	}
}

// Query 是采购单列表的查询条件。
type Query struct {
	// From / To 是**制单时间**区间(闭区间)。空 = 不过滤。
	From string
	To   string
	// BranchID 是**源库**分店号(思迅 branch_no)。
	BranchID string
	// SupplierID 是供应商编码。空 = 不过滤。
	SupplierID string
	// ApproveStatus 是审核状态(draft / approved)。空 = 不过滤。
	ApproveStatus string
	// OrderNo 是单据号(精确)。空 = 不过滤。
	OrderNo string
	// DocumentType 是单据类型(PI / RO / PO …)。空 = 不过滤。
	//
	// ⚠️ PLAN §3.4 的参数表里没有这一项 —— 这是**附加**的:
	// 图5 的左侧单据列表要能按"收货单 / 退货单"分开看。
	DocumentType string
	// Page 从 1 开始;PageSize 是每页条数。
	Page     int
	PageSize int
}

func (q Query) normalized() Query {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = DefaultPageSize
	}
	if q.PageSize > MaxPageSize {
		q.PageSize = MaxPageSize
	}
	return q
}

// ---- cube 维度清单 ----

// purchaseSheetDims 是 cube `purchase_sheet` model 的维度。
func purchaseSheetDims() []string {
	return []string{
		"purchase_sheet.id", "purchase_sheet.supplier_id", "purchase_sheet.document_type",
		"purchase_sheet.approve_status", "purchase_sheet.order_status",
		"purchase_sheet.branch_id", "purchase_sheet.pay_way", "purchase_sheet.currency",
		"purchase_sheet.created_at", "purchase_sheet.finished_at",
		"purchase_sheet.operator_id", "purchase_sheet.confirmer", "purchase_sheet.remark",
	}
}

// purchaseSheetLineDims 是 cube `purchase_sheet_line` model 的维度。
func purchaseSheetLineDims() []string {
	return []string{
		"purchase_sheet_line.id", "purchase_sheet_line.sheet_id",
		"purchase_sheet_line.product_id",
	}
}

// sheetFilters 构造采购单的过滤条件(两条路径同一套口径)。
func sheetFilters(q Query) []cubeclient.CubeFilter {
	var out []cubeclient.CubeFilter
	if q.BranchID != "" {
		out = append(out, cuberead.Filter("purchase_sheet.branch_id",
			cubeclient.OperatorEquals, q.BranchID))
	}
	if q.SupplierID != "" {
		out = append(out, cuberead.Filter("purchase_sheet.supplier_id",
			cubeclient.OperatorEquals, q.SupplierID))
	}
	if q.OrderNo != "" {
		out = append(out, cuberead.Filter("purchase_sheet.id",
			cubeclient.OperatorEquals, q.OrderNo))
	}
	if q.DocumentType != "" {
		out = append(out, cuberead.Filter("purchase_sheet.document_type",
			cubeclient.OperatorEquals, q.DocumentType))
	}
	if q.ApproveStatus != "" {
		// ⚠️ 调用方给的是**译文**(draft / approved),cube 侧存的是 "0" / "1"
		// ⇒ 必须反查源码(翻译规则见 approveStatusOf)。
		out = append(out, cuberead.Filter("purchase_sheet.approve_status",
			cubeclient.OperatorIn, toAnySlice(approveFlagsFor(q.ApproveStatus))...))
	}
	if q.From != "" {
		out = append(out, cuberead.Filter("purchase_sheet.created_at",
			"gte", dayStart(q.From)))
	}
	if q.To != "" {
		out = append(out, cuberead.Filter("purchase_sheet.created_at",
			"lte", dayEnd(q.To)))
	}
	return out
}

// List 分页查采购单。
func (s *Service) List(ctx context.Context, branchID string, q Query) (*model.PurchaseSheetPage, error) {
	useCube, err := s.readPath(ctx, branchID)
	if err != nil {
		return nil, err
	}
	q = q.normalized()
	if useCube {
		return s.cubeList(ctx, q)
	}
	return s.localList(ctx, branchID, q)
}

// cubeList 走 cube 的采购单列表。
func (s *Service) cubeList(ctx context.Context, q Query) (*model.PurchaseSheetPage, error) {
	rows, total, err := s.reader.LoadCubeQueryPage(ctx, "purchase_sheet", cubeclient.CubeQuery{
		Measures: []string{
			"purchase_sheet.total_amount_yuan",
			"purchase_sheet.total_net_amount_yuan",
		},
		Dimensions: purchaseSheetDims(),
		Filters:    sheetFilters(q),
		Order:      []cubeclient.CubeOrder{{ID: "purchase_sheet.created_at", Order: "desc"}},
		Limit:      q.PageSize,
		Offset:     (q.Page - 1) * q.PageSize,
		Total:      true,
	})
	if err != nil {
		return nil, err
	}
	names, err := s.supplierNames(ctx, rows)
	if err != nil {
		return nil, err
	}
	items := make([]model.PurchaseSheetItem, 0, len(rows))
	for _, r := range rows {
		item := purchaseSheetItemOf(r)
		if n, ok := names[item.SupplierID]; ok {
			item.SupplierName = strPtr(n)
		}
		items = append(items, item)
	}
	return &model.PurchaseSheetPage{
		Items:      items,
		Count:      len(items),
		Page:       q.Page,
		PageSize:   q.PageSize,
		Total:      total,
		DataSource: "cube",
	}, nil
}

// supplierNames 取供应商字典(货号 → 名称)。
//
// ⚠️ cube 的 live↔duck 不许 join(purchase_sheet 是 live、supplier 是 duck)
// ⇒ 单独一趟字典查询(见 internal/salesagg/cubeq 的说明)。
func (s *Service) supplierNames(ctx context.Context, rows []map[string]any) (map[string]string, error) {
	out := map[string]string{}
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		id := cuberead.Str(r, "purchase_sheet.supplier_id")
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	data, _, err := s.reader.LoadCubeQueryPage(ctx, "supplier", cubeclient.CubeQuery{
		Measures:   []string{"supplier.count"},
		Dimensions: []string{"supplier.id", "supplier.name"},
		Filters:    []cubeclient.CubeFilter{cuberead.InFilter("supplier.id", ids)},
		Limit:      len(ids) * 2,
	})
	if err != nil {
		return nil, err
	}
	for _, r := range data {
		id := cuberead.Str(r, "supplier.id")
		if id == "" {
			continue
		}
		out[id] = cuberead.Str(r, "supplier.name")
	}
	return out, nil
}

// purchaseSheetItemOf 把 cube 行组装成响应项。
func purchaseSheetItemOf(r map[string]any) model.PurchaseSheetItem {
	total := cuberead.Dec(r, "purchase_sheet.total_amount_yuan")
	net := cuberead.Dec(r, "purchase_sheet.total_net_amount_yuan")
	item := model.PurchaseSheetItem{
		SheetNo:           cuberead.Str(r, "purchase_sheet.id"),
		BranchID:          cuberead.Str(r, "purchase_sheet.branch_id"),
		SupplierID:        cuberead.Str(r, "purchase_sheet.supplier_id"),
		DocumentType:      cuberead.Str(r, "purchase_sheet.document_type"),
		DocumentTypeLabel: documentTypeLabel(cuberead.Str(r, "purchase_sheet.document_type")),
		OrderStatus:       cuberead.Str(r, "purchase_sheet.order_status"),
		CreatedAt:         cuberead.Time(cuberead.Str(r, "purchase_sheet.created_at")),
		CreatedBy:         cuberead.Str(r, "purchase_sheet.operator_id"),
		ConfirmedBy:       cuberead.Str(r, "purchase_sheet.confirmer"),
		TotalAmountYuan:    cuberead.Money2Ptr(&total),
		TotalNetAmountYuan: cuberead.Money2Ptr(&net),
		Currency:           normalizeCurrency(cuberead.Str(r, "purchase_sheet.currency")),
		PayWay:             cuberead.Str(r, "purchase_sheet.pay_way"),
		Remark:             cuberead.Str(r, "purchase_sheet.remark"),
	}
	if v := cuberead.Time(cuberead.Str(r, "purchase_sheet.finished_at")); v != "" {
		item.ConfirmedAt = &v
	}
	item.ApproveStatus, item.ApproveStatusLabel = approveStatusOf(
		cuberead.Str(r, "purchase_sheet.approve_status"))
	return item
}

// Get 查单个采购单(头 + 明细)。
func (s *Service) Get(ctx context.Context, branchID, sheetNo string) (*model.PurchaseSheetDetail, error) {
	useCube, err := s.readPath(ctx, branchID)
	if err != nil {
		return nil, err
	}
	sheetNo = strings.TrimSpace(sheetNo)
	if sheetNo == "" {
		return nil, model.ErrPurchaseSheetNotFound
	}
	if useCube {
		return s.cubeGet(ctx, sheetNo)
	}
	return s.localGet(ctx, branchID, sheetNo)
}

// cubeGet 走 cube 的采购单详情。
func (s *Service) cubeGet(ctx context.Context, sheetNo string) (*model.PurchaseSheetDetail, error) {
	rows, _, err := s.reader.LoadCubeQueryPage(ctx, "purchase_sheet", cubeclient.CubeQuery{
		Measures: []string{
			"purchase_sheet.total_amount_yuan",
			"purchase_sheet.total_net_amount_yuan",
		},
		Dimensions: purchaseSheetDims(),
		Filters: []cubeclient.CubeFilter{
			cuberead.Filter("purchase_sheet.id", cubeclient.OperatorEquals, sheetNo),
		},
		Limit: 2,
	})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, model.ErrPurchaseSheetNotFound
	}
	head := purchaseSheetItemOf(rows[0])
	names, err := s.supplierNames(ctx, rows)
	if err != nil {
		return nil, err
	}
	if n, ok := names[head.SupplierID]; ok {
		head.SupplierName = strPtr(n)
	}
	lines, err := s.cubeLines(ctx, sheetNo)
	if err != nil {
		return nil, err
	}
	return &model.PurchaseSheetDetail{PurchaseSheetItem: head, Lines: lines}, nil
}

// cubeLines 查一张采购单的明细行。
//
// ⚠️ 行里**没有** trans_no ⇒ 段(收货 / 退货)由**头**的 document_type 决定,
// 这里不重复判段(重复判必然有一处口径错)。
func (s *Service) cubeLines(ctx context.Context, sheetNo string) ([]model.PurchaseSheetLine, error) {
	rows, _, err := s.reader.LoadCubeQueryPage(ctx, "purchase_sheet_line", cubeclient.CubeQuery{
		Measures: []string{
			"purchase_sheet_line.total_amount_yuan",
			"purchase_sheet_line.total_received_quantity",
			"purchase_sheet_line.total_order_quantity",
			"purchase_sheet_line.avg_valid_price_yuan",
		},
		Dimensions: purchaseSheetLineDims(),
		Filters: []cubeclient.CubeFilter{
			cuberead.Filter("purchase_sheet_line.sheet_id", cubeclient.OperatorEquals, sheetNo),
		},
		Order: []cubeclient.CubeOrder{{ID: "purchase_sheet_line.id", Order: "asc"}},
		Limit: 2000,
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, cuberead.Str(r, "purchase_sheet_line.product_id"))
	}
	names, units, err := s.productDict(ctx, ids)
	if err != nil {
		return nil, err
	}
	lines := make([]model.PurchaseSheetLine, 0, len(rows))
	for i, r := range rows {
		orderQty := cuberead.Dec(r, "purchase_sheet_line.total_order_quantity")
		recvQty := cuberead.Dec(r, "purchase_sheet_line.total_received_quantity")
		price := cuberead.Dec(r, "purchase_sheet_line.avg_valid_price_yuan")
		amount := cuberead.Dec(r, "purchase_sheet_line.total_amount_yuan")
		pid := cuberead.Str(r, "purchase_sheet_line.product_id")
		line := model.PurchaseSheetLine{
			LineNo:         i + 1,
			ProductID:      pid,
			OrderQty:       cuberead.Money2Ptr(&orderQty),
			ReceivedQty:    cuberead.Money2Ptr(&recvQty),
			ValidPriceYuan: cuberead.Money2Ptr(&price),
			AmountYuan:     cuberead.Money2Ptr(&amount),
		}
		if n, ok := names[pid]; ok {
			line.ProductName = strPtr(n)
		}
		if u, ok := units[pid]; ok {
			line.Unit = strPtr(u)
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// productDict 取商品名 / 单位字典(货号 → 名称 / 单位)。
func (s *Service) productDict(ctx context.Context, ids []string) (map[string]string, map[string]string, error) {
	names := map[string]string{}
	units := map[string]string{}
	if len(ids) == 0 {
		return names, units, nil
	}
	data, _, err := s.reader.LoadCubeQueryPage(ctx, "product", cubeclient.CubeQuery{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.id", "product.name", "product.unit"},
		Filters:    []cubeclient.CubeFilter{cuberead.InFilter("product.id", ids)},
		Limit:      len(ids) * 2,
	})
	if err != nil {
		return nil, nil, err
	}
	for _, r := range data {
		id := cuberead.Str(r, "product.id")
		if id == "" {
			continue
		}
		names[id] = cuberead.Str(r, "product.name")
		if u := cuberead.Str(r, "product.unit"); u != "" {
			units[id] = u
		}
	}
	return names, units, nil
}

// localList 走本域 PG 的采购单列表。
func (s *Service) localList(ctx context.Context, branchID string, q Query) (*model.PurchaseSheetPage, error) {
	if s.db == nil {
		return nil, model.ErrLocalStoreNotReady
	}
	tx := s.db.WithContext(ctx).Model(&model.PurchaseSheet{}).Where("branch_id = ?", branchID)
	if q.BranchID != "" {
		tx = tx.Where("source_branch_no = ?", q.BranchID)
	}
	if q.SupplierID != "" {
		tx = tx.Where("supplier_id = ?", q.SupplierID)
	}
	if q.OrderNo != "" {
		tx = tx.Where("sheet_no = ?", q.OrderNo)
	}
	if q.DocumentType != "" {
		tx = tx.Where("document_type = ?", q.DocumentType)
	}
	if q.ApproveStatus != "" {
		tx = tx.Where("approve_status = ?", q.ApproveStatus)
	}
	if q.From != "" {
		tx = tx.Where("created_at >= ?", dayStart(q.From))
	}
	if q.To != "" {
		tx = tx.Where("created_at <= ?", dayEnd(q.To))
	}
	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, err
	}
	var rows []model.PurchaseSheet
	if err := tx.Order("created_at DESC, sheet_no").
		Offset((q.Page - 1) * q.PageSize).
		Limit(q.PageSize).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]model.PurchaseSheetItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, purchaseSheetItemOfLocal(r))
	}
	return &model.PurchaseSheetPage{
		Items:      items,
		Count:      len(items),
		Page:       q.Page,
		PageSize:   q.PageSize,
		Total:      int(total),
		DataSource: "local",
	}, nil
}

// purchaseSheetItemOfLocal 把本地行组装成响应项(**同款结构**)。
func purchaseSheetItemOfLocal(r model.PurchaseSheet) model.PurchaseSheetItem {
	total := r.TotalAmountYuan
	net := r.TotalNetAmountYuan
	item := model.PurchaseSheetItem{
		SheetNo:           r.SheetNo,
		BranchID:          r.SourceBranch,
		SupplierID:        r.SupplierID,
		DocumentType:      r.DocumentType,
		DocumentTypeLabel: documentTypeLabel(r.DocumentType),
		ApproveStatus:     r.ApproveStatus,
		ApproveStatusLabel: approveStatusLabel(r.ApproveStatus),
		OrderStatus:       r.OrderStatus,
		CreatedBy:         r.CreatedBy,
		ConfirmedBy:       r.ConfirmedBy,
		TotalAmountYuan:    cuberead.Money2Ptr(&total),
		TotalNetAmountYuan: cuberead.Money2Ptr(&net),
		Currency:           normalizeCurrency(r.Currency),
		PayWay:             r.PayWay,
		Remark:             r.Remark,
	}
	if r.BranchName != "" {
		item.BranchName = strPtr(r.BranchName)
	}
	if r.SupplierName != "" {
		item.SupplierName = strPtr(r.SupplierName)
	}
	if r.CreatedAt != nil {
		item.CreatedAt = r.CreatedAt.Format(time.RFC3339)
	}
	if r.ConfirmedAt != nil {
		v := r.ConfirmedAt.Format(time.RFC3339)
		item.ConfirmedAt = &v
	}
	return item
}

// localGet 走本域 PG 的采购单详情。
func (s *Service) localGet(ctx context.Context, branchID, sheetNo string) (*model.PurchaseSheetDetail, error) {
	if s.db == nil {
		return nil, model.ErrLocalStoreNotReady
	}
	var row model.PurchaseSheet
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND sheet_no = ?", branchID, sheetNo).
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return nil, model.ErrPurchaseSheetNotFound
	}
	if err != nil {
		return nil, err
	}
	var lineRows []model.PurchaseSheetLineRow
	if err := s.db.WithContext(ctx).Model(&model.PurchaseSheetLineRow{}).
		Where("branch_id = ? AND sheet_id = ?", branchID, sheetNo).
		Order("line_no").
		Find(&lineRows).Error; err != nil {
		return nil, err
	}
	lines := make([]model.PurchaseSheetLine, 0, len(lineRows))
	for _, r := range lineRows {
		orderQty := r.OrderQty
		recvQty := r.ReceivedQty
		price := r.ValidPriceYuan
		amount := r.AmountYuan
		line := model.PurchaseSheetLine{
			LineNo:         r.LineNo,
			ProductID:      r.ProductID,
			OrderQty:       cuberead.Money2Ptr(&orderQty),
			ReceivedQty:    cuberead.Money2Ptr(&recvQty),
			ValidPriceYuan: cuberead.Money2Ptr(&price),
			AmountYuan:     cuberead.Money2Ptr(&amount),
		}
		if r.ProductName != "" {
			line.ProductName = strPtr(r.ProductName)
		}
		if r.Unit != "" {
			line.Unit = strPtr(r.Unit)
		}
		lines = append(lines, line)
	}
	head := purchaseSheetItemOfLocal(row)
	return &model.PurchaseSheetDetail{PurchaseSheetItem: head, Lines: lines}, nil
}

// ---- 枚举翻译 ----

// documentTypeLabel 把单据类型代码翻成中文标签。
//
// ⚠️ 前端**不硬编码**这份文案(由后端下发),
// 否则两边各维护一份 ⇒ 迟早有一处写成"采购进货单"。
func documentTypeLabel(t string) string {
	switch strings.TrimSpace(t) {
	case "PI":
		return "采购收货单"
	case "RO":
		return "采购退货单"
	case "RT":
		return "采购退货通知单"
	case "PO":
		return "采购订单"
	case "WP":
		return "线上采购订单"
	case "XJ":
		return "询价单"
	case "YX":
		return "永续订单"
	default:
		return "未知单据"
	}
}

// approveStatusOf 把 cube 的审核状态("0" / "1")翻译成 (状态, 中文标签)。
//
// ⚠️ cube 侧存的是源码 ⇒ 这里**必须**翻译,不许透传
// (前端拿到 "1" 不知道是什么状态)。
func approveStatusOf(flag string) (string, string) {
	switch strings.TrimSpace(flag) {
	case "1":
		return "approved", "已审核"
	case "0":
		return "draft", "未审核"
	default:
		return "unknown", "未知"
	}
}

// approveStatusLabel 给译文状态配中文标签。
func approveStatusLabel(status string) string {
	switch strings.TrimSpace(status) {
	case "approved":
		return "已审核"
	case "draft":
		return "未审核"
	default:
		return "未知"
	}
}

// approveFlagsFor 反查译文状态对应的**源码**集合。
func approveFlagsFor(status string) []string {
	switch strings.TrimSpace(status) {
	case "approved":
		return []string{"1"}
	case "draft":
		return []string{"0"}
	default:
		return []string{"0", "1"}
	}
}

// normalizeCurrency 把源库的币种代码归一成前端用的写法。
//
// ⚠️ 源列 coin_no 实测恒为 "RMB",而 PLAN §3.4 的示例写的是 "CNY"
// ⇒ 这里做一次显式映射(不做"看起来差不多"的替换)。
func normalizeCurrency(c string) string {
	switch strings.ToUpper(strings.TrimSpace(c)) {
	case "RMB", "CNY":
		return "CNY"
	case "":
		return "CNY"
	default:
		return strings.ToUpper(strings.TrimSpace(c))
	}
}

// toAnySlice 把 []string 转成 []any(cube filter 的 values 是 []any)。
func toAnySlice(in []string) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}

// strPtr 取字符串的指针。
func strPtr(s string) *string { return &s }

// dayStart / dayEnd 把 "YYYY-MM-DD" 补成一天的起止。
func dayStart(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 10 {
		return s + " 00:00:00"
	}
	return s
}

func dayEnd(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 10 {
		return s + " 23:59:59"
	}
	return s
}
