package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/internal/member/service"
	"github.com/YunBright/supertrade/pkg/outbox"
)

// TestOnSalePaid_AddsPoints 锁"消费 1 元得 1 积分,向上取整"。
func TestOnSalePaid_AddsPoints(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")
	ctx := context.Background()

	res, err := svc.OnSalePaid(ctx, service.SalePaidPayload{
		OpID:            "op-sale-1",
		SaleID:          "SO20261021000001",
		BranchID:        "10",
		MemberID:        "M0001",
		OperatorID:      "u-1",
		TotalAmountYuan: decimal.RequireFromString("22.20"),
		OrderStatus:     "S",
	})
	require.NoError(t, err)
	assert.False(t, res.Skipped)
	assert.Equal(t, "22", res.Points.String(), "ceil(22.20)=22")

	member, err := svc.Get(ctx, "10", "M0001")
	require.NoError(t, err)
	assert.Equal(t, "22", member.PointsAccumulated.String())
	assert.True(t, member.ConsumedAmountYuan.Equal(decimal.RequireFromString("22.20")),
		"累计消费金额应是 22.20,实际 %s", member.ConsumedAmountYuan)
	assert.Equal(t, 1, member.ConsumedCount)
}

// TestOnSalePaid_DuplicateDelivery_AddsPointsOnce 是**幂等守卫测试**。
//
// ⚠️ 同一 op_id 投递两次 ⇒ member_point_ledger 只有 1 行、积分只加一次。
//
//	这是 outbox「至少一次」投递语义的必然要求:发布成功但标记失败会重发。
func TestOnSalePaid_DuplicateDelivery_AddsPointsOnce(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")
	ctx := context.Background()

	payload := service.SalePaidPayload{
		OpID:            "op-sale-1",
		SaleID:          "SO20261021000001",
		BranchID:        "10",
		MemberID:        "M0001",
		OperatorID:      "u-1",
		TotalAmountYuan: decimal.RequireFromString("20.50"),
		OrderStatus:     "S",
	}

	first, err := svc.OnSalePaid(ctx, payload)
	require.NoError(t, err)
	assert.Equal(t, "21", first.Points.String(), "ceil(20.50)=21")

	second, err := svc.OnSalePaid(ctx, payload)
	require.NoError(t, err, "重复投递不得报错")
	assert.True(t, second.Replayed, "第二次应命中幂等")

	var n int64
	require.NoError(t, db.Model(&model.MemberPointLedger{}).
		Where("op_id = ?", "op-sale-1").Count(&n).Error)
	assert.Equal(t, int64(1), n, "member_point_ledger 只应有 1 行")

	member, err := svc.Get(ctx, "10", "M0001")
	require.NoError(t, err)
	assert.Equal(t, "21", member.PointsAccumulated.String(), "积分只能加一次")
	assert.Equal(t, 1, member.ConsumedCount, "消费次数只能加一次")
}

// TestOnSalePaid_NoMemberID_Skipped 锁"普通单不计积分"。
func TestOnSalePaid_NoMemberID_Skipped(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	ctx := context.Background()

	res, err := svc.OnSalePaid(ctx, service.SalePaidPayload{
		OpID:            "op-sale-1",
		SaleID:          "SO20261021000001",
		BranchID:        "10",
		TotalAmountYuan: decimal.RequireFromString("22.20"),
		OrderStatus:     "S",
	})
	require.NoError(t, err)
	assert.True(t, res.Skipped, "member_id 为空 ⇒ 不计积分")

	var n int64
	require.NoError(t, db.Model(&model.MemberPointLedger{}).Count(&n).Error)
	assert.Equal(t, int64(0), n, "不该有积分流水")
}

// TestOnSalePaid_ReturnOrder_Skipped 锁"退货单(R)⇒ **没有积分可扣时**跳过"。
//
// ⚠️ 2026-10-22 起,R **不再是无条件跳过** ——
//   思迅会按退掉金额**扣回积分**(源库实测:全库 8 行负积分全是退货台单据),
//   见 subscribe_refund_points_test.go。
//   ⇒ 这里之所以仍跳过,是因为这个会员**一分积分都没有** ⇒ 没得扣。
func TestOnSalePaid_ReturnOrder_Skipped(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")
	ctx := context.Background()

	res, err := svc.OnSalePaid(ctx, service.SalePaidPayload{
		OpID:            "op-sale-1",
		SaleID:          "SO20261021000002",
		BranchID:        "10",
		MemberID:        "M0001",
		TotalAmountYuan: decimal.RequireFromString("22.20"),
		OrderStatus:     "R",
	})
	require.NoError(t, err)
	assert.True(t, res.Skipped)
}

