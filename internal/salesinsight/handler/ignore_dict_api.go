package handler

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/salesinsight/fact/supplier"
)

// 供应商字典的只读端点（供 App 维护忽略名单时用）。
//
// # 为什么需要它
//
// 推荐判定依赖 `type` 与 `cust_flag` 两个字段，而它们只存在于
// cube 的 supplier 字典里 —— App 侧拿不到：
//
//   - catalog 的 `/suppliers` 只返回 id/name/contact/phone（实测 2026-10-17），
//     且当前身份还没有 `supplier:view` scope（403）；
//   - build 内部的 SupplierDict 只在"按销售流水 TopN 的编码"那条路径上跑，
//     用完即弃，没有"列出本门店字典"的入口。
//
// ⚠️ 所以字典由**服务端**查，而不是让 App 把自己看到的记录发上来。
//
//	早先的实现是"App 传字典、服务端只做判定"—— 那在服务端有查询能力时是错的：
//	App 上的数据源可能与 cube 不同，判定结果会与生成报告时用的不一致，
//	用户会看到"我配好了，报告里还是自用"这种无法自洽的现象。

// DictProvider 列出某门店的供应商字典（分页 + 搜索）。
type DictProvider interface {
	SupplierDictPage(ctx context.Context, storeBranchID string,
		offset, limit int, query string) ([]supplier.Record, error)
}

// MaxDictPageSize 是单页字典条数上限。
//
// ⚠️ 实测单店 219 条（含 49 个客户）。给 500 留余量，
//
//	而必须有上限：cube 的 supplier 是 duck 快照，全量拉会拖慢接口，
//	而 App 端只需要候选，不需要一次拿全。
const MaxDictPageSize = 500

// defaultDictPageSize 是未指定 limit 时的默认条数。
//
// 取 300：略高于实测的 219 条，意味着"不带搜索词直接打开"
// 一次就能拿到本门店全部字典。
const defaultDictPageSize = 300

// getIgnoreDict GET /insights/supplier-ignore/dict
//
// 按门店列供应商字典，支持按名称/编码模糊搜索。
func (h *Handler) getIgnoreDict(c *gin.Context) {
	if h.dict == nil {
		c.JSON(http.StatusServiceUnavailable, errBody("dict_not_ready",
			"供应商字典服务未就绪"))
		return
	}
	branch := branchOf(c)
	if branch == "" {
		c.JSON(http.StatusBadRequest, errBody("store_required",
			"缺少门店（X-Branch-ID），无法确定要读哪家店的字典"))
		return
	}

	limit := parseQueryInt(c.Query("limit"), defaultDictPageSize)
	if limit <= 0 {
		limit = defaultDictPageSize
	}
	if limit > MaxDictPageSize {
		limit = MaxDictPageSize
	}
	offset := parseQueryInt(c.Query("offset"), 0)
	if offset < 0 {
		offset = 0
	}

	recs, err := h.dict.SupplierDictPage(c.Request.Context(), branch,
		offset, limit, strings.TrimSpace(c.Query("q")))
	if err != nil {
		c.JSON(http.StatusInternalServerError, errBody("dict_failed", err.Error()))
		return
	}
	if recs == nil {
		// ⚠️ 空结果必须是 `[]` 而不是 null：
		//   App 侧 `raw is! List` 会把 null 判成"响应格式异常"而报错，
		//   而"这家店没有供应商"是一个正常结果。
		recs = []supplier.Record{}
	}
	c.JSON(http.StatusOK, gin.H{"records": recs})
}

// parseQueryInt 解析查询参数。
//
// ⚠️ 非法值**回落默认**而不是报错 —— 这是展示类分页参数，
//   App 传了个坏值不该让整个页面报错；
//   真正必填的参数（门店）走的是另一条路径（显式 400）。
func parseQueryInt(raw string, def int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}
