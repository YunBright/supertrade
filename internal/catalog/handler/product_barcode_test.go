package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- 扫编码查商品(候选列表) ----

// 响应是**候选列表**而不是单个商品。
func TestGetProductByBarcode_ReturnsCandidates(t *testing.T) {
	r := newLocalTestRouter(t)

	// 照思迅实测:同一自编码对应 正常品 + 换购品 两个货号。
	for _, p := range []map[string]any{
		{"id": "02000110", "name": "远航九江双蒸酒精品500ml", "sub_code": "6904406383122"},
		{"id": "02000111", "name": "(换购)远航九江双蒸酒精品500ml", "sub_code": "6904406383122", "is_exchange": true},
	} {
		w := doJSON(t, r, http.MethodPost, "/products", "10", p)
		require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())
	}

	w := do(t, r, "/products/by-barcode/6904406383122", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body struct {
		Products []struct {
			ID         string `json:"id"`
			SubCode    string `json:"sub_code"`
			IsExchange bool   `json:"is_exchange"`
		} `json:"products"`
		Count int `json:"count"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, 2, body.Count, "一个自编码可能对应两个商品")
	require.Len(t, body.Products, 2)
	assert.Equal(t, "02000110", body.Products[0].ID)
	assert.Equal(t, "02000111", body.Products[1].ID)
	assert.True(t, body.Products[1].IsExchange)
}

// 扫**货号** ⇒ 只有一个候选。
func TestGetProductByBarcode_ByItemNo(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/products", "10", map[string]any{
		"id": "02000111", "name": "(换购)远航九江双蒸酒精品500ml", "sub_code": "6904406383122", "is_exchange": true,
	})
	require.Equal(t, http.StatusCreated, w.Code)

	w = do(t, r, "/products/by-barcode/02000111", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body struct {
		Count int `json:"count"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, 1, body.Count)
}

// 条码表里的条码也要能查到(条码不在 products.barcode 上)。
func TestGetProductByBarcode_FromBarcodeTable(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/products", "10", map[string]any{
		"id": "P1", "name": "酸奶", "barcode": "6908946288183",
	})
	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())

	w = doJSON(t, r, http.MethodPost, "/products/P1/barcodes", "10", map[string]any{
		"barcode": "6908946288185", "label": "桔子味",
	})
	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())

	// 扫**变体**条码 ⇒ 也要能查到同一个商品。
	w = do(t, r, "/products/by-barcode/6908946288185", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body struct {
		Products []struct {
			ID string `json:"id"`
		} `json:"products"`
		Count int `json:"count"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, 1, body.Count)
	require.Len(t, body.Products, 1)
	require.Equal(t, "P1", body.Products[0].ID)
}

func TestGetProductByBarcode_NotFound404(t *testing.T) {
	r := newLocalTestRouter(t)

	w := do(t, r, "/products/by-barcode/NOPE", "10")
	require.Equal(t, http.StatusNotFound, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "product_not_found", errorCode(t, w))
}

// /products/by-barcode/:barcode 必须命中 by-barcode 端点,**不能**被
// /products/:id 吃掉(否则 barcode 会被当成商品 id)。
//
// gin 的路由树里静态段优先于参数段 —— 但这条依赖 gin 版本,必须实测。
// ⚠️ 如果这条挂了,症状是"扫条码返回 503 cube_unavailable"
// (落到 cube 兜底的 getProduct),看起来像"cube 挂了",实际是路由问题。
func TestProductsByBarcodeRouteWinsOverParam(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/products", "10", map[string]any{
		"id": "P1", "name": "X", "barcode": "B1",
	})
	require.Equal(t, http.StatusCreated, w.Code)

	w = do(t, r, "/products/by-barcode/B1", "10")
	require.Equal(t, http.StatusOK, w.Code, "命中了错误的 handler:%s", w.Body.String())
}

// ---- 条码集合的增删 ----

func TestProductBarcodes_ListAndDelete(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/products", "10", map[string]any{"id": "P1", "name": "X"})
	require.Equal(t, http.StatusCreated, w.Code)

	for _, b := range []string{"B1", "B2"} {
		w = doJSON(t, r, http.MethodPost, "/products/P1/barcodes", "10", map[string]any{"barcode": b})
		require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())
	}

	w = do(t, r, "/products/P1/barcodes", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var body struct {
		Barcodes []map[string]any `json:"barcodes"`
		Count    int              `json:"count"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, 2, body.Count)

	w = doJSON(t, r, http.MethodDelete, "/products/P1/barcodes/B1", "10", nil)
	require.Equal(t, http.StatusNoContent, w.Code, "body=%s", w.Body.String())

	w = do(t, r, "/products/P1/barcodes", "10")
	require.Equal(t, http.StatusOK, w.Code)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, 1, body.Count, "删掉一条后应剩一条")
}

// 缺 X-Branch-ID ⇒ 400 branch_required(与其它写侧端点一致)。
func TestProductBarcodes_MissingBranch400(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/products/P1/barcodes", "", map[string]any{"barcode": "B1"})
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "branch_required", errorCode(t, w))
}
