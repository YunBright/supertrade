// Package auth 提供 WS 升级前的 audience 校验。
//
// 注意:本服务**不**做 JWT 验签。
// cmdbootstrap 已经把 claims.GinMiddleware() 装到路由链上,
// /ws handler 可以从 gin.Context 直接拿 *claims.Claims。
// 这里只校验 audience 含 "notification-gateway",作为路由层的二次防御。
package auth

import (
	"errors"

	"github.com/YunBright/authkit/claims"
)

// ExpectedAudience 本服务要求的 aud 标记(与 dapr app-id 一致)。
const ExpectedAudience = "notification-gateway"

// Errors returned by RequireAudience.
var (
	ErrAudienceMismatch = errors.New("ws auth: audience mismatch")
	ErrNoAudience       = errors.New("ws auth: no audience in claims")
)

// RequireAudience 校验 claims.Aud 是否含 ExpectedAudience。
func RequireAudience(c *claims.Claims) error {
	if c == nil {
		return ErrNoAudience
	}
	if !c.IsAudience(ExpectedAudience) {
		return ErrAudienceMismatch
	}
	return nil
}
