package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/rbac"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/settlement-auditor/handler"
	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
	"github.com/YunBright/supertrade/internal/settlement-auditor/service"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// ---- fakes ----

// archiveSourceResolver 是 cubesource.Resolver 的 fake。
type archiveSourceResolver struct {
	src cubesource.Source
	err error
}

func (f archiveSourceResolver) Resolve(_ context.Context, _ string) (cubesource.Source, error) {
	if f.err != nil {
		return cubesource.Source{}, f.err
	}
	return f.src, nil
}

// archiveLocalSource 是"自营店"(没配数据源)。
func archiveLocalSource() archiveSourceResolver {
	return archiveSourceResolver{src: cubesource.Source{BranchID: "10"}}
}

// archiveExternalSource 是"配了外部数据源且启用"。
func archiveExternalSource() archiveSourceResolver {
	return archiveSourceResolver{src: cubesource.Source{
		BranchID: "10", Configured: true, Enabled: true, CubeSourceName: "cube-sixun-ysx-fb",
	}}
}

// archiveResolveError 是"问不到路由"。
type archiveResolveError struct{}

func (archiveResolveError) Error() string { return "cube-router 问不到" }

// ---- 基建 ----

const archiveBranch = "10"

func newArchiveDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := testdb.OpenSQLite(":memory:?_pragma=foreign_keys(1)")
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.ApprovalInstance{}, &model.ApprovalSettlement{}, &model.AuditSignal{},
		&model.AuditLog{},
	))
	return db
}

// seedArchiveApproval 造一条审批实例 + 一条关联 + 一条信号 + 一条日志。
func seedArchiveApproval(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Create(&model.ApprovalInstance{
		ID: "ap-1", CorpID: "wcorp", BranchID: archiveBranch,
		ApplicantUserID: "u-1", ApplicantWecomID: "wx-1",
		ManualAmountYuan: decimal.NewFromInt(100), Status: model.StatusHumanApproved,
		SpNo: "SP-1", ApproverUserIDs: datatypes.JSON([]byte(`["wx-2"]`)),
		ApplicantBearer: "eyJhbGciOiJIUzI1NiJ9.SECRET-TOKEN",
	}).Error)
	require.NoError(t, db.Create(&model.ApprovalSettlement{
		ID: "aps-1", ApprovalID: "ap-1", SettlementID: "S-1", SupplierID: "sup-1",
		DocumentType: "DP", GoodsAmountYuan: decimal.NewFromInt(88),
	}).Error)
	require.NoError(t, db.Create(&model.AuditSignal{
		ID: "sig-1", ApprovalID: "ap-1", SettlementID: "S-1",
		SignalType: "qty_deviation", Level: model.SignalWarn, Message: "数量偏差",
		Evidence: datatypes.JSON([]byte(`{"qty":3}`)),
	}).Error)
	require.NoError(t, db.Create(&model.AuditLog{
		ID: "log-1", ApprovalID: "ap-1", Step: "cube_check",
		InputSummary: "in", OutputSummary: "out", LLMTrace: "RAW-LLM-PROMPT",
	}).Error)
}

func newArchiveRouter(t *testing.T, db *gorm.DB, resolver cubesource.Resolver) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(
			claims.WithClaims(c.Request.Context(), &claims.Claims{Sub: "u-1"}))
		c.Next()
	})
	archive := service.NewArchiveService(db)
	archive.SetSourceResolver(resolver)
	allow := rbac.ScopeResolver(func(_ *gin.Context, _, _, scope string) (bool, error) {
		return scope == service.ScopeSettlementView || scope == service.ScopeSettlementAudit, nil
	})
	handler.NewArchiveHandler(archive, allow).RegisterRoutes(r)
	return r
}

