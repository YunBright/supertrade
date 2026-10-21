package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/pricing/service"
)

// stubCube 让用例能精确指定"哪个方法返回哪个错误"。
//
// 用结构体而不是 cubeclientfake:后者永远不返错,
// 而**错误到状态码的映射**正是这套 handler 最容易出错的地方 ——
// 把 ErrCubeAuthRejected 和 ErrCubeUnavailable 合并成同一个 503,
// 后果是登录态问题被显示成「系统繁忙」,且不会有任何测试变红。
type stubCube struct {
	cubeclient.Client
	productErr error
	promoErr   error
	promo      *cubeclient.PromotionDTO
	product    *cubeclient.ProductDTO
	gotBarcode string
	gotDate    string
}

func (s *stubCube) GetProduct(_ context.Context, id string) (*cubeclient.ProductDTO, error) {
	s.gotBarcode = id
	if s.productErr != nil {
		return nil, s.productErr
	}
	return s.product, nil
}

func (s *stubCube) GetActivePromotion(_ context.Context, itemNo, onDate string) (*cubeclient.PromotionDTO, error) {
	s.gotBarcode, s.gotDate = itemNo, onDate
	if s.promoErr != nil {
		return nil, s.promoErr
	}
	return s.promo, nil
}

// bareRouter 只挂 handler,**不挂 RBAC 中间件**。
//
// 为什么要分层:RBAC(authkit 的 RequireScopeWithBranch)不在本包职责内,
// 它需要真 userd。而本包真正要守住的是「错误 → 状态码」映射,那在 handler 里。
// RBAC 接线由文件末尾的 TestRoutes_RequireBranchHeader 单独锁。
func bareRouter(svc *service.PriceService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := New(svc, nil, nil, nil, nil)
	r.GET("/quotes", h.getQuote)
	r.GET("/promotions/active", h.getActivePromo)
	r.GET("/meta", h.getMeta)
	return r
}

func do(t *testing.T, r *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func svcWith(c cubeclient.Client) *service.PriceService {
	return service.NewPriceService(c)
}

func TestQuote_MissingBarcodeIs400(t *testing.T) {
	w := do(t, bareRouter(svcWith(&stubCube{})), "/quotes")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺 barcode 应 400,实际 %d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] != "missing_barcode" {
		t.Errorf("code 应为 missing_barcode,实际 %v", body["code"])
	}
}

func TestQuote_ShortBarcodeIs400InvalidBarcode(t *testing.T) {
	w := do(t, bareRouter(svcWith(&stubCube{})), "/quotes?barcode=123")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("过短条码应 400,实际 %d", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] != "invalid_barcode" {
		t.Errorf("code 应为 invalid_barcode,实际 %v", body["code"])
	}
}

// 无生效促销 = 200 + promo 缺省。这是绝大多数商品的正常状态。
func TestQuote_NoPromoIs200Not404(t *testing.T) {
	s := &stubCube{product: &cubeclient.ProductDTO{ID: "P-1"}}
	w := do(t, bareRouter(svcWith(s)), "/quotes?barcode=6957583900828")
	if w.Code != http.StatusOK {
		t.Fatalf("无促销应是 200,实际 %d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if _, ok := body["promo"]; ok {
		t.Errorf("无促销时不应出现 promo 键,实际 %v", body["promo"])
	}
	if body["barcode"] != "6957583900828" {
		t.Errorf("barcode 应回显入参,实际 %v", body["barcode"])
	}
}

// barcode 必须去掉首尾空格再查 —— 扫码枪常带 CR/LF。
func TestQuote_TrimsBarcodeBeforeQuery(t *testing.T) {
	s := &stubCube{product: &cubeclient.ProductDTO{ID: "P-1"}}
	do(t, bareRouter(svcWith(s)), "/quotes?barcode=%206957583900828%20")
	if s.gotBarcode != "6957583900828" {
		t.Errorf("下发给 cube 的条码应已 trim,实际 %q", s.gotBarcode)
	}
}

// 这一组是本文件的核心:每个错误必须映射到**各自不同**的状态码。
// 只断言"非 200",多组错误会被同一条断言一起放过。
func TestQuote_ErrorsMapToDistinctStatusCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"cube 链路故障", cubeclient.ErrCubeUnavailable, http.StatusServiceUnavailable, "cube_unavailable"},
		{"凭证被拒(登录态没了)", cubeclient.ErrCubeAuthRejected, http.StatusUnauthorized, "unauthorized"},
		{"权限不足", cubeclient.ErrCubeForbidden, http.StatusForbidden, "forbidden"},
		{"查询不合法(调用方 bug)", cubeclient.ErrCubeBadRequest, http.StatusBadRequest, "bad_request"},
		{"未知错误", errors.New("boom"), http.StatusInternalServerError, "internal_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &stubCube{productErr: tc.err}
			w := do(t, bareRouter(svcWith(s)), "/quotes?barcode=6957583900828")
			if w.Code != tc.want {
				t.Fatalf("状态码应 %d,实际 %d body=%s", tc.want, w.Code, w.Body.String())
			}
			var body map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if body["code"] != tc.code {
				t.Errorf("code 应 %q,实际 %v", tc.code, body["code"])
			}
		})
	}
}

