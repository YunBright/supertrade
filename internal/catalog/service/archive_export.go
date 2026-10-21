// Package service / archive_export.go —— 档案**导出**(archivedto 契约)。
//
// # 它是"导出方"(本域这一侧)
//
// 门店 → cube 实例的映射只有 cube-router 知道(`branch_cube_sources`),
// 所以"把某门店的档案导出成结构化数据"这件事天然归它 ——
// catalog 这一侧做的是**同一个动作的本域版本**:
//
//	有源(富百家 / 佳乐)⇒ 转发 cube-router 的 GET /v1/export(body 透传)
//	没源(自营店 10)    ⇒ 从本域 PG 读,组装成**同一份** archivedto 契约
//
// ⚠️ **有源但 cube 里没有 ⇒ 导出空数据,不许回落本地**:
// 回落 = 把"外部系统里没有这份档案"说成"本地也没有",而真相是"真相在外部系统"。
//
// # 契约只有一份
//
// 字段名见 internal/archivedto/dto.go(ExportColumns / ExportPage)。
// ⚠️ 两边各写一份 struct 的代价是**静默丢数据**:
// JSON 反序列化对不认识的字段静默忽略、对缺失的字段留零值
// ⇒ "商品名字段名拼错"的后果是 27565 个商品的 name 全是空,而**没有一行报错**。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/catalog/model"
)

// ErrUnsupportedExportScope 是"这个资料域 catalog 不导"。
//
// ⚠️ 与上游导出端点同名同码(`unsupported_export_scope`):
// 本域只拥有 categories / suppliers / products / item_barcodes 四张表,
// 其余资料域(售价 / 促销 / 库存 / 会员)归别的服务 ——
// 本域导出它们只能得到"空",那比明确报错更坏。
var ErrUnsupportedExportScope = errors.New("catalog: 不支持的导出资料域")

// 导出分页口径(与上游导出端点一致)。
const (
	// ExportDefaultLimit 是导出的默认每页行数。
	//
	// ⚠️ 不是"一次拉完":门店 00 实测 27565 个商品,一次拉完会让 sidecar 内存爆。
	ExportDefaultLimit = 5000
	// ExportMaxLimit 是导出的每页行数上限。
	ExportMaxLimit = 10000
)

// ExportPage 是档案导出的一页(响应体)。
//
// ⚠️ 与 archivedto.ExportPage **同形**,只多一个 as_of(基准日):
// 上游导出端点会回 as_of(促销是时效数据);本域导出的四个资料域都不是时效数据
// ⇒ 本地路径按同款口径回"今天",保证两条路径的**字段集逐字一致**。
type ExportPage struct {
	archivedto.ExportPage
	// AsOf 是基准日(yyyy-mm-dd)。
	AsOf string `json:"as_of,omitempty"`
}

// ArchiveExporter 取一页档案(上游导出端点)。
//
// 实现见 DaprArchiveExporter(经 dapr 服务调用打 cube-router 的 /v1/export)。
type ArchiveExporter interface {
	// Export 取某个 scope 的一页。offset / limit 由调用方决定
	// (导出端点**无状态**,分页是调用方的循环)。
	Export(ctx context.Context, bearer, branchID, scope string, offset, limit int) (*ExportPage, error)
}

// ExportInput 是导出的入参。
type ExportInput struct {
	// BranchID 是被导出的门店(只从 X-Branch-ID 取)。
	BranchID string
	// Bearer 是调用者的 Authorization 头原值("Bearer <token>"),
	// 转发给上游时**原样**带过去(上游按它做 per-branch scope 守门)。
	Bearer string
	// Scope 是资料域(categories / suppliers / products / item_barcodes …)。
	Scope string
	// Offset / Limit 是分页位置。
	Offset int
	Limit  int
}

// Export 导出一页档案(按数据源分流)。
//
// ⚠️ 读端点的三态判据在这里同样成立:
//
//	配了源且启用 ⇒ 转发上游(该门店的档案在外部系统里)
//	没配源       ⇒ 读本域 PG
//	问不到       ⇒ 503(不回落本地)
//	配了但停用   ⇒ 503
func (s *ArchiveService) Export(ctx context.Context, in ExportInput) (*ExportPage, error) {
	scope := strings.TrimSpace(in.Scope)
	if scope == "" {
		return nil, model.ErrInvalidInput
	}
	if in.BranchID == "" {
		return nil, model.ErrInvalidInput
	}
	useCube, err := s.useCubeForRead(ctx, in.BranchID)
	if err != nil {
		return nil, err
	}
	if useCube {
		if s.exporter == nil {
			// ⚠️ 没有上游客户端 ⇒ 503,**不**回落本地:
			// 回落会把"导不出来"说成"这个门店没有商品"。
			return nil, ErrCubeUnavailable
		}
		return s.exporter.Export(ctx, in.Bearer, in.BranchID, scope, in.Offset, in.Limit)
	}
	return s.localExport(ctx, in, scope)
}

