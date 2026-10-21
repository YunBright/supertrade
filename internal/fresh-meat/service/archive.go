// Package service / archive.go —— fresh-meat 域的**归档导出 + 批量导入**。
//
// # 端点
//
//	GET  /archive/export?scope=whole_pigs|pig_cuts|pork_cuts_stocktakes|
//	                         line_sales_by_pigs|branch_cut_mappings|waste_logs
//	POST /archive/import
//
// # 这一域**不在 cube 里**
//
//	cube 只有商品/分类/供应商/条码/价格/促销/库存/会员/会员类别 +
//	销售结算类 model;整猪/部位/日终盘点/按猪聚合的销售/部位映射/报损
//	是**本域自己产生的** ⇒ 读端点永远走本地。
//
//	但导入导出仍按同一套范式走(见 internal/archiveroute):
//	  - 没配 cube 源 ⇒ 导本地 / 写本地;
//	  - 配了 cube 源 ⇒ 导出**空数据** + 导入**拒绝(409 只读)**。
//
// # ⚠️ 导出的字段取舍
//
//	本域的表**没有凭据列**(没有口令 / 密钥 / 人脸特征)。
//
//	**排除**两个 LLM 产物列:
//	  - whole_pigs.llm_advice_json   —— 整猪的分割建议(含原始 prompt 片段);
//	  - pork_cuts_stocktakes.llm_review_json —— 日终盘点的 LLM 复核。
//
//	它们是**派生的调试产物**:体积不受控、内含原始 prompt 的片段,
//	而且下一次跑 LLM 就会被重写 ⇒ 导出它们只会让归档文件更大、敏感面更宽,
//	恢复它们也不会让任何业务判断更准。
//
//	`data_source`(stub/llm/history_avg/erp)是**怎么来的**的事实,导出。
//
// # ⚠️ 导入 = **数据恢复**,不是业务重放
//
//	只写数据:不调 RecordWholePig(那会**同步调 LLM** 并回写 llm_advice_json)、
//	不调 RecordPigCut / RecordPorkCutsStocktake / RecordWasteLog
//	(那些会发 dapr 事件 pork.cuts.stocktaken / waste.log.recorded)、
//	不重算毛利、不改门店的部位映射。
//
//	⇒ 恢复出来的数据停在**它被导出的那个状态**上。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/archiveroute"
	"github.com/YunBright/supertrade/internal/fresh-meat/model"
)

// 归档的资料域(scope)名。
const (
	// ScopeWholePigs 是整猪(whole_pigs)。
	ScopeWholePigs = "whole_pigs"
	// ScopePigCuts 是部位(pig_cuts)。
	ScopePigCuts = "pig_cuts"
	// ScopePorkCutsStocktakes 是日终按部位盘点(pork_cuts_stocktakes)。
	ScopePorkCutsStocktakes = "pork_cuts_stocktakes"
	// ScopeLineSalesByPigs 是按猪聚合的销售(line_sales_by_pigs)。
	ScopeLineSalesByPigs = "line_sales_by_pigs"
	// ScopeBranchCutMappings 是门店部位 ↔ cube 商品映射(branch_cut_mappings)。
	ScopeBranchCutMappings = "branch_cut_mappings"
	// ScopeWasteLogs 是报损(waste_logs)。
	ScopeWasteLogs = "waste_logs"
)

// 归档端点的守卫 scope(与 auth 的 permissions seed 逐字相同)。
const (
	// ScopeFreshmeatView 是导出(freshmeat:view)。
	ScopeFreshmeatView = "freshmeat:view"
	// ScopeFreshmeatWrite 是导入(freshmeat:write)。
	ScopeFreshmeatWrite = "freshmeat:write"
)

// ArchiveService 是 fresh-meat 的归档导出 / 批量导入服务。
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

// WholePigRow 是整猪的导出行。
//
// ⚠️ **没有** llm_advice_json(见文件头)。
type WholePigRow struct {
	ID                    string `json:"id"`
	EarTag                string `json:"ear_tag"`
	GrossWeightKg         string `json:"gross_weight_kg"`
	PurchaseUnitPriceYuan string `json:"purchase_unit_price_yuan"`
	PurchaseCostYuan      string `json:"purchase_cost_yuan"`
	SupplierID            string `json:"supplier_id"`
	ArrivedAt             string `json:"arrived_at"`
	RecordedBy            string `json:"recorded_by"`
	DataSource            string `json:"data_source"`
	HalfPig               bool   `json:"half_pig"`
	OffalIncluded         bool   `json:"offal_included"`
	PurchaseGroupID       string `json:"purchase_group_id"`
	CreatedAt             string `json:"created_at"`
	UpdatedAt             string `json:"updated_at"`
}