// 内部错误**不能**回显原始 err 文本(可能带 SQL / DSN 片段)。
func TestQuote_InternalErrorDoesNotLeakRawError(t *testing.T) {
	s := &stubCube{productErr: errors.New("dsn=postgres://user:pw@host failed")}
	w := do(t, bareRouter(svcWith(s)), "/quotes?barcode=6957583900828")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("应 500,实际 %d", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, "postgres://") || strings.Contains(body, "pw@") {
		t.Errorf("500 响应泄漏了内部错误细节: %s", body)
	}
}

// 促销端点:无促销仍 200,且响应里明确带 promo 键(=null),让调用方能区分
// "查了没有"与"没查"。
func TestActivePromo_AlwaysCarriesPromoKey(t *testing.T) {
	s := &stubCube{product: &cubeclient.ProductDTO{ID: "P-1"}}
	w := do(t, bareRouter(svcWith(s)), "/promotions/active?barcode=6957583900828")
	if w.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	v, ok := body["promo"]
	if !ok {
		t.Fatal("/promotions/active 必须显式带 promo 键(可为 null)")
	}
	if v != nil {
		t.Errorf("无促销时 promo 应为 null,实际 %v", v)
	}
}

// on_date 必须原样透传给 cubeclient —— 判定基准日是业务口径,不能被服务改写。
func TestQuote_PassesOnDateThrough(t *testing.T) {
	s := &stubCube{product: &cubeclient.ProductDTO{ID: "P-1"}}
	do(t, bareRouter(svcWith(s)), "/quotes?barcode=6957583900828&on_date=2026-12-25")
	if s.gotDate != "2026-12-25" {
		t.Errorf("on_date 应原样透传,实际 %q", s.gotDate)
	}
}

// meta 下发价格标签字段名枚举 —— 前端不再自己硬编码一份。
func TestMeta_ListsPromoDateLabels(t *testing.T) {
	w := do(t, bareRouter(svcWith(&stubCube{})), "/meta")
	if w.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d", w.Code)
	}
	var body struct {
		PromoDateLabels []string `json:"promo_date_labels"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if len(body.PromoDateLabels) == 0 {
		t.Fatal("promo_date_labels 不应为空")
	}
	want := map[string]bool{"特价日期": true, "店特日期": true, "活动日期": true}
	for _, l := range body.PromoDateLabels {
		if !want[l] {
			t.Errorf("意外的价格标签字段名 %q", l)
		}
		delete(want, l)
	}
	if len(want) != 0 {
		t.Errorf("缺少价格标签字段名: %v", want)
	}
}

// TestRoutes_RequireBranchHeader 锁 RBAC 接线:RegisterRoutes 出来的路由
// 必须挂 per-branch scope 守门,缺 X-Branch-ID 时 400 branch_required。
//
// 这条用真实的 RegisterRoutes + authkit 中间件(users=nil 时守门会拒),
// 只断言"缺 branch 被拒"这一条 —— 它证明的是**接线存在**,
// 而不是 authkit 自己的鉴权逻辑。
func TestRoutes_RequireBranchHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// 故意不挂 middleware.XBranchID():模拟前端没带 X-Branch-ID 的请求。
	New(svcWith(&stubCube{}), nil, nil, nil, nil).RegisterRoutes(r)

	for _, path := range []string{
		"/quotes?barcode=6957583900828",
		"/promotions/active?barcode=6957583900828",
		"/meta",
		// ⚠️ 归档导出也是 per-branch 守门:导出的是**某个门店**的价格。
		"/archive/export?scope=prices",
	} {
		w := do(t, r, path)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s 缺 X-Branch-ID 应 400,实际 %d", path, w.Code)
			continue
		}
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if body["code"] != "branch_required" {
			t.Errorf("%s 的 code 应为 branch_required,实际 %v", path, body["code"])
		}
	}
}