// TestOnSalePaid_StringAmounts 锁"金额/数量是字符串也能解"。
//
// ⚠️ payload 里的金额是**字符串**("22.20"),这是 pos 服务定的形状。
func TestOnSalePaid_StringAmounts(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")
	ctx := context.Background()

	body := `{
	  "op_id": "op-sale-1",
	  "sale_id": "SO20261021000001",
	  "branch_id": "10",
	  "member_id": "M0001",
	  "operator_id": "u-1",
	  "total_amount_yuan": "38.50",
	  "paid_at": "2026-10-21T12:00:00+08:00",
	  "order_status": "S",
	  "lines": [
	    {"line_id":"L1","product_id":"00000297","qty":"2",
	     "unit_price_yuan":"19.25","amount_yuan":"38.50"}
	  ]
	}`
	var payload service.SalePaidPayload
	require.NoError(t, json.Unmarshal([]byte(body), &payload))

	res, err := svc.OnSalePaid(ctx, payload)
	require.NoError(t, err)
	assert.Equal(t, "39", res.Points.String(), "ceil(38.50)=39")
}

// TestOnSalePaid_WritesEvent 锁"积分事件与流水同事务"。
func TestOnSalePaid_WritesEvent(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")
	ctx := context.Background()

	_, err := svc.OnSalePaid(ctx, service.SalePaidPayload{
		OpID:            "op-sale-1",
		SaleID:          "SO20261021000001",
		BranchID:        "10",
		MemberID:        "M0001",
		OperatorID:      "u-1",
		TotalAmountYuan: decimal.RequireFromString("69.60"),
		OrderStatus:     "S",
	})
	require.NoError(t, err)

	var row outbox.Outbox
	require.NoError(t, db.Where("topic = ?", service.TopicMemberPointsChanged).First(&row).Error)
	assert.Equal(t, "member:op-sale-1", row.ID, "发件箱行 id 带服务命名空间")
	assert.Contains(t, row.Payload, `"direction":"earn"`)
	assert.Contains(t, row.Payload, `"points":"70"`)
}

// TestPointsSettingsRate 锁"积分率取自门店配置"。
func TestPointsSettingsRate(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")
	ctx := context.Background()

	// 默认 1.0。
	res, err := svc.OnSalePaid(ctx, service.SalePaidPayload{
		OpID: "op-sale-1", SaleID: "SO1", BranchID: "10", MemberID: "M0001",
		TotalAmountYuan: decimal.RequireFromString("10.00"), OrderStatus: "S",
	})
	require.NoError(t, err)
	assert.Equal(t, "10", res.Points.String())

	// 配置成 2.0 ⇒ 20 分。
	require.NoError(t, db.Create(&model.MemberSettings{
		BranchID: "10", MembershipEnabled: true,
		PointsRate: decimal.NewFromInt(2), PointsDeductRate: decimal.Zero,
		MaxFacePerMember: 1, UpdatedBy: "test", UpdatedAt: time.Now().UTC(),
	}).Error)
	res, err = svc.OnSalePaid(ctx, service.SalePaidPayload{
		OpID: "op-sale-2", SaleID: "SO2", BranchID: "10", MemberID: "M0001",
		TotalAmountYuan: decimal.RequireFromString("10.00"), OrderStatus: "S",
	})
	require.NoError(t, err)
	assert.Equal(t, "20", res.Points.String())
}

// ---- 越权授权链路 ----

// fakeChecker 是 ScopeChecker 的测试替身。
type fakeChecker struct {
	// scopes 是 "user|branch|scope" 的集合。
	scopes map[string]bool
	// err 非空时一律返回错误(模拟 userd 不可用)。
	err error
}

// HasScope 实现 service.ScopeChecker。
func (f fakeChecker) HasScope(_ context.Context, userID, branchID, scope string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.scopes[userID+"|"+branchID+"|"+scope], nil
}

