// Package service / archive.go —— stocktake 域的**归档导出 + 批量导入**。
//
// # 端点
//
//	GET  /archive/export?scope=headers|lines|line_operations|plan_items
//	POST /archive/import
//
// # 这一域**不在 cube 里**
//
//	cube 只有商品/分类/供应商/条码/价格/促销/库存/会员/会员类别 +
//	销售结算类 model;盘点单 / 盘点明细 / 操作历史是**本域自己产生的**
//	⇒ 读端点永远走本地。
//
//	但导入导出仍按同一套范式走(见 internal/archiveroute):
//	  - 没配 cube 源 ⇒ 导本地 / 写本地;
//	  - 配了 cube 源 ⇒ 导出**空数据** + 导入**拒绝(409 只读)**。
//
// # ⚠️ 导出的字段取舍
//
//	本域的表**没有凭据列**(没有口令 / 密钥 / 人脸特征):
//	盘点单里存的是"谁在什么时候盘了多少",不是任何人的凭证。
//
//	**软删的盘点明细不导出**:stocktake_lines 用 gorm.DeletedAt 软删,
//	被删掉的行是"这条明细被撤回了",不是"有一条明细"。
//	⇒ 导出时 GORM 自动过滤(ArchiveService 走普通查询)。
//
// # ⚠️ 导入 = **数据恢复**,不是业务重放
//
//	只写数据:不调 AddLine / UpdateLine(那些会写操作历史、算差异、
//	发 dapr 事件),不改库存,不动状态机。
//	⇒ 恢复出来的盘点单停在**它被导出的那个状态**(counting/adjusted/approved),
//	不会被"提交"或"审核"流程继续推进。
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
	"github.com/YunBright/supertrade/internal/stocktake/model"
)

// 归档的资料域(scope)名。
const (
	// ScopeHeaders 是盘点单头(stocktake_headers)。
	ScopeHeaders = "headers"
	// ScopeLines 是盘点明细(stocktake_lines)。
	ScopeLines = "lines"
	// ScopeLineOperations 是明细操作历史(stocktake_line_operations)。
	ScopeLineOperations = "line_operations"
	// ScopePlanItems 是计划盘点商品(stocktake_plan_items)。
	ScopePlanItems = "plan_items"
)

// 归档端点的守卫 scope(与 auth 的 permissions seed 逐字相同)。
const (
	// ScopeInventoryView 是导出(inventory:view,仓管 view)。
	ScopeInventoryView = "inventory:view"
	// ScopeInventoryManage 是导入(inventory:manage,仓管)。
	ScopeInventoryManage = "inventory:manage"
)

// ArchiveService 是 stocktake 的归档导出 / 批量导入服务。
type ArchiveService struct {
	db *gorm.DB
	// resolver 解析"这个门店配了哪个数据源"。
	resolver archiveroute.Resolver
	now      func() time.Time
}

