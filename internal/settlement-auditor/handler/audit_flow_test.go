package handler_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/YunBright/authkit/claims"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// ---- 流程定制配置端点 + 管理员人工放行端点（2026-10-28 用户口径）----
//
// 本文件锁两件事：
//  1. **权限**：配置端点读走 supplier:view / 写走 supplier:manage；
//     人工放行走 settlement:audit **且** 角色 admin（双重守门）。
//  2. **契约**：端点的路径、请求体、响应形状（前端按这个写）。

// claimsHeaderWithRoles 造带角色的 claims（用户口径里的「管理员」= 角色 admin）。
func claimsHeaderWithRoles(userID string, roles ...string) string {
	b, _ := json.Marshal(claims.Claims{Sub: userID, Roles: roles})
	return string(b)
}

// doReq 打一个请求并返回 recorder。
func doReq(t *testing.T, r http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// adminFlowHdr 是「有 supplier:* + settlement:* scope 的管理员」请求头。
func adminFlowHdr(roles ...string) map[string]string {
	return map[string]string{
		"X-Test-Claims": claimsHeaderWithRoles("u-admin", roles...),
		"X-Branch-ID":   testBranchID,
		"Authorization": "Bearer test-admin-token",
	}
}

// ---- 管理员人工放行：双重守门 ----

// seedRejectedForApprove 造一条「被机器拒绝」的审批单。
//
// ⚠️ 必须带**流程选人快照**（审批人/办理人/抄送人）——
//	放行要重新提交企微审批单，没有节点成员 ⇒ buildProcess 报错 ⇒ 单子提交不出去。
func seedRejectedForApprove(t *testing.T, db *gorm.DB, id string) {
	t.Helper()
	db.Create(&model.ApprovalInstance{
		ID: id, BranchID: testBranchID, Status: model.StatusRejected,
		ApplicantUserID: "u-1", ApplicantWecomID: "wecom-u1",
		RejectReason:     "机器拒了",
		ApproverUserIDs:  datatypes.JSON([]byte(`["apr1"]`)),
		HandlerUserIDs:   datatypes.JSON([]byte(`["handler1"]`)),
		CcUserIDs:        datatypes.JSON([]byte(`["cc1"]`)),
	})
}

// TestHandler_AdminApprove_403_NotAdminRole 是**最关键**的一条。
//
// ⚠️ merchant（店长）**持有** settlement:audit ⇒ 只守 scope 不够。
// 判别力自检：把 handler 里的角色校验去掉 ⇒ 本用例立刻红。
func TestHandler_AdminApprove_403_NotAdminRole(t *testing.T) {
	r, db, _ := buildTestHandler(t, true)
	seedRejectedForApprove(t, db, "ap-rejected-1")
	// u-admin 持有 settlement:audit（fakeUserD 给的）⇒ 能过 scope 守门，
	// 但**没有 admin 角色** ⇒ 必须 403。
	w := doReq(t, r, http.MethodPost, "/settlement-approvals/ap-rejected-1/admin-approve", "",
		map[string]string{
			"X-Test-Claims": claimsHeaderWithRoles("u-admin"), // 无角色
			"X-Branch-ID":   testBranchID,
		})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403(非管理员不得放行), body=%s", w.Code, w.Body.String())
	}
	// 状态不该被改。
	var got model.ApprovalInstance
	db.First(&got, "id = ?", "ap-rejected-1")
	if got.Status != model.StatusRejected {
		t.Errorf("被拒的单不该被改状态, got %q", got.Status)
	}
}

// TestHandler_AdminApprove_403_NoScope 没有 settlement:audit ⇒ 403。
func TestHandler_AdminApprove_403_NoScope(t *testing.T) {
	r, _, _ := buildTestHandler(t, true)
	w := doReq(t, r, http.MethodPost, "/settlement-approvals/ap-x/admin-approve", "",
		map[string]string{
			"X-Test-Claims": claimsHeaderWithRoles("u-nobody", "admin"), // 有角色没 scope
			"X-Branch-ID":   testBranchID,
		})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403(无 scope), body=%s", w.Code, w.Body.String())
	}
}

