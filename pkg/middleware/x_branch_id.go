// Package middleware 提供跨 dapr app 复用的 Gin 中间件。
//
// 业务定位:三元权限(user × branch × scope)模型下,所有业务端点都要知道"当前
// 操作门店"。这个门店从 X-Branch-ID header 透传,但 handler 自己不应该重复
// parse UUID、trim whitespace —— 中间件负责搬运,handler 负责消费。
//
// 与 auth 仓库中间件的关系:
//   - auth 仓库的 middleware.XBranchID 走"silent fallback"(auth/README §中间件),
//     不阻断响应,适合所有 userd 端点(公开 + 鉴权后);即使 X-Branch-ID 是垃圾
//     也不应让 /me /login 等挂掉。
//   - 本中间件走严格校验(非合法 UUID 必 400),适合业务端点;业务守门一旦
//     拿错 branchID 就会对错门店的 scope,后果是数据安全级别,应在网关层就拒。
//
// 用法:
//
//	r.Use(middleware.XBranchID())
//	r.GET("/suppliers/:id", func(c *gin.Context) {
//	    branchID := middleware.BranchFromCtx(c)
//	    if branchID == nil { /* 返 400 */ }
//	    ...
//	})
package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ctxBranchKey 是 gin.Context.Set/Get 用的 key,存 *uuid.UUID(指针允许 nil 表示未设)。
//
// 跨服务通用,无 stocktake 前缀(原 internal/stocktake/middleware 用
// "stocktake.x_branch_id",迁移至 pkg 后统一为 "branch.x_branch_id")。
const ctxBranchKey = "branch.x_branch_id"

// XBranchID 解析 X-Branch-ID header 并置入 gin.Context。
//
// 行为:
//   - header 不存在 → 不设 key(handler 拿到的 *uuid.UUID = nil,代表"无 branch 上下文");
//     不阻断请求,因为部分端点(全局列表 / 配置项)不需要 branch。
//   - header 为合法 UUID → trim 后置 ctx(pointer),handler 用 BranchFromCtx(c) 取。
//   - header 非合法 UUID → 400 bad_request 阻断;不允许脏数据流到业务层。
//
// 设计要点:
//   - 强校验(非合法 UUID 必 400):handler 的有效 scope 判断依赖这个 UUID,
//     一个 typo 的 UUID 会让该 user 在错的门店下被允许 → 数据安全风险。
//   - 不阻断未传 header:盘点单列表等端点不该被中间件强迫"必须有门店上下文"。
//   - whitespace 自动 trim:"  <uuid>  " 是 curl 复制粘贴常见错误,中间件替 caller 容错。
//
// 别名: BranchFromHeader(原 internal/stocktake/middleware 命名),保留兼容。
func XBranchID() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := c.GetHeader("X-Branch-ID")
		if raw == "" {
			c.Next()
			return
		}
		trimmed := trimSpace(raw)
		if trimmed == "" {
			c.Next()
			return
		}
		id, err := uuid.Parse(trimmed)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"code":    "bad_request",
				"message": "X-Branch-ID 必须为合法 UUID",
			})
			return
		}
		c.Set(ctxBranchKey, &id)
		c.Next()
	}
}

// BranchFromHeader 是 XBranchID 的别名,保留兼容老 import 路径的代码可读性。
//
// 推荐新代码用 XBranchID()(语义更通用)。
func BranchFromHeader() gin.HandlerFunc { return XBranchID() }

// BranchFromCtx 从 ctx 取出 *uuid.UUID。未设 key 或显式存 nil 都返 nil。
//
// 调用方一般这么用:
//
//	branchID := middleware.BranchFromCtx(c)
//	if branchID == nil {
//	    // 走无 branch 守门逻辑,或返 400
//	} else {
//	    svc.HasEffectiveScope(ctx, sub, branchID.String(), scope)
//	}
func BranchFromCtx(c *gin.Context) *uuid.UUID {
	v, ok := c.Get(ctxBranchKey)
	if !ok || v == nil {
		return nil
	}
	id, ok := v.(*uuid.UUID)
	if !ok {
		return nil
	}
	return id
}

// trimSpace 简单实现,避免引入 strings 包(只 trim ASCII 空格 + tab;UUID 不含其它 whitespace)。
func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}