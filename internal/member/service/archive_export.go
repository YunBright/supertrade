package service

// 会员档案的**归档导出**（有源走上游、无源走本地）。
//
// # 为什么 member 域也要有归档导出
//
// 2026-10-22 决策：每个服务都提供「导出 + 批量导入」端口，且遵循同一条范式：
//
//	配了数据源 ⇒ 导出该数据源的数据（上游 cube 的实时值）
//	没配数据源   ⇒ 导出本域 PG 的数据
//
// member 域的导出是 cube-async 搬运「富百家(00) → 云商(10)」的会员部分：
// 源门店 00 配了数据源 ⇒ 它的会员档案在思迅里 ⇒ 必须从上游导（不是从本域导，
// 本域的 00 已经没有副本了 —— 按 2026-10-22 决策，00 的本地副本已删除）。
//
// ⚠️ 有源时**原样转发**上游的导出响应（字段名与 cube-router 的 /v1/export 逐字一致），
// 不自己重新组装 —— 少一次转换就少一处可能不一致的地方。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/member/model"
)

// archiveExportInvokeTimeout 是导出单次调用的超时。
//
// ⚠️ 比业务查询长：一页 5000 行，比 /v1/load 重得多。
const archiveExportInvokeTimeout = 120 * time.Second

// ArchiveExporter 调上游导出端点（配了数据源的门店用）。
type ArchiveExporter interface {
	Export(ctx context.Context, bearer, branchID, scope string, offset, limit int) (*archivedto.ExportPage, error)
}

// archiveExporterInvoker 是 dapr 客户端的**最小能力面**（只取服务调用）。
//
// ⚠️ 收窄依赖面：单测要能断言"到底往上游发了什么"就得手写整个 dapr.Client
// （几十个方法）⇒ 没人愿意写 ⇒ 只能靠肉眼 review 保护。
type archiveExporterInvoker interface {
	InvokeMethod(ctx context.Context, appID, methodName, verb string) ([]byte, error)
}

// DaprArchiveExporter 经 dapr 服务调用调上游的导出端点。
type DaprArchiveExporter struct {
	dapr    archiveExporterInvoker
	appID   string
	timeout time.Duration
}

// NewDaprArchiveExporter 构造。
//
// appID 默认取 cubeclient.DefaultCubeAppID()（生产是 supertrade-cube-router）；
// ⚠️ 它与 /v1/load 是**同一个 app** ⇒ 装配时用同一个对象，不要建两个。
func NewDaprArchiveExporter(d archiveExporterInvoker, appID string) *DaprArchiveExporter {
	if appID == "" {
		appID = cubeclient.DefaultCubeAppID()
	}
	return &DaprArchiveExporter{dapr: d, appID: appID, timeout: archiveExportInvokeTimeout}
}

var _ ArchiveExporter = (*DaprArchiveExporter)(nil)

// Export 取某个 scope 的一页。
func (e *DaprArchiveExporter) Export(ctx context.Context, bearer, branchID, scope string, offset, limit int) (*archivedto.ExportPage, error) {
	if e == nil || e.dapr == nil {
		return nil, model.ErrCubeSourceUnavailable
	}
	q := url.Values{}
	q.Set("scope", scope)
	q.Set("offset", strconv.Itoa(offset))
	q.Set("limit", strconv.Itoa(limit))
	// ⚠️ query 拼在 method 里：dapr SDK 会把它拆成 method + querystring。
	method := "v1/export?" + q.Encode()

	if bearer != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", bearer)
	}
	if branchID != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-branch-id", branchID)
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	raw, err := e.dapr.InvokeMethod(ctx, e.appID, method, "GET")
	if err != nil {
		return nil, classifyArchiveExportError(err)
	}
	var page archivedto.ExportPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("member: 解析上游导出响应失败(scope=%s): %w", scope, err)
	}
	return &page, nil
}

// classifyArchiveExportError 把 dapr/gRPC 错误映射成与 cube 查询**同一套**语义。
//
// ⚠️ 403 与 503 必须分开：403 = "这个调用者没有该门店的读权限"，
// 503 = "上游挂了"。混在一起 ⇒ 用户会去查上游健康度，而真正的原因是权限。
func classifyArchiveExportError(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("%w: %v", model.ErrCubeSourceUnavailable, err)
	}
	switch st.Code() {
	case codes.Unauthenticated:
		return fmt.Errorf("%w: %s: %s", cubeclient.ErrCubeAuthRejected, st.Code(), st.Message())
	case codes.PermissionDenied:
		return fmt.Errorf("%w: %s: %s", cubeclient.ErrCubeForbidden, st.Code(), st.Message())
	case codes.NotFound:
		return fmt.Errorf("%w: %s: %s", cubeclient.ErrCubeNotFound, st.Code(), st.Message())
	default:
		return fmt.Errorf("%w: %s: %s", model.ErrCubeSourceUnavailable, st.Code(), st.Message())
	}
}

// SetArchiveExporter 注入上游导出器（配了数据源的门店用它）。
func (s *Service) SetArchiveExporter(e ArchiveExporter) { s.archiveExporter = e }