// TestHandler_AdminApprove_200_Admin 管理员（有 scope + admin 角色）放行成功。
func TestHandler_AdminApprove_200_Admin(t *testing.T) {
	r, db, _ := buildTestHandler(t, true)
	seedRejectedForApprove(t, db, "ap-rejected-2")
	w := doReq(t, r, http.MethodPost, "/settlement-approvals/ap-rejected-2/admin-approve", "",
		adminFlowHdr("admin"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if !resp.OK || resp.ID != "ap-rejected-2" {
		t.Errorf("响应应为 {ok:true,id:ap-rejected-2}, got %s", w.Body.String())
	}
}

// ---- 流程配置端点：CRUD + 正则试跑 + 供应商字典 ----

// TestHandler_AuditFlow_SupplierFlows_CRUD 走完一整轮。
func TestHandler_AuditFlow_SupplierFlows_CRUD(t *testing.T) {
	r, _, _ := buildTestHandler(t, true)

	// 建
	w := doReq(t, r, http.MethodPost, "/audit-flows/suppliers",
		`{"supplier_id":"SUP1","supplier_name":"测试供应商","mode":"logic","logic_kind":"daily_single","enabled":true,"note":"备注"}`,
		adminFlowHdr())
	if w.Code != http.StatusOK {
		t.Fatalf("建 status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var row model.AuditSupplierFlow
	if err := json.Unmarshal(w.Body.Bytes(), &row); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if row.ID == "" || row.SupplierID != "SUP1" || row.Mode != model.FlowModeLogic {
		t.Fatalf("响应字段不对: %+v", row)
	}

	// 重复建 ⇒ 409（前端要能把它显示成可读提示，不能是"未知错误"）
	w = doReq(t, r, http.MethodPost, "/audit-flows/suppliers",
		`{"supplier_id":"SUP1","supplier_name":"测试供应商","mode":"auto_pass","enabled":true}`,
		adminFlowHdr())
	if w.Code != http.StatusConflict {
		t.Fatalf("重复建 status = %d, want 409, body=%s", w.Code, w.Body.String())
	}

	// 列
	w = doReq(t, r, http.MethodGet, "/audit-flows/suppliers", "", adminFlowHdr())
	if w.Code != http.StatusOK {
		t.Fatalf("列 status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var list struct {
		Items []model.AuditSupplierFlow `json:"items"`
		Total int                       `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析列表: %v", err)
	}
	if list.Total != 1 || len(list.Items) != 1 {
		t.Fatalf("列表应有 1 条, got total=%d items=%d", list.Total, len(list.Items))
	}

	// 改
	w = doReq(t, r, http.MethodPut, "/audit-flows/suppliers/"+row.ID,
		`{"supplier_id":"SUP1","supplier_name":"改名了","mode":"auto_pass","enabled":false,"note":"改"}`,
		adminFlowHdr())
	if w.Code != http.StatusOK {
		t.Fatalf("改 status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var upd model.AuditSupplierFlow
	json.Unmarshal(w.Body.Bytes(), &upd)
	if upd.Mode != model.FlowModeAutoPass || upd.Enabled || upd.SupplierName != "改名了" {
		t.Fatalf("改后字段不对: %+v", upd)
	}

	// 删
	w = doReq(t, r, http.MethodDelete, "/audit-flows/suppliers/"+row.ID, "", adminFlowHdr())
	if w.Code != http.StatusOK {
		t.Fatalf("删 status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	// 删完再删 ⇒ 404
	w = doReq(t, r, http.MethodDelete, "/audit-flows/suppliers/"+row.ID, "", adminFlowHdr())
	if w.Code != http.StatusNotFound {
		t.Fatalf("重复删 status = %d, want 404, body=%s", w.Code, w.Body.String())
	}
}

// TestHandler_AuditFlow_PaymentItemFlow_And_RegexRef 付款项流程引用正则。
func TestHandler_AuditFlow_PaymentItemFlow_And_RegexRef(t *testing.T) {
	r, _, _ := buildTestHandler(t, true)

	// 先建一条正则
	w := doReq(t, r, http.MethodPost, "/audit-flows/regex-patterns",
		`{"name":"月份","pattern":"(\\d{1,2})\\s*月","group_index":1,"enabled":true}`,
		adminFlowHdr())
	if w.Code != http.StatusOK {
		t.Fatalf("建正则 status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var re model.AuditRegexPattern
	json.Unmarshal(w.Body.Bytes(), &re)
	if re.ID == "" || re.GroupIndex != 1 {
		t.Fatalf("正则响应不对: %+v", re)
	}

	// 付款项流程引用它
	w = doReq(t, r, http.MethodPost, "/audit-flows/payment-items",
		`{"item_type":"水电费","mode":"logic","logic_kind":"monthly_single","regex_id":"`+re.ID+`","enabled":true}`,
		adminFlowHdr())
	if w.Code != http.StatusOK {
		t.Fatalf("建付款项流程 status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var item model.AuditPaymentItemFlow
	json.Unmarshal(w.Body.Bytes(), &item)
	if item.RegexID != re.ID || item.LogicKind != model.LogicMonthlySingle {
		t.Fatalf("付款项流程响应不对: %+v", item)
	}

	// 引用**不存在**的正则 ⇒ 400（不是 500）
	w = doReq(t, r, http.MethodPost, "/audit-flows/payment-items",
		`{"item_type":"租金","mode":"logic","logic_kind":"monthly_single","regex_id":"no-such-id","enabled":true}`,
		adminFlowHdr())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("引用不存在正则 status = %d, want 400, body=%s", w.Code, w.Body.String())
	}

	// 非法正则 ⇒ 400（存之前就编译，别等判一笔付款才暴露）
	w = doReq(t, r, http.MethodPost, "/audit-flows/regex-patterns",
		`{"name":"坏的","pattern":"([a-z","group_index":1,"enabled":true}`,
		adminFlowHdr())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法正则 status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
}

// TestHandler_AuditFlow_RegexTest 锁「9月水电费」→ 9 月（用户口径原例）。
func TestHandler_AuditFlow_RegexTest(t *testing.T) {
	r, _, _ := buildTestHandler(t, true)

	w := doReq(t, r, http.MethodPost, "/audit-flows/regex-test",
		`{"pattern":"(\\d{1,2})\\s*月","group_index":1,"sample":"9月水电费"}`,
		adminFlowHdr())
	if w.Code != http.StatusOK {
		t.Fatalf("试跑 status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var res struct {
		Pattern string   `json:"pattern"`
		Sample  string   `json:"sample"`
		Matched bool     `json:"matched"`
		Value   string   `json:"value"`
		Groups  []string `json:"groups"`
		Month   int      `json:"month"`
		Year    int      `json:"year"`
		Error   string   `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("解析试跑结果: %v", err)
	}
	if !res.Matched {
		t.Fatalf("应匹配到, body=%s", w.Body.String())
	}
	if res.Value != "9" {
		t.Errorf("取值应为 \"9\", got %q", res.Value)
	}
	if res.Month != 9 {
		t.Errorf("解析出的月份应为 9, got %d", res.Month)
	}
	if res.Error != "" {
		t.Errorf("成功时不该有 error, got %q", res.Error)
	}

	// 组号越界 ⇒ 明确报错（不静默返空串）
	w = doReq(t, r, http.MethodPost, "/audit-flows/regex-test",
		`{"pattern":"(\\d{1,2})\\s*月","group_index":5,"sample":"9月水电费"}`,
		adminFlowHdr())
	if w.Code != http.StatusOK {
		t.Fatalf("越界试跑 status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if res.Error == "" {
		t.Errorf("组号越界应给出可读错误, got %+v", res)
	}
}

// TestHandler_AuditFlow_SupplierDict 供应商字典（空字典也是 200，不是错误）。
func TestHandler_AuditFlow_SupplierDict(t *testing.T) {
	r, _, _ := buildTestHandler(t, true)
	w := doReq(t, r, http.MethodGet, "/audit-flows/supplier-dict", "", adminFlowHdr())
	if w.Code != http.StatusOK {
		t.Fatalf("字典 status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Items []struct {
			SupplierID   string `json:"supplier_id"`
			SupplierName string `json:"supplier_name"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析字典: %v", err)
	}
	if resp.Total != len(resp.Items) {
		t.Errorf("total 与 items 长度不一致: total=%d items=%d", resp.Total, len(resp.Items))
	}
}

// TestHandler_AuditFlow_WriteRequiresManageScope 写端点必须守 supplier:manage。
//
// u-viewonly 有 supplier:view（能读）但没有 supplier:manage（不能写）。
//
// ⚠️ 判别力自检：把写端点的 scope 改成 supplier:view ⇒ 本用例立刻红。
func TestHandler_AuditFlow_WriteRequiresManageScope(t *testing.T) {
	r, _, _ := buildTestHandler(t, true)
	hdr := map[string]string{
		"X-Test-Claims": claimsHeaderWithRoles("u-viewonly"),
		"X-Branch-ID":   testBranchID,
	}
	// 读端点：supplier:view ⇒ 200
	w := doReq(t, r, http.MethodGet, "/audit-flows/suppliers", "", hdr)
	if w.Code != http.StatusOK {
		t.Fatalf("读 status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	// 写端点：需要 supplier:manage ⇒ 403
	w = doReq(t, r, http.MethodPost, "/audit-flows/suppliers",
		`{"supplier_id":"SUP9","mode":"auto_pass","enabled":true}`, hdr)
	if w.Code != http.StatusForbidden {
		t.Fatalf("写 status = %d, want 403(写端点需 supplier:manage), body=%s", w.Code, w.Body.String())
	}
}