// PigCutRow 是部位的导出行。
type PigCutRow struct {
	ID        string `json:"id"`
	PigID     string `json:"pig_id"`
	CutType   string `json:"cut_type"`
	BranchID  string `json:"branch_id"`
	Barcode   string `json:"barcode"`
	WeightKg  string `json:"weight_kg"`
	ExpiresAt string `json:"expires_at"`
	CreatedAt string `json:"created_at"`
}

// PorkCutsStocktakeRow 是日终按部位盘点的导出行。
//
// ⚠️ **没有** llm_review_json(见文件头)。
type PorkCutsStocktakeRow struct {
	ID         string          `json:"id"`
	TakenAt    string          `json:"taken_at"`
	TakenBy    string          `json:"taken_by"`
	IsComplete bool            `json:"is_complete"`
	Cuts       json.RawMessage `json:"cuts,omitempty"`
	CreatedAt  string          `json:"created_at"`
}

// LineSalesByPigRow 是按猪聚合的销售的导出行。
type LineSalesByPigRow struct {
	ID            uint64 `json:"id"`
	SaleDate      string `json:"sale_date"`
	PigID         string `json:"pig_id"`
	CutType       string `json:"cut_type"`
	QtyKg         string `json:"qty_kg"`
	AmountYuan    string `json:"amount_yuan"`
	UnitPriceYuan string `json:"unit_price_yuan"`
	Source        string `json:"source"`
	PosLineID     string `json:"pos_line_id"`
	PosBranchID   string `json:"pos_branch_id"`
	CubeProductID string `json:"cube_product_id"`
	OrderStatus   string `json:"order_status"`
	CreatedAt     string `json:"created_at"`
}

