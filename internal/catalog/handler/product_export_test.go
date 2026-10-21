// 本文件锁住**离线商品库**(GET /products/export)的 HTTP 层行为。
//
// # 为什么单独立一个文件
//
// `GET /products/local`(分页)与 `GET /products/export`(全量)是**两个端点**:
// 响应形状不同(有无分页字段)、过滤口径不同(含不含换购品/停用品)。
// 混在一个文件里,后人会以为"export 只是 local 的一个变体"。
//
// # 守门链说明
//
// 生产上这个端点挂 `rbac.RequireScopeWithBranch("product:view", ...)`,
// 需要 userd 才能跑起来,单测里拉不起来。
// 这里只挂 handler(与既有本地档案测试同款),业务行为(200 / 400 / 字段名)
// 与权限层是**两件事**。
package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/catalog/service"
)

// exportRow 是导出响应里的一行(只声明用到的字段)。
type exportRow struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	SubCode    string   `json:"sub_code"`
	Barcode    string   `json:"barcode"`
	Barcodes   []string `json:"barcodes"`
	Unit       string   `json:"unit"`
	Spec       string   `json:"spec"`
	CategoryID string   `json:"category_id"`
	SupplierID string   `json:"supplier_id"`
	Status     string   `json:"status"`
	UpdatedAt  string   `json:"updated_at"`
}

// exportBody 是导出响应。
type exportBody struct {
	BranchID string      `json:"branch_id"`
	Products []exportRow `json:"products"`
	Count    int         `json:"count"`
	Total    int         `json:"total"`
}

// TestExportProducts_ReturnsEveryProductIncludingSpecial 锁"**换购品 / 停用品也要给**"。
//
// ⚠️ `/products/local` 默认**过滤掉**换购品与停用品(见 service/product.go 的 List)。
//
//	离线商品库缺这两类是**已知缺口**:收银员扫到换购品时
//	POS 查不到商品 ⇒ 只能干瞪眼。
//
// ⇒ 导出端点必须给全,一条都不能少。
func TestExportProducts_ReturnsEveryProductIncludingSpecial(t *testing.T) {
	r := newLocalTestRouter(t)

	// 普通品 / 换购品 / 停用品 各一个。
	for _, c := range []map[string]any{
		{"id": "P-NORMAL", "name": "普通品"},
		{"id": "P-EXCHANGE", "name": "换购品", "is_exchange": true},
		{"id": "P-VOID", "name": "停用品", "is_void": true},
	} {
		w := doJSON(t, r, http.MethodPost, "/products", "10", c)
		require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())
	}

	w := do(t, r, "/products/export", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body exportBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Products, 3, "换购品/停用品也必须在导出里,body=%s", w.Body.String())
	assert.Equal(t, 3, body.Count)
	assert.Equal(t, 3, body.Total)
	assert.Equal(t, "10", body.BranchID)

	ids := map[string]bool{}
	for _, p := range body.Products {
		ids[p.ID] = true
	}
	assert.True(t, ids["P-EXCHANGE"], "换购品必须在导出里")
	assert.True(t, ids["P-VOID"], "停用品必须在导出里")
}

// TestExportProducts_BarcodeFallsBackToProductID 锁"**barcode 回落成 id**"。
//
// ⚠️ `products.barcode` 在生产**根本没赋值**(导入时不写这一列)
// ⇒ 该列全是空串。而绝大多数商品的扫码键就是 `products.id`(思迅 item_no)。
//
// ⇒ 不回落 ⇒ 离线 POS 扫任何码都扫不出商品,而且**零报错**。
func TestExportProducts_BarcodeFallsBackToProductID(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/products", "10", map[string]any{
		"id": "6922303199721", "name": "唯得水乡特色S码抽纸",
	})
	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())

	w = do(t, r, "/products/export", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body exportBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Products, 1)
	assert.Equal(t, "6922303199721", body.Products[0].Barcode,
		"barcode 为空时必须回落成 id(扫码键)")
	assert.Equal(t, []string{"6922303199721"}, body.Products[0].Barcodes,
		"barcodes 也必须含回落后的 id")
}

// TestExportProducts_BarcodesMergeSubTableAndFallback 锁"barcodes = 子表条码 + 回落 id(去重)"。
//
// ⚠️ 一个商品 1~7 条码(口味/规格变体),扫码枪可能扫到**任一条**
// ⇒ 客户端要一张"码 → 商品"的完整索引,不能只给一条。
func TestExportProducts_BarcodesMergeSubTableAndFallback(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/products", "10", map[string]any{
		"id": "P1", "name": "多条码商品",
	})
	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())

	for _, code := range []string{"6908946288183", "6908946288185"} {
		w = doJSON(t, r, http.MethodPost, "/products/P1/barcodes", "10", map[string]any{
			"barcode": code, "label": "口味",
		})
		require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())
	}

	w = do(t, r, "/products/export", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body exportBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Products, 1)
	assert.Equal(t, []string{"6908946288183", "6908946288185", "P1"},
		body.Products[0].Barcodes, "子表条码 + 回落 id,去重")
	assert.Equal(t, "P1", body.Products[0].Barcode)
}

