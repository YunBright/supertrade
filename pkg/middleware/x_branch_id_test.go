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
	// gin 在 test 模式下不打印路由信息
	gin.SetMode(gin.TestMode)
}

// newRouter 构造一个最小 router + 一个 dummy handler,中间件挂路由组上,
// handler 调 middleware.BranchFromCtx 拿值并写入响应 body,便于断言。
func newRouter(mw ...gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	r.Use(mw...)
	r.GET("/probe", func(c *gin.Context) {
		id := middleware.BranchFromCtx(c)
		if id == nil {
			c.JSON(http.StatusOK, gin.H{"branch_id": nil})
			return
		}
		c.JSON(http.StatusOK, gin.H{"branch_id": id.String()})
	})
	return r
}

func TestXBranchID_ValidUUID(t *testing.T) {
	want := uuid.New().String()
	r := newRouter(middleware.XBranchID())

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("X-Branch-ID", want)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "合法 UUID header 应 200: %s", rr.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, want, body["branch_id"], "ctx 中的 branch_id 应等于 header")
}

func TestXBranchID_MissingHeader(t *testing.T) {
	// 未传 header → 不阻断,handler 拿 nil(branch_id=null)。
	r := newRouter(middleware.XBranchID())

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "未传 header 不应 4xx")

	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Nil(t, body["branch_id"], "未设 header 时 BranchFromCtx 应返 nil")
}

func TestXBranchID_InvalidUUID(t *testing.T) {
	// 非合法 UUID → 400 bad_request,handler 不应被调用。
	handlerCalled := false
	r := gin.New()
	r.Use(middleware.XBranchID())
	r.GET("/probe", func(c *gin.Context) {
		handlerCalled = true
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("X-Branch-ID", "not-a-uuid")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code,
		"非法 UUID header 应 400: %s", rr.Body.String())
	assert.False(t, handlerCalled, "handler 不应被调用")

	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "bad_request", body["code"])
	assert.Contains(t, body["message"], "X-Branch-ID")
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
	assert.Equal(t, raw, body["branch_id"])
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
	assert.Nil(t, body["branch_id"], "全空白视为未传,BranchFromCtx 应返 nil")
}

func TestBranchFromCtx_NilOnMissingKey(t *testing.T) {
	// 不挂 XBranchID 中间件,直接调 BranchFromCtx → 应返 nil 不 panic。
	r := gin.New()
	var got *uuid.UUID
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
	assert.Equal(t, want, body["branch_id"], "BranchFromHeader 别名应与 XBranchID 等价")
}