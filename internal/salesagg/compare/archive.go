// Package compare / archive.go —— 门店对比模板的**归档导出 + 批量导入**。
//
// # 端点
//
//	GET  /archive/export?scope=compare_templates
//	POST /archive/import
//
// # 这一域**不在 cube 里**
//
//	cube 只有商品/分类/供应商/条码/价格/促销/库存/会员/会员类别 +
//	销售结算类 model;门店对比模板是**本域自己配的字典**
//	⇒ 读端点永远走本地。
//
//	但导入导出仍按同一套范式走(见 internal/archiveroute):
//	  - 没配 cube 源 ⇒ 导本地 / 写本地;
//	  - 配了 cube 源 ⇒ 导出**空数据** + 导入**拒绝(409 只读)**。
//
// # ⚠️ 模板表**没有 branch_id** —— 它是全局共享的 wiki 式资源
//
//	模板本身是"门店 + 维度 + 对比行映射"的**配置**,不属于任何一家店。
//	但归档端点的守卫是 **per-branch** scope ⇒ 导出/导入按
//	「模板的 stores 里**含当前门店**」来过滤/校验:
//
//	  - 导出:只导 stores 含当前门店的模板;
//	  - 导入:stores 不含当前门店的模板**跳过** + warning
//	    (一个只有 A 店权限的调用方不该改写一份"B、C 店"的全局模板)。
//
//	⇒ 这样"per-branch 守门"与"全局资源"两件事才对得上。
//
// # ⚠️ 导入 = **数据恢复**,不是业务重放
//
//	只写 sales_compare_templates 一张表:不重算数据、不调 cube 取数、
//	不发任何事件、不推进 revision 之外的任何状态。
package compare

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/archiveroute"
)

// 归档的资料域(scope)名。
const (
	// ScopeCompareTemplates 是门店对比模板(sales_compare_templates)。
	ScopeCompareTemplates = "compare_templates"
)

// 归档端点的守卫 scope(与 auth 的 permissions seed 逐字相同)。
const (
	// ScopeInventoryView 是导出(inventory:view)。
	ScopeInventoryView = "inventory:view"
	// ScopeInventoryManage 是导入(inventory:manage)。
	ScopeInventoryManage = "inventory:manage"
)

// archiveActor 是导入写入的审计人。
const archiveActor = "import"

// ArchiveService 是门店对比模板的归档导出 / 批量导入服务。
type ArchiveService struct {
	store *Store
	// resolver 解析"这个门店配了哪个数据源"。
	resolver archiveroute.Resolver
	now      func() time.Time
}

// NewArchiveService 构造 ArchiveService。
func NewArchiveService(store *Store) *ArchiveService {
	return &ArchiveService{store: store, now: func() time.Time { return time.Now() }}
}

// SetSourceResolver 注入数据源解析器。
func (a *ArchiveService) SetSourceResolver(r archiveroute.Resolver) {
	a.resolver = r
}

// TemplateRow 是门店对比模板的导出行。
type TemplateRow struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Dimension     string    `json:"dimension"`
	Stores        []string  `json:"stores"`
	Rows          []RowJSON `json:"rows"`
	Revision      int64     `json:"revision"`
	CreatedBy     string    `json:"created_by"`
	CreatedByName string    `json:"created_by_name"`
	UpdatedBy     string    `json:"updated_by"`
	UpdatedByName string    `json:"updated_by_name"`
	CreatedAt     string    `json:"created_at"`
	UpdatedAt     string    `json:"updated_at"`
}

// ExportResult 是导出的响应体。
//
// ⚠️ 数组**永远存在**(空时是 `[]`)。
type ExportResult struct {
	Scope    string `json:"scope"`
	BranchID string `json:"branch_id"`
	// External = 该门店配了外部数据源 ⇒ 本次导出**空**。
	External bool `json:"external"`
	// CompareTemplates 是本次导出的模板行。
	CompareTemplates []TemplateRow `json:"compare_templates"`
	// Count 是行数。
	Count int `json:"count"`
}