// fakeVerifier 是 EmployeeVerifier 的测试替身。
type fakeVerifier struct {
	auth *service.EmployeeAuth
	err  error
}

// VerifyEmployee 实现 service.EmployeeVerifier。
func (f fakeVerifier) VerifyEmployee(_ context.Context, _, _, _ string) (*service.EmployeeAuth, error) {
	return f.auth, f.err
}

// TestAuthorize_NoScope_PermissionRequired 锁"没有 scope ⇒ 403 member_permission_required"。
func TestAuthorize_NoScope_PermissionRequired(t *testing.T) {
	svc, _ := newService(t)
	svc.SetScopeChecker(fakeChecker{scopes: map[string]bool{}})
	ctx := context.Background()

	_, err := svc.Authorize(ctx, service.AuthorizeInput{
		OperatorID: "u-1", BranchID: "10",
		Scope: model.ScopePointDeduct, Feature: "冲减积分",
	})
	require.Error(t, err)
	var pe *service.PermissionError
	require.True(t, errors.As(err, &pe), "应是 PermissionError,实际 %v", err)
	assert.Equal(t, service.CodePermissionRequired, pe.Code)
	assert.Equal(t, "当前收银员没有冲减积分权限，请输入授权", pe.Message)
	assert.Equal(t, model.ScopePointDeduct, pe.RequiredScope)
}

// TestAuthorize_HasScope_Allowed 锁"自己有 scope ⇒ 放行"。
func TestAuthorize_HasScope_Allowed(t *testing.T) {
	svc, _ := newService(t)
	svc.SetScopeChecker(fakeChecker{scopes: map[string]bool{
		"u-1|10|member:point-deduct": true,
	}})
	ctx := context.Background()

	res, err := svc.Authorize(ctx, service.AuthorizeInput{
		OperatorID: "u-1", BranchID: "10",
		Scope: model.ScopePointDeduct, Feature: "冲减积分",
	})
	require.NoError(t, err)
	assert.Equal(t, model.ScopePointDeduct, res.Scope)
	assert.Empty(t, res.AuthorizedBy, "自己有权限 ⇒ 没有越权授权人")
}

// TestAuthorize_OnlyAuthorizedBy_Denied 锁"**只带** authorized_by ⇒ 必须被拒"。
//
// ⚠️ 2026-10-21 安全修复:旧实现里这一支直接放行 ⇒
//
//	任何调用方只要在 body 里填一个工号,就能在**没有任何权限**的情况下
//	完成写操作(开卡 / 赠分 / 充值 / 改状态 …)——
//	而 POST /members 这些端点是**登录用户就能调到的**。
//
// ⇒ 现在:调用方自己没有该 scope 时,必须走 authorization(工号 + 口令)让 member 验。
// ⇒ 返的是 member_permission_required(带 required_scope):
//
//	POS 收到它会弹授权框(图4)⇒ 调用方补凭据即可继续。
func TestAuthorize_OnlyAuthorizedBy_Denied(t *testing.T) {
	svc, _ := newService(t)
	svc.SetScopeChecker(fakeChecker{scopes: map[string]bool{}})
	ctx := context.Background()

	_, err := svc.Authorize(ctx, service.AuthorizeInput{
		OperatorID: "u-1", BranchID: "10",
		Scope: model.ScopePointDeduct, Feature: "冲减积分",
		AuthorizedBy: "1003",
	})
	require.Error(t, err, "只带 authorized_by 必须被拒(提权漏洞)")
	var pe *service.PermissionError
	require.True(t, errors.As(err, &pe), "应是 PermissionError,实际 %v", err)
	assert.Equal(t, service.CodePermissionRequired, pe.Code)
	assert.Equal(t, model.ScopePointDeduct, pe.RequiredScope)
}

