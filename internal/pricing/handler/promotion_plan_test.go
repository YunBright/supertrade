// 本文件锁住「促销方案(图4)」的 HTTP 层:状态码、响应形状、错误映射,
// 以及**过滤真的收窄命中数**。
//
// ⚠️ 本文件是 `package handler`(与同目录既有测试同包)⇒ 不重复定义
// handler_test.go 里已有的 `do` / `bareRouter` / `stubCube`。
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	_ "modernc.org/sqlite" // 注册驱动名 "sqlite"(纯 Go 驱动,无需 CGO)

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead/cubereadfake"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/pricing/model"
	"github.com/YunBright/supertrade/internal/pricing/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// planSource 替身 cubesource.Resolver。
type planSource struct {
	src cubesource.Source
	err error
}

func (f *planSource) Resolve(_ context.Context, _ string) (cubesource.Source, error) {
	if f.err != nil {
		return cubesource.Source{}, f.err
	}
	return f.src, nil
}

// newPlanDB 构造**文件型** SQLite。
func newPlanDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "pricing.db")
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: dsn}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.PriceList{}, &model.Promotion{}, &model.PromotionPlan{}), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// newPlanRouter 造一个带数据源解析器 + 本地库 + cube 读端的路由。
func newPlanRouter(t *testing.T, src cubesource.Resolver) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.XBranchID())
	db := newPlanDB(t)
	reader := cubereadfake.New()
	reader.Handler = func(m string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		if m != "promotion_plan" {
			return nil, 0, nil
		}
		// ⚠️ 按查询里的方案号过滤(模拟 cube 的 filter 语义)。
		for _, f := range q.Filters {
			if f.Member != "promotion_plan.plan_no" {
				continue
			}
			want := ""
			if len(f.Values) > 0 {
				want, _ = f.Values[0].(string)
			}
			if want != "" && want != "PS002109160001" {
				return nil, 0, nil
			}
		}
		return []map[string]any{{
			"promotion_plan.plan_no": "PS002109160001", "promotion_plan.plan_name": "开业促销",
			"promotion_plan.plan_memo": "开业促销", "promotion_plan.begin_date": "2021-09-17",
			"promotion_plan.end_date": "2021-09-18",
			"promotion_plan.created_at": "09 17 2021  9:55AM",
			"promotion_plan.created_by": "2001",
			"promotion_plan.approved_at": "09 17 2021  9:56AM",
			"promotion_plan.approved_by": "2001",
			"promotion_plan.stopped_at": "", "promotion_plan.stopped_by": "",
			"promotion_plan.approve_flag": "2", "promotion_plan.member_level": "无",
		}}, 1, nil
	}
	plans := service.NewPromotionPlanService(db, nil)
	plans.SetCubeSource(src, reader)
	h := New(nil, nil, nil, nil, nil)
	h.SetPromotionPlans(plans)
	r.GET("/promotion-plans", h.listPromotionPlans)
	r.GET("/promotion-plans/:plan_no", h.getPromotionPlan)
	return r, db
}

// seedPlans 灌本域 PG 的门店 10 促销方案。
func seedPlans(t *testing.T, db *gorm.DB) {
	t.Helper()
	begin := time.Date(2021, 9, 17, 0, 0, 0, 0, time.UTC)
	end := time.Date(2021, 9, 18, 0, 0, 0, 0, time.UTC)
	created := time.Date(2021, 9, 17, 9, 55, 13, 0, time.UTC)
	rows := []any{
		&model.PromotionPlan{PlanNo: "PS002109160001", BranchID: "10", PlanName: "开业促销",
			BeginDate: &begin, EndDate: &end, CreatedAt: &created, CreatedBy: "2001",
			ApproveFlag: "2", ApproveStatus: "approved", MemberLevel: "无"},
		&model.PromotionPlan{PlanNo: "PS002109160002", BranchID: "10", PlanName: "草稿方案",
			BeginDate: &begin, EndDate: &end, CreatedAt: &created, CreatedBy: "2001",
			ApproveFlag: "1", ApproveStatus: "draft", MemberLevel: "无"},
	}
	for _, r := range rows {
		require.NoError(t, db.Create(r).Error, "seed")
	}
}

func doPlan(r *gin.Engine, path, branch string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if branch != "" {
		req.Header.Set("X-Branch-ID", branch)
	}
	r.ServeHTTP(w, req)
	return w
}

