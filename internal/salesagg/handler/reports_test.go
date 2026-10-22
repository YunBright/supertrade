// 本文件锁住「图7 销售报表」的 HTTP 层。
//
// # ⚠️ 本文件最重要的一条:**新端点必须挂 scope 守门**
//
// 现有的 `GET /reports/sales`(`handler.go` 里直接 `r.GET("/reports/sales", h.Report)`)
// **漏挂了** rbac.RequireScopeWithBranch —— PLAN §3.5 明写:
// 「所有报表端点必须挂 rbac.RequireScopeWithBranch(现有 /reports/sales 漏挂了 ——
// 新端点不要沿用这个疏漏)」。
//
// ⇒ 本文件用**可判别的**方式锁住它:
// 带 X-Branch-ID 但**不带 token** 时,新端点必须 401(守门拦下),
// 而没挂守门的端点会一路走到 handler(400 store_required)。
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead/cubereadfake"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/salesagg/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// reportSource 替身 cubesource.Resolver。
type reportSource struct {
	src cubesource.Source
	err error
}

func (f *reportSource) Resolve(_ context.Context, _ string) (cubesource.Source, error) {
	if f.err != nil {
		return cubesource.Source{}, f.err
	}
	return f.src, nil
}

// newReportRouter 造一个**只挂业务端点**(不挂 RBAC)的路由。
//
// ⚠️ RBAC 需要真 userd ⇒ 本文件分两层:
//   - 业务逻辑(状态码 / 形状 / 过滤)在这里测;
//   - 守门是否挂上,在 TestReportsRoutes_RequireScope 里用 RegisterRoutes 测。
func newReportRouter(t *testing.T, src cubesource.Resolver) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())
	reader := cubereadfake.New()
	reader.Handler = func(m string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		switch m {
		case "sales_report":
			return []map[string]any{{
				"sales_report.item_no": "00001", "sales_report.oper_date": "2026-10-22",
				"sales_report.supcust_no": "084",
				"sales_report.total_net_amount": "52.80",
				"sales_report.total_net_qty": "11.09",
				"sales_report.total_ref_cost": "30.00",
				"sales_report.costed_rows": "1", "sales_report.count": "1",
				"item.name": "茄子", "item.unit": "kg", "item.brand_name": "PM",
			}}, 1, nil
		case "sale_day":
			return []map[string]any{{
				"sale_day.branch_id": "0001", "sale_day.product_id": "00001",
				"sale_day.business_date": "2026-10-22",
				"sale_day.net_sale_amount_yuan": "52.80",
				"sale_day.net_sale_quantity": "11.09",
				"sale_day.total_cost_yuan": "30.00", "sale_day.count": "1",
			}}, 1, nil
		case "product":
			return []map[string]any{{"product.id": "00001", "product.name": "茄子",
				"product.unit": "kg", "product.category_id": "010201"}}, 1, nil
		case "category":
			return []map[string]any{
				{"category.id": "010201", "category.name": "叶菜类", "category.parent_id": "01"},
				{"category.id": "01", "category.name": "生鲜联营", "category.parent_id": ""},
			}, 2, nil
		case "supplier":
			return []map[string]any{{"supplier.id": "084", "supplier.name": "水果(自营)"}}, 1, nil
		}
		return nil, 0, nil
	}
	svc := service.NewReportsService(reader, nil)
	svc.SetSourceResolver(src)
	h := NewReportsHandler(svc, nil, nil)
	r.GET("/reports/sales/menu-items", h.menuItems)
	r.GET("/reports/sales/by-code", h.byCode)
	return r
}

func doReport(r *gin.Engine, path, branch string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if branch != "" {
		req.Header.Set("X-Branch-ID", branch)
	}
	r.ServeHTTP(w, req)
	return w
}

func decodeReport(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
	return m
}

// TestMenuItems_ReturnsEleven 锁"菜单返回 11 项"。
func TestMenuItems_ReturnsEleven(t *testing.T) {
	r := newReportRouter(t, &reportSource{src: cubesource.Source{BranchID: "00"}})
	w := doReport(r, "/reports/sales/menu-items", "00")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	m := decodeReport(t, w)
	items, ok := m["items"].([]any)
	require.True(t, ok)
	assert.Len(t, items, 11)
}

// TestByCode_RequiresCode 锁"缺 code ⇒ 400"。
func TestByCode_RequiresCode(t *testing.T) {
	r := newReportRouter(t, &reportSource{src: cubesource.Source{BranchID: "00"}})
	w := doReport(r, "/reports/sales/by-code", "00")
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, "bad_request", decodeReport(t, w)["code"])
}

// TestByCode_UnknownCodeIs400 锁"未知 code ⇒ 400 invalid_report_code"。
func TestByCode_UnknownCodeIs400(t *testing.T) {
	r := newReportRouter(t, &reportSource{src: cubesource.Source{BranchID: "00"}})
	w := doReport(r, "/reports/sales/by-code?code=nope", "00")
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Equal(t, "invalid_report_code", decodeReport(t, w)["code"])
}

// TestByCode_ReturnsRows 锁"报表返回真实行 + 汇总带覆盖率"。
func TestByCode_ReturnsRows(t *testing.T) {
	r := newReportRouter(t, &reportSource{src: cubesource.Source{
		BranchID: "00", Configured: true, Enabled: true,
	}})
	w := doReport(r, "/reports/sales/by-code?code=product_detail&from=2026-10-22&to=2026-10-22", "00")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	m := decodeReport(t, w)
	assert.Equal(t, "product_detail", m["code"])
	rows, ok := m["rows"].([]any)
	require.True(t, ok)
	require.Len(t, rows, 1)
	row := rows[0].(map[string]any)
	assert.Equal(t, "52.80", row["net_amount_yuan"])
	summary, ok := m["summary"].(map[string]any)
	require.True(t, ok)
	// ⚠️ 成本是估算值 ⇒ 汇总里**必须**带 costed_rows。
	assert.Contains(t, summary, "costed_rows")
	assert.Contains(t, summary, "cost_coverage")
	assert.Contains(t, summary, "margin_available")
}

// TestByCode_ResolveFailureIs503 锁"问不到数据源 ⇒ 503(**不是**空报表)"。
func TestByCode_ResolveFailureIs503(t *testing.T) {
	r := newReportRouter(t, &reportSource{err: errors.New("cube-router 不可达")})
	w := doReport(r, "/reports/sales/by-code?code=product_detail", "00")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "cube_unavailable", decodeReport(t, w)["code"])
}

// TestReportsRoutes_RequireScope 锁"新端点**挂了** scope 守门"。
//
// ⚠️ 判别方式:带 X-Branch-ID 但不带 token ⇒
//   挂了守门 ⇒ 401 unauthenticated(claims 缺失,守门先拦)
//   没挂守门 ⇒ 一路走到 handler(400 store_required)
//
// 这正是 PLAN §3.5 点名的那个疏漏 —— 必须用能判别的断言锁住。
func TestReportsRoutes_RequireScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())
	reader := cubereadfake.New()
	svc := service.NewReportsService(reader, nil)
	svc.SetSourceResolver(&reportSource{src: cubesource.Source{
		BranchID: "00", Configured: true, Enabled: true,
	}})
	NewReportsHandler(svc, nil, nil).RegisterRoutes(r)

	w := doReport(r, "/reports/sales/menu-items", "00")
	require.Equal(t, http.StatusUnauthorized, w.Code,
		"新端点必须挂 scope 守门(没有 token ⇒ 401),不能像现有 /reports/sales 那样裸奔")
	assert.Equal(t, "unauthenticated", decodeReport(t, w)["code"])
}
