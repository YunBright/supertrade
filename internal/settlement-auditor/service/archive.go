// Package service / archive.go —— settlement-auditor 域的**归档导出 + 批量导入**。
//
// # 端点
//
//	GET  /archive/export?scope=approvals|approval_settlements|audit_signals|audit_logs
//	POST /archive/import
//
// # 这一域**不在 cube 里**
//
//	cube 只有商品/分类/供应商/条码/价格/促销/库存/会员/会员类别 +
//	销售结算类 model;审批实例 / 核对信号 / 推理日志是**本域自己产生的**
//	⇒ 读端点永远走本地。
//
//	但导入导出仍按同一套范式走(见 internal/archiveroute):
//	  - 没配 cube 源 ⇒ 导本地 / 写本地;
//	  - 配了 cube 源 ⇒ 导出**空数据** + 导入**拒绝(409 只读)**。
//
// # ⚠️ 导出的字段取舍(这一域有**真的**凭据列)
//
//	`approval_instances.applicant_bearer` 是**申请人的 JWT**
//	(异步流水线要用它以申请人身份调 cube)⇒ **绝不导出**。
//	模型上它已经标了 `json:"-"`,但归档用的是**自己的行类型**
//	⇒ 必须在这里显式不列它(否则将来有人"顺手补一个字段"就把令牌导出去了)。
//
//	`approval_instances.last_command_id` 是 dapr 命令 id(内部管道)⇒ 也不导。
//
//	`audit_logs.llm_trace` 是 LLM 的**原始 prompt/response**
//	(单行可达 8KB,内含结算单金额与供应商信息)⇒ 不导:
//	它是排障用的调试产物,不是业务数据;导出它会让归档文件体积与敏感面双输。
//	(step / input_summary / output_summary 已经足够复盘"这一步做了什么"。)
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
	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// 归档的资料域(scope)名。
const (
	// ScopeApprovals 是审批实例(approval_instances)。
	ScopeApprovals = "approvals"
	// ScopeApprovalSettlements 是审批↔结算单的关联(approval_settlements)。
	ScopeApprovalSettlements = "approval_settlements"
	// ScopeAuditSignals 是核对信号(audit_signals)。
	ScopeAuditSignals = "audit_signals"
	// ScopeAuditLogs 是推理分步日志(audit_logs)。
	ScopeAuditLogs = "audit_logs"
)

// 归档端点的守卫 scope(与 auth 的 permissions seed 逐字相同)。
const (
	// ScopeSettlementView 是导出(settlement:view)。
	ScopeSettlementView = "settlement:view"
	// ScopeSettlementAudit 是导入(settlement:audit)。
	ScopeSettlementAudit = "settlement:audit"
)

// archiveActor 是导入写入的审计人。
const archiveActor = "import"

// ArchiveService 是 settlement-auditor 的归档导出 / 批量导入服务。
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