// ExportArchive 导出会员域的归档数据（archivedto 契约）。
//
// 范式（2026-10-22 决策）：
//
//	配了数据源 ⇒ **原样转发**上游导出（字段名与 cube-router 的 /v1/export 逐字一致）
//	没配数据源 ⇒ 从本域 PG 组装同一份契约
//	问不到路由 ⇒ 503（**不许**回落本地：回落 = 把一次故障显示成"这个门店没会员"）
//
// ⚠️ 本地路径的值是**本域语义**（status="normal"、card_type="MT-STD-10"），
// 而上游路径是**源库原值**（status="0"、card_type="0"）。
// 两者不互换：搬运方向永远是「有源 → 无源」，语义归一化在导入端做。
func (s *Service) ExportArchive(
	ctx context.Context, branchID, bearer, scope string, offset, limit int,
) (*archivedto.ExportPage, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", model.ErrInvalidInput)
	}
	scope = strings.TrimSpace(scope)
	if scope != archivedto.ScopeMembers && scope != archivedto.ScopeMemberTypes {
		return nil, fmt.Errorf("%w: scope=%q 不支持(只有 %s / %s)",
			model.ErrInvalidInput, scope, archivedto.ScopeMembers, archivedto.ScopeMemberTypes)
	}
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	if offset < 0 {
		offset = 0
	}

	_, useCube, err := s.resolveCubeSource(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if useCube {
		if s.archiveExporter == nil {
			return nil, fmt.Errorf("%w: 未装配上游导出器", model.ErrCubeSourceUnavailable)
		}
		return s.archiveExporter.Export(ctx, bearer, branchID, scope, offset, limit)
	}
	return s.exportArchiveLocal(ctx, branchID, scope, offset, limit)
}

// exportArchiveLocal 从本域 PG 组装 archivedto 契约的一页。
func (s *Service) exportArchiveLocal(
	ctx context.Context, branchID, scope string, offset, limit int,
) (*archivedto.ExportPage, error) {
	page := &archivedto.ExportPage{
		Scope:    scope,
		BranchID: branchID,
		Offset:   offset,
		Limit:    limit,
	}
	switch scope {
	case archivedto.ScopeMemberTypes:
		var total int64
		if err := s.db.WithContext(ctx).Model(&model.MemberType{}).
			Where("branch_id = ? AND deleted_at IS NULL", branchID).
			Count(&total).Error; err != nil {
			return nil, err
		}
		var rows []model.MemberType
		if err := s.db.WithContext(ctx).Where("branch_id = ? AND deleted_at IS NULL", branchID).
			Order("priority ASC, name ASC").Offset(offset).Limit(limit).
			Find(&rows).Error; err != nil {
			return nil, err
		}
		page.Total = int(total)
		page.Count = len(rows)
		page.Rows = make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			page.Rows = append(page.Rows, map[string]any{
				"id":                 r.ID,
				"name":               r.Name,
				"discount_rate":      r.DiscountRate.String(),
				"allow_stored_value": boolToContract(r.AllowStoredValue),
				"allow_points_pay":   boolToContract(r.AllowPointsPay),
			})
		}
	case archivedto.ScopeMembers:
		var total int64
		if err := s.db.WithContext(ctx).Model(&model.Member{}).
			Where("branch_id = ?", branchID).
			Count(&total).Error; err != nil {
			return nil, err
		}
		var rows []model.Member
		if err := s.db.WithContext(ctx).Where("branch_id = ?", branchID).
			Order("id ASC").Offset(offset).Limit(limit).
			Find(&rows).Error; err != nil {
			return nil, err
		}
		page.Total = int(total)
		page.Count = len(rows)
		page.Rows = make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			fillDerived(&r)
			page.Rows = append(page.Rows, memberArchiveRow(r))
		}
	}
	return page, nil
}

// memberArchiveRow 把一行会员转成 archivedto 的行(map)。
//
// ⚠️ 列名取自 archivedto.ExportColumns[ScopeMembers]（导出与导入共用一份字段名）。
func memberArchiveRow(m model.Member) map[string]any {
	row := map[string]any{
		"id":                   m.ID,
		"card_face_no":         m.CardFaceNo,
		"card_type":            m.CardTypeID,
		"name":                 m.Name,
		"gender":               m.Gender,
		"phone":                m.Phone,
		"mobile":               m.Mobile,
		"id_card_no":           m.IDCardNo,
		"status":               m.Status,
		"fee_amount_yuan":      m.FeeAmountYuan.String(),
		"limited_use_count":    m.LimitedUseCount,
		"points_accumulated":   m.PointsAccumulated.String(),
		"points_used":          m.PointsUsed.String(),
		"balance_yuan":         m.BalanceYuan.String(),
		"consumed_amount_yuan": m.ConsumedAmountYuan.String(),
		"consumed_count":       m.ConsumedCount,
		"favorite":             m.Favorite,
		"remark":               m.Remark,
		"branch_no":            m.BranchID,
		"operator_id":          m.UpdatedBy,
		"open_id":              m.OpenID,
	}
	putTime(row, "birthday", m.Birthday)
	putTimeValue(row, "operated_at", m.UpdatedAt)
	putTime(row, "valid_from", m.ValidFrom)
	putTime(row, "valid_until", m.ValidUntil)
	return row
}

// putTimeValue 写一个**非指针**的时间字段。
func putTimeValue(row map[string]any, key string, t time.Time) {
	if t.IsZero() {
		row[key] = nil
		return
	}
	row[key] = t.UTC().Format(time.RFC3339)
}

// putTime 写时间字段（nil ⇒ null）。
func putTime(row map[string]any, key string, t *time.Time) {
	if t == nil {
		row[key] = nil
		return
	}
	row[key] = t.UTC().Format(time.RFC3339)
}

// boolToContract 把 bool 转成契约里的字符串布尔（"1"/"0"）。
//
// ⚠️ 思迅的 allow_stored_value / allow_points_pay 是 char(1) 的 '0'/'1'
// ⇒ 契约用字符串，免得导入端把 true/false 与 '1'/'0' 搞混。
func boolToContract(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
