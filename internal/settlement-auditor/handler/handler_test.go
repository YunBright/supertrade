package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/userinfo"
	dapr "github.com/dapr/go-sdk/client"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/settlement-auditor/handler"
	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
	"github.com/YunBright/supertrade/internal/settlement-auditor/service"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// ---- fakes ----

// fakeCube 实现 service.CubeQuerier,灌 S1 全通过数据(货款 100)。
type fakeCube struct {
	settlements []service.SettlementRow
}

func (f *fakeCube) ListSettlements(_ context.Context, _ string, ids []string) ([]service.SettlementRow, error) {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var out []service.SettlementRow
	for _, r := range f.settlements {
		if want[r.ID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeCube) ListAuditedSettlements(_ context.Context, _, _, _, _ string, _ int) ([]service.SettlementRow, error) {
	return f.settlements, nil
}

func (f *fakeCube) ListPurchaseLines(_ context.Context, _, settlementID string) ([]service.SettlementLineRow, error) {
	if settlementID == "S1" {
		return []service.SettlementLineRow{
			{SettlementID: "S1", VoucherID: "P1", SourceDocType: "PI", TotalGoodsYuan: decimal.NewFromInt(100)},
		}, nil
	}
	return nil, nil
}

func (f *fakeCube) ListPurchaseSheets(_ context.Context, _ string, ids []string) ([]service.PurchaseSheetRow, error) {
	for _, id := range ids {
		if id == "P1" {
			return []service.PurchaseSheetRow{{ID: "P1", SupplierID: "SUP1", DocumentType: "PI", ApproveStatus: "1"}}, nil
		}
	}
	return nil, nil
}

func (f *fakeCube) ListSheetLines(_ context.Context, _ string, _ []string) ([]service.SheetLineRow, error) {
	return []service.SheetLineRow{{SheetID: "P1", ProductID: "PR1", ReceivedQuantity: decimal.NewFromInt(10)}}, nil
}

func (f *fakeCube) SumSalesByProduct(_ context.Context, _ string, _ []string, _, _ string) ([]service.SaleRow, error) {
	return []service.SaleRow{{ProductID: "PR1", NetSaleQuantity: decimal.NewFromInt(10)}}, nil
}

func (f *fakeCube) GetStocks(_ context.Context, _ string, _ []string) ([]service.StockRow, error) {
	return nil, nil
}

func seedCube() *fakeCube {
	return &fakeCube{settlements: []service.SettlementRow{
		{ID: "S1", SupplierID: "SUP1", DocumentType: "DP", ApproveStatus: "1", TotalGoodsYuan: decimal.NewFromInt(100)},
	}}
}

// fakePublisher 记录发布(丢弃即可,handler 层只要求非 nil 以便 apply_event 不报错)。
type fakePublisher struct {
	mu     sync.Mutex
	topics []string
}

func (f *fakePublisher) Publish(_ context.Context, topic string, _ any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.topics = append(f.topics, topic)
	return nil
}

// fakeUserD 模拟 userd:u-admin 放行 settlement 两个 scope,其余用户空。
func fakeUserD(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/invoke/userd/method/internal/users/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/permissions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		bid := r.URL.Query().Get("branch_id")
		if bid == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var scopes []string
		if strings.Contains(r.URL.Path, "u-admin") {
			scopes = []string{"settlement:view", "settlement:audit"}
		}
		resp := userinfo.BranchPermissions{
			Branches: []userinfo.BranchPermissionsRow{{BranchID: bid, Scopes: scopes}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	return httptest.NewServer(mux)
}

// fakeDaprForUserd 把 dapr.Client.InvokeMethodWithContent 转成 httptest HTTP 调用。
type fakeDaprForUserd struct {
	dapr.Client
	baseURL string
}

func (f *fakeDaprForUserd) InvokeMethodWithContent(
	ctx context.Context, appID, method, verb string, content *dapr.DataContent,
) ([]byte, error) {
	path := method
	q := ""
	if i := strings.Index(method, "?"); i >= 0 {
		path = method[:i]
		q = method[i+1:]
	}
	target := f.baseURL + "/v1.0/invoke/" + appID + "/method/" + path
	if q != "" {
		target += "?" + q
	}
	var body io.Reader
	if content != nil && len(content.Data) > 0 {
		body = bytes.NewReader(content.Data)
	}
	req, err := http.NewRequestWithContext(ctx, verb, target, body)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		if resp.StatusCode == 404 {
			return nil, status.Error(codes.NotFound, string(data))
		}
		return nil, status.Error(codes.Internal, string(data))
	}
	return data, nil
}

// ---- 测试基建 ----

// testBranchID 合法 UUID 风格 branch(userinfo 兼容)。
var testBranchID = uuid.NewSHA1(uuid.NameSpaceURL, []byte("test-branch-settlement")).String()

func buildTestHandler(t *testing.T, withCube bool) (*gin.Engine, *gorm.DB, *fakePublisher) {
	t.Helper()
	db, err := testdb.OpenSQLite(":memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.ApprovalInstance{}, &model.ApprovalSettlement{}, &model.AuditSignal{},
		&model.AuditLog{}, &model.LLMVerdict{}, &model.ApprovalStocktakeLink{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pub := &fakePublisher{}
	svc := service.New(db, service.Config{CorpID: "wcorp-test", TemplateID: "tpl-1"})
	if withCube {
		svc.SetCube(seedCube())
	}
	svc.SetPublisher(pub)

	userd := fakeUserD(t)
	t.Cleanup(userd.Close)
	users, err := userinfo.New("userd", userinfo.WithMockClient(&fakeDaprForUserd{baseURL: userd.URL}))
	if err != nil {
		t.Fatalf("userinfo.New: %v", err)
	}
	svc.SetUserInfo(users)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(injectTestClaims())
	r.Use(middleware.XBranchID())
	h := handler.New(svc)
	h.RegisterRoutes(r)
	h.RegisterSubscribeRoutes(r, nil)
	return r, db, pub
}

// injectTestClaims 模拟 sidecar 验签后注入 claims(X-Test-Claims header 控制)。
func injectTestClaims() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := c.GetHeader("X-Test-Claims")
		if raw == "" {
			c.Next()
			return
		}
		var cl claims.Claims
		if err := json.Unmarshal([]byte(raw), &cl); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"code": "bad_claims"})
			return
		}
		c.Request = c.Request.WithContext(claims.WithClaims(c.Request.Context(), &cl))
		c.Next()
	}
}

