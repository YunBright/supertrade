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
	"github.com/YunBright/supertrade/pkg/middleware"
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

// requireLocalSource 拦住「该门店的商品档案来自外部系统」的写操作。
//
// # 为什么写侧也要有这道闸门
//
// 读侧已经按数据源分流(配了源 ⇒ 走那个 cube 实例),所以本域 PG 里
// **对有源门店的行永远没人读**。
//
// ⇒ 写侧若不拦:写进本域 PG 的一行"没人看的商品",而前台还显示"写成功了"
// ⇒ 现场看到的是"商品改了但查出来还是老的",排查方向完全跑偏。
//
// ⚠️ 与会员域(internal/member/handler.go 的同名中间件)**同一套判据**
// (internal/cubesource):同一个门店,会员档案与商品档案必须给同一种解释。
//
// # 为什么复用 ArchiveService.EnsureLocalSource
//
// 它与归档导入路径**用的是同一个方法** ⇒ 同一个门店的"手动建档"与
// "批量导入"给**同一个答案**。
//
// ⚠️ 判据要放在**写端点的第一层**(在 rbac 之后、handler 之前):
// 不然写方法会在本域 PG 里落一行"没人看的商品"。
//
// ⚠️ **问不到路由时放行**(与导入路径同一处理):
// cube-router 抖一下不该挡住自营店(门店 10)的正常建档;
// 而真写了也没关系 —— 没配源的门店本来就读本域。
func (h *Handler) requireLocalSource(c *gin.Context) {
	branchID := middleware.SingleBranchFromCtx(c)
	if branchID == "" {
		// 门店头缺失由 rbac 中间件判 400,这里不重复处理。
		c.Next()
		return
	}
	if h.archive == nil {
		// 装配漏了 archive(只在单测/极简装配里出现)⇒ 判据问不到
		// ⇒ 与"问不到路由"同一处理:放行。
		c.Next()
		return
	}
	if err := h.archive.EnsureLocalSource(c.Request.Context(), branchID); err != nil {
		h.mapErr(c, err)
		c.Abort()
		return
	}
	c.Next()
}
