package handler_test

// 2026-10-30:**多门店分级基线对比**的权限守卫。
//
// # 锁的是什么
//
//	`GET /forecast-baselines/compare?branches=00,01` —— 请求里的**每一个**门店
//	都必须有 `freshmeat:view` ⇒ 任一无权 ⇒ **403**。
//
//	⚠️ 这是**数据泄露**的守卫:一个门店不该看到别家的分割基线(那是别店的出肉率)。
//
// ⚠ 这些用例走**真实 HTTP 路由** + 真实 SQLite。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/authkit/claims"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/fresh-meat/handler"
	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/YunBright/supertrade/internal/fresh-meat/testdb"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// fcPerms 是权限读取器的 fake(按门店给 scope)。
type fcPerms struct{ scopes map[string][]string }

func (f fcPerms) GetBranchPermissions(_ context.Context, _ string, branchID string) (*userinfo.BranchPermissions, error) {
	return &userinfo.BranchPermissions{
		Branches: []userinfo.BranchPermissionsRow{{BranchID: branchID, Scopes: f.scopes[branchID]}},
	}, nil
}

// newFCRouter 起一个带**按门店 scope**的 fresh-meat 路由,并铺两行基线。
func newFCRouter(t *testing.T, scopes map[string][]string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := testdb.OpenSQLite(":memory:?_pragma=foreign_keys(1)")
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.WholePig{}, &model.PigCut{}, &model.PorkCutsStocktake{},
		&model.LineSalesByPig{}, &model.BranchCutMapping{}, &model.WasteLog{},
		&model.CounterLoadRecord{}, &model.BusinessDay{},
		&model.PigForecastBaseline{}, &model.PigCutEstimate{}, &model.PigForecastBaselineBorrow{},
	))
	svc := service.New(db)
	svc.SetBranchPermissionsReader(fcPerms{scopes: scopes})

	r := gin.New()
	r.Use(middleware.XBranchID())
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(
			claims.WithClaims(c.Request.Context(), &claims.Claims{Sub: "u-1"}))
		c.Next()
	})
	handler.New(svc).RegisterRoutes(r)

	now := time.Now().UTC()
	for _, b := range []string{"S001", "S002"} {
		require.NoError(t, db.Create(&model.PigForecastBaseline{
			ID: "PFB-" + b, BranchID: b, WeightBand: "200-249", CutType: model.CutBelly,
			SampleCount: 5, SampleSource: service.SampleSourceRecorded, SourceBranchID: b,
			MeanKg: decimal.NewFromInt(10), P10Kg: decimal.NewFromInt(8),
			P50Kg: decimal.NewFromInt(10), P90Kg: decimal.NewFromInt(12),
			ComputedAt: now,
		}).Error, "插基线")
	}
	return r
}

func doFC(t *testing.T, r *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(""))
	req.Header.Set("X-Branch-ID", "S001")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestCompareForecastBaselines_NoScopeOnOneBranch_403 锁「任一门店无权 ⇒ 403」。
//
// ⚠️ 只对 S001 有权、想看 S001+S002 ⇒ **403** ⇒ 门店看不到别家的基线。
func TestCompareForecastBaselines_NoScopeOnOneBranch_403(t *testing.T) {
	r := newFCRouter(t, map[string][]string{
		"S001": {service.ScopeFreshmeatView},
		// S002 没给 scope。
	})

	w := doFC(t, r, "/forecast-baselines/compare?branches=S001,S002")
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "forbidden")
	// ⚠️ 403 的响应里**不能**漏出别家的基线数据。
	require.NotContains(t, w.Body.String(), "p50_kg")
}

// TestCompareForecastBaselines_ScopeOnAllBranches_200 锁「都有权 ⇒ 200 且只列请求的门店」。
func TestCompareForecastBaselines_ScopeOnAllBranches_200(t *testing.T) {
	r := newFCRouter(t, map[string][]string{
		"S001": {service.ScopeFreshmeatView},
		"S002": {service.ScopeFreshmeatView},
	})

	w := doFC(t, r, "/forecast-baselines/compare?branches=S001,S002")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var out struct {
		Branches []string `json:"branches"`
		Buckets  []struct {
			WeightBand string `json:"weight_band"`
			CutType    string `json:"cut_type"`
			Baselines  []struct {
				BranchID    string `json:"branch_id"`
				SampleCount int    `json:"sample_count"`
				P50Kg       string `json:"p50_kg"`
				Usable      bool   `json:"usable"`
			} `json:"baselines"`
			MinP50Kg  string  `json:"min_p50_kg"`
			SpreadPct float64 `json:"spread_pct"`
		} `json:"buckets"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.Branches, 2)
	require.Equal(t, "S001", out.Branches[0])
	require.Equal(t, "S002", out.Branches[1])
	require.Len(t, out.Buckets, 1)
	require.Len(t, out.Buckets[0].Baselines, 2)
	for _, b := range out.Buckets[0].Baselines {
		require.Contains(t, []string{"S001", "S002"}, b.BranchID)
	}
	require.Equal(t, "10.000", out.Buckets[0].Baselines[0].P50Kg)
	require.True(t, out.Buckets[0].Baselines[0].Usable)
}
