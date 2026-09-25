package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/YunBright/supertrade/pkg/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newRouter 构造一个最小 router + 一个 dummy handler,中间件挂路由组上,
// handler 调 middleware.BranchFromCtx 拿值并写入响应 body,便于断言。
func newRouter(mw ...gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	r.Use(mw...)
	r.GET("/probe", func(c *gin.Context) {
		bs := middleware.BranchFromCtx(c)
		if len(bs) == 0 {
			c.JSON(http.StatusOK, gin.H{"branch_ids": nil})
			return
		}
		c.JSON(http.StatusOK, gin.H{"branch_ids": bs})
	})
	return r
}

func TestXBranchID_SingleUUID(t *testing.T) {
	want := uuid.New().String()
	r := newRouter(middleware.XBranchID())

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("X-Branch-ID", want)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "合法 UUID header 应 200: %s", rr.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, []any{want}, body["branch_ids"], "ctx 应是 [uuid]")
}

func TestXBranchID_MultiBranch(t *testing.T) {
	// 多店用户 `X-Branch-ID: 01,02` → 应被解析为 ["01","02"]。
	r := newRouter(middleware.XBranchID())

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("X-Branch-ID", "01,02")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, []any{"01", "02"}, body["branch_ids"])
}

func TestXBranchID_Star(t *testing.T) {
	// `*` 表示全部 accessible branches → 应被原值透传。
	r := newRouter(middleware.XBranchID())

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("X-Branch-ID", "*")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, []any{"*"}, body["branch_ids"])
}

func TestXBranchID_MissingHeader(t *testing.T) {
	// 未传 header → 不阻断,handler 拿到 nil/空切片。
	r := newRouter(middleware.XBranchID())

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "未传 header 不应 4xx")

	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Nil(t, body["branch_ids"], "未设 header 时 BranchFromCtx 应返 nil")
}

func TestXBranchID_InvalidUUIDPassesThrough(t *testing.T) {
	// 2026-09 简化:中间件不再 400;非合法 UUID 也按字面值透传(handler 自己解析)。
	handlerCalled := false
	r := gin.New()
	r.Use(middleware.XBranchID())
	r.GET("/probe", func(c *gin.Context) {
		handlerCalled = true
		c.JSON(http.StatusOK, gin.H{"branch_ids": middleware.BranchFromCtx(c)})
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("X-Branch-ID", "not-a-uuid")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.True(t, handlerCalled, "handler 应被调用(中间件不拦)")

	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, []any{"not-a-uuid"}, body["branch_ids"], "字面值透传")
}

func TestXBranchID_WhitespacePadded(t *testing.T) {
	// curl 复制粘贴常见错误:"  <uuid>  " → 应 trim 后正常解析。
	raw := uuid.New().String()
	r := newRouter(middleware.XBranchID())

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("X-Branch-ID", "  "+raw+"  ")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code,
		"带 whitespace 的合法 UUID 应 200(中间件 trim): %s", rr.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, []any{raw}, body["branch_ids"])
}

func TestXBranchID_WhitespaceBetweenBranches(t *testing.T) {
	// 多店 + 空白: "01, 02 , 03" → ["01","02","03"]。
	r := newRouter(middleware.XBranchID())

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("X-Branch-ID", "01, 02 , 03")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, []any{"01", "02", "03"}, body["branch_ids"])
}

func TestXBranchID_AllWhitespaceHeader(t *testing.T) {
	// header 全是空白(没 UUID)→ 等同未传,不阻断。
	r := newRouter(middleware.XBranchID())

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("X-Branch-ID", "   ")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "全空白 header 不应 4xx")

	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Nil(t, body["branch_ids"], "全空白视为未传,BranchFromCtx 应返 nil")
}

func TestBranchFromCtx_NilOnMissingKey(t *testing.T) {
	// 不挂 XBranchID 中间件,直接调 BranchFromCtx → 应返 nil 不 panic。
	r := gin.New()
	var got []string
	r.GET("/probe", func(c *gin.Context) {
		got = middleware.BranchFromCtx(c)
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.Nil(t, got, "未挂中间件时 BranchFromCtx 应返 nil")
}

// TestBranchFromHeader_Alias 验证老命名仍可调用(向后兼容)。
func TestBranchFromHeader_Alias(t *testing.T) {
	want := uuid.New().String()
	r := newRouter(middleware.BranchFromHeader())

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("X-Branch-ID", want)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, []any{want}, body["branch_ids"], "BranchFromHeader 别名应与 XBranchID 等价")
}

// TestRequireBranch_MissingHeader 验证强制要求中间件。
func TestRequireBranch_MissingHeader(t *testing.T) {
	handlerCalled := false
	r := gin.New()
	r.Use(middleware.RequireBranch())
	r.GET("/probe", func(c *gin.Context) {
		handlerCalled = true
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code,
		"未传 header 应 400: %s", rr.Body.String())
	assert.False(t, handlerCalled, "handler 不应被调用")

	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "missing_branch_id", body["code"])
}

// TestRequireBranch_PresentHeader 通过。
//
// 注意:RequireBranch 假设 XBranchID 已经在它之前跑过(handler 标准用法是
// r.Use(middleware.XBranchID()) 全局挂,各端点再单独加 RequireBranch())。
func TestRequireBranch_PresentHeader(t *testing.T) {
	handlerCalled := false
	r := gin.New()
	r.Use(middleware.XBranchID(), middleware.RequireBranch())
	r.GET("/probe", func(c *gin.Context) {
		handlerCalled = true
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("X-Branch-ID", "01")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	assert.True(t, handlerCalled, "有 header 时 handler 应被调用")
}

// TestSingleBranchFromCtx 验证便捷 accessor。
func TestSingleBranchFromCtx(t *testing.T) {
	r := gin.New()
	r.Use(middleware.XBranchID())
	var got string
	r.GET("/probe", func(c *gin.Context) {
		got = middleware.SingleBranchFromCtx(c)
		c.Status(http.StatusOK)
	})

	// 单店
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("X-Branch-ID", "abc")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	assert.Equal(t, "abc", got)

	// 多店 → 只返第一项
	got = ""
	req = httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("X-Branch-ID", "x,y")
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	assert.Equal(t, "x", got)

	// 未传 → ""
	got = "sentinel"
	req = httptest.NewRequest(http.MethodGet, "/probe", nil)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	assert.Equal(t, "", got)
}