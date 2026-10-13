package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/YunBright/core/testrun"
	"github.com/YunBright/supertrade/internal/settlement-auditor/handler"
	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
)

const purgeRunID = "ctr-20261013-160000-dd33ee"

func newPurgeDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := testdb.OpenSQLite(":memory:?_pragma=foreign_keys(1)")
	require.NoError(t, err)
	require.NoError(t, testrun.EnsureTable(db))
	require.NoError(t, db.AutoMigrate(
		&model.ApprovalInstance{}, &model.ApprovalSettlement{}, &model.AuditSignal{},
		&model.AuditLog{}, &model.LLMVerdict{}, &model.ApprovalStocktakeLink{},
	))
	require.NoError(t, testrun.RegisterGorm(db))
	return db
}

// create 走带 RunID 的 context —— 与真实链路一致：账是由上下文里的 run_id
// 触发的，测试不手工往旁表塞数据（那会绕开最该被测的那段代码）。
func create(t *testing.T, db *gorm.DB, runID string, v any) {
	t.Helper()
	require.NoError(t, db.WithContext(testrun.WithRunID(context.Background(), runID)).Create(v).Error)
}

// seedApproval 造一条指定状态的审批 + 5 张从表各一行。
func seedApproval(t *testing.T, db *gorm.DB, id, status, runID string) {
	t.Helper()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	inst := &model.ApprovalInstance{
		ID: id, CorpID: "wcorp-test", BranchID: "B1",
		ApplicantUserID: "u1", Remark: "contract-test", Status: status,
		SpStatus: 0, ApproverUserIDs: []byte("[]"), CcUserIDs: []byte("[]"),
		CreatedAt: now, UpdatedAt: now,
	}
	create(t, db, runID, inst)
	for _, row := range []any{
		&model.ApprovalSettlement{
			ID: id + "-S1", ApprovalID: id, SettlementID: "DP1", SupplierID: "sup1",
			DocumentType: "DP", GoodsAmountYuan: decimal.RequireFromString("100.00"),
			Checked: true, CreatedAt: now,
		},
		&model.AuditSignal{
			ID: id + "-G1", ApprovalID: id, SettlementID: "DP1", ProductID: "P1",
			SignalType: "amount_mismatch", Level: "warn", Message: "m",
			Evidence: []byte("{}"), CreatedAt: now,
		},
		&model.AuditLog{
			ID: id + "-L1", ApprovalID: id, Step: "step1",
			InputSummary: "in", OutputSummary: "out", DurationMs: 12, CreatedAt: now,
		},
		&model.LLMVerdict{
			ID: id + "-V1", ApprovalID: id, Conclusion: "pass", Reason: "ok",
			Confidence: decimal.RequireFromString("0.9"), RawJSON: []byte("{}"),
			Source: model.VerdictSourceRuleFallback, CreatedAt: now,
		},
		&model.ApprovalStocktakeLink{
			ID: id + "-K1", ApprovalID: id, StocktakeHeaderID: "ST-1", CreatedAt: now,
		},
	} {
		create(t, db, runID, row)
	}
}

// childModels 是审批的 5 张从表（approval_id 无外键，必须显式级联）。
func childModels() map[string]any {
	return map[string]any{
		"approval_settlements":     &model.ApprovalSettlement{},
		"audit_signals":            &model.AuditSignal{},
		"audit_logs":               &model.AuditLog{},
		"llm_verdicts":             &model.LLMVerdict{},
		"approval_stocktake_links": &model.ApprovalStocktakeLink{},
	}
}

func approvalExists(t *testing.T, db *gorm.DB, id string) bool {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(&model.ApprovalInstance{}).Where("id = ?", id).Count(&n).Error)
	return n > 0
}

func childCount(t *testing.T, db *gorm.DB, m any, approvalID string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Unscoped().Model(m).Where("approval_id = ?", approvalID).Count(&n).Error)
	return n
}

