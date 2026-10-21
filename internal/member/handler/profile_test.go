package handler_test

import (
	"net/http"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/member/model"
)

// TestHandler_UpdateMember_RejectsFinancialFields 锁"改积分/储值/状态 ⇒ 400"。
//
// ⚠️ 这些列是本域 POS 业务的事实源,只能走专用端点。
//
//	这里若"收下但忽略",症状是"前端以为改了积分,其实没改"——
//	没有任何人会发现(静默忽略比报错危险)。
func TestHandler_UpdateMember_RejectsFinancialFields(t *testing.T) {
	r, db := newTestEngine(t, fakeChecker{scopes: map[string]bool{
		"u-1|10|member:config": true,
	}}, nil)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10", decimal.Zero)

	for _, field := range []string{
		"points_accumulated", "points_used", "balance_yuan",
		"consumed_amount_yuan", "consumed_count", "used_count", "status",
	} {
		w := do(t, r, http.MethodPut, "/members/M0001", "10", map[string]any{
			"op_id": "op-prof-" + field,
			"name":  "李四",
			field:   999,
		})
		require.Equal(t, http.StatusBadRequest, w.Code,
			"带 %s 应当 400,body=%s", field, w.Body.String())
		assert.Contains(t, w.Body.String(), `"code":"bad_request"`)
	}
}

// TestHandler_UpdateMember_NoScope_PermissionRequired 锁"无 scope ⇒ 403 member_permission_required"。
func TestHandler_UpdateMember_NoScope_PermissionRequired(t *testing.T) {
	r, db := newTestEngine(t, fakeChecker{scopes: map[string]bool{}}, nil)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10", decimal.Zero)

	w := do(t, r, http.MethodPut, "/members/M0001", "10", map[string]any{
		"op_id": "op-prof-1",
		"name":  "李四",
	})
	require.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	body := w.Body.String()
	assert.Contains(t, body, `"code":"member_permission_required"`)
	assert.Contains(t, body, `"required_scope":"member:config"`)
}

// TestHandler_UpdateMember_Allowed 锁"有 scope ⇒ 200 + member"。
func TestHandler_UpdateMember_Allowed(t *testing.T) {
	r, db := newTestEngine(t, fakeChecker{scopes: map[string]bool{
		"u-1|10|member:config": true,
	}}, nil)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10", decimal.Zero)

	w := do(t, r, http.MethodPut, "/members/M0001", "10", map[string]any{
		"op_id": "op-prof-1",
		"name":  "李四",
	})
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), `"member"`)
	assert.Contains(t, w.Body.String(), "李四")

	var m model.Member
	require.NoError(t, db.Where("id = ? AND branch_id = ?", "M0001", "10").First(&m).Error)
	assert.Equal(t, "李四", m.Name)
}

// TestHandler_UpdateMember_UnknownCardType_404 锁"类别不存在 ⇒ 404 member_type_not_found"。
func TestHandler_UpdateMember_UnknownCardType_404(t *testing.T) {
	r, db := newTestEngine(t, fakeChecker{scopes: map[string]bool{
		"u-1|10|member:config": true,
	}}, nil)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10", decimal.Zero)

	w := do(t, r, http.MethodPut, "/members/M0001", "10", map[string]any{
		"op_id":        "op-prof-1",
		"card_type_id": "NOPE",
	})
	require.Equal(t, http.StatusNotFound, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), `"code":"member_type_not_found"`)
}

// TestHandler_Import_NoScope_403 锁"导入端点缺 scope ⇒ 403 forbidden"。
//
// ⚠️ 导入端点挂的是**标准 scope 守门**(member:config),不是图4/图5 的授权链路
//
//	⇒ 无权限时是 403 forbidden(而不是 member_permission_required)。
func TestHandler_Import_NoScope_403(t *testing.T) {
	r, db := newTestEngine(t, fakeChecker{scopes: map[string]bool{}}, nil)
	seedType(t, db, "T1", "10", "会员卡")

	w := do(t, r, http.MethodPost, "/import", "10", map[string]any{
		"source_branch_id": "00",
		"mode":             "incremental",
		"scopes":           []string{"members"},
		"data": map[string]any{
			"members": []map[string]any{{
				"id": "M0001", "card_type": "0", "name": "张三",
				"status": "0", "balance_yuan": "10",
			}},
		},
	})
	require.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), `"code":"forbidden"`)
}