// TestAuthorize_HasScope_AuthorizedByPassedThrough 锁"自己**有** scope ⇒ authorized_by 仍可透传(审计)。
//
// ⚠️ 修复只堵"没有权限却自称已授权"这条路;
//
//	有权限的调用方带 authorized_by(例如"店长在场")仍然放行并写审计。
func TestAuthorize_HasScope_AuthorizedByPassedThrough(t *testing.T) {
	svc, _ := newService(t)
	svc.SetScopeChecker(fakeChecker{scopes: map[string]bool{
		"u-1|10|member:point-deduct": true,
	}})
	ctx := context.Background()

	res, err := svc.Authorize(ctx, service.AuthorizeInput{
		OperatorID: "u-1", BranchID: "10",
		Scope: model.ScopePointDeduct, Feature: "冲减积分",
		AuthorizedBy: "1003",
	})
	require.NoError(t, err, "自己有权限 ⇒ 放行")
	assert.Equal(t, "1003", res.AuthorizedBy, "authorized_by 作为审计信息透传")
}

// TestAuthorize_VerificationDenied 锁"授权不通过 ⇒ 403 member_permission_denied"。
func TestAuthorize_VerificationDenied(t *testing.T) {
	svc, _ := newService(t)
	svc.SetScopeChecker(fakeChecker{scopes: map[string]bool{}})
	svc.SetEmployeeVerifier(fakeVerifier{err: service.ErrCredentialsInvalid})
	ctx := context.Background()

	_, err := svc.Authorize(ctx, service.AuthorizeInput{
		OperatorID: "u-1", BranchID: "10",
		Scope: model.ScopePointDeduct, Feature: "冲减积分",
		Authorization: &service.Authorization{EmployeeCode: "1003", Password: "bad"},
	})
	require.Error(t, err)
	var pe *service.PermissionError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, service.CodePermissionDenied, pe.Code)
	assert.Equal(t, "你没有冲减积分的权限！", pe.Message)
}

// TestAuthorize_AuthorizerWithoutScope_Denied 锁"授权人自己没这个 scope ⇒ 403 denied"。
func TestAuthorize_AuthorizerWithoutScope_Denied(t *testing.T) {
	svc, _ := newService(t)
	svc.SetScopeChecker(fakeChecker{scopes: map[string]bool{}})
	svc.SetEmployeeVerifier(fakeVerifier{auth: &service.EmployeeAuth{
		EmployeeCode: "1003", Verified: true,
		Scopes: []string{model.ScopeView}, // 只有查询权,没有冲减权
	}})
	ctx := context.Background()

	_, err := svc.Authorize(ctx, service.AuthorizeInput{
		OperatorID: "u-1", BranchID: "10",
		Scope: model.ScopePointDeduct, Feature: "冲减积分",
		Authorization: &service.Authorization{EmployeeCode: "1003", Password: "ok"},
	})
	require.Error(t, err)
	var pe *service.PermissionError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, service.CodePermissionDenied, pe.Code)
}

// TestAuthorize_AuthorizerWithScope_Allowed 锁"授权人有这个 scope ⇒ 放行 + 写审计"。
func TestAuthorize_AuthorizerWithScope_Allowed(t *testing.T) {
	svc, _ := newService(t)
	svc.SetScopeChecker(fakeChecker{scopes: map[string]bool{}})
	svc.SetEmployeeVerifier(fakeVerifier{auth: &service.EmployeeAuth{
		EmployeeCode: "1003", Verified: true,
		Scopes: []string{model.ScopeView, model.ScopePointDeduct},
	}})
	ctx := context.Background()

	res, err := svc.Authorize(ctx, service.AuthorizeInput{
		OperatorID: "u-1", BranchID: "10",
		Scope: model.ScopePointDeduct, Feature: "冲减积分",
		Authorization: &service.Authorization{EmployeeCode: "1003", Password: "ok"},
	})
	require.NoError(t, err)
	assert.Equal(t, "1003", res.AuthorizedBy)
}

// TestAuthorize_CheckerError_FailClosed 锁"权限服务不可用 ⇒ 报错(fail-closed)"。
func TestAuthorize_CheckerError_FailClosed(t *testing.T) {
	svc, _ := newService(t)
	svc.SetScopeChecker(fakeChecker{err: errors.New("userd unreachable")})
	ctx := context.Background()

	_, err := svc.Authorize(ctx, service.AuthorizeInput{
		OperatorID: "u-1", BranchID: "10",
		Scope: model.ScopePointDeduct, Feature: "冲减积分",
	})
	require.Error(t, err)
	var pe *service.PermissionError
	assert.False(t, errors.As(err, &pe), "权限服务不可用**不是** 403,而是 503")
}