// ApprovalRow 是审批实例的导出行。
//
// ⚠️ **没有** applicant_bearer / last_command_id(见文件头)。
type ApprovalRow struct {
	ID               string   `json:"id"`
	CorpID           string   `json:"corp_id"`
	ApplicantUserID  string   `json:"applicant_user_id"`
	ApplicantWecomID string   `json:"applicant_wecom_id"`
	ManualAmountYuan string   `json:"manual_amount_yuan"`
	Remark           string   `json:"remark"`
	Status           string   `json:"status"`
	SpNo             string   `json:"sp_no"`
	ApproverUserIDs  []string `json:"approver_userids"`
	CcUserIDs        []string `json:"cc_userids"`
	SpStatus         int      `json:"sp_status"`
	RejectReason     string   `json:"reject_reason"`
	Error            string   `json:"error"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
}

// ApprovalSettlementRow 是审批↔结算单关联的导出行。
type ApprovalSettlementRow struct {
	ID              string `json:"id"`
	ApprovalID      string `json:"approval_id"`
	SettlementID    string `json:"settlement_id"`
	SupplierID      string `json:"supplier_id"`
	DocumentType    string `json:"document_type"`
	GoodsAmountYuan string `json:"goods_amount_yuan"`
	Checked         bool   `json:"checked"`
	CreatedAt       string `json:"created_at"`
}

// AuditSignalRow 是核对信号的导出行。
type AuditSignalRow struct {
	ID           string          `json:"id"`
	ApprovalID   string          `json:"approval_id"`
	SettlementID string          `json:"settlement_id"`
	ProductID    string          `json:"product_id"`
	SignalType   string          `json:"signal_type"`
	Level        string          `json:"level"`
	Message      string          `json:"message"`
	Evidence     json.RawMessage `json:"evidence,omitempty"`
	CreatedAt    string          `json:"created_at"`
}

// AuditLogRow 是推理分步日志的导出行。
//
// ⚠️ **没有** llm_trace(见文件头)。
type AuditLogRow struct {
	ID            string `json:"id"`
	ApprovalID    string `json:"approval_id"`
	Step          string `json:"step"`
	InputSummary  string `json:"input_summary"`
	OutputSummary string `json:"output_summary"`
	DurationMs    int64  `json:"duration_ms"`
	CreatedAt     string `json:"created_at"`
}

// ExportResult 是导出的响应体。
//
// ⚠️ 各数组**永远存在**(空时是 `[]`):"这个 scope 没有数据"与
//
//	"这个 scope 不在响应里"是两件事。
type ExportResult struct {
	Scope    string `json:"scope"`
	BranchID string `json:"branch_id"`
	// External = 该门店配了外部数据源 ⇒ 本次导出**空**。
	External bool `json:"external"`
	// 各 scope 的行;本次只填 scope 指向的那一个。
	Approvals           []ApprovalRow           `json:"approvals"`
	ApprovalSettlements []ApprovalSettlementRow `json:"approval_settlements"`
	AuditSignals        []AuditSignalRow        `json:"audit_signals"`
	AuditLogs           []AuditLogRow           `json:"audit_logs"`
	// Count 是本次 scope 的行数。
	Count int `json:"count"`
}

// ImportRequest 是导入请求体(与导出响应同形 ⇒ 可往返)。
type ImportRequest struct {
	Scope string `json:"scope"`
	// 各 scope 的行;本次只处理 Scope 指向的那一个。
	Approvals           []ApprovalRow           `json:"approvals,omitempty"`
	ApprovalSettlements []ApprovalSettlementRow `json:"approval_settlements,omitempty"`
	AuditSignals        []AuditSignalRow        `json:"audit_signals,omitempty"`
	AuditLogs           []AuditLogRow           `json:"audit_logs,omitempty"`
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
//
// # 判据(唯一一份,见 internal/archiveroute)
//
//	没配 cube 源 ⇒ 导本地;配了 ⇒ 导出**空**;问不到 ⇒ 503。
func (a *ArchiveService) Export(ctx context.Context, branchID, scope string) (*ExportResult, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	scope = strings.TrimSpace(scope)
	switch scope {
	case ScopeApprovals, ScopeApprovalSettlements, ScopeAuditSignals, ScopeAuditLogs:
	default:
		return nil, fmt.Errorf("%w: scope=%q 非法", ErrInvalidInput, scope)
	}

	mode, err := archiveroute.DecideExport(ctx, a.resolver, branchID)
	if err != nil {
		return nil, err
	}

	out := &ExportResult{
		Scope:               scope,
		BranchID:            branchID,
		External:            mode == archiveroute.ModeExternal,
		Approvals:           []ApprovalRow{},
		ApprovalSettlements: []ApprovalSettlementRow{},
		AuditSignals:        []AuditSignalRow{},
		AuditLogs:           []AuditLogRow{},
	}
	if mode == archiveroute.ModeExternal {
		// ⚠️ 空,**不回落本地**(见文件头)。
		return out, nil
	}

	switch scope {
	case ScopeApprovals:
		rows, err := a.exportApprovals(ctx, branchID)
		if err != nil {
			return nil, err
		}
		out.Approvals = rows
		out.Count = len(rows)
	case ScopeApprovalSettlements:
		rows, err := a.exportApprovalSettlements(ctx, branchID)
		if err != nil {
			return nil, err
		}
		out.ApprovalSettlements = rows
		out.Count = len(rows)
	case ScopeAuditSignals:
		rows, err := a.exportAuditSignals(ctx, branchID)
		if err != nil {
			return nil, err
		}
		out.AuditSignals = rows
		out.Count = len(rows)
	case ScopeAuditLogs:
		rows, err := a.exportAuditLogs(ctx, branchID)
		if err != nil {
			return nil, err
		}
		out.AuditLogs = rows
		out.Count = len(rows)
	}
	return out, nil
}

// branchApprovalIDs 取该门店的审批实例 id(从表按它过滤)。
func (a *ArchiveService) branchApprovalIDs(ctx context.Context, branchID string) ([]string, error) {
	var ids []string
	err := a.db.WithContext(ctx).Model(&model.ApprovalInstance{}).
		Where("branch_id = ?", branchID).
		Pluck("id", &ids).Error
	return ids, err
}

// exportApprovals 导出审批实例(按 branch_id 过滤)。
func (a *ArchiveService) exportApprovals(ctx context.Context, branchID string) ([]ApprovalRow, error) {
	var rows []model.ApprovalInstance
	if err := a.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("created_at desc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ApprovalRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ApprovalRow{
			ID:               r.ID,
			CorpID:           r.CorpID,
			ApplicantUserID:  r.ApplicantUserID,
			ApplicantWecomID: r.ApplicantWecomID,
			ManualAmountYuan: r.ManualAmountYuan.String(),
			Remark:           r.Remark,
			Status:           r.Status,
			SpNo:             r.SpNo,
			ApproverUserIDs:  decodeStringList(r.ApproverUserIDs),
			CcUserIDs:        decodeStringList(r.CcUserIDs),
			SpStatus:         r.SpStatus,
			RejectReason:     r.RejectReason,
			Error:            r.Error,
			CreatedAt:        r.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:        r.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// exportApprovalSettlements 导出审批↔结算单关联。
//
// ⚠️ 这张表**没有 branch_id** ⇒ 靠 approval_id 关联到该门店的审批实例。
func (a *ArchiveService) exportApprovalSettlements(ctx context.Context, branchID string) ([]ApprovalSettlementRow, error) {
	ids, err := a.branchApprovalIDs(ctx, branchID)
	if err != nil {
		return nil, err
	}
	out := []ApprovalSettlementRow{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []model.ApprovalSettlement
	if err := a.db.WithContext(ctx).
		Where("approval_id IN ?", ids).
		Order("created_at asc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, ApprovalSettlementRow{
			ID:              r.ID,
			ApprovalID:      r.ApprovalID,
			SettlementID:    r.SettlementID,
			SupplierID:      r.SupplierID,
			DocumentType:    r.DocumentType,
			GoodsAmountYuan: r.GoodsAmountYuan.String(),
			Checked:         r.Checked,
			CreatedAt:       r.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// exportAuditSignals 导出核对信号(靠 approval_id 关联到该门店)。
func (a *ArchiveService) exportAuditSignals(ctx context.Context, branchID string) ([]AuditSignalRow, error) {
	ids, err := a.branchApprovalIDs(ctx, branchID)
	if err != nil {
		return nil, err
	}
	out := []AuditSignalRow{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []model.AuditSignal
	if err := a.db.WithContext(ctx).
		Where("approval_id IN ?", ids).
		Order("created_at asc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, AuditSignalRow{
			ID:           r.ID,
			ApprovalID:   r.ApprovalID,
			SettlementID: r.SettlementID,
			ProductID:    r.ProductID,
			SignalType:   r.SignalType,
			Level:        r.Level,
			Message:      r.Message,
			Evidence:     json.RawMessage(r.Evidence),
			CreatedAt:    r.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// exportAuditLogs 导出推理分步日志(靠 approval_id 关联到该门店)。
func (a *ArchiveService) exportAuditLogs(ctx context.Context, branchID string) ([]AuditLogRow, error) {
	ids, err := a.branchApprovalIDs(ctx, branchID)
	if err != nil {
		return nil, err
	}
	out := []AuditLogRow{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []model.AuditLog
	if err := a.db.WithContext(ctx).
		Where("approval_id IN ?", ids).
		Order("created_at asc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, AuditLogRow{
			ID:            r.ID,
			ApprovalID:    r.ApprovalID,
			Step:          r.Step,
			InputSummary:  r.InputSummary,
			OutputSummary: r.OutputSummary,
			DurationMs:    r.DurationMs,
			CreatedAt:     r.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// Import 把导出的行**幂等 upsert** 回本域表。
//
// # ⚠️ 这是「数据恢复」,不是「业务重放」
//
//	只写数据:不跑审批流水线、不调 LLM、不发企微命令、
//	不建盘点单、不改付款台账。
//
//	⇒ 恢复出来的审批实例停在**它被导出的那个状态**上
//	(如 human_approved),不会因为"状态还没到终态"而被流水线继续推进。
//
// # 幂等
//
//	按**主键** upsert(各表自己的主键)。重复导入同一份归档 ⇒ 行数不变、内容不变。
func (a *ArchiveService) Import(ctx context.Context, branchID string, req ImportRequest) (*ImportResult, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	scope := strings.TrimSpace(req.Scope)
	switch scope {
	case ScopeApprovals, ScopeApprovalSettlements, ScopeAuditSignals, ScopeAuditLogs:
	default:
		return nil, fmt.Errorf("%w: scope=%q 非法", ErrInvalidInput, scope)
	}

	// 判据:配了 cube 源 ⇒ 本域只读(409);解析失败 ⇒ 放行(导入路径自己兜底)。
	mode, err := archiveroute.DecideImport(ctx, a.resolver, branchID)
	if err != nil {
		slog.Warn("settlement-auditor: 问不到门店数据源,导入按本域处理",
			"branch_id", branchID, "err", err)
		mode = archiveroute.ModeLocal
	}
	if mode == archiveroute.ModeExternal {
		return nil, fmt.Errorf("%w: 门店 %s 的审批数据来自外部数据源,请在原系统操作",
			archiveroute.ErrExternalReadOnly, branchID)
	}

	res := &ImportResult{Scope: scope, BranchID: branchID}
	switch scope {
	case ScopeApprovals:
		a.importApprovals(ctx, branchID, req.Approvals, res)
	case ScopeApprovalSettlements:
		a.importApprovalSettlements(ctx, branchID, req.ApprovalSettlements, res)
	case ScopeAuditSignals:
		a.importAuditSignals(ctx, branchID, req.AuditSignals, res)
	case ScopeAuditLogs:
		a.importAuditLogs(ctx, branchID, req.AuditLogs, res)
	}
	return res, nil
}

// importApprovals 幂等写入审批实例。
//
// ⚠️ **不写 applicant_bearer / last_command_id**:
//
//	前者是调用方的 JWT(凭据),后者是 dapr 命令 id(内部管道)。
//	恢复出来的实例没有 bearer ⇒ 异步流水线**不会**用它去调 cube
//	(那本来就该由重跑请求重新提供,见 model 的注释)。
func (a *ArchiveService) importApprovals(ctx context.Context, branchID string, rows []ApprovalRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.ApprovalInstance
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
		createdAt := parseRFC3339(r.CreatedAt, a.now())
		updatedAt := parseRFC3339(r.UpdatedAt, createdAt)
		row := model.ApprovalInstance{
			ID:               id,
			CorpID:           r.CorpID,
			BranchID:         branchID,
			ApplicantUserID:  r.ApplicantUserID,
			ApplicantWecomID: r.ApplicantWecomID,
			ManualAmountYuan: parseDecimalLoose(r.ManualAmountYuan),
			Remark:           r.Remark,
			Status:           r.Status,
			SpNo:             r.SpNo,
			ApproverUserIDs:  encodeStringList(r.ApproverUserIDs),
			CcUserIDs:        encodeStringList(r.CcUserIDs),
			SpStatus:         r.SpStatus,
			RejectReason:     r.RejectReason,
			Error:            r.Error,
			CreatedAt:        createdAt,
			UpdatedAt:        updatedAt,
		}
		err := a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"corp_id", "branch_id", "applicant_user_id", "applicant_wecom_id",
				"manual_amount_yuan", "remark", "status", "sp_no",
				// ⚠️ 列名是 GORM 的 snake_case(approver_user_ids),
				//   **不是** json tag 里的 approver_userids —— 两者不是一回事。
				"approver_user_ids", "cc_user_ids", "sp_status",
				"reject_reason", "error", "created_at", "updated_at",
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

// importApprovalSettlements 幂等写入审批↔结算单关联。
func (a *ArchiveService) importApprovalSettlements(ctx context.Context, branchID string, rows []ApprovalSettlementRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.ApprovalSettlement
	if err := a.db.WithContext(ctx).Where("id IN ?", ids).Find(&found).Error; err == nil {
		for _, f := range found {
			existing[f.ID] = true
		}
	}
	_ = branchID // 从表没有 branch_id,归属靠 approval_id

	for _, r := range rows {
		id := strings.TrimSpace(r.ID)
		approvalID := strings.TrimSpace(r.ApprovalID)
		if id == "" || approvalID == "" {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "id / approval_id 必填"})
			continue
		}
		row := model.ApprovalSettlement{
			ID:              id,
			ApprovalID:      approvalID,
			SettlementID:    r.SettlementID,
			SupplierID:      r.SupplierID,
			DocumentType:    r.DocumentType,
			GoodsAmountYuan: parseDecimalLoose(r.GoodsAmountYuan),
			Checked:         r.Checked,
			CreatedAt:       parseRFC3339(r.CreatedAt, a.now()),
		}
		err := a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"approval_id", "settlement_id", "supplier_id", "document_type",
				"goods_amount_yuan", "checked", "created_at",
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

// importAuditSignals 幂等写入核对信号。
func (a *ArchiveService) importAuditSignals(ctx context.Context, branchID string, rows []AuditSignalRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.AuditSignal
	if err := a.db.WithContext(ctx).Where("id IN ?", ids).Find(&found).Error; err == nil {
		for _, f := range found {
			existing[f.ID] = true
		}
	}
	_ = branchID // 从表没有 branch_id,归属靠 approval_id

	for _, r := range rows {
		id := strings.TrimSpace(r.ID)
		approvalID := strings.TrimSpace(r.ApprovalID)
		if id == "" || approvalID == "" {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "id / approval_id 必填"})
			continue
		}
		row := model.AuditSignal{
			ID:           id,
			ApprovalID:   approvalID,
			SettlementID: r.SettlementID,
			ProductID:    r.ProductID,
			SignalType:   r.SignalType,
			Level:        r.Level,
			Message:      r.Message,
			Evidence:     datatypes.JSON(r.Evidence),
			CreatedAt:    parseRFC3339(r.CreatedAt, a.now()),
		}
		err := a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"approval_id", "settlement_id", "product_id", "signal_type",
				"level", "message", "evidence", "created_at",
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

// importAuditLogs 幂等写入推理分步日志。
//
// ⚠️ **不写 llm_trace**(见文件头):恢复出来的日志保留 step 与摘要,
//
//	原始 LLM 轨迹不恢复。
func (a *ArchiveService) importAuditLogs(ctx context.Context, branchID string, rows []AuditLogRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.AuditLog
	if err := a.db.WithContext(ctx).Where("id IN ?", ids).Find(&found).Error; err == nil {
		for _, f := range found {
			existing[f.ID] = true
		}
	}
	_ = branchID // 从表没有 branch_id,归属靠 approval_id

	for _, r := range rows {
		id := strings.TrimSpace(r.ID)
		approvalID := strings.TrimSpace(r.ApprovalID)
		if id == "" || approvalID == "" {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "id / approval_id 必填"})
			continue
		}
		row := model.AuditLog{
			ID:            id,
			ApprovalID:    approvalID,
			Step:          r.Step,
			InputSummary:  r.InputSummary,
			OutputSummary: r.OutputSummary,
			DurationMs:    r.DurationMs,
			CreatedAt:     parseRFC3339(r.CreatedAt, a.now()),
		}
		err := a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"approval_id", "step", "input_summary", "output_summary",
				"duration_ms", "created_at",
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

// decodeStringList 把 datatypes.JSON 还原成 []string;解不开返空切片。
func decodeStringList(raw datatypes.JSON) []string {
	out := []string{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return []string{}
	}
	return out
}

// encodeStringList 把 []string 编码成 datatypes.JSON;空 ⇒ nil。
func encodeStringList(in []string) datatypes.JSON {
	if len(in) == 0 {
		return nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil
	}
	return datatypes.JSON(b)
}

// parseDecimalLoose 解析金额字符串;解析不了按 0。
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