// TestExportProducts_FieldNamesAreExact 锁"**字段名逐字对齐**"。
//
// ⚠️ 客户端按这些名字建本地表 —— 差一个字符就是"整列读不到",
// 而且**零报错**(只是那个字段永远是空)。
func TestExportProducts_FieldNamesAreExact(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/products", "10", map[string]any{
		"id": "P1", "name": "商品名", "sub_code": "SC1", "unit": "提", "spec": "1*8",
		"category_id": "C1", "supplier_id": "S1",
	})
	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())

	w = do(t, r, "/products/export", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var raw struct {
		Products []map[string]any `json:"products"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	require.Len(t, raw.Products, 1, "body=%s", w.Body.String())

	want := []string{
		"id", "name", "sub_code", "barcode", "barcodes", "unit", "spec",
		"category_id", "supplier_id", "status", "updated_at",
	}
	got := make([]string, 0, len(raw.Products[0]))
	for k := range raw.Products[0] {
		got = append(got, k)
	}
	assert.ElementsMatch(t, want, got, "导出行的字段名必须逐字对齐")
}

// TestExportProducts_MissingBranch_400 锁"缺 X-Branch-ID ⇒ 400 branch_required"。
//
// ⚠️ 门店只从 header 取 —— 少了它就没有"哪家店的数据"这回事,
// 不能退化成"导全部分店"。
func TestExportProducts_MissingBranch_400(t *testing.T) {
	r := newLocalTestRouter(t)

	w := do(t, r, "/products/export", "")
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	require.Equal(t, "branch_required", errorCode(t, w))
}

// TestExportProducts_OnlyOwnBranch 锁"只导本门店的商品"。
func TestExportProducts_OnlyOwnBranch(t *testing.T) {
	r := newLocalTestRouter(t)

	for _, branch := range []string{"10", "01"} {
		w := doJSON(t, r, http.MethodPost, "/products", branch, map[string]any{
			"id": "P-" + branch, "name": "商品" + branch,
		})
		require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())
	}

	w := do(t, r, "/products/export", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body exportBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Products, 1)
	assert.Equal(t, "P-10", body.Products[0].ID, "不能导出别的门店的商品")
}

// TestProductsExportRouteWinsOverParam 锁"**/products/export 不被 /products/:id 吃掉**"。
//
// gin 的路由树里静态段优先于参数段 —— 但这条依赖 gin 版本,必须实测
// (既有 TestProductsLocalRouteWinsOverParam 是同一条纪律)。
//
// ⚠️ 如果这条挂了,症状是"GET /products/export 返回 404 product_not_found",
// 看起来像"数据没导入",实际是路由被吃掉了 —— 排查方向会完全跑偏。
func TestProductsExportRouteWinsOverParam(t *testing.T) {
	r := newLocalTestRouter(t)

	w := doJSON(t, r, http.MethodPost, "/products", "10", map[string]any{"id": "P1", "name": "X"})
	require.Equal(t, http.StatusCreated, w.Code)

	w = do(t, r, "/products/export", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body exportBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Products, 1, "命中了错误的 handler:%s", w.Body.String())
	assert.Equal(t, 1, body.Total)
}

// TestExportProducts_ServiceKeepsSpecialProducts 锁"service 层也**不过滤**换购/停用"。
//
// ⚠️ handler 只是壳:过滤若发生在 service,handler 的测试会**假绿**。
func TestExportProducts_ServiceKeepsSpecialProducts(t *testing.T) {
	db := newTestDB(t)
	svc := service.NewProductService(db)

	for _, c := range []struct {
		id   string
		swap bool
	}{
		{"P-NORMAL", false},
		{"P-EXCHANGE", true},
		{"P-VOID", false},
	} {
		_, err := svc.Create(t.Context(), service.CreateProductInput{
			ID: c.id, BranchID: "10", Name: c.id,
			IsExchange: c.swap, IsVoid: !c.swap && c.id == "P-VOID",
		})
		require.NoError(t, err)
	}

	rows, err := svc.ExportAll(t.Context(), "10")
	require.NoError(t, err)
	assert.Len(t, rows, 3, "ExportAll 不得过滤换购品/停用品")
}