func decodePlan(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
	return m
}

// TestPromotionPlans_LocalReturnsRows 锁"没配数据源 ⇒ 200 + 本地数据"。
func TestPromotionPlans_LocalReturnsRows(t *testing.T) {
	r, db := newPlanRouter(t, &planSource{src: cubesource.Source{BranchID: "10"}})
	seedPlans(t, db)

	// ⚠️ exclude_expired 默认 **true** ⇒ 2021 年的方案会被剔除
	// (促销方案是时效数据,过期的不该混在"当前有效"里)。
	// 这里显式关掉它,才能断言"两条路径都能看到历史方案"。
	w := doPlan(r, "/promotion-plans?exclude_expired=false&page=1&page_size=50", "10")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	m := decodePlan(t, w)
	assert.Equal(t, float64(2), m["total"])
	assert.Equal(t, "local", m["data_source"])
	items := m["items"].([]any)
	require.Len(t, items, 2)
	row := items[0].(map[string]any)
	// ⚠️ 审核状态是**翻译后**的,不是源码 "2"。
	assert.Equal(t, "approved", row["approve_status"])
	assert.Equal(t, "已审核", row["approve_status_label"])
}

// TestPromotionPlans_ExcludeExpiredHidesOldOnes 锁"过期方案默认被剔除"。
func TestPromotionPlans_ExcludeExpiredHidesOldOnes(t *testing.T) {
	r, db := newPlanRouter(t, &planSource{src: cubesource.Source{BranchID: "10"}})
	seedPlans(t, db)

	w := doPlan(r, "/promotion-plans", "10")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	m := decodePlan(t, w)
	assert.Equal(t, float64(0), m["total"], "过期的方案默认不该出现在列表里")
}

// TestPromotionPlans_FilterNarrowsCount 锁"过滤真的收窄命中数"。
func TestPromotionPlans_FilterNarrowsCount(t *testing.T) {
	r, db := newPlanRouter(t, &planSource{src: cubesource.Source{BranchID: "10"}})
	seedPlans(t, db)

	all := decodePlan(t, doPlan(r, "/promotion-plans?exclude_expired=false", "10"))
	require.Equal(t, float64(2), all["total"])

	byNo := decodePlan(t, doPlan(r, "/promotion-plans?exclude_expired=false&plan_no=PS002109160001", "10"))
	assert.Equal(t, float64(1), byNo["total"], "方案号过滤必须真的收窄命中数")

	byStatus := decodePlan(t, doPlan(r, "/promotion-plans?exclude_expired=false&approve_status=draft", "10"))
	assert.Equal(t, float64(1), byStatus["total"], "审核状态过滤必须真的收窄命中数")

	none := decodePlan(t, doPlan(r, "/promotion-plans?exclude_expired=false&plan_no=NOPE", "10"))
	assert.Equal(t, float64(0), none["total"], "不存在的条件 ⇒ 命中 0 行")
}

// TestPromotionPlan_NotFoundIs404 锁"查不到 ⇒ 404"。
func TestPromotionPlan_NotFoundIs404(t *testing.T) {
	r, _ := newPlanRouter(t, &planSource{src: cubesource.Source{
		BranchID: "00", Configured: true, Enabled: true,
	}})
	w := doPlan(r, "/promotion-plans/NOPE", "00")
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.Equal(t, "promotion_plan_not_found", decodePlan(t, w)["code"])
}

// TestPromotionPlans_ResolveFailureIs503 锁"问不到数据源 ⇒ 503(**不是**空列表)"。
func TestPromotionPlans_ResolveFailureIs503(t *testing.T) {
	r, db := newPlanRouter(t, &planSource{err: errors.New("cube-router 不可达")})
	seedPlans(t, db)

	w := doPlan(r, "/promotion-plans", "10")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "cube_unavailable", decodePlan(t, w)["code"])
}

// TestPromotionPlans_SourceDisabledIs503 锁"数据源被停用 ⇒ 503"。
func TestPromotionPlans_SourceDisabledIs503(t *testing.T) {
	r, db := newPlanRouter(t, &planSource{src: cubesource.Source{
		BranchID: "00", Configured: true, Enabled: false,
	}})
	seedPlans(t, db)

	w := doPlan(r, "/promotion-plans", "10")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal(t, "cube_source_disabled", decodePlan(t, w)["code"])
}