// BranchCutMappingRow 是门店部位 ↔ cube 商品映射的导出行。
type BranchCutMappingRow struct {
	ID              string `json:"id"`
	CutType         string `json:"cut_type"`
	CubeProductID   string `json:"cube_product_id"`
	CubeProductName string `json:"cube_product_name"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

// WasteLogRow 是报损的导出行。
type WasteLogRow struct {
	ID            string `json:"id"`
	PigID         string `json:"pig_id"`
	CutType       string `json:"cut_type"`
	CubeProductID string `json:"cube_product_id"`
	QtyKg         string `json:"qty_kg"`
	Reason        string `json:"reason"`
	RecordedAt    string `json:"recorded_at"`
	OperatorID    string `json:"operator_id"`
	CreatedAt     string `json:"created_at"`
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
	WholePigs          []WholePigRow          `json:"whole_pigs"`
	PigCuts            []PigCutRow            `json:"pig_cuts"`
	PorkCutsStocktakes []PorkCutsStocktakeRow `json:"pork_cuts_stocktakes"`
	LineSalesByPigs    []LineSalesByPigRow    `json:"line_sales_by_pigs"`
	BranchCutMappings  []BranchCutMappingRow  `json:"branch_cut_mappings"`
	WasteLogs          []WasteLogRow          `json:"waste_logs"`
	// Count 是本次 scope 的行数。
	Count int `json:"count"`
}

// ImportRequest 是导入请求体(与导出响应同形 ⇒ 可往返)。
type ImportRequest struct {
	Scope string `json:"scope"`
	// 各 scope 的行;本次只处理 Scope 指向的那一个。
	WholePigs          []WholePigRow          `json:"whole_pigs,omitempty"`
	PigCuts            []PigCutRow            `json:"pig_cuts,omitempty"`
	PorkCutsStocktakes []PorkCutsStocktakeRow `json:"pork_cuts_stocktakes,omitempty"`
	LineSalesByPigs    []LineSalesByPigRow    `json:"line_sales_by_pigs,omitempty"`
	BranchCutMappings  []BranchCutMappingRow  `json:"branch_cut_mappings,omitempty"`
	WasteLogs          []WasteLogRow          `json:"waste_logs,omitempty"`
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
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	scope = strings.TrimSpace(scope)
	switch scope {
	case ScopeWholePigs, ScopePigCuts, ScopePorkCutsStocktakes,
		ScopeLineSalesByPigs, ScopeBranchCutMappings, ScopeWasteLogs:
	default:
		return nil, fmt.Errorf("%w: scope=%q 非法", ErrInvalidInput, scope)
	}

	mode, err := archiveroute.DecideExport(ctx, a.resolver, branchID)
	if err != nil {
		return nil, err
	}

	out := &ExportResult{
		Scope:              scope,
		BranchID:           branchID,
		External:           mode == archiveroute.ModeExternal,
		WholePigs:          []WholePigRow{},
		PigCuts:            []PigCutRow{},
		PorkCutsStocktakes: []PorkCutsStocktakeRow{},
		LineSalesByPigs:    []LineSalesByPigRow{},
		BranchCutMappings:  []BranchCutMappingRow{},
		WasteLogs:          []WasteLogRow{},
	}
	if mode == archiveroute.ModeExternal {
		// ⚠️ 空,**不回落本地**(见文件头)。
		return out, nil
	}

	switch scope {
	case ScopeWholePigs:
		rows, err := a.exportWholePigs(ctx, branchID)
		if err != nil {
			return nil, err
		}
		out.WholePigs = rows
		out.Count = len(rows)
	case ScopePigCuts:
		rows, err := a.exportPigCuts(ctx, branchID)
		if err != nil {
			return nil, err
		}
		out.PigCuts = rows
		out.Count = len(rows)
	case ScopePorkCutsStocktakes:
		rows, err := a.exportPorkCutsStocktakes(ctx, branchID)
		if err != nil {
			return nil, err
		}
		out.PorkCutsStocktakes = rows
		out.Count = len(rows)
	case ScopeLineSalesByPigs:
		rows, err := a.exportLineSalesByPigs(ctx, branchID)
		if err != nil {
			return nil, err
		}
		out.LineSalesByPigs = rows
		out.Count = len(rows)
	case ScopeBranchCutMappings:
		rows, err := a.exportBranchCutMappings(ctx, branchID)
		if err != nil {
			return nil, err
		}
		out.BranchCutMappings = rows
		out.Count = len(rows)
	case ScopeWasteLogs:
		rows, err := a.exportWasteLogs(ctx, branchID)
		if err != nil {
			return nil, err
		}
		out.WasteLogs = rows
		out.Count = len(rows)
	}
	return out, nil
}

// exportWholePigs 导出整猪(按 branch_id 过滤)。
func (a *ArchiveService) exportWholePigs(ctx context.Context, branchID string) ([]WholePigRow, error) {
	var rows []model.WholePig
	if err := a.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("arrived_at desc, id desc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]WholePigRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, WholePigRow{
			ID:                    r.ID,
			EarTag:                r.EarTag,
			GrossWeightKg:         r.GrossWeightKg.String(),
			PurchaseUnitPriceYuan: r.PurchaseUnitPriceYuan.String(),
			PurchaseCostYuan:      r.PurchaseCostYuan.String(),
			SupplierID:            r.SupplierID,
			ArrivedAt:             r.ArrivedAt.UTC().Format(time.RFC3339),
			RecordedBy:            r.RecordedBy,
			DataSource:            string(r.DataSource),
			HalfPig:               r.HalfPig,
			OffalIncluded:         r.OffalIncluded,
			PurchaseGroupID:       r.PurchaseGroupID,
			CreatedAt:             r.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:             r.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// exportPigCuts 导出部位(按 branch_id 过滤)。
func (a *ArchiveService) exportPigCuts(ctx context.Context, branchID string) ([]PigCutRow, error) {
	var rows []model.PigCut
	if err := a.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("id asc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]PigCutRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, PigCutRow{
			ID:        r.ID,
			PigID:     r.PigID,
			CutType:   string(r.CutType),
			BranchID:  r.BranchID,
			Barcode:   r.Barcode,
			WeightKg:  r.WeightKg.String(),
			ExpiresAt: r.ExpiresAt.UTC().Format(time.RFC3339),
			CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// exportPorkCutsStocktakes 导出日终按部位盘点(按 branch_id 过滤)。
func (a *ArchiveService) exportPorkCutsStocktakes(ctx context.Context, branchID string) ([]PorkCutsStocktakeRow, error) {
	var rows []model.PorkCutsStocktake
	if err := a.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("taken_at desc, id desc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]PorkCutsStocktakeRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, PorkCutsStocktakeRow{
			ID:         r.ID,
			TakenAt:    r.TakenAt.UTC().Format(time.RFC3339),
			TakenBy:    r.TakenBy,
			IsComplete: r.IsComplete,
			Cuts:       json.RawMessage(r.Cuts),
			CreatedAt:  r.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// exportLineSalesByPigs 导出按猪聚合的销售(按 branch_id 过滤)。
func (a *ArchiveService) exportLineSalesByPigs(ctx context.Context, branchID string) ([]LineSalesByPigRow, error) {
	var rows []model.LineSalesByPig
	if err := a.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("id asc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]LineSalesByPigRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, LineSalesByPigRow{
			ID:            r.ID,
			SaleDate:      time.Time(r.SaleDate).Format("2006-01-02"),
			PigID:         r.PigID,
			CutType:       string(r.CutType),
			QtyKg:         r.QtyKg.String(),
			AmountYuan:    r.AmountYuan.String(),
			UnitPriceYuan: r.UnitPriceYuan.String(),
			Source:        r.Source,
			PosLineID:     r.PosLineID,
			PosBranchID:   r.PosBranchID,
			CubeProductID: r.CubeProductID,
			OrderStatus:   r.OrderStatus,
			CreatedAt:     r.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// exportBranchCutMappings 导出门店部位映射(按 branch_id 过滤)。
func (a *ArchiveService) exportBranchCutMappings(ctx context.Context, branchID string) ([]BranchCutMappingRow, error) {
	var rows []model.BranchCutMapping
	if err := a.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("id asc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]BranchCutMappingRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, BranchCutMappingRow{
			ID:              r.ID,
			CutType:         string(r.CutType),
			CubeProductID:   r.CubeProductID,
			CubeProductName: r.CubeProductName,
			CreatedAt:       r.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:       r.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// exportWasteLogs 导出报损(按 branch_id 过滤)。
func (a *ArchiveService) exportWasteLogs(ctx context.Context, branchID string) ([]WasteLogRow, error) {
	var rows []model.WasteLog
	if err := a.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("recorded_at desc, id desc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]WasteLogRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, WasteLogRow{
			ID:            r.ID,
			PigID:         r.PigID,
			CutType:       string(r.CutType),
			CubeProductID: r.CubeProductID,
			QtyKg:         r.QtyKg.String(),
			Reason:        r.Reason,
			RecordedAt:    r.RecordedAt.UTC().Format(time.RFC3339),
			OperatorID:    r.OperatorID,
			CreatedAt:     r.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// Import 把导出的行**幂等 upsert** 回本域表。
//
// # ⚠️ 这是「数据恢复」,不是「业务重放」
//
//	不调 RecordWholePig(会**同步调 LLM**)、不调 RecordPigCut /
//	RecordPorkCutsStocktake / RecordWasteLog(会发 dapr 事件)、
//	不重算毛利、不改门店部位映射。
//
// # 幂等
//
//	按**业务主键** upsert:
//	  - 大部分表用主键 id;
//	  - line_sales_by_pigs 用唯一索引 (pos_line_id, pos_branch_id)
//	    —— 它的主键是自增 bigint,跨库搬运会撞号,
//	    而"同一条 POS 明细在同一门店"才是真正的同一行。
func (a *ArchiveService) Import(ctx context.Context, branchID string, req ImportRequest) (*ImportResult, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	scope := strings.TrimSpace(req.Scope)
	switch scope {
	case ScopeWholePigs, ScopePigCuts, ScopePorkCutsStocktakes,
		ScopeLineSalesByPigs, ScopeBranchCutMappings, ScopeWasteLogs:
	default:
		return nil, fmt.Errorf("%w: scope=%q 非法", ErrInvalidInput, scope)
	}

	// 判据:配了 cube 源 ⇒ 本域只读(409);解析失败 ⇒ 放行(导入路径自己兜底)。
	mode, err := archiveroute.DecideImport(ctx, a.resolver, branchID)
	if err != nil {
		slog.Warn("fresh-meat: 问不到门店数据源,导入按本域处理",
			"branch_id", branchID, "err", err)
		mode = archiveroute.ModeLocal
	}
	if mode == archiveroute.ModeExternal {
		return nil, fmt.Errorf("%w: 门店 %s 的猪肉业务数据来自外部数据源,请在原系统操作",
			archiveroute.ErrExternalReadOnly, branchID)
	}

	res := &ImportResult{Scope: scope, BranchID: branchID}
	switch scope {
	case ScopeWholePigs:
		a.importWholePigs(ctx, branchID, req.WholePigs, res)
	case ScopePigCuts:
		a.importPigCuts(ctx, branchID, req.PigCuts, res)
	case ScopePorkCutsStocktakes:
		a.importPorkCutsStocktakes(ctx, branchID, req.PorkCutsStocktakes, res)
	case ScopeLineSalesByPigs:
		a.importLineSalesByPigs(ctx, branchID, req.LineSalesByPigs, res)
	case ScopeBranchCutMappings:
		a.importBranchCutMappings(ctx, branchID, req.BranchCutMappings, res)
	case ScopeWasteLogs:
		a.importWasteLogs(ctx, branchID, req.WasteLogs, res)
	}
	return res, nil
}

// importWholePigs 幂等写入整猪。
//
// ⚠️ **不写 llm_advice_json**(见文件头);`data_source` 原样恢复。
func (a *ArchiveService) importWholePigs(ctx context.Context, branchID string, rows []WholePigRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.WholePig
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
		arrivedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(r.ArrivedAt))
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "arrived_at 不是 RFC3339 时间"})
			continue
		}
		row := model.WholePig{
			ID:                    id,
			EarTag:                r.EarTag,
			GrossWeightKg:         parseDecimalLoose(r.GrossWeightKg),
			PurchaseUnitPriceYuan: parseDecimalLoose(r.PurchaseUnitPriceYuan),
			PurchaseCostYuan:      parseDecimalLoose(r.PurchaseCostYuan),
			SupplierID:            r.SupplierID,
			BranchID:              branchID,
			ArrivedAt:             arrivedAt,
			RecordedBy:            r.RecordedBy,
			DataSource:            model.DataSource(r.DataSource),
			HalfPig:               r.HalfPig,
			OffalIncluded:         r.OffalIncluded,
			PurchaseGroupID:       r.PurchaseGroupID,
			CreatedAt:             parseRFC3339(r.CreatedAt, a.now()),
			UpdatedAt:             parseRFC3339(r.UpdatedAt, a.now()),
		}
		err = a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"ear_tag", "gross_weight_kg", "purchase_unit_price_yuan",
				"purchase_cost_yuan", "supplier_id", "branch_id", "arrived_at",
				"recorded_by", "data_source", "half_pig", "offal_included",
				"purchase_group_id", "created_at", "updated_at",
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

// importPigCuts 幂等写入部位。
func (a *ArchiveService) importPigCuts(ctx context.Context, branchID string, rows []PigCutRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.PigCut
	if err := a.db.WithContext(ctx).Where("id IN ?", ids).Find(&found).Error; err == nil {
		for _, f := range found {
			existing[f.ID] = true
		}
	}

	for _, r := range rows {
		id := strings.TrimSpace(r.ID)
		pigID := strings.TrimSpace(r.PigID)
		if id == "" || pigID == "" {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "id / pig_id 必填"})
			continue
		}
		expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(r.ExpiresAt))
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "expires_at 不是 RFC3339 时间"})
			continue
		}
		row := model.PigCut{
			ID:        id,
			PigID:     pigID,
			CutType:   model.CutType(r.CutType),
			BranchID:  branchID,
			Barcode:   r.Barcode,
			WeightKg:  parseDecimalLoose(r.WeightKg),
			ExpiresAt: expiresAt,
			CreatedAt: parseRFC3339(r.CreatedAt, a.now()),
		}
		err = a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"pig_id", "cut_type", "branch_id", "barcode", "weight_kg",
				"expires_at", "created_at",
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

// importPorkCutsStocktakes 幂等写入日终按部位盘点。
//
// ⚠️ **不写 llm_review_json**(见文件头)。
func (a *ArchiveService) importPorkCutsStocktakes(ctx context.Context, branchID string, rows []PorkCutsStocktakeRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.PorkCutsStocktake
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
		takenAt, err := time.Parse(time.RFC3339, strings.TrimSpace(r.TakenAt))
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "taken_at 不是 RFC3339 时间"})
			continue
		}
		row := model.PorkCutsStocktake{
			ID:         id,
			BranchID:   branchID,
			TakenAt:    takenAt,
			TakenBy:    r.TakenBy,
			IsComplete: r.IsComplete,
			Cuts:       datatypes.JSON(r.Cuts),
			CreatedAt:  parseRFC3339(r.CreatedAt, a.now()),
		}
		err = a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"branch_id", "taken_at", "taken_by", "is_complete", "cuts", "created_at",
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

// importLineSalesByPigs 幂等写入按猪聚合的销售。
//
// ⚠️ 冲突键用**唯一索引 (pos_line_id, pos_branch_id)**,不用自增主键:
//
//	主键是跨库搬运会撞号的 bigint,而"同一条 POS 明细在同一门店"
//	才是真正的同一行(这正是建这个唯一索引的原因)。
func (a *ArchiveService) importLineSalesByPigs(ctx context.Context, branchID string, rows []LineSalesByPigRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var found []model.LineSalesByPig
	if err := a.db.WithContext(ctx).
		Where("branch_id = ? AND pos_line_id IN ?", branchID, posLineIDs(rows)).
		Find(&found).Error; err == nil {
		for _, f := range found {
			existing[f.PosLineID] = true
		}
	}

	for _, r := range rows {
		posLineID := strings.TrimSpace(r.PosLineID)
		if posLineID == "" {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: fmt.Sprint(r.ID), Error: "pos_line_id 为空"})
			continue
		}
		saleDate, err := time.Parse("2006-01-02", strings.TrimSpace(r.SaleDate))
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: posLineID, Error: "sale_date 不是 YYYY-MM-DD"})
			continue
		}
		row := model.LineSalesByPig{
			BranchID:      branchID,
			SaleDate:      datatypes.Date(saleDate),
			PigID:         r.PigID,
			CutType:       model.CutType(r.CutType),
			QtyKg:         parseDecimalLoose(r.QtyKg),
			AmountYuan:    parseDecimalLoose(r.AmountYuan),
			UnitPriceYuan: parseDecimalLoose(r.UnitPriceYuan),
			Source:        r.Source,
			PosLineID:     posLineID,
			PosBranchID:   r.PosBranchID,
			CubeProductID: r.CubeProductID,
			OrderStatus:   r.OrderStatus,
			CreatedAt:     parseRFC3339(r.CreatedAt, a.now()),
		}
		err = a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "pos_line_id"}, {Name: "pos_branch_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"branch_id", "sale_date", "pig_id", "cut_type", "qty_kg",
				"amount_yuan", "unit_price_yuan", "source", "cube_product_id",
				"order_status", "created_at",
			}),
		}).Create(&row).Error
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: posLineID, Error: err.Error()})
			continue
		}
		if existing[posLineID] {
			res.Updated++
		} else {
			res.Inserted++
			existing[posLineID] = true
		}
	}
}

// importBranchCutMappings 幂等写入门店部位映射。
func (a *ArchiveService) importBranchCutMappings(ctx context.Context, branchID string, rows []BranchCutMappingRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.BranchCutMapping
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
		row := model.BranchCutMapping{
			ID:              id,
			BranchID:        branchID,
			CutType:         model.CutType(r.CutType),
			CubeProductID:   r.CubeProductID,
			CubeProductName: r.CubeProductName,
			CreatedAt:       parseRFC3339(r.CreatedAt, a.now()),
			UpdatedAt:       parseRFC3339(r.UpdatedAt, a.now()),
		}
		err := a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"branch_id", "cut_type", "cube_product_id", "cube_product_name",
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

// importWasteLogs 幂等写入报损。
func (a *ArchiveService) importWasteLogs(ctx context.Context, branchID string, rows []WasteLogRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.WasteLog
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
		recordedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(r.RecordedAt))
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "recorded_at 不是 RFC3339 时间"})
			continue
		}
		row := model.WasteLog{
			ID:            id,
			BranchID:      branchID,
			PigID:         r.PigID,
			CutType:       model.CutType(r.CutType),
			CubeProductID: r.CubeProductID,
			QtyKg:         parseDecimalLoose(r.QtyKg),
			Reason:        r.Reason,
			RecordedAt:    recordedAt,
			OperatorID:    r.OperatorID,
			CreatedAt:     parseRFC3339(r.CreatedAt, a.now()),
		}
		err = a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"branch_id", "pig_id", "cut_type", "cube_product_id", "qty_kg",
				"reason", "recorded_at", "operator_id", "created_at",
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

// posLineIDs 取各行的 pos_line_id(用于预查已存在的行)。
func posLineIDs(rows []LineSalesByPigRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if id := strings.TrimSpace(r.PosLineID); id != "" {
			out = append(out, id)
		}
	}
	return out
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
