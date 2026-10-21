// Package handler / source.go —— 「门店 → 数据源」在 HTTP 层的落地。
//
// # 判据只有一个
//
// supertrade 是 ERP,也是别的 ERP 的代理:门店在 cube-router 的
// `branch_cube_sources` 里配了数据源(富百家 00 / 佳乐 01)⇒
// **该门店的业务数据在外部系统里**,本域 PG 里没有它的行;
// 没配数据源的门店(自营店 10)⇒ 数据在本域 PG。
//
// ⇒ 读端点必须先问一句"我这个门店配了数据源吗":
//
//	配了 ⇒ 走 cube;没配 ⇒ 走本域 PG;问不到 ⇒ 503(不回落本地)
//
// ⚠️ 同一套判据在会员域(internal/member)已经落地 ——
// 同一个门店在不同服务里给出不同的解释,排查时每个服务看起来都"有道理"。
//
// # 错误码与会员域同名同义
//
//	cube_unavailable        503 问不到数据源路由
//	cube_source_disabled    503 数据源被管理员停用
//	forbidden               403 调用者缺该门店的读权限
//	unauthorized            401 登录态失效
//	member_source_is_external 409 该门店的档案在外部系统里(本域只读)
package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/cubeclient"
)

// writeSourceErr 把「数据源」相关的错误映到 HTTP;处理了才返 true。
//
// ⚠️ **403 / 401 与 503 必须分开**:
// 403 = "这个调用者没有该门店的读权限";503 = "数据源路由问不到"。
// 混在一起 ⇒ 用户看到"暂不可用"就去查数据源健康度,
// 而真正的原因是权限(2026-10-21 审计里 member 域踩过这个坑)。
func (h *Handler) writeSourceErr(c *gin.Context, err error) bool {
	switch {
	case errors.Is(err, model.ErrCubeSourceUnavailable):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code":    "cube_unavailable",
			"message": "数据源路由暂不可用,请稍后重试",
		})
	case errors.Is(err, model.ErrCubeSourceDisabled):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"code":    "cube_source_disabled",
			"message": "该门店的数据源已停用",
		})
	case errors.Is(err, model.ErrExternalSourceReadOnly):
		// 409:状态冲突(这个门店的档案在外部系统里,不是权限问题)。
		//
		// ⚠️ code 与会员域同名(`member_source_is_external`):
		// 判据全服务一致 ⇒ 同一个门店的两种档案得到同一种解释。
		//
		// ⚠️ message 里**不出现**内部 app-id(如 cube-sixun-ysx-fb):
		// 那是运维细节,不该出现在给终端用户的文案里。
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"code":    "member_source_is_external",
			"message": err.Error(),
		})
	case errors.Is(err, cubeclient.ErrCubeForbidden):
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"code":    "forbidden",
			"message": "没有该门店的数据读权限",
		})
	case errors.Is(err, cubeclient.ErrCubeAuthRejected):
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"code":    "unauthorized",
			"message": "登录已失效,请重新登录",
		})
	default:
		return false
	}
	return true
}
