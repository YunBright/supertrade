// Package handler / archive.go —— 会员档案的**导入**端点(归档同步用)。
//
// 路由(nginx 已剥过 /api/v1/member/)：
//
//	POST /import   导入会员档案(member:config)
//
// # 它与其它服务的导入端点是**同一套契约**
//
// 请求体 = `archivedto.ImportRequest`(导出方 cube-router / 编排方 cubesync /
// 导入方 member **共用一份字段名** —— 各写一份的后果是
// 「导入报告显示成功 N 条,但字段全是空的」**且零报错**)。
//
// # ⚠️ **目标门店只从 X-Branch-ID header 取**
//
// body 里带一份门店就能跨店写入 —— body 里的门店**根本不读**。
//
// # ⚠️ 守门用 `member:config` 而不是图4/图5 的授权链路
//
//	导入是**管理员/运维的后台批量动作**(一次性建档),不是"收银员在终端上
//	想干没权限的事"⇒ 没必要让 POS 弹授权框。
//	scope 选 member:config 的理由:
//	  - 它在 auth 的 permissions seed 里**已存在**且 admin 在门店10 已有
//	    (不用改 auth 仓、不用加迁移);
//	  - 会员类别(会员档案的引用对象)本来就归 member:config 管
//	    ⇒ 导入"类别 + 会员"用同一个 scope,语义自洽;
//	  - member:card-open 只覆盖"开卡"这一个动作,而导入还会批量改状态/积分/储值,
//	    语义太窄。
package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/member/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// importMembers POST /import
//
// 守门:`member:config` scope(per-branch),与会员类别/门店会员配置同档。
func (h *Handler) importMembers(c *gin.Context) {
	branchID := strings.TrimSpace(middleware.SingleBranchFromCtx(c))
	if branchID == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "branch_required",
			"message": "X-Branch-ID header 必填",
		})
		return
	}
	var req archivedto.ImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "bad_request",
			"message": err.Error(),
		})
		return
	}
	rep, err := h.svc.ImportMembers(c.Request.Context(), service.MemberImportInput{
		SourceBranchID: req.SourceBranchID,
		TargetBranchID: branchID,
		Mode:           req.Mode,
		Data:           req.Data,
	})
	if err != nil {
		mapErr(c, err)
		return
	}
	// ⚠️ 响应体是**共用的** archivedto.ImportResult —— 编排方用同一套代码解析所有下游。
	c.JSON(http.StatusOK, rep)
}