// localExport 从本域 PG 组装一页档案(archivedto 契约)。
func (s *ArchiveService) localExport(ctx context.Context, in ExportInput, scope string) (*ExportPage, error) {
	rows, total, err := s.localExportRows(ctx, in.BranchID, scope, in.Offset, in.Limit)
	if err != nil {
		return nil, err
	}
	asOf, err := archivedto.NormalizeAsOf("", time.Now())
	if err != nil {
		return nil, err
	}
	return &ExportPage{
		ExportPage: archivedto.ExportPage{
			Scope:    scope,
			BranchID: in.BranchID,
			// ⚠️ 本域数据没有外部数据源 ⇒ 空(不编造一个 cube 名字)。
			CubeSource: "",
			Rows:       rows,
			Count:      len(rows),
			Total:      total,
			Offset:     in.Offset,
			Limit:      in.Limit,
		},
		AsOf: asOf,
	}, nil
}

// localExportRows 读本域 PG 的一个资料域,组装成契约行。
//
// ⚠️ **列名逐字取自 archivedto.ExportColumns** ——
// 那是导入端(以及编排方)读列名的唯一真相源。
func (s *ArchiveService) localExportRows(ctx context.Context, branchID, scope string, offset, limit int) ([]map[string]any, int, error) {
	switch scope {
	case archivedto.ScopeCategories:
		var rows []model.Category
		tx := s.db.WithContext(ctx).Model(&model.Category{}).Where("branch_id = ?", branchID)
		var total int64
		if err := tx.Count(&total).Error; err != nil {
			return nil, 0, err
		}
		if err := tx.Order("id").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
			return nil, 0, err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]any{
				"id":        r.ID,
				"name":      r.Name,
				"parent_id": r.ParentID,
			})
		}
		return out, int(total), nil

	case archivedto.ScopeSuppliers:
		var rows []model.Supplier
		tx := s.db.WithContext(ctx).Model(&model.Supplier{}).
			Where("branch_id = ? AND deleted_at IS NULL", branchID)
		var total int64
		if err := tx.Count(&total).Error; err != nil {
			return nil, 0, err
		}
		if err := tx.Order("id").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
			return nil, 0, err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]any{
				"id":      r.ID,
				"name":    r.Name,
				"type":    r.Type,
				"contact": r.Contact,
				"phone":   r.Phone,
				"email":   r.Email,
				"address": r.Address,
				"status":  r.Status,
			})
		}
		return out, int(total), nil

	case archivedto.ScopeProducts:
		var rows []model.Product
		tx := s.db.WithContext(ctx).Model(&model.Product{}).Where("branch_id = ?", branchID)
		var total int64
		if err := tx.Count(&total).Error; err != nil {
			return nil, 0, err
		}
		if err := tx.Order("id").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
			return nil, 0, err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]any{
				"id":          r.ID,
				"name":        r.Name,
				"category_id": r.CategoryID,
				"supplier_id": r.SupplierID,
				"unit":        r.Unit,
				"status":      r.Status,
				"sub_code":    r.SubCode,
				"spec":        r.Spec,
				// ⚠️ 价格**不在本域**(归 pricing 服务)⇒ 空字符串。
				//   空 = "这一列没有值";编一个 0 元会被当成真价灌进下游库。
				"price_yuan": "",
				"cost_yuan":  "",
			})
		}
		return out, int(total), nil

	case archivedto.ScopeItemBarcodes:
		var rows []model.ProductBarcode
		tx := s.db.WithContext(ctx).Model(&model.ProductBarcode{}).Where("branch_id = ?", branchID)
		var total int64
		if err := tx.Count(&total).Error; err != nil {
			return nil, 0, err
		}
		if err := tx.Order("product_id, barcode").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
			return nil, 0, err
		}
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]any{
				"barcode":    r.Barcode,
				"product_id": r.ProductID,
				"label":      r.Label,
			})
		}
		return out, int(total), nil

	default:
		// ⚠️ 本域只有四张表;其余资料域归别的服务。
		//   明确报错比"导出一堆空行"诚实 —— 后者会被下游当成"这个门店真的没有"。
		return nil, 0, fmt.Errorf("%w: scope=%q 不归 catalog(本域没有这张表)",
			ErrUnsupportedExportScope, scope)
	}
}

// ExportScopeAll 取某个 scope 的**全部**行(翻页循环)。
//
// ⚠️ 上游导出端点是**无状态**的(只按 offset/limit 返回一页 + total),
// 拉全量是调用方的循环(编排方 cubesync 也是这么做的)。
func (s *ArchiveService) ExportScopeAll(ctx context.Context, bearer, branchID, scope string) ([]map[string]any, error) {
	if s.exporter == nil {
		return nil, ErrCubeUnavailable
	}
	const pageSize = 5000
	out := []map[string]any{}
	offset := 0
	for {
		page, err := s.exporter.Export(ctx, bearer, branchID, scope, offset, pageSize)
		if err != nil {
			return nil, err
		}
		if page == nil || len(page.Rows) == 0 {
			break
		}
		out = append(out, page.Rows...)
		offset += len(page.Rows)
		if len(page.Rows) < pageSize {
			break
		}
		if page.Total > 0 && offset >= page.Total {
			break
		}
	}
	return out, nil
}
