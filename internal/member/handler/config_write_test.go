package handler_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestHandler_ConfigWrite_NoScope_403 锁"配置类写端点缺 scope ⇒ 403 forbidden"。
//
// ⚠️ 与会员域的**交易类**写端点不同:那些走 service.Authorize,
// 无 scope ⇒ 403 member_permission_required(要弹授权框);
// 配置类是**管理员后台动作** ⇒ 标准 rbac 403 forbidden 就够了。
//
// ⚠️ users == nil ⇒ rbac 的 resolver 返 (false, nil) ⇒ 403。
func TestHandler_ConfigWrite_NoScope_403(t *testing.T) {
	r, db := newTestEngine(t, nil, nil)
	seedType(t, db, "T1", "10", "会员卡")

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"写门店会员配置", http.MethodPut, "/config", map[string]any{"membership_enabled": true}},
		{"建会员类别", http.MethodPost, "/types", map[string]any{"id": "T2", "name": "储值卡"}},
		{"改会员类别", http.MethodPut, "/types/T1", map[string]any{"name": "普通会员卡"}},
		{"删会员类别", http.MethodDelete, "/types/T1", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, r, c.method, c.path, "10", c.body)
			assert.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
			assert.Contains(t, w.Body.String(), `"code":"forbidden"`)
		})
	}
}

// TestHandler_ConfigWrite_NoBranch_400 锁"缺 X-Branch-ID ⇒ 400 branch_required"。
//
// ⚠️ 会员类别 / 会员参数是**按门店**的 ⇒ 门店必须明确。
func TestHandler_ConfigWrite_NoBranch_400(t *testing.T) {
	r, _ := newTestEngine(t, nil, nil)

	w := do(t, r, http.MethodPut, "/config", "", map[string]any{"membership_enabled": true})
	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), `"code":"branch_required"`)
}
