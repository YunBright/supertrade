package handler

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/salesinsight/fact/supplier"
	"github.com/YunBright/supertrade/internal/salesinsight/ignore"
)

// 供应商忽略名单的读写接口。
//
// # 为什么放在本服务而不是 catalog
//
// 名单只被**本服务**消费（生成报告时按它排除自用供应商），
// 而 catalog 是"主数据"服务。让 catalog 为一个消费方专有的配置开接口，
// 会让它背上别的服务的业务语义。
//
// # 门店从哪来
//
// ⚠️ 一律从 **X-Branch-ID** 读，**不**从请求体读 store。
//
//	若允许请求体指定门店，A 店的登录态就能改 B 店的名单 ——
//	而"谁能改哪家店的配置"必须由中间件的鉴权决定，不能由参数决定。
//	这也是与 /insights/generate 不同的：那个端点的 store 是**必填**的，
//	因为它决定"读谁的数据"；这里门店来自可信头。
//
// # 路径
//
//	GET    /insights/supplier-ignore              读名单
//	PUT    /insights/supplier-ignore              全量覆盖保存
//	POST   /insights/supplier-ignore/suggest      按字典给推荐（维护工具）

// MaxIgnoreEntries 是单店名单条数上限。
//
// ⚠️ 必须有上限：名单是全量覆盖写入的，一个误操作（把全部供应商提交进来）
//
//	会让"供应商榜单"整个空掉，而那看起来像"这家店没有供应商"。
//	实测单店自用单元是 8~12 条，200 已有两个数量级余量。
const MaxIgnoreEntries = 200

// registerIgnoreRoutes 注册名单相关路由。
//
// store 为 nil 时这些端点返回 503 —— 与 generate 的处理一致，
// 避免"名单服务没装配"被误读成"名单为空"。
func (h *Handler) registerIgnoreRoutes(r gin.IRouter) {
	r.GET("/insights/supplier-ignore", h.getIgnoreList)
	r.PUT("/insights/supplier-ignore", h.putIgnoreList)
	r.GET("/insights/supplier-ignore/dict", h.getIgnoreDict)
	r.POST("/insights/supplier-ignore/suggest", h.postIgnoreSuggest)
}

// branchOf 从请求头取门店。
func branchOf(c *gin.Context) string {
	return strings.TrimSpace(c.GetHeader("X-Branch-ID"))
}

// getIgnoreList GET /insights/supplier-ignore
func (h *Handler) getIgnoreList(c *gin.Context) {
	if h.ignores == nil {
		c.JSON(http.StatusServiceUnavailable, errBody("ignore_store_not_ready",
			"供应商忽略名单服务未就绪"))
		return
	}
	branch := branchOf(c)
	if branch == "" {
		c.JSON(http.StatusBadRequest, errBody("store_required",
			"缺少门店（X-Branch-ID），无法确定要读哪家店的名单"))
		return
	}
	l, err := h.ignores.Load(branch)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errBody("ignore_load_failed", err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"list": l})
}

// putIgnoreReq 是 PUT 的请求体。
type putIgnoreReq struct {
	// Entries 是全量名单（**不是**增量补丁）。
	//
	// ⚠️ 全量而非增删：增量语义要求客户端持有完整现状，
	//   而两份 App（iOS/Android）并发编辑同一份名单时，
	//   "基于旧版本的增量"会静默丢条目。全量覆盖的冲突是显式的。
	Entries []ignore.Entry `json:"entries"`
}

// putIgnoreList PUT /insights/supplier-ignore
//
// ⚠️ PUT 是**全量覆盖**(destructive):请求里的 entries 取代现有名单,
// 未列出的条目会被清掉。
func (h *Handler) putIgnoreList(c *gin.Context) {
	if h.ignores == nil {
		c.JSON(http.StatusServiceUnavailable, errBody("ignore_store_not_ready",
			"供应商忽略名单服务未就绪"))
		return
	}
	branch := branchOf(c)
	if branch == "" {
		c.JSON(http.StatusBadRequest, errBody("store_required",
			"缺少门店（X-Branch-ID），无法确定要写哪家店的名单"))
		return
	}

	var req putIgnoreReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errBody("bad_request", "参数不合法："+err.Error()))
		return
	}
	// ⚠️ 超限**报错**而不是静默截断：
	//   截断会让运营以为名单保存成功了，而末尾几条静默消失。
	if len(req.Entries) > MaxIgnoreEntries {
		c.JSON(http.StatusBadRequest, errBody("too_many_entries",
			"名单条数超出上限（最多 "+strconv.Itoa(MaxIgnoreEntries)+" 条）"))
		return
	}
	l, err := h.ignores.Save(branch, req.Entries)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errBody("ignore_save_failed", err.Error()))
		return
	}
	c.JSON(http.StatusOK, gin.H{"list": l})
}

// suggestReq 是 POST /insights/supplier-ignore/suggest 的请求体。
type suggestReq struct {
	// Query 是可选的搜索词（限定在哪些候选里找）。
	Query string `json:"query"`
	// Records 是**可选**的候选记录。
	//
	// ⚠️ 留空（正常用法）时由服务端按门店去查字典 ——
	//   见 postIgnoreSuggest 的说明。
	//
	//   它存在的唯一理由是**测试**：单测不需要起 cube 就能驱动判定。
	//   保留它不会削弱判别的可靠性，因为两条路径最后都汇入同一个 ignore.Suggest。
	Records []supplier.Record `json:"records"`
}

// postIgnoreSuggest POST /insights/supplier-ignore/suggest
//
// ⚠️ 这是**维护工具**，不是判定链路的一部分：
// **只读**——只返回候选建议，不改名单、不写任何数据。
//
//	它只是把候选缩到十几条，让人复核勾选。
//	报告生成时不会调用它 —— 那是这次改造要省掉的"每次都做的智能活"。
//
// # 为什么由服务端查字典，而不是 App 传上来
//
//	判定依赖 `type` 与 `cust_flag`，它们只存在于 cube 的 supplier 字典里。
//	若 App 传的记录缺这两字段（catalog 的 `/suppliers` 就不返回它们），
//	判定会**退化成只看名称** —— 而名称层置信度最低、最容易误判。
//
//	更糟的是：App 传的字典可能与生成报告时用的那份不同，
//	于是用户会看到"我按推荐配好了，报告里还是自用"—— 无法自洽的现象。
func (h *Handler) postIgnoreSuggest(c *gin.Context) {
	var req suggestReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errBody("bad_request", "参数不合法："+err.Error()))
		return
	}

	recs := req.Records
	if len(recs) == 0 {
		if h.dict == nil {
			c.JSON(http.StatusServiceUnavailable, errBody("dict_not_ready",
				"供应商字典服务未就绪，无法给出推荐"))
			return
		}
		branch := branchOf(c)
		if branch == "" {
			c.JSON(http.StatusBadRequest, errBody("store_required",
				"缺少门店（X-Branch-ID），无法确定要查哪家店的供应商字典"))
			return
		}
		page, err := h.dict.SupplierDictPage(c.Request.Context(), branch,
			0, defaultDictPageSize, req.Query)
		if err != nil {
			c.JSON(http.StatusInternalServerError, errBody("dict_failed", err.Error()))
			return
		}
		recs = page
	}

	c.JSON(http.StatusOK, gin.H{"suggestions": ignore.Suggest(recs)})
}
