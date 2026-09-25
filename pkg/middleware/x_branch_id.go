// Package middleware 提供跨 dapr app 复用的 Gin 中间件。
//
// 业务定位:三元权限(user × branch × scope)模型下,业务端点要知道"当前
// 操作门店"。这个门店从 X-Branch-ID header 透传,middleware 只负责搬运
// 到 ctx,不校验格式、不强制存在;handler 自己决定要不要拒绝。
//
// 为什么 middleware 不校验:
//   - 多店用户会用 `X-Branch-ID: 01,02` 或 `*`(全部 accessible branches) —
//     UUID 校验会让这些合法用法 400。
//   - 部分端点(全局配置 / 跨店聚合)不应该被中间件强制要求门店。
//   - 需要严格单店操作的端点(handler 层),用 BranchRequired() 或自己 parse。
//
// 与 auth 仓库中间件的关系:
//   - auth 仓库的 middleware.XBranchID 走"silent fallback",不阻断响应;
//     适合所有 userd 端点(公开 + 鉴权后)。
//   - 本中间件同样 silent;区别在于仓库归属 + 命名约定(`branch.x_branch_id` key)。
//
// 用法:
//
//	r.Use(middleware.XBranchID())                 // 透传,不强制
//	r.GET("/suppliers/:id",
//	    middleware.RequireBranch(),                // 此端点强制要求 header
//	    handler.ListSuppliers,
//	)
//
//	branchIDs := middleware.BranchFromCtx(c)      // "" = 未传;否则原值(含 "*" / "01,02")
//	if len(branchIDs) == 1 { svc.DoSingle(...) }
//	else { svc.DoMulti(...) }
package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// ctxBranchKey 是 gin.Context.Set/Get 用的 key,存 []string(切片允许多店 + 区分未设)。
//
// 跨服务通用,无 stocktake 前缀(原 internal/stocktake/middleware 用
// "stocktake.x_branch_id",迁移至 pkg 后统一为 "branch.x_branch_id")。
const ctxBranchKey = "branch.x_branch_id"

// XBranchID 解析 X-Branch-ID header 并置入 gin.Context(原值透传,不校验)。
//
// 行为:
//   - header 不存在 → 不设 key(handler 拿到 []string{});不阻断。
//   - header 为空字符串 → 同上,设成 []。
//   - header 为 "*" → ctx = []string{"*"},表示"全部 accessible branches",
//     由下游解析(userd / catalog 等拿到后自己展开 JWT claims.AccessibleBranches)。
//   - header 为 "01,02" → ctx = []string{"01","02"},逗号分隔多店;
//     本 middleware 不验证每项,只 trim 空白。
//   - header 为单 UUID → ctx = []string{"<uuid>"};handler 需要严格单店
//     UUID 操作时自己 uuid.Parse + 拒错。
//
// 设计要点(2026-09 简化):
//   - 不再强制 UUID 解析:多店用户`*` / `01,02` 是合法用法,UUID 校验会误拒。
//   - 不阻断未传 header:盘点单列表等端点不该被中间件强制"必须有门店上下文"。
//   - whitespace 自动 trim 各项:curl 复制粘贴常见错误。
//   - 中间件值是"字面值传给下游" — 下游自己解析业务语义。
func XBranchID() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := c.GetHeader("X-Branch-ID")
		if raw == "" {
			c.Next()
			return
		}
		parts := splitAndTrim(raw, ",")
		if len(parts) == 0 {
			c.Next()
			return
		}
		c.Set(ctxBranchKey, parts)
		c.Next()
	}
}

// BranchFromHeader 是 XBranchID 的别名,保留兼容老 import 路径的代码可读性。
//
// 推荐新代码用 XBranchID()(语义更通用)。
func BranchFromHeader() gin.HandlerFunc { return XBranchID() }

// BranchFromCtx 从 ctx 取出 []string。返回:
//   - nil:header 未传或显式清空
//   - []string{"*"}:`*` 全部 accessible
//   - []string{"01","02"}:多店列表
//   - []string{"<uuid>"}:单店(可能是 UUID 或别的业务标识)
//
// 调用方一般这么用:
//
//	bs := middleware.BranchFromCtx(c)
//	if len(bs) == 0 {
//	    // handler 自己决定 400 还是放行(跨店聚合端点)
//	}
//	for _, b := range bs {
//	    // 单店处理
//	}
func BranchFromCtx(c *gin.Context) []string {
	v, ok := c.Get(ctxBranchKey)
	if !ok || v == nil {
		return nil
	}
	bs, ok := v.([]string)
	if !ok {
		return nil
	}
	return bs
}

// SingleBranchFromCtx 返回 ctx 第一个 branch(单店场景的便捷访问)。
//
// 多店 / `*` 时只返第一项,业务侧应避免用此 helper 走 per-branch scope 校验
// (会拿到错的 branch);多店应该用 BranchFromCtx 迭代。
//
// header 未传时返 ""。
func SingleBranchFromCtx(c *gin.Context) string {
	bs := BranchFromCtx(c)
	if len(bs) == 0 {
		return ""
	}
	return bs[0]
}

// RequireBranch 强制要求 X-Branch-ID header 非空;缺失返 400 missing_branch_id。
//
// 用于"必须有门店上下文"的业务端点(handler 不想自己写 if 判断时挂这个中间件)。
// 不校验 UUID —— UUID 校验交给 handler(用 uuid.Parse 进一步拒非合法格式)。
//
// 路由组用法:
//
//	api := r.Group("/api", middleware.XBranchID())
//	api.GET("/suppliers", middleware.RequireBranch(), handler.ListSuppliers)
func RequireBranch() gin.HandlerFunc {
	return func(c *gin.Context) {
		if len(BranchFromCtx(c)) == 0 {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"code":    "missing_branch_id",
				"message": "X-Branch-ID header 必填(支持 UUID / * / 逗号多店)",
			})
			return
		}
		c.Next()
	}
}

// splitAndTrim 按 sep 切 s,逐项 trim ASCII 空白,跳过空项。
func splitAndTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// (历史保留 trimSpace / uuid import 已移除;新实现走 strings.Split + TrimSpace,
// 简化包内 API 边界。)