// NewArchiveService 构造 ArchiveService。
func NewArchiveService(db *gorm.DB) *ArchiveService {
	return &ArchiveService{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// SetSourceResolver 注入数据源解析器。
func (a *ArchiveService) SetSourceResolver(r archiveroute.Resolver) {
	a.resolver = r
}

// ---- 导出行类型(本域自己的 DTO,不扩 archivedto)----

// HeaderRow 是盘点单头的导出行。
type HeaderRow struct {
	ID                  string `json:"id"`
	CountDate           string `json:"count_date"`
	Status              string `json:"status"`
	Type                string `json:"type"`
	OperatorID          string `json:"operator_id"`
	AuditorID           string `json:"auditor_id"`
	ParentHeaderID      string `json:"parent_header_id"`
	TotalDiffQty        string `json:"total_diff_qty"`
	TotalDiffAmountYuan string `json:"total_diff_amount_yuan"`
	Remark              string `json:"remark"`
	CreatedAt           string `json:"created_at"`
	UpdatedAt           string `json:"updated_at"`
}

// LineRow 是盘点明细的导出行。
type LineRow struct {
	ID             string `json:"id"`
	HeaderID       string `json:"header_id"`
	ProductID      string `json:"product_id"`
	ProductName    string `json:"product_name"`
	Unit           string `json:"unit"`
	Spec           string `json:"spec"`
	BookQty        string `json:"book_qty"`
	BookQtyAt      string `json:"book_qty_at"`
	ActualQty      string `json:"actual_qty"`
	DiffQty        string `json:"diff_qty"`
	AvgCostYuan    string `json:"avg_cost_yuan"`
	DiffAmountYuan string `json:"diff_amount_yuan"`
	DiffReason     string `json:"diff_reason"`
	Remark         string `json:"remark"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// LineOperationRow 是明细操作历史的导出行。
type LineOperationRow struct {
	ID        string `json:"id"`
	HeaderID  string `json:"header_id"`
	LineID    string `json:"line_id"`
	OpType    string `json:"op_type"`
	ActorID   string `json:"actor_id"`
	ActorName string `json:"actor_name"`
	PrevQty   string `json:"prev_qty"`
	NewQty    string `json:"new_qty"`
	QtyDelta  string `json:"qty_delta"`
	OpAt      string `json:"op_at"`
	Method    string `json:"method"`
	Remark    string `json:"remark"`
	CreatedAt string `json:"created_at"`
}

// PlanItemRow 是计划盘点商品的导出行。
type PlanItemRow struct {
	ID          string `json:"id"`
	HeaderID    string `json:"header_id"`
	ProductID   string `json:"product_id"`
	ProductName string `json:"product_name"`
	Unit        string `json:"unit"`
	Barcode     string `json:"barcode"`
	SortOrder   int    `json:"sort_order"`
	CreatedAt   string `json:"created_at"`
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
	Headers        []HeaderRow        `json:"headers"`
	Lines          []LineRow          `json:"lines"`
	LineOperations []LineOperationRow `json:"line_operations"`
	PlanItems      []PlanItemRow      `json:"plan_items"`
	// Count 是本次 scope 的行数。
	Count int `json:"count"`
}

// ImportRequest 是导入请求体(与导出响应同形 ⇒ 可往返)。
type ImportRequest struct {
	Scope string `json:"scope"`
	// 各 scope 的行;本次只处理 Scope 指向的那一个。
	Headers        []HeaderRow        `json:"headers,omitempty"`
	Lines          []LineRow          `json:"lines,omitempty"`
	LineOperations []LineOperationRow `json:"line_operations,omitempty"`
	PlanItems      []PlanItemRow      `json:"plan_items,omitempty"`
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
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidStatus)
	}
	scope = strings.TrimSpace(scope)
	switch scope {
	case ScopeHeaders, ScopeLines, ScopeLineOperations, ScopePlanItems:
	default:
		return nil, fmt.Errorf("%w: scope=%q 非法", ErrInvalidStatus, scope)
	}

	mode, err := archiveroute.DecideExport(ctx, a.resolver, branchID)
	if err != nil {
		return nil, err
	}

	out := &ExportResult{
		Scope:          scope,
		BranchID:       branchID,
		External:       mode == archiveroute.ModeExternal,
		Headers:        []HeaderRow{},
		Lines:          []LineRow{},
		LineOperations: []LineOperationRow{},
		PlanItems:      []PlanItemRow{},
	}
	if mode == archiveroute.ModeExternal {
		// ⚠️ 空,**不回落本地**(见文件头)。
		return out, nil
	}

	switch scope {
	case ScopeHeaders:
		rows, err := a.exportHeaders(ctx, branchID)
		if err != nil {
			return nil, err
		}
		out.Headers = rows
		out.Count = len(rows)
	case ScopeLines:
		ids, err := a.branchHeaderIDs(ctx, branchID)
		if err != nil {
			return nil, err
		}
		rows, err := a.exportLines(ctx, ids)
		if err != nil {
			return nil, err
		}
		out.Lines = rows
		out.Count = len(rows)
	case ScopeLineOperations:
		ids, err := a.branchHeaderIDs(ctx, branchID)
		if err != nil {
			return nil, err
		}
		rows, err := a.exportLineOperations(ctx, ids)
		if err != nil {
			return nil, err
		}
		out.LineOperations = rows
		out.Count = len(rows)
	case ScopePlanItems:
		ids, err := a.branchHeaderIDs(ctx, branchID)
		if err != nil {
			return nil, err
		}
		rows, err := a.exportPlanItems(ctx, ids)
		if err != nil {
			return nil, err
		}
		out.PlanItems = rows
		out.Count = len(rows)
	}
	return out, nil
}

// branchHeaderIDs 取该门店的盘点单 id(从表按它过滤)。
func (a *ArchiveService) branchHeaderIDs(ctx context.Context, branchID string) ([]string, error) {
	var ids []string
	err := a.db.WithContext(ctx).Model(&model.StocktakeHeader{}).
		Where("branch_id = ?", branchID).
		Pluck("id", &ids).Error
	return ids, err
}

// exportHeaders 导出盘点单头(按 branch_id 过滤)。
func (a *ArchiveService) exportHeaders(ctx context.Context, branchID string) ([]HeaderRow, error) {
	var rows []model.StocktakeHeader
	if err := a.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("count_date desc, id desc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]HeaderRow, 0, len(rows))
	for _, r := range rows {
		row := HeaderRow{
			ID:                  r.ID,
			CountDate:           r.CountDate.Format("2006-01-02"),
			Status:              string(r.Status),
			Type:                string(r.Type),
			OperatorID:          r.OperatorID,
			AuditorID:           r.AuditorID,
			TotalDiffQty:        r.TotalDiffQty.String(),
			TotalDiffAmountYuan: r.TotalDiffAmountYuan.String(),
			Remark:              r.Remark,
			CreatedAt:           r.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:           r.UpdatedAt.UTC().Format(time.RFC3339),
		}
		if r.ParentHeaderID != nil {
			row.ParentHeaderID = *r.ParentHeaderID
		}
		out = append(out, row)
	}
	return out, nil
}

// exportLines 导出盘点明细(按 header_id 关联到该门店)。
func (a *ArchiveService) exportLines(ctx context.Context, headerIDs []string) ([]LineRow, error) {
	out := []LineRow{}
	if len(headerIDs) == 0 {
		return out, nil
	}
	var rows []model.StocktakeLine
	if err := a.db.WithContext(ctx).
		Where("header_id IN ?", headerIDs).
		Order("header_id asc, id asc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		row := LineRow{
			ID:             r.ID,
			HeaderID:       r.HeaderID,
			ProductID:      r.ProductID,
			ProductName:    r.ProductName,
			Unit:           r.Unit,
			Spec:           r.Spec,
			BookQty:        r.BookQty.String(),
			BookQtyAt:      r.BookQtyAt.UTC().Format(time.RFC3339),
			ActualQty:      r.ActualQty.String(),
			DiffQty:        r.DiffQty.String(),
			AvgCostYuan:    r.AvgCostYuan.String(),
			DiffAmountYuan: r.DiffAmountYuan.String(),
			DiffReason:     string(r.DiffReason),
			CreatedAt:      r.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:      r.UpdatedAt.UTC().Format(time.RFC3339),
		}
		if r.Remark != nil {
			row.Remark = *r.Remark
		}
		out = append(out, row)
	}
	return out, nil
}

// exportLineOperations 导出明细操作历史(按 header_id 关联到该门店)。
func (a *ArchiveService) exportLineOperations(ctx context.Context, headerIDs []string) ([]LineOperationRow, error) {
	out := []LineOperationRow{}
	if len(headerIDs) == 0 {
		return out, nil
	}
	var rows []model.StocktakeLineOperation
	if err := a.db.WithContext(ctx).
		Where("header_id IN ?", headerIDs).
		Order("op_at asc, id asc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, LineOperationRow{
			ID:        r.ID,
			HeaderID:  r.HeaderID,
			LineID:    r.LineID,
			OpType:    string(r.OpType),
			ActorID:   r.ActorID,
			ActorName: r.ActorName,
			PrevQty:   r.PrevQty.String(),
			NewQty:    r.NewQty.String(),
			QtyDelta:  r.QtyDelta.String(),
			OpAt:      r.OpAt.UTC().Format(time.RFC3339),
			Method:    string(r.Method),
			Remark:    r.Remark,
			CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// exportPlanItems 导出计划盘点商品(按 header_id 关联到该门店)。
func (a *ArchiveService) exportPlanItems(ctx context.Context, headerIDs []string) ([]PlanItemRow, error) {
	out := []PlanItemRow{}
	if len(headerIDs) == 0 {
		return out, nil
	}
	var rows []model.StocktakePlanItem
	if err := a.db.WithContext(ctx).
		Where("header_id IN ?", headerIDs).
		Order("header_id asc, sort_order asc, id asc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, PlanItemRow{
			ID:          r.ID,
			HeaderID:    r.HeaderID,
			ProductID:   r.ProductID,
			ProductName: r.ProductName,
			Unit:        r.Unit,
			Barcode:     r.Barcode,
			SortOrder:   r.SortOrder,
			CreatedAt:   r.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// Import 把导出的行**幂等 upsert** 回本域表。
//
// # ⚠️ 这是「数据恢复」,不是「业务重放」
//
//	不调 AddLine / UpdateLine / Submit / Approve:
//	那些会写操作历史、重算差异、发 dapr 事件、推进状态机。
//	导入只把行写回去,恢复到"导出那一刻"的样子。
//
// # 幂等
//
//	按**主键** upsert(各表自己的主键)。重复导入同一份归档 ⇒ 行数不变、内容不变。
func (a *ArchiveService) Import(ctx context.Context, branchID string, req ImportRequest) (*ImportResult, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidStatus)
	}
	scope := strings.TrimSpace(req.Scope)
	switch scope {
	case ScopeHeaders, ScopeLines, ScopeLineOperations, ScopePlanItems:
	default:
		return nil, fmt.Errorf("%w: scope=%q 非法", ErrInvalidStatus, scope)
	}

	// 判据:配了 cube 源 ⇒ 本域只读(409);解析失败 ⇒ 放行(导入路径自己兜底)。
	mode, err := archiveroute.DecideImport(ctx, a.resolver, branchID)
	if err != nil {
		slog.Warn("stocktake: 问不到门店数据源,导入按本域处理",
			"branch_id", branchID, "err", err)
		mode = archiveroute.ModeLocal
	}
	if mode == archiveroute.ModeExternal {
		return nil, fmt.Errorf("%w: 门店 %s 的盘点数据来自外部数据源,请在原系统操作",
			archiveroute.ErrExternalReadOnly, branchID)
	}

	res := &ImportResult{Scope: scope, BranchID: branchID}
	switch scope {
	case ScopeHeaders:
		a.importHeaders(ctx, branchID, req.Headers, res)
	case ScopeLines:
		a.importLines(ctx, branchID, req.Lines, res)
	case ScopeLineOperations:
		a.importLineOperations(ctx, branchID, req.LineOperations, res)
	case ScopePlanItems:
		a.importPlanItems(ctx, branchID, req.PlanItems, res)
	}
	return res, nil
}

// importHeaders 幂等写入盘点单头。
func (a *ArchiveService) importHeaders(ctx context.Context, branchID string, rows []HeaderRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.StocktakeHeader
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
		countDate, err := time.Parse("2006-01-02", strings.TrimSpace(r.CountDate))
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "count_date 不是 YYYY-MM-DD"})
			continue
		}
		status := model.StocktakeStatus(strings.TrimSpace(r.Status))
		if status == "" {
			status = model.StatusCounting
		}
		tt := model.StocktakeType(strings.TrimSpace(r.Type))
		if tt == "" {
			tt = model.TypeGeneral
		}
		var parent *string
		if p := strings.TrimSpace(r.ParentHeaderID); p != "" {
			parent = &p
		}
		row := model.StocktakeHeader{
			ID:                  id,
			BranchID:            branchID,
			CountDate:           countDate,
			Status:              status,
			Type:                tt,
			OperatorID:          r.OperatorID,
			AuditorID:           r.AuditorID,
			ParentHeaderID:      parent,
			TotalDiffQty:        parseDecimalLoose(r.TotalDiffQty),
			TotalDiffAmountYuan: parseDecimalLoose(r.TotalDiffAmountYuan),
			Remark:              r.Remark,
			CreatedAt:           parseRFC3339(r.CreatedAt, a.now()),
			UpdatedAt:           parseRFC3339(r.UpdatedAt, a.now()),
		}
		err = a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"branch_id", "count_date", "status", "type", "operator_id", "auditor_id",
				"parent_header_id", "total_diff_qty", "total_diff_amount_yuan",
				"remark", "created_at", "updated_at",
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

// importLines 幂等写入盘点明细。
//
// ⚠️ **不写操作历史**:恢复出来的明细没有"谁在什么时候改的"这条轨迹
// (那是 AddLine / UpdateLine 的职责,会触发 dapr 事件)。
func (a *ArchiveService) importLines(ctx context.Context, branchID string, rows []LineRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.StocktakeLine
	if err := a.db.WithContext(ctx).Where("id IN ?", ids).Find(&found).Error; err == nil {
		for _, f := range found {
			existing[f.ID] = true
		}
	}
	_ = branchID // 从表无 branch_id,归属靠 header_id

	for _, r := range rows {
		id := strings.TrimSpace(r.ID)
		headerID := strings.TrimSpace(r.HeaderID)
		productID := strings.TrimSpace(r.ProductID)
		if id == "" || headerID == "" || productID == "" {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "id / header_id / product_id 必填"})
			continue
		}
		bookQtyAt, err := time.Parse(time.RFC3339, strings.TrimSpace(r.BookQtyAt))
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "book_qty_at 不是 RFC3339 时间"})
			continue
		}
		var remark *string
		if s := strings.TrimSpace(r.Remark); s != "" {
			remark = &s
		}
		row := model.StocktakeLine{
			ID:             id,
			HeaderID:       headerID,
			ProductID:      productID,
			ProductName:    r.ProductName,
			Unit:           r.Unit,
			Spec:           r.Spec,
			BookQty:        parseDecimalLoose(r.BookQty),
			BookQtyAt:      bookQtyAt,
			ActualQty:      parseDecimalLoose(r.ActualQty),
			DiffQty:        parseDecimalLoose(r.DiffQty),
			AvgCostYuan:    parseDecimalLoose(r.AvgCostYuan),
			DiffAmountYuan: parseDecimalLoose(r.DiffAmountYuan),
			DiffReason:     model.DiffReason(r.DiffReason),
			Remark:         remark,
			CreatedAt:      parseRFC3339(r.CreatedAt, a.now()),
			UpdatedAt:      parseRFC3339(r.UpdatedAt, a.now()),
		}
		err = a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"header_id", "product_id", "product_name", "unit", "spec",
				"book_qty", "book_qty_at", "actual_qty", "diff_qty",
				"avg_cost_yuan", "diff_amount_yuan", "diff_reason", "remark",
				"created_at", "updated_at",
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

// importLineOperations 幂等写入明细操作历史。
func (a *ArchiveService) importLineOperations(ctx context.Context, branchID string, rows []LineOperationRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.StocktakeLineOperation
	if err := a.db.WithContext(ctx).Where("id IN ?", ids).Find(&found).Error; err == nil {
		for _, f := range found {
			existing[f.ID] = true
		}
	}
	_ = branchID // 从表无 branch_id,归属靠 header_id

	for _, r := range rows {
		id := strings.TrimSpace(r.ID)
		headerID := strings.TrimSpace(r.HeaderID)
		lineID := strings.TrimSpace(r.LineID)
		if id == "" || headerID == "" || lineID == "" {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "id / header_id / line_id 必填"})
			continue
		}
		opAt, err := time.Parse(time.RFC3339, strings.TrimSpace(r.OpAt))
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "op_at 不是 RFC3339 时间"})
			continue
		}
		row := model.StocktakeLineOperation{
			ID:        id,
			HeaderID:  headerID,
			LineID:    lineID,
			OpType:    model.LineOpType(r.OpType),
			ActorID:   r.ActorID,
			ActorName: r.ActorName,
			PrevQty:   parseDecimalLoose(r.PrevQty),
			NewQty:    parseDecimalLoose(r.NewQty),
			QtyDelta:  parseDecimalLoose(r.QtyDelta),
			OpAt:      opAt,
			Method:    model.OpMethod(r.Method),
			Remark:    r.Remark,
			CreatedAt: parseRFC3339(r.CreatedAt, a.now()),
		}
		err = a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"header_id", "line_id", "op_type", "actor_id", "actor_name",
				"prev_qty", "new_qty", "qty_delta", "op_at", "method", "remark",
				"created_at",
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

// importPlanItems 幂等写入计划盘点商品。
func (a *ArchiveService) importPlanItems(ctx context.Context, branchID string, rows []PlanItemRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.StocktakePlanItem
	if err := a.db.WithContext(ctx).Where("id IN ?", ids).Find(&found).Error; err == nil {
		for _, f := range found {
			existing[f.ID] = true
		}
	}
	_ = branchID // 从表无 branch_id,归属靠 header_id

	for _, r := range rows {
		id := strings.TrimSpace(r.ID)
		headerID := strings.TrimSpace(r.HeaderID)
		productID := strings.TrimSpace(r.ProductID)
		if id == "" || headerID == "" || productID == "" {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "id / header_id / product_id 必填"})
			continue
		}
		row := model.StocktakePlanItem{
			ID:          id,
			HeaderID:    headerID,
			ProductID:   productID,
			ProductName: r.ProductName,
			Unit:        r.Unit,
			Barcode:     r.Barcode,
			SortOrder:   r.SortOrder,
			CreatedAt:   parseRFC3339(r.CreatedAt, a.now()),
		}
		err := a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"header_id", "product_id", "product_name", "unit", "barcode",
				"sort_order", "created_at",
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
//
// ⚠️ 数量解析不了按 0(而不是跳整行):盘点明细的身份(哪个商品、哪张单)
//
//	比数量更重要,丢掉整行会让"这张单少了一个商品"无从查起。
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
