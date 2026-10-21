package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite" // 注册驱动名 "sqlite"(纯 Go 驱动,无需 CGO)

	"github.com/YunBright/supertrade/internal/pricing/model"
	"github.com/YunBright/supertrade/internal/pricing/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// newTestDB 构造 SQLite 内存 DB + AutoMigrate。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: ":memory:?_pragma=foreign_keys(1)"}
	db, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err, "open sqlite")
	require.NoError(t, db.AutoMigrate(&model.PriceList{}, &model.Promotion{}), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	return db
}

// localRouter 挂"本地表"相关端点(带 X-Branch-ID 中间件)。
func localRouter(t *testing.T) *gin.Engine {
	t.Helper()
	db := newTestDB(t)
	prices := service.NewPriceListService(db)
	promos := service.NewPromotionService(db)
	// 没有 cube client:本地端点不需要它。
	h := New(service.NewPriceService(nil).WithStore(prices, promos), prices, promos, nil, nil)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())
	r.POST("/archive/import", h.importArchive)
	r.GET("/prices/local", h.getLocalPrices)
	r.GET("/promotions/local", h.getLocalPromotions)
	return r
}

func postJSON(t *testing.T, r *gin.Engine, path, branch, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if branch != "" {
		req.Header.Set("X-Branch-ID", branch)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func getJSON(t *testing.T, r *gin.Engine, path, branch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if branch != "" {
		req.Header.Set("X-Branch-ID", branch)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestImportArchive_WritesPricesAndPromotions 是本文件的主用例:
// 导进去 → 读出来 → 值与类型都对。
//
// ⚠️ 断言金额是**字符串** —— JSON number 到了 Dart 侧会被 jsonDecode 变成 double,
// 精度当场丢掉。
func TestImportArchive_WritesPricesAndPromotions(t *testing.T) {
	r := localRouter(t)
	body := `{
		"source_branch_id": "00",
		"mode": "overwrite",
		"as_of": "2026-09-17",
		"data": {
			"prices": [
				{"product_id": "6957583900828", "price_yuan": "9.900000"},
				{"product_id": "6904406383122", "price_yuan": "16.50", "member_price_yuan": "15.00"}
			],
			"promotions": [
				{"plan_no": "PS-001", "product_id": "6957583900828", "plan_name": "特价",
				 "price_yuan": "8.80", "orig_price_yuan": "9.90",
				 "begin_day": "2026-09-01", "end_day": "2026-09-30"}
			]
		}
	}`
	w := postJSON(t, r, "/archive/import", "10", body)
	require.Equal(t, http.StatusOK, w.Code, "导入应 200: %s", w.Body.String())

	var res struct {
		SourceBranchID string `json:"source_branch_id"`
		TargetBranchID string `json:"target_branch_id"`
		Mode           string `json:"mode"`
		// ByScope 是**统一**的导入结果形状(与 catalog 的 /archive/import 同构):
		// 两行 —— prices 与 promotions。
		ByScope []struct {
			Scope    string `json:"scope"`
			Received int    `json:"received"`
			Inserted int    `json:"inserted"`
		} `json:"by_scope"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	assert.Equal(t, "00", res.SourceBranchID)
	assert.Equal(t, "10", res.TargetBranchID, "目标门店必须来自 header,不能来自 body")
	byScope := map[string]int{}
	for _, s := range res.ByScope {
		byScope[s.Scope] = s.Inserted
	}
	assert.Equal(t, 2, byScope["prices"])
	assert.Equal(t, 1, byScope["promotions"])

	// 读回来:售价必须是**字符串**。
	w = getJSON(t, r, "/prices/local?product_ids=6957583900828,6904406383122", "10")
	require.Equal(t, http.StatusOK, w.Code, "读本地价应 200: %s", w.Body.String())
	var page struct {
		Prices []struct {
			ProductID       string  `json:"product_id"`
			PriceYuan       string  `json:"price_yuan"`
			MemberPriceYuan *string `json:"member_price_yuan"`
		} `json:"prices"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
	require.Len(t, page.Prices, 2)
	byID := map[string]int{}
	for i, p := range page.Prices {
		byID[p.ProductID] = i
	}
	assert.Equal(t, "9.9", page.Prices[byID["6957583900828"]].PriceYuan)
	assert.Equal(t, "16.5", page.Prices[byID["6904406383122"]].PriceYuan)
	assert.Equal(t, "15", *page.Prices[byID["6904406383122"]].MemberPriceYuan)
	assert.Nil(t, page.Prices[byID["6957583900828"]].MemberPriceYuan,
		"没会员价时不能给 0 元(那是'没有会员价',不是'会员价 0 元')")

	// 促销读回来。
	w = getJSON(t, r, "/promotions/local?product_ids=6957583900828&on_date=2026-09-17", "10")
	require.Equal(t, http.StatusOK, w.Code)
	var promoPage struct {
		Promotions []struct {
			ProductID string `json:"product_id"`
			PlanNo    string `json:"plan_no"`
			PriceYuan string `json:"price_yuan"`
			BeginDay  string `json:"begin_day"`
			EndDay    string `json:"end_day"`
		} `json:"promotions"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &promoPage))
	require.Len(t, promoPage.Promotions, 1)
	assert.Equal(t, "PS-001", promoPage.Promotions[0].PlanNo)
	assert.Equal(t, "8.8", promoPage.Promotions[0].PriceYuan)
	assert.Equal(t, "2026-09-01", promoPage.Promotions[0].BeginDay)
	assert.Equal(t, "2026-09-30", promoPage.Promotions[0].EndDay)
}

// TestImportArchive_RequiresBranchHeader 锁"目标门店只从 header 取"。
func TestImportArchive_RequiresBranchHeader(t *testing.T) {
	r := localRouter(t)
	w := postJSON(t, r, "/archive/import", "", `{"prices":[]}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	assert.Equal(t, "branch_required", body["code"])
}

// TestImportArchive_RejectsBadMode 锁"非法 mode 必须报错"。
func TestImportArchive_RejectsBadMode(t *testing.T) {
	r := localRouter(t)
	w := postJSON(t, r, "/archive/import", "10", `{"mode":"destroy","prices":[]}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	assert.Equal(t, "bad_mode", body["code"])
}

// TestImportArchive_InvalidMoneyIsReportedNotSwallowed 锁"非法金额被报出来"。
//
// ⚠️ 静默当 0 ⇒ 一条脏数据变成"售价 0 元",POS 会照印出去。
func TestImportArchive_InvalidMoneyIsReportedNotSwallowed(t *testing.T) {
	r := localRouter(t)
	w := postJSON(t, r, "/archive/import", "10",
		`{"mode":"overwrite","data":{"prices":[{"product_id":"P-BAD-01","price_yuan":"abc"}]}}`)
	require.Equal(t, http.StatusOK, w.Code, "逐行错误不该让整次导入失败")

	var res struct {
		// ByScope 是统一形状:prices / promotions 各一行。
		ByScope []struct {
			Scope    string `json:"scope"`
			Received int    `json:"received"`
			Skipped  int    `json:"skipped"`
		} `json:"by_scope"`
		Errors []struct {
			Scope string `json:"scope"`
			ID    string `json:"id"`
			Error string `json:"error"`
		} `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
	skipped := 0
	for _, s := range res.ByScope {
		if s.Scope == "prices" {
			skipped = s.Skipped
		}
	}
	assert.Equal(t, 1, skipped)
	require.Len(t, res.Errors, 1, "非法金额必须出现在 errors 里")
	assert.Equal(t, "P-BAD-01", res.Errors[0].ID)
	assert.Contains(t, res.Errors[0].Error, "售价解析失败")

	// 那一行**不能**变成售价 0 元。
	w = getJSON(t, r, "/prices/local?product_ids=P-BAD-01", "10")
	require.Equal(t, http.StatusOK, w.Code)
	var page struct {
		Prices []map[string]any `json:"prices"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
	assert.Empty(t, page.Prices, "非法金额的行不应落库")
}

// TestImportArchive_UnpricedProductCreatesNoRow 锁"没维护价不建行"。
//
// POS 要印「未维护售价」,而不是 ¥0.00。
func TestImportArchive_UnpricedProductCreatesNoRow(t *testing.T) {
	r := localRouter(t)
	w := postJSON(t, r, "/archive/import", "10",
		`{"mode":"overwrite","prices":[{"product_id":"P-NO-PRICE","price_yuan":""}]}`)
	require.Equal(t, http.StatusOK, w.Code)

	w = getJSON(t, r, "/prices/local?product_ids=P-NO-PRICE", "10")
	require.Equal(t, http.StatusOK, w.Code)
	var page struct {
		Prices []map[string]any `json:"prices"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
	assert.Empty(t, page.Prices, "没维护价 ⇒ 没有行(而不是有一行、价是 0)")
}

// TestLocalRead_RequiresProductIDs 锁"空集合 ≠ 不过滤"。
func TestLocalRead_RequiresProductIDs(t *testing.T) {
	r := localRouter(t)
	for _, path := range []string{
		"/prices/local",
		"/prices/local?product_ids=",
		"/promotions/local",
	} {
		w := getJSON(t, r, path, "10")
		assert.Equal(t, http.StatusBadRequest, w.Code, "%s 应 400", path)
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		assert.Equal(t, "missing_product_ids", body["code"], "%s 的 code 不对", path)
	}
}

// TestLocalRead_RejectsBadOnDate 锁"非法基准日必须报错"。
func TestLocalRead_RejectsBadOnDate(t *testing.T) {
	r := localRouter(t)
	w := getJSON(t, r, "/promotions/local?product_ids=P-0001&on_date=not-a-date", "10")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	assert.Equal(t, "bad_on_date", body["code"])
}
