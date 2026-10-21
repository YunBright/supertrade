package service

import "context"

// bearerCtxKey / branchCtxKey 是本包的 ctx key。
//
// 用**私有空结构体**做 key(而不是 string):别的包不可能构造出同一个 key,
// ctx 里不会撞车。
type bearerCtxKey struct{}

type branchCtxKey struct{}

// WithBearer 把 caller 的 Authorization 头**原文**注入 ctx。
//
// 原文(含 "Bearer " 前缀)—— 下游的验签要的就是这一串。
// 空串表示"无 token"(内部 job 路径),下游会以 401 拒绝,符合预期。
func WithBearer(ctx context.Context, bearer string) context.Context {
	return context.WithValue(ctx, bearerCtxKey{}, bearer)
}

// WithBranchID 把 caller's X-Branch-ID 注入 ctx。
//
// 字面值原样传(支持 `*` / `01,02` 多店),下游自己解析业务语义。
func WithBranchID(ctx context.Context, branchID string) context.Context {
	return context.WithValue(ctx, branchCtxKey{}, branchID)
}

// BearerFromContext 读 ctx 里的 bearer(没有 → "")。
func BearerFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(bearerCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// BranchFromContext 读 ctx 里的门店(没有 → "")。
func BranchFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(branchCtxKey{}).(string); ok {
		return v
	}
	return ""
}
