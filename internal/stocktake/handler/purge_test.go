package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/YunBright/core/testrun"
	"github.com/YunBright/supertrade/internal/stocktake/handler"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/YunBright/supertrade/internal/stocktake/testdb"
)

const purgeRunID = "ctr-20261013-140000-aa11bb"

func newPurgeDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := testdb.OpenSQLite(":memory:?_pragma=foreign_keys(1)")
	require.NoError(t, err)
	require.NoError(t, testrun.EnsureTable(db))
	require.NoError(t, db.AutoMigrate(
		&model.StocktakeHeader{},
		&model.StocktakeLine{},
		&model.StocktakeLineOperation{},
		&model.StocktakePlanItem{},
		&model.StocktakeBranchDefault{},
		&model.StocktakeExcludedScope{},
	))
	require.NoError(t, testrun.RegisterGorm(db))
	return db
}

// create 走带 RunID 的 context —— 与真实链路一致：账是由上下文里的 run_id
// 触发的，测试不手工往旁表塞数据（那会绕开最该被测的那段代码）。
func create(t *testing.T, db *gorm.DB, runID string, v any) {
	t.Helper()
	require.NoError(t, db.WithContext(testrun.WithRunID(context.Background(), runID)).Create(v).Error)
}

// seedHeaderWithChildren 造一张盘点单并给 4 张从表各塞一行。
//
// runID 非空 = 本次运行造的；空 = 真实业务单（不记账）。
// branchID 单独给：branch_defaults 的主键是 branch_id，用它区分
// 「测试门店的默认单」与「真实门店的默认单」。
func seedHeaderWithChildren(t *testing.T, db *gorm.DB, id, branchID, runID string) {
	t.Helper()
	now := time.Now()
	h := &model.StocktakeHeader{
		ID: id, BranchID: branchID, CountDate: now, Status: model.StatusCounting,
		Type: model.TypeGeneral, OperatorID: "u1",
		CreatedAt: now, UpdatedAt: now,
	}
	create(t, db, runID, h)

	line := &model.StocktakeLine{
		ID: id + "-L1", HeaderID: id, ProductID: "P1", ProductName: "商品",
		BookQty: decimal.NewFromInt(10), ActualQty: decimal.NewFromInt(9),
		BookQtyAt: now, CreatedAt: now, UpdatedAt: now,
	}
	create(t, db, runID, line)

	for _, row := range []any{
		&model.StocktakeLineOperation{
			ID: id + "-O1", HeaderID: id, LineID: line.ID, OpType: model.OpCreate,
			ActorID: "u1", ActorName: "n", PrevQty: decimal.Zero,
			NewQty: decimal.NewFromInt(9), QtyDelta: decimal.NewFromInt(9),
			OpAt: now, Method: model.MethodManual, CreatedAt: now,
		},
		&model.StocktakePlanItem{
			ID: id + "-P1", HeaderID: id, ProductID: "P1", ProductName: "商品", CreatedAt: now,
		},
		&model.StocktakeBranchDefault{
			BranchID: branchID, HeaderID: id, UpdatedBy: "u1", UpdatedAt: now,
		},
	} {
		create(t, db, runID, row)
	}
}

// countRows 统计某表里属于 headerID 的行数。
//
// 用 Unscoped() 是刻意的：stocktake_lines 带 gorm.DeletedAt，若用默认
// 查询，软删掉的行就"看不见"了 —— 测试会绿，而行还躺在库里。
// 兜底清理要验的是"物理上没了"，所以这里也按物理行数算。
func countRows(t *testing.T, db *gorm.DB, m any, headerID string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Unscoped().Model(m).Where("header_id = ?", headerID).Count(&n).Error)
	return n
}

