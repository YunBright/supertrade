// 本文件锁住 inventory 的 HTTP 层行为(端点 + Dapr 订阅路由)。
//
// # 守门链说明
//
// 生产上这三个端点挂 `rbac.RequireScopeWithBranch("inventory:view", ...)`,
// 需要 userd 才能跑起来,单测里拉不起来。
// 这里只挂 `middleware.XBranchID()`(注入门店上下文),业务行为
// (200 / 400 / 404 / 分页)与权限层是**两件事** ——
// 权限层由 deployer 的契约测试 + auth 侧保证。
package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite" // 注册驱动名 "sqlite"(纯 Go 驱动,无需 CGO)

	"github.com/YunBright/supertrade/internal/inventory/model"
	"github.com/YunBright/supertrade/internal/inventory/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// newTestRouter 挂 inventory 的端点(不带 rbac 层)。
func newTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())

	dsn := filepath.Join(t.TempDir(), "inventory.db")
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: dsn}
	db, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err, "open sqlite")
	require.NoError(t, db.AutoMigrate(&model.StockLevel{}, &model.StockMove{}), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	// ⚠️ 必须关连接:Windows 上文件被占用时 t.TempDir() 的清理会失败。
	t.Cleanup(func() { _ = sqlDB.Close() })

	h := New(service.NewStockService(db), nil, nil)

	r.GET("/stock-levels", h.listStockLevels)
	r.GET("/stock-levels/:product_id", h.getStockLevel)
	r.GET("/stock-moves", h.listStockMoves)

	// Dapr 内部路由(生产上注册在 auth 中间件之前)。
	h.RegisterSubscribeRoutes(r, nil)

	return r
}

// doGet 打一个 GET 请求。
func doGet(t *testing.T, r *gin.Engine, path, branch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if branch != "" {
		req.Header.Set("X-Branch-ID", branch)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// doPostJSON 打一个带 JSON body 的 POST 请求。
func doPostJSON(t *testing.T, r *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, json.NewEncoder(&buf).Encode(body), "encode body")
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// salePaidBody 是 pos 的 sale.paid 事件体(契约见方案 v2 §6.5)。
func salePaidBody(opID, branchID, orderStatus string, lines []map[string]string) map[string]any {
	return map[string]any{
		"op_id":             opID,
		"sale_id":           "SO20261021000001",
		"branch_id":         branchID,
		"member_id":         "M0001",
		"operator_id":       "u-1",
		"total_amount_yuan": "22.20",
		"paid_at":           "2026-10-21T12:00:00+08:00",
		"order_status":      orderStatus,
		"lines":             lines,
	}
}

// ---- 端点 ----

func TestHandler_ListStockLevels_Pagination(t *testing.T) {
	r := newTestRouter(t)

	// 先用事件灌 3 个商品。
	for i, pid := range []string{"P001", "P002", "P003"} {
		w := doPostJSON(t, r, "/events/sale.paid", salePaidBody(
			"op-"+pid, "10", "S",
			[]map[string]string{{
				"line_id": "L1", "product_id": pid,
				"qty": "1", "unit_price_yuan": "10.00", "amount_yuan": "10.00",
			}},
		))
		require.Equal(t, http.StatusOK, w.Code, "第 %d 次投递应成功", i)
	}

	w := doGet(t, r, "/stock-levels?page=1&page_size=2", "10")
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		StockLevels []model.StockLevel `json:"stock_levels"`
		Count       int                `json:"count"`
		Total       int64              `json:"total"`
		Page        int                `json:"page"`
		PageSize    int                `json:"page_size"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.StockLevels, 2, "第 1 页应只有 2 条")
	assert.Equal(t, int64(3), resp.Total, "total 是过滤后的总条数")
	assert.Equal(t, 2, resp.Count)
	assert.Equal(t, 1, resp.Page)
	assert.Equal(t, 2, resp.PageSize)

	// ⚠️ 金额在 JSON 里是**字符串**(Dart 侧不会变成 double)。
	var raw map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	lvl := raw["stock_levels"].([]any)[0].(map[string]any)
	_, isString := lvl["qty"].(string)
	assert.True(t, isString, "qty 在 JSON 里应是字符串,实际 %T", lvl["qty"])
}

func TestHandler_ListStockLevels_FilterByProductID(t *testing.T) {
	r := newTestRouter(t)
	for _, pid := range []string{"P001", "P002"} {
		doPostJSON(t, r, "/events/sale.paid", salePaidBody(
			"op-"+pid, "10", "S",
			[]map[string]string{{
				"line_id": "L1", "product_id": pid,
				"qty": "1", "unit_price_yuan": "10.00", "amount_yuan": "10.00",
			}},
		))
	}

	w := doGet(t, r, "/stock-levels?product_id=P002", "10")
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		StockLevels []model.StockLevel `json:"stock_levels"`
		Total       int64              `json:"total"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.StockLevels, 1)
	assert.Equal(t, "P002", resp.StockLevels[0].ProductID)
	assert.Equal(t, int64(1), resp.Total)
}

func TestHandler_GetStockLevel_NotFound(t *testing.T) {
	r := newTestRouter(t)

	w := doGet(t, r, "/stock-levels/NOPE", "10")
	require.Equal(t, http.StatusNotFound, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "stock_level_not_found", body["code"], "错误信封应是 {code,message}")
	assert.NotEmpty(t, body["message"])
}

func TestHandler_GetStockLevel_OK(t *testing.T) {
	r := newTestRouter(t)
	doPostJSON(t, r, "/events/sale.paid", salePaidBody(
		"op-1", "10", "S",
		[]map[string]string{{
			"line_id": "L1", "product_id": "P001",
			"qty": "2", "unit_price_yuan": "11.10", "amount_yuan": "22.20",
		}},
	))

	w := doGet(t, r, "/stock-levels/P001", "10")
	require.Equal(t, http.StatusOK, w.Code)
	var lvl model.StockLevel
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &lvl))
	assert.Equal(t, "P001", lvl.ProductID)
	assert.Equal(t, "10", lvl.BranchID)
}

