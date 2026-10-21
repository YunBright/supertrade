package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/YunBright/authkit/claims"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite"

	"github.com/YunBright/supertrade/internal/member/handler"
	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/internal/member/service"
	"github.com/YunBright/supertrade/pkg/middleware"
	"github.com/YunBright/supertrade/pkg/outbox"
)

// testOperatorSub 是测试里的当前操作人(auth user id)。
const testOperatorSub = "u-1"

// fakeChecker 是 service.ScopeChecker 的测试替身。
type fakeChecker struct {
	scopes map[string]bool
	err    error
}

// HasScope 实现 service.ScopeChecker。
func (f fakeChecker) HasScope(_ context.Context, userID, branchID, scope string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.scopes[userID+"|"+branchID+"|"+scope], nil
}

// fakeVerifier 是 service.EmployeeVerifier 的测试替身。
type fakeVerifier struct {
	auth *service.EmployeeAuth
	err  error
}

// VerifyEmployee 实现 service.EmployeeVerifier。
func (f fakeVerifier) VerifyEmployee(_ context.Context, _, _, _ string) (*service.EmployeeAuth, error) {
	return f.auth, f.err
}

// newTestEngine 装配一个内存版 handler(不接 auth 验签,只测业务与错误信封)。
func newTestEngine(t *testing.T, checker service.ScopeChecker, verifier service.EmployeeVerifier) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dsn := filepath.Join(t.TempDir(), "member.db")
	db, err := gorm.Open(sqlite.Dialector{DriverName: "sqlite", DSN: dsn}, &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	// 关掉连接,否则 TempDir 清理会因文件被占用而失败。
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(
		&model.Member{},
		&model.MemberType{},
		&model.MemberPointLedger{},
		&model.MemberAccountLedger{},
		&model.MemberSettings{},
		&model.PaymentWay{},
		&outbox.Outbox{},
	))

	svc := service.New(db)
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) })
	if checker != nil {
		svc.SetScopeChecker(checker)
	}
	if verifier != nil {
		svc.SetEmployeeVerifier(verifier)
	}

	r := gin.New()
	r.Use(middleware.XBranchID())
	// ⚠️ 注入 claims:生产里 claims 由 auth 中间件放 ctx,
	//   没有它的话 `callerSub` 只能兜底成 "system",scope 判定就落不到测试用户上。
	r.Use(func(c *gin.Context) {
		ctx := claims.WithClaims(c.Request.Context(), &claims.Claims{Sub: testOperatorSub})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	handler.New(svc, nil, nil).RegisterRoutes(r)
	return r, db
}

// seedType 造一个会员类别。
func seedType(t *testing.T, db *gorm.DB, id, branchID, name string) {
	t.Helper()
	require.NoError(t, db.Create(&model.MemberType{
		ID: id, BranchID: branchID, Name: name,
		DiscountRate: decimal.NewFromInt(1), FeeAmountYuan: decimal.Zero,
		Status: "active", CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)
}

// seedMember 造一个会员。
func seedMember(t *testing.T, db *gorm.DB, id, branchID string, points decimal.Decimal) {
	t.Helper()
	require.NoError(t, db.Create(&model.Member{
		ID: id, BranchID: branchID, CardTypeID: "T1", Name: "张三",
		Status:            model.MemberStatusNormal,
		PointsAccumulated: points, PointsUsed: decimal.Zero,
		BalanceYuan: decimal.Zero, ConsumedAmountYuan: decimal.Zero,
		CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)
}

// do 发起一次请求。
func do(t *testing.T, r *gin.Engine, method, path, branchID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if branchID != "" {
		req.Header.Set("X-Branch-ID", branchID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestHandler_ReadEndpoint_NoScope_403 锁"读端点缺 scope ⇒ 403"。
//
// ⚠️ users == nil ⇒ rbac 的 resolver 返 (false, nil) ⇒ 403 forbidden。
func TestHandler_ReadEndpoint_NoScope_403(t *testing.T) {
	r, db := newTestEngine(t, nil, nil)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10", decimal.Zero)

	w := do(t, r, http.MethodGet, "/members/M0001", "10", nil)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"forbidden"`)
}

// TestHandler_Deduct_NoScope_PermissionRequired 锁"写端点无 scope ⇒ 403 member_permission_required"。
func TestHandler_Deduct_NoScope_PermissionRequired(t *testing.T) {
	r, db := newTestEngine(t, fakeChecker{scopes: map[string]bool{}}, nil)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10", decimal.NewFromInt(100))

	w := do(t, r, http.MethodPost, "/members/M0001/points/deduct", "10", map[string]any{
		"op_id":  "op-1",
		"points": "10",
	})
	assert.Equal(t, http.StatusForbidden, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"code":"member_permission_required"`)
	assert.Contains(t, body, "当前收银员没有冲减积分权限，请输入授权")
	assert.Contains(t, body, `"required_scope":"member:point-deduct"`)
}

// TestHandler_Deduct_OnlyAuthorizedBy_Denied 锁"**只带** authorized_by ⇒ 403"。
//
// ⚠️ 2026-10-21 安全修复:旧实现里"只带 authorized_by"直接放行 ⇒
//
//	任何调用方只要在 body 里填一个工号,就能在**没有任何权限**的情况下
//	完成写操作(这里是冲减积分)。
//
// ⇒ 现在必须带 authorization(工号 + 口令)让 member 侧验。
func TestHandler_Deduct_OnlyAuthorizedBy_Denied(t *testing.T) {
	r, db := newTestEngine(t, fakeChecker{scopes: map[string]bool{}}, nil)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10", decimal.NewFromInt(100))

	w := do(t, r, http.MethodPost, "/members/M0001/points/deduct", "10", map[string]any{
		"op_id":         "op-1",
		"points":        "10",
		"authorized_by": "1003",
	})
	require.Equal(t, http.StatusForbidden, w.Code,
		"只带 authorized_by 必须被拒(提权漏洞),body=%s", w.Body.String())
	body := w.Body.String()
	assert.Contains(t, body, `"code":"member_permission_required"`)
	assert.Contains(t, body, `"required_scope":"member:point-deduct"`)

	// ⚠️ **不能有流水落库** —— 被拒的写操作不能留下痕迹。
	var n int64
	require.NoError(t, db.Model(&model.MemberPointLedger{}).Count(&n).Error)
	assert.Equal(t, int64(0), n, "被拒的写操作不得留下积分流水")
}

// TestHandler_Deduct_WithAuthorization_Allowed_And_Audited 锁"带工号+口令 ⇒ 放行 + 写审计"。
func TestHandler_Deduct_WithAuthorization_Allowed_And_Audited(t *testing.T) {
	r, db := newTestEngine(t,
		fakeChecker{scopes: map[string]bool{}},
		fakeVerifier{auth: &service.EmployeeAuth{
			EmployeeCode: "1003", Verified: true,
			Scopes: []string{model.ScopePointDeduct},
		}})
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10", decimal.NewFromInt(100))

	w := do(t, r, http.MethodPost, "/members/M0001/points/deduct", "10", map[string]any{
		"op_id":  "op-1",
		"points": "10",
		"authorization": map[string]any{
			"employee_code": "1003",
			"password":      "pwd",
		},
	})
	require.Equal(t, http.StatusOK, w.Code, "带工号+口令必须放行,body=%s", w.Body.String())

	var row model.MemberPointLedger
	require.NoError(t, db.Where("op_id = ?", "op-1").First(&row).Error)
	assert.Equal(t, "1003", row.AuthorizedBy, "authorized_by 必须写进积分流水")
	assert.Equal(t, "-10", row.Points.String())
}

// TestHandler_Deduct_AuthorizationDenied_403 锁"授权不通过 ⇒ 403 member_permission_denied"。
func TestHandler_Deduct_AuthorizationDenied_403(t *testing.T) {
	r, db := newTestEngine(t,
		fakeChecker{scopes: map[string]bool{}},
		fakeVerifier{err: service.ErrCredentialsInvalid})
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10", decimal.NewFromInt(100))

	w := do(t, r, http.MethodPost, "/members/M0001/points/deduct", "10", map[string]any{
		"op_id":  "op-1",
		"points": "10",
		"authorization": map[string]any{
			"employee_code": "1003",
			"password":      "wrong",
		},
	})
	assert.Equal(t, http.StatusForbidden, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"code":"member_permission_denied"`)
	assert.Contains(t, body, "你没有冲减积分的权限！")
}

// TestHandler_Deduct_OperatorHasScope_Allowed 锁"操作人自己有 scope ⇒ 放行"。
func TestHandler_Deduct_OperatorHasScope_Allowed(t *testing.T) {
	r, db := newTestEngine(t, fakeChecker{scopes: map[string]bool{
		"u-1|10|member:point-deduct": true,
	}}, nil)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10", decimal.NewFromInt(100))

	w := do(t, r, http.MethodPost, "/members/M0001/points/deduct", "10", map[string]any{
		"op_id":  "op-1",
		"points": "10",
	})
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
}

// TestHandler_MemberNotFound_404 锁"会员不存在 ⇒ 404"。
func TestHandler_MemberNotFound_404(t *testing.T) {
	r, db := newTestEngine(t, fakeChecker{scopes: map[string]bool{
		"u-1|10|member:point-deduct": true,
	}}, nil)
	seedType(t, db, "T1", "10", "会员卡")

	w := do(t, r, http.MethodPost, "/members/NOPE/points/deduct", "10", map[string]any{
		"op_id":  "op-1",
		"points": "10",
	})
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"member_not_found"`)
}

// TestHandler_PointsInsufficient_409 锁"积分不足 ⇒ 409"。
func TestHandler_PointsInsufficient_409(t *testing.T) {
	r, db := newTestEngine(t, fakeChecker{scopes: map[string]bool{
		"u-1|10|member:point-deduct": true,
	}}, nil)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10", decimal.NewFromInt(5))

	w := do(t, r, http.MethodPost, "/members/M0001/points/deduct", "10", map[string]any{
		"op_id":  "op-1",
		"points": "10",
	})
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"member_points_insufficient"`)
}

// TestHandler_StatusNotAllowed_409 锁"冻结的卡动积分 ⇒ 409"。
func TestHandler_StatusNotAllowed_409(t *testing.T) {
	r, db := newTestEngine(t, fakeChecker{scopes: map[string]bool{
		"u-1|10|member:point-deduct": true,
	}}, nil)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10", decimal.NewFromInt(100))
	require.NoError(t, db.Model(&model.Member{}).
		Where("id = ? AND branch_id = ?", "M0001", "10").
		Update("status", model.MemberStatusFrozen).Error)

	w := do(t, r, http.MethodPost, "/members/M0001/points/deduct", "10", map[string]any{
		"op_id":  "op-1",
		"points": "10",
	})
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"member_status_not_allowed"`)
}

// TestHandler_IdempotentReplay_200 锁"同一 op_id 重复提交 ⇒ 200 且只落一条流水"。
func TestHandler_IdempotentReplay_200(t *testing.T) {
	r, db := newTestEngine(t, fakeChecker{scopes: map[string]bool{
		"u-1|10|member:point-deduct": true,
	}}, nil)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10", decimal.NewFromInt(100))

	body := map[string]any{"op_id": "op-1", "points": "10"}
	w1 := do(t, r, http.MethodPost, "/members/M0001/points/deduct", "10", body)
	require.Equal(t, http.StatusOK, w1.Code, "body=%s", w1.Body.String())
	w2 := do(t, r, http.MethodPost, "/members/M0001/points/deduct", "10", body)
	require.Equal(t, http.StatusOK, w2.Code, "重放必须返回幂等的成功响应,body=%s", w2.Body.String())

	var n int64
	require.NoError(t, db.Model(&model.MemberPointLedger{}).Count(&n).Error)
	assert.Equal(t, int64(1), n, "member_point_ledger 只应有 1 行")
}

// TestHandler_SubscribeList 锁"订阅清单里有 sale.paid,且 pubsub 名是全平台唯一常量"。
func TestHandler_SubscribeList(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc := service.New(nil)
	handler.New(svc, nil, nil).RegisterSubscribeRoutes(r, nil)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dapr/subscribe", nil))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"topic":"sale.paid"`)
	assert.Contains(t, w.Body.String(), `"pubsubname":"tradewind-pubsub"`)
}