func doArchive(t *testing.T, r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Branch-ID", archiveBranch)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ---- 用例 ----

// TestArchiveExport_NoSource_ReturnsLocalRows 锁"没配数据源 ⇒ 导本地"。
func TestArchiveExport_NoSource_ReturnsLocalRows(t *testing.T) {
	db := newArchiveDB(t)
	seedArchiveApproval(t, db)
	r := newArchiveRouter(t, db, archiveLocalSource())

	w := doArchive(t, r, http.MethodGet, "/archive/export?scope=approvals", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got service.ExportResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, service.ScopeApprovals, got.Scope)
	assert.False(t, got.External)
	assert.Equal(t, 1, got.Count)
	require.Len(t, got.Approvals, 1)
	assert.Equal(t, "ap-1", got.Approvals[0].ID)
	assert.Equal(t, "SP-1", got.Approvals[0].SpNo)
	assert.Equal(t, []string{"wx-2"}, got.Approvals[0].ApproverUserIDs)
}

// TestArchiveExport_SourceConfigured_ReturnsEmpty 锁"配了源 ⇒ 导出**空**"。
//
// ⚠️ 本地明明有 1 条审批实例,导出结果**仍然必须是空的**。
func TestArchiveExport_SourceConfigured_ReturnsEmpty(t *testing.T) {
	db := newArchiveDB(t)
	seedArchiveApproval(t, db)
	r := newArchiveRouter(t, db, archiveExternalSource())

	for _, scope := range []string{
		service.ScopeApprovals, service.ScopeApprovalSettlements,
		service.ScopeAuditSignals, service.ScopeAuditLogs,
	} {
		w := doArchive(t, r, http.MethodGet, "/archive/export?scope="+scope, "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var got service.ExportResult
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		assert.True(t, got.External, scope)
		assert.Equal(t, 0, got.Count, scope)
		assert.Empty(t, got.Approvals, scope)
		assert.Empty(t, got.ApprovalSettlements, scope)
		assert.Empty(t, got.AuditSignals, scope)
		assert.Empty(t, got.AuditLogs, scope)
	}
}

// TestArchiveExport_ResolveFailure_503 锁"问不到路由 ⇒ 503"。
func TestArchiveExport_ResolveFailure_503(t *testing.T) {
	db := newArchiveDB(t)
	seedArchiveApproval(t, db)
	r := newArchiveRouter(t, db, archiveSourceResolver{err: archiveResolveError{}})

	w := doArchive(t, r, http.MethodGet, "/archive/export?scope=approvals", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "cube_unavailable")
}

// TestArchiveImport_SourceConfigured_409 锁"配了源 ⇒ 导入 409 只读"。
func TestArchiveImport_SourceConfigured_409(t *testing.T) {
	db := newArchiveDB(t)
	seedArchiveApproval(t, db)
	r := newArchiveRouter(t, db, archiveExternalSource())

	body := `{"scope":"approvals","approvals":[{"id":"ap-2","status":"approved"}]}`
	w := doArchive(t, r, http.MethodPost, "/archive/import", body)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "member_source_is_external")
	// ⚠️ message 不得暴露内部 app-id。
	assert.NotContains(t, w.Body.String(), "cube-sixun-ysx-fb")

	var n int64
	require.NoError(t, db.Model(&model.ApprovalInstance{}).Count(&n).Error)
	assert.Equal(t, int64(1), n, "409 之后本地数据不许被改动")
}

// TestArchiveImport_Local_WritesAndIsIdempotent 锁"没配源 ⇒ 写本地 + 幂等"。
func TestArchiveImport_Local_WritesAndIsIdempotent(t *testing.T) {
	db := newArchiveDB(t)
	r := newArchiveRouter(t, db, archiveLocalSource())

	body := `{"scope":"approvals","approvals":[` +
		`{"id":"ap-9","corp_id":"wcorp","applicant_user_id":"u-9","status":"approved",` +
		`"manual_amount_yuan":"12.50","approver_userids":["wx-3"]}]}`
	w := doArchive(t, r, http.MethodPost, "/archive/import", body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var rep service.ImportResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rep))
	assert.Equal(t, 1, rep.Received)
	assert.Equal(t, 1, rep.Inserted)
	assert.Equal(t, 0, rep.Updated)

	// 幂等:再导一次 ⇒ updated=1,行数不变。
	w2 := doArchive(t, r, http.MethodPost, "/archive/import", body)
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	var rep2 service.ImportResult
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &rep2))
	assert.Equal(t, 0, rep2.Inserted)
	assert.Equal(t, 1, rep2.Updated)

	var n int64
	require.NoError(t, db.Model(&model.ApprovalInstance{}).Count(&n).Error)
	assert.Equal(t, int64(1), n)
}

// TestArchiveExport_ExcludesBearerAndLLMTrace 锁"**不导出**凭据与 LLM 原始轨迹"。
//
// ⚠️ approval_instances.applicant_bearer 是**申请人的 JWT**,
//
//	audit_logs.llm_trace 是 LLM 的原始 prompt/response。
//	两者都不得出现在归档里。
func TestArchiveExport_ExcludesBearerAndLLMTrace(t *testing.T) {
	db := newArchiveDB(t)
	seedArchiveApproval(t, db)
	r := newArchiveRouter(t, db, archiveLocalSource())

	w := doArchive(t, r, http.MethodGet, "/archive/export?scope=approvals", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "SECRET-TOKEN")
	assert.NotContains(t, w.Body.String(), "bearer")
	assert.NotContains(t, w.Body.String(), "last_command_id")

	w = doArchive(t, r, http.MethodGet, "/archive/export?scope=audit_logs", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "RAW-LLM-PROMPT")
	assert.NotContains(t, w.Body.String(), "llm_trace")
}

// TestArchiveImport_DoesNotWriteBearer 锁"导入不写 applicant_bearer"。
func TestArchiveImport_DoesNotWriteBearer(t *testing.T) {
	db := newArchiveDB(t)
	r := newArchiveRouter(t, db, archiveLocalSource())

	body := `{"scope":"approvals","approvals":[{"id":"ap-9","status":"approved"}]}`
	w := doArchive(t, r, http.MethodPost, "/archive/import", body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var got model.ApprovalInstance
	require.NoError(t, db.First(&got, "id = ?", "ap-9").Error)
	assert.Empty(t, got.ApplicantBearer, "导入绝不能写回调用方的 JWT")
}