// TestPurge_NeverTouchesInFlightApproval —— 本服务最重要的一条。
//
// 与 auth 的 admin 闸门同源，但风险不同：这里后台**流水线还在写**。
// 流水线跑一半时删主表，它下一次写 audit_signals 会撞"审批实例不存在"，
// 于是一次本该成功的审核变成 failed —— 清理本身制造了新的脏数据。
//
// ⚠️ 这条同时锁住**从表**：旁表记账让每张表都被独立定位，
// 若从表不带「父审批可删」的条件，那么 received/auditing/approved 的
// audit_signals / llm_verdicts 会被删光 —— 父行还在、子行没了 = 孤行。
// 反证：把 childOfDeletableApproval() 退回恒真条件时，
// 三个在途审批的从表行必须被删掉，测试要红。
func TestPurge_NeverTouchesInFlightApproval(t *testing.T) {
	db := newPurgeDB(t)
	seedApproval(t, db, "AP-RECEIVED", model.StatusReceived, purgeRunID)
	seedApproval(t, db, "AP-AUDITING", model.StatusAuditing, purgeRunID)
	seedApproval(t, db, "AP-APPROVED", model.StatusApproved, purgeRunID)
	seedApproval(t, db, "AP-DONE", model.StatusRejected, purgeRunID)

	rep, err := handler.PurgeRegistry().Purge(context.Background(), db, purgeRunID, false)
	require.NoError(t, err)
	require.True(t, rep.OK(), "清理报错: %v", rep.Errors)

	assert.EqualValues(t, 1, rep.Deleted["approval_instances"])
	assert.EqualValues(t, 3, rep.Skipped["approval_instances"], "三个在途/未终结状态必须被跳过")

	for _, id := range []string{"AP-RECEIVED", "AP-AUDITING", "AP-APPROVED"} {
		assert.True(t, approvalExists(t, db, id), "%s 在途审批被删了", id)
		for name, m := range childModels() {
			assert.NotZero(t, childCount(t, db, m, id),
				"%s 的从表 %s 被删 —— 流水线还要往里写", id, name)
		}
	}

	assert.False(t, approvalExists(t, db, "AP-DONE"))
	for name, m := range childModels() {
		assert.Zero(t, childCount(t, db, m, "AP-DONE"), "%s 残留 —— approval_id 无外键", name)
	}
}

// TestPurge_CascadesAllChildTables —— 5 张从表逐张断言。
func TestPurge_CascadesAllChildTables(t *testing.T) {
	db := newPurgeDB(t)
	for _, st := range []string{
		model.StatusRejected, model.StatusFailed, model.StatusHumanApproved,
		model.StatusHumanRejected, model.StatusCancelled,
	} {
		seedApproval(t, db, "AP-"+st, st, purgeRunID)
	}

	rep, err := handler.PurgeRegistry().Purge(context.Background(), db, purgeRunID, false)
	require.NoError(t, err)
	require.True(t, rep.OK(), "清理报错: %v", rep.Errors)

	assert.EqualValues(t, 5, rep.Deleted["approval_instances"])
	for name := range childModels() {
		assert.EqualValues(t, 5, rep.Deleted[name], "%s 应删 5 行", name)
	}
}

// TestPurge_LeavesRealAndOtherRuns —— 绝不跨 run / 碰真实审批。
func TestPurge_LeavesRealAndOtherRuns(t *testing.T) {
	db := newPurgeDB(t)
	seedApproval(t, db, "AP-OTHER", model.StatusRejected, "ctr-20261013-170000-ee44ff")
	seedApproval(t, db, "AP-REAL", model.StatusRejected, "")

	_, err := handler.PurgeRegistry().Purge(context.Background(), db, purgeRunID, false)
	require.NoError(t, err)

	for _, id := range []string{"AP-OTHER", "AP-REAL"} {
		assert.True(t, approvalExists(t, db, id), "%s 不该被删", id)
		for name, m := range childModels() {
			assert.NotZero(t, childCount(t, db, m, id), "%s 的 %s 被误删", id, name)
		}
	}
}

// TestPurgeRoute_DefaultsToDryRunAndIsUnauthenticated —— 端点层行为。
func TestPurgeRoute_DefaultsToDryRunAndIsUnauthenticated(t *testing.T) {
	db := newPurgeDB(t)
	seedApproval(t, db, "AP-DONE", model.StatusRejected, purgeRunID)

	gin.SetMode(gin.TestMode)
	r := gin.New() // 不挂鉴权中间件：收尾清理不能依赖 token
	handler.RegisterPurgeRoutes(r, db)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/internal/datastore/purge", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	w := post(`{"run_id":"` + purgeRunID + `"}`)
	require.Equal(t, http.StatusOK, w.Code, "免鉴权:收尾清理不能依赖 token")
	var rep struct {
		DryRun  bool             `json:"dry_run"`
		Deleted map[string]int64 `json:"deleted"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rep))
	assert.True(t, rep.DryRun)
	assert.EqualValues(t, 1, rep.Deleted["llm_verdicts"], "dry-run 也要报告将从表删几行")
	assert.True(t, approvalExists(t, db, "AP-DONE"), "dry-run 绝不能真删")

	w = post(`{"run_id":"` + purgeRunID + `","confirm":true}`)
	require.Equal(t, http.StatusOK, w.Code)
	assert.False(t, approvalExists(t, db, "AP-DONE"))

	w = post(`{"run_id":"; DROP TABLE approval_instances;--","confirm":true}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_run_id")
}