// TestPurge_CascadesAllChildTables —— 本服务最核心的断言。
//
// 少删任何一张表这条就红。历史最容易漏的是 stocktake_branch_defaults：
// 它没有外键，删主表不会带走它，于是"门店默认盘点单"指向一张已删的单，
// 前端"快速进入盘点"直接坏掉 —— 而所有查询都不报错，只是数据在腐烂。
//
// 反证价值：从表若只按"父 id IN (本次运行的父 id)"定位，且 registry 把主表
// 排在前面，子查询恒空 → 各删 0 行且不报错。旁表记账让那条顺序约束不再存在。
func TestPurge_CascadesAllChildTables(t *testing.T) {
	db := newPurgeDB(t)
	seedHeaderWithChildren(t, db, "ST-MINE", "BR-TEST", purgeRunID)

	rep, err := handler.PurgeRegistry().Purge(context.Background(), db, purgeRunID, false)
	require.NoError(t, err)
	require.True(t, rep.OK(), "清理报错: %v", rep.Errors)

	for _, tc := range []struct {
		table string
		m     any
		want  int64
	}{
		{"stocktake_line_operations", &model.StocktakeLineOperation{}, 1},
		{"stocktake_plan_items", &model.StocktakePlanItem{}, 1},
		{"stocktake_branch_defaults", &model.StocktakeBranchDefault{}, 1},
		{"stocktake_lines", &model.StocktakeLine{}, 1},
		{"stocktake_headers", &model.StocktakeHeader{}, 1},
	} {
		assert.EqualValues(t, tc.want, rep.Deleted[tc.table], "%s 删除行数不符", tc.table)
	}

	for _, tc := range []struct {
		table string
		m     any
	}{
		{"stocktake_lines", &model.StocktakeLine{}},
		{"stocktake_line_operations", &model.StocktakeLineOperation{}},
		{"stocktake_plan_items", &model.StocktakePlanItem{}},
		{"stocktake_branch_defaults", &model.StocktakeBranchDefault{}},
	} {
		assert.Zero(t, countRows(t, db, tc.m, "ST-MINE"),
			"%s 残留 —— 这是**无外键**表,不会自动级联,必须显式删", tc.table)
	}
}

// TestPurge_LeavesRealAndOtherRuns —— 绝不跨 run / 碰真实业务单。
func TestPurge_LeavesRealAndOtherRuns(t *testing.T) {
	db := newPurgeDB(t)
	seedHeaderWithChildren(t, db, "ST-MINE", "BR-TEST", purgeRunID)
	seedHeaderWithChildren(t, db, "ST-OTHER", "BR-TEST2", "ctr-20261013-150000-cc22dd")
	seedHeaderWithChildren(t, db, "ST-REAL", "00", "")

	_, err := handler.PurgeRegistry().Purge(context.Background(), db, purgeRunID, false)
	require.NoError(t, err)

	for _, id := range []string{"ST-OTHER", "ST-REAL"} {
		for _, m := range []any{
			&model.StocktakeLine{}, &model.StocktakeLineOperation{},
			&model.StocktakePlanItem{}, &model.StocktakeBranchDefault{},
		} {
			assert.NotZero(t, countRows(t, db, m, id),
				"%s 的从表被误删 —— sweep 绝不能跨 run", id)
		}
	}
}

// TestPurgeRoute_DefaultsToDryRunAndIsUnauthenticated —— 端点层行为。
//
// 免鉴权是刻意的（收尾时未必拿得到 token），公网由 nginx 404 兜底。
func TestPurgeRoute_DefaultsToDryRunAndIsUnauthenticated(t *testing.T) {
	db := newPurgeDB(t)
	seedHeaderWithChildren(t, db, "ST-MINE", "BR-TEST", purgeRunID)

	gin.SetMode(gin.TestMode)
	r := gin.New() // 注意：不挂任何鉴权中间件
	handler.RegisterPurgeRoutes(r, db)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/internal/datastore/purge", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	w := post(`{"run_id":"` + purgeRunID + `"}`)
	require.Equal(t, http.StatusOK, w.Code, "免鉴权:收尾清理不能依赖 token")

	var rep struct {
		DryRun  bool             `json:"dry_run"`
		Deleted map[string]int64 `json:"deleted"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rep))
	assert.True(t, rep.DryRun)
	assert.EqualValues(t, 1, rep.Deleted["stocktake_headers"], "dry-run 也要报告将从表删几行")
	assert.EqualValues(t, 4, rep.Deleted["stocktake_lines"]+rep.Deleted["stocktake_line_operations"]+
		rep.Deleted["stocktake_plan_items"]+rep.Deleted["stocktake_branch_defaults"])
	assert.NotZero(t, countRows(t, db, &model.StocktakeLine{}, "ST-MINE"), "dry-run 绝不能真删")

	w = post(`{"run_id":"` + purgeRunID + `","confirm":true}`)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Zero(t, countRows(t, db, &model.StocktakeLine{}, "ST-MINE"))

	// run_id 非法 → 400（免鉴权端点上的硬要求）
	w = post(`{"run_id":"DROP TABLE users","confirm":true}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_run_id")
}