// ImportRequest 是导入请求体(与导出响应同形 ⇒ 可往返)。
type ImportRequest struct {
	Scope string `json:"scope"`
	// CompareTemplates 是模板行。
	CompareTemplates []TemplateRow `json:"compare_templates,omitempty"`
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

// Export 导出该门店的对比模板。
//
// # 判据(唯一一份,见 internal/archiveroute)
//
//	没配 cube 源 ⇒ 导本地;配了 ⇒ 导出**空**;问不到 ⇒ 503。
func (a *ArchiveService) Export(ctx context.Context, branchID, scope string) (*ExportResult, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalid)
	}
	scope = strings.TrimSpace(scope)
	if scope != ScopeCompareTemplates {
		return nil, fmt.Errorf("%w: scope=%q 非法(只能是 %s)", ErrInvalid, scope, ScopeCompareTemplates)
	}

	mode, err := archiveroute.DecideExport(ctx, a.resolver, branchID)
	if err != nil {
		return nil, err
	}

	out := &ExportResult{
		Scope:            scope,
		BranchID:         branchID,
		External:         mode == archiveroute.ModeExternal,
		CompareTemplates: []TemplateRow{},
	}
	if mode == archiveroute.ModeExternal {
		// ⚠️ 空,**不回落本地**(见文件头)。
		return out, nil
	}

	templates, err := a.store.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range templates {
		tj, err := templates[i].ToJson()
		if err != nil {
			return nil, err
		}
		// ⚠️ 模板表**没有 branch_id** ⇒ 按「stores 含当前门店」过滤
		//   (见文件头:per-branch 守门 vs 全局资源)。
		if !containsStore(tj.Stores, branchID) {
			continue
		}
		out.CompareTemplates = append(out.CompareTemplates, TemplateRow{
			ID:            tj.ID,
			Name:          tj.Name,
			Dimension:     tj.Dimension,
			Stores:        tj.Stores,
			Rows:          tj.Rows,
			Revision:      tj.Revision,
			CreatedBy:     tj.CreatedBy,
			CreatedByName: tj.CreatedByName,
			UpdatedBy:     tj.UpdatedBy,
			UpdatedByName: tj.UpdatedByName,
			CreatedAt:     tj.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:     tj.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	out.Count = len(out.CompareTemplates)
	return out, nil
}

// Import 把导出的模板行**幂等 upsert** 回 sales_compare_templates。
//
// # ⚠️ 这是「数据恢复」,不是「业务重放」
//
//	只写这一张表:不重算对比数据、不调 cube 取数、不发事件。
//
// # 幂等
//
//	按**主键 id** upsert。重复导入同一份归档 ⇒ 行数不变、内容不变。
//
// # ⚠️ 跨店模板不导入
//
//	stores 不含当前门店的模板 ⇒ 跳过 + warning:
//	调用方只有当前门店的权限,不该改写一份别的门店的全局模板。
func (a *ArchiveService) Import(ctx context.Context, branchID string, req ImportRequest) (*ImportResult, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalid)
	}
	scope := strings.TrimSpace(req.Scope)
	if scope != ScopeCompareTemplates {
		return nil, fmt.Errorf("%w: scope=%q 非法(只能是 %s)", ErrInvalid, scope, ScopeCompareTemplates)
	}

	// 判据:配了 cube 源 ⇒ 本域只读(409);解析失败 ⇒ 放行(导入路径自己兜底)。
	mode, err := archiveroute.DecideImport(ctx, a.resolver, branchID)
	if err != nil {
		slog.Warn("sales-agg: 问不到门店数据源,导入按本域处理",
			"branch_id", branchID, "err", err)
		mode = archiveroute.ModeLocal
	}
	if mode == archiveroute.ModeExternal {
		return nil, fmt.Errorf("%w: 门店 %s 的对比模板来自外部数据源,请在原系统操作",
			archiveroute.ErrExternalReadOnly, branchID)
	}

	res := &ImportResult{Scope: scope, BranchID: branchID, Received: len(req.CompareTemplates)}
	// 预查已存在的行:upsert 的 RowsAffected 分不出"新建"与"覆盖"
	// ⇒ 先查一遍,报告里的 inserted/updated 才是真的。
	existing := map[string]bool{}
	for _, row := range req.CompareTemplates {
		id := strings.TrimSpace(row.ID)
		if id == "" {
			continue
		}
		if _, err := a.store.Get(ctx, id); err == nil {
			existing[id] = true
		}
	}
	for _, row := range req.CompareTemplates {
		id := strings.TrimSpace(row.ID)
		if id == "" {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "id 为空"})
			continue
		}
		if !containsStore(row.Stores, branchID) {
			res.Skipped++
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("compare_templates:模板 %q 的门店列表不含 %s,已跳过(跨店模板不由本店导入)", id, branchID))
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "模板的门店列表不含当前门店"})
			continue
		}
		// 复用保存时的校验(维度 / 门店数 / 行数)——
		// 归档里的行也要满足**存得进去**的规则,否则恢复出来的是一份
		// 前端根本渲染不了的模板。
		if err := ValidateSave(SaveInput{
			Name:      row.Name,
			Dimension: row.Dimension,
			Stores:    row.Stores,
			Rows:      row.Rows,
		}); err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: err.Error()})
			continue
		}
		storesJSON, err := json.Marshal(row.Stores)
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: err.Error()})
			continue
		}
		rowsJSON, err := json.Marshal(row.Rows)
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: err.Error()})
			continue
		}
		now := a.now()
		tmpl := &Template{
			ID:            id,
			Name:          row.Name,
			Dimension:     row.Dimension,
			StoresJSON:    string(storesJSON),
			RowsJSON:      string(rowsJSON),
			Revision:      row.Revision,
			CreatedBy:     firstNonEmptyStr(row.CreatedBy, archiveActor),
			CreatedByName: row.CreatedByName,
			UpdatedBy:     firstNonEmptyStr(row.UpdatedBy, archiveActor),
			UpdatedByName: row.UpdatedByName,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if tmpl.Revision <= 0 {
			tmpl.Revision = 1
		}
		err = a.store.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"name", "dimension", "stores", "rows", "revision",
				"updated_by", "updated_by_name", "updated_at",
			}),
		}).Create(tmpl).Error
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
	return res, nil
}

// containsStore 判断门店列表里是否含该门店。
func containsStore(stores []string, branchID string) bool {
	for _, s := range stores {
		if strings.TrimSpace(s) == branchID {
			return true
		}
	}
	return false
}

// firstNonEmptyStr 取第一个非空字符串。
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