func TestHandler_BranchRequired(t *testing.T) {
	r := newTestRouter(t)

	w := doGet(t, r, "/stock-levels", "")
	require.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "branch_required", body["code"])
}

func TestHandler_BranchMismatch(t *testing.T) {
	r := newTestRouter(t)

	// query 的 branch_id 与 header 不一致 ⇒ 拒(否则能看别人的库存)。
	w := doGet(t, r, "/stock-levels?branch_id=20", "10")
	require.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "branch_mismatch", body["code"])
}

func TestHandler_ListStockMoves(t *testing.T) {
	r := newTestRouter(t)
	doPostJSON(t, r, "/events/sale.paid", salePaidBody(
		"op-1", "10", "S",
		[]map[string]string{{
			"line_id": "L1", "product_id": "P001",
			"qty": "2", "unit_price_yuan": "11.10", "amount_yuan": "22.20",
		}},
	))

	w := doGet(t, r, "/stock-moves?product_id=P001&move_type=sale", "10")
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		StockMoves []model.StockMove `json:"stock_moves"`
		Total      int64             `json:"total"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.StockMoves, 1)
	assert.Equal(t, int64(1), resp.Total)
	assert.Equal(t, "sale", resp.StockMoves[0].MoveType)
	assert.Equal(t, "op-1", resp.StockMoves[0].RefID)

	// 过滤不中 ⇒ 空列表而不是 404。
	w = doGet(t, r, "/stock-moves?move_type=refund", "10")
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.StockMoves, 0)
}

// ---- Dapr 订阅路由 ----

func TestHandler_SubscribeList(t *testing.T) {
	r := newTestRouter(t)

	w := doGet(t, r, "/dapr/subscribe", "")
	require.Equal(t, http.StatusOK, w.Code)
	var subs []Subscription
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &subs))
	require.Len(t, subs, 1)
	assert.Equal(t, "sale.paid", subs[0].Topic)
	assert.Equal(t, "/events/sale.paid", subs[0].Route)
	// ⚠️ pubsubname 必须是全平台唯一总线名(与发布侧逐字相同)。
	assert.Equal(t, "tradewind-pubsub", subs[0].PubsubName)
}

func TestHandler_SalePaid_IdempotentOverHTTP(t *testing.T) {
	r := newTestRouter(t)

	body := salePaidBody("op-1", "10", "S", []map[string]string{{
		"line_id": "L1", "product_id": "P001",
		"qty": "2", "unit_price_yuan": "11.10", "amount_yuan": "22.20",
	}})

	w := doPostJSON(t, r, "/events/sale.paid", body)
	require.Equal(t, http.StatusOK, w.Code)

	w = doPostJSON(t, r, "/events/sale.paid", body)
	require.Equal(t, http.StatusOK, w.Code)

	w = doGet(t, r, "/stock-levels/P001", "10")
	require.Equal(t, http.StatusOK, w.Code)
	var lvl model.StockLevel
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &lvl))
	assert.Equal(t, "-2", lvl.Qty.String(), "重复投递后库存应仍为 -2")
}

func TestHandler_SalePaid_BadPayload(t *testing.T) {
	r := newTestRouter(t)

	// 缺 branch_id ⇒ 400 且不落任何行。
	w := doPostJSON(t, r, "/events/sale.paid", salePaidBody(
		"op-1", "", "S",
		[]map[string]string{{
			"line_id": "L1", "product_id": "P001",
			"qty": "2", "unit_price_yuan": "11.10", "amount_yuan": "22.20",
		}},
	))
	require.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "bad_request", body["code"])

	// lines 为空 ⇒ 400。
	w = doPostJSON(t, r, "/events/sale.paid", salePaidBody("op-1", "10", "S", nil))
	require.Equal(t, http.StatusBadRequest, w.Code)

	// 一行都没落。
	w = doGet(t, r, "/stock-levels", "10")
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Total int64 `json:"total"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, int64(0), resp.Total, "非法载荷不应留下任何行")
}