func claimsHeader(userID string) string {
	b, _ := json.Marshal(claims.Claims{Sub: userID})
	return string(b)
}

// adminReq 带 u-admin claims + branch 的请求头设置。
func adminReq(req *http.Request) {
	req.Header.Set("X-Test-Claims", claimsHeader("u-admin"))
	req.Header.Set("X-Branch-ID", testBranchID)
	// 生产请求恒带 JWT;Submit/Retry 把它透传给流水线作 cube 调用凭证。
	req.Header.Set("Authorization", "Bearer test-admin-token")
}

func waitTerminal(t *testing.T, db *gorm.DB, id string) model.ApprovalInstance {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var ap model.ApprovalInstance
	for {
		if err := db.First(&ap, "id = ?", id).Error; err == nil {
			switch ap.Status {
			case model.StatusApproved, model.StatusRejected, model.StatusFailed:
				return ap
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待终态超时:id=%s status=%s", id, ap.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---- 提交 ----

func TestHandler_Submit_202(t *testing.T) {
	r, db, pub := buildTestHandler(t, true)
	body := `{"applicant_wecom_id":"wecom-u1","settlement_ids":["S1"],"manual_amount_yuan":"100","remark":"9月货款","approver_userids":["apr1"],"cc_userids":["cc1"]}`
	req := httptest.NewRequest(http.MethodPost, "/settlement-approvals", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	adminReq(req)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != model.StatusReceived {
		t.Errorf("响应 status = %s, want received", resp.Status)
	}
	ap := waitTerminal(t, db, resp.ID)
	if ap.Status != model.StatusApproved {
		t.Errorf("流水线终态 = %s, want approved(err=%s)", ap.Status, ap.Error)
	}
	pub.mu.Lock()
	defer pub.mu.Unlock()
	found := false
	for _, tp := range pub.topics {
		if tp == service.TopicWecomApprovalCommand {
			found = true
		}
	}
	if !found {
		t.Error("应发布 wecom.approval.command(apply_event)")
	}
}

func TestHandler_Submit_400_BadAmount(t *testing.T) {
	r, _, _ := buildTestHandler(t, true)
	body := `{"applicant_wecom_id":"wecom-u1","settlement_ids":["S1"],"manual_amount_yuan":"abc"}`
	req := httptest.NewRequest(http.MethodPost, "/settlement-approvals", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	adminReq(req)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
}

func TestHandler_Submit_400_NoBranch(t *testing.T) {
	r, _, _ := buildTestHandler(t, true)
	body := `{"applicant_wecom_id":"wecom-u1","settlement_ids":["S1"],"manual_amount_yuan":"100"}`
	req := httptest.NewRequest(http.MethodPost, "/settlement-approvals", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-admin")) // 无 X-Branch-ID
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400(branch_required)", w.Code)
	}
}

func TestHandler_Submit_401_NoClaims(t *testing.T) {
	r, _, _ := buildTestHandler(t, true)
	body := `{"applicant_wecom_id":"wecom-u1","settlement_ids":["S1"],"manual_amount_yuan":"100"}`
	req := httptest.NewRequest(http.MethodPost, "/settlement-approvals", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Branch-ID", testBranchID) // 无 claims
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHandler_Submit_403_NoScope(t *testing.T) {
	r, _, _ := buildTestHandler(t, true)
	body := `{"applicant_wecom_id":"wecom-u1","settlement_ids":["S1"],"manual_amount_yuan":"100"}`
	req := httptest.NewRequest(http.MethodPost, "/settlement-approvals", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Claims", claimsHeader("u-nobody"))
	req.Header.Set("X-Branch-ID", testBranchID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403, body=%s", w.Code, w.Body.String())
	}
}

// ---- 查询 ----

func TestHandler_List_200(t *testing.T) {
	r, db, _ := buildTestHandler(t, true)
	db.Create(&model.ApprovalInstance{
		ID: "ap-1", BranchID: testBranchID, Status: model.StatusApproved,
		ApplicantUserID: "u-1", ManualAmountYuan: decimal.NewFromInt(100),
	})
	req := httptest.NewRequest(http.MethodGet, "/settlement-approvals?status=approved", nil)
	adminReq(req)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Items []model.ApprovalInstance `json:"items"`
		Total int64                    `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || len(resp.Items) != 1 || resp.Items[0].ID != "ap-1" {
		t.Errorf("total=%d items=%d, want 1(ap-1)", resp.Total, len(resp.Items))
	}
}

func TestHandler_Get_200(t *testing.T) {
	r, db, _ := buildTestHandler(t, true)
	db.Create(&model.ApprovalInstance{
		ID: "ap-1", BranchID: testBranchID, Status: model.StatusApproved,
		ApplicantUserID: "u-1", ManualAmountYuan: decimal.NewFromInt(100),
	})
	db.Create(&model.ApprovalSettlement{
		ID: "snap-1", ApprovalID: "ap-1", SettlementID: "S1",
		GoodsAmountYuan: decimal.NewFromInt(100), Checked: true,
	})
	req := httptest.NewRequest(http.MethodGet, "/settlement-approvals/ap-1", nil)
	adminReq(req)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ap-1"`) || !strings.Contains(w.Body.String(), `"S1"`) {
		t.Errorf("详情应含实例与结算单快照: %s", w.Body.String())
	}
}

func TestHandler_Get_404(t *testing.T) {
	r, _, _ := buildTestHandler(t, true)
	req := httptest.NewRequest(http.MethodGet, "/settlement-approvals/nope", nil)
	adminReq(req)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandler_GetLogs_200(t *testing.T) {
	r, db, _ := buildTestHandler(t, true)
	db.Create(&model.ApprovalInstance{
		ID: "ap-1", BranchID: testBranchID, Status: model.StatusApproved,
		ApplicantUserID: "u-1", ManualAmountYuan: decimal.NewFromInt(100),
	})
	db.Create(&model.AuditLog{ID: "log-1", ApprovalID: "ap-1", Step: "fetch_settlements", OutputSummary: "命中 1 张"})
	req := httptest.NewRequest(http.MethodGet, "/settlement-approvals/ap-1/logs", nil)
	adminReq(req)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fetch_settlements") {
		t.Errorf("日志应含 step: %s", w.Body.String())
	}
}

// ---- 重跑 ----

func TestHandler_Retry_400_NotFailed(t *testing.T) {
	r, db, _ := buildTestHandler(t, true)
	db.Create(&model.ApprovalInstance{
		ID: "ap-1", BranchID: testBranchID, Status: model.StatusApproved,
		ApplicantUserID: "u-1", ManualAmountYuan: decimal.NewFromInt(100),
	})
	req := httptest.NewRequest(http.MethodPost, "/settlement-approvals/ap-1/retry", nil)
	adminReq(req)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400(非 failed 不可重跑), body=%s", w.Code, w.Body.String())
	}
}

func TestHandler_Retry_202(t *testing.T) {
	r, db, _ := buildTestHandler(t, true)
	db.Create(&model.ApprovalInstance{
		ID: "ap-1", BranchID: testBranchID, Status: model.StatusFailed, Error: "cube 抖动",
		ApplicantUserID: "u-1", ApplicantWecomID: "wecom-u1",
		ApproverUserIDs: datatypes.JSON([]byte(`["apr1"]`)),
		ManualAmountYuan: decimal.NewFromInt(100),
	})
	db.Create(&model.ApprovalSettlement{ID: "snap-1", ApprovalID: "ap-1", SettlementID: "S1"})
	req := httptest.NewRequest(http.MethodPost, "/settlement-approvals/ap-1/retry", nil)
	adminReq(req)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202, body=%s", w.Code, w.Body.String())
	}
	ap := waitTerminal(t, db, "ap-1")
	if ap.Status != model.StatusApproved {
		t.Errorf("重跑终态 = %s, want approved(err=%s)", ap.Status, ap.Error)
	}
}

// ---- cube 透传 ----

func TestHandler_CubeSettlements_200(t *testing.T) {
	r, _, _ := buildTestHandler(t, true)
	req := httptest.NewRequest(http.MethodGet, "/settlement-approvals/cube/settlements", nil)
	adminReq(req)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"S1"`) {
		t.Errorf("应含 S1: %s", w.Body.String())
	}
}

func TestHandler_CubeSettlements_503_NoCube(t *testing.T) {
	r, _, _ := buildTestHandler(t, false)
	req := httptest.NewRequest(http.MethodGet, "/settlement-approvals/cube/settlements", nil)
	adminReq(req)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "cube_unavailable") {
		t.Errorf("code 应为 cube_unavailable: %s", w.Body.String())
	}
}

// ---- 打印对账 ----

func TestHandler_Export_200(t *testing.T) {
	r, db, _ := buildTestHandler(t, true)
	db.Create(&model.ApprovalInstance{
		ID: "ap-1", BranchID: testBranchID, Status: model.StatusHumanApproved, SpNo: "SP1",
		ApplicantUserID: "u-1", ManualAmountYuan: decimal.NewFromInt(100),
	})
	db.Create(&model.ApprovalSettlement{
		ID: "snap-1", ApprovalID: "ap-1", SettlementID: "S1", GoodsAmountYuan: decimal.NewFromInt(100), Checked: true,
	})
	req := httptest.NewRequest(http.MethodGet, "/settlement-approvals/export", nil)
	adminReq(req)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %s, want text/html", ct)
	}
	body := w.Body.String()
	for _, want := range []string{"合计", "S1", "100.00", "申请人签名", "财务确认签名"} {
		if !strings.Contains(body, want) {
			t.Errorf("导出 HTML 缺 %q", want)
		}
	}
}

// ---- dapr 订阅 ----

func TestHandler_SubscribeList(t *testing.T) {
	r, _, _ := buildTestHandler(t, true)
	req := httptest.NewRequest(http.MethodGet, "/dapr/subscribe", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var subs []handler.Subscription
	if err := json.Unmarshal(w.Body.Bytes(), &subs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(subs) != 3 {
		t.Fatalf("订阅数 = %d, want 3", len(subs))
	}
	topics := map[string]bool{}
	for _, s := range subs {
		topics[s.Topic] = true
	}
	for _, want := range []string{service.TopicWecomStatusChanged, service.TopicWecomCommandResult, service.TopicAuthAccessChanged} {
		if !topics[want] {
			t.Errorf("缺订阅 %s", want)
		}
	}
}

func TestHandler_SubscribeCommandResult(t *testing.T) {
	r, db, _ := buildTestHandler(t, true)
	db.Create(&model.ApprovalInstance{
		ID: "ap-1", BranchID: testBranchID, Status: model.StatusApproved, LastCommandID: "CMD-1",
		ApplicantUserID: "u-1", ManualAmountYuan: decimal.NewFromInt(100),
	})
	body := `{"data":{"command_id":"CMD-1","type":"apply_event","ok":true,"sp_no":"SP123"}}`
	req := httptest.NewRequest(http.MethodPost, "/events/"+service.TopicWecomCommandResult, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200(订阅永远 200)", w.Code)
	}
	var ap model.ApprovalInstance
	db.First(&ap, "id = ?", "ap-1")
	if ap.SpNo != "SP123" || ap.SpStatus != 1 {
		t.Errorf("回填 sp_no=%s sp_status=%d, want SP123/1", ap.SpNo, ap.SpStatus)
	}
}

func TestHandler_RescanJob(t *testing.T) {
	r, _, _ := buildTestHandler(t, true)
	req := httptest.NewRequest(http.MethodPost, "/job/settlement-auditor-rescan", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"rescanned":0`) {
		t.Errorf("无卡死实例应 rescanned=0: %s", w.Body.String())
	}
}
