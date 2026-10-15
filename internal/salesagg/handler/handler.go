// Package handler 实现销售报表的 HTTP 端点。
//
// 路由（nginx 剥掉 /api/v1/sales-agg/ 前缀后）：
//
//	GET /reports/sales          销售报表（Flutter 仪表盘「销售报表」页）
//	GET /reports/sales/today    今日按类别统计（wechat-bot 群卡片用）
//
// # 为什么两个端点而不是一个带参数
//
// 群卡片每天 10:45 自动跑，它要的**永远**是"今天 + 类别 + 不限行数"，
// 而报表页要的是"任意区间 + 三种维度 + 分页"。
// 把它们合成一个端点，就得让调用方传一堆它永远不会改的参数，
// 还得在服务端校验"这组参数组合是否有意义"——
// 收益只是省一次路由声明，代价是每个调用方都要知道自己该传什么。
//
// 第二个端点顺带承担一个职责：**它是 dapr 服务间调用的稳定契约**。
// wechat-bot 经 service invocation 调它，而 wechat-bot 不该知道 cube
// 的存在、更不该知道 branch 怎么路由到具体实例。
package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/salesagg/service"
)

// Handler 是销售报表 handler。
type Handler struct {
	svc *service.Service
	now func() time.Time
}

// New 构造 Handler。
func New(svc *service.Service) *Handler {
	return &Handler{svc: svc, now: time.Now}
}

// RegisterRoutes 注册路由。
//
// ⚠️ 本服务**没有** /dapr/subscribe 端点，也不需要 RegisterDapr 窗口：
// 它只被调用，不订阅任何 topic。开窗只会无谓扩大"能连到 app 端口就能
// 伪造请求"的暴露面（见 pkg/cmdbootstrap 文档第 2 段）。
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	r.GET("/reports/sales", h.Report)
	r.GET("/reports/sales/today", h.TodayByCategory)
}

// defaultLimit 是报表页未指定 limit 时的默认行数。
//
// 商品维度实测 7 天有 2140 个商品，全量返回会让手机端表格卡住；
// 类别维度只有 64 组，分店维度更少 —— 所以 limit 是**每维度**的合理默认，
// 而不是统一值。调用方可以显式传 limit 覆盖。
const defaultLimit = 200

// Report GET /reports/sales
//
// 查询参数：
//
//	store  门店（X-Branch-ID）；**必填**
//	dimension  category | product（默认 category）
//	preset      today | yesterday | week | month（与 from/to 二选一）
//	from/to     自定义区间（YYYY-MM-DD，闭区间）；给了就优先于 preset
//	filter      类别筛选（仅 dimension=category 有意义）
//	limit       行数上限
//
// # 为什么叫 store 而不是 branch
//
// 本系统里"branch"这个词历史上被两个东西占用了：
//
//   - **X-Branch-ID**：门店。本系统的一级隔离单位（branch_cube_sources
//     按它路由到该门店专属的 cube 实例）。
//   - **思迅源库的 branch_no**：门店区域，是门店的**下一层**。
//
// 两者是无关编码，拿后者去过滤前者会永远匹配不上。所以对外参数一律叫
// `store` / `StoreBranchID`，把门店这个语义钉死。
// 源库的 branch_no **完全不参与过滤**（见 service.Query.StoreBranchID 注释）。
//
// # 为什么 store 必填
//
// ⚠️ 我最初把这行注释写成「空 = 该 cube 实例内全部分店」，那是**错的**：
// 空值会让本服务不带 X-Branch-ID header 发给 cube-router，而
// `internal/cube-router/handler/proxy.go` 里 `branchID == ""` 直接
// 400 `branch_required`。也就是说那条路径从来跑不通，只是我把它
// 文档化成了特性。
//
// 现在改为在本服务就明确拒绝，并且说清下一步该做什么。
func (h *Handler) Report(c *gin.Context) {
	store := strings.TrimSpace(c.Query("store"))
	if store == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code": "store_required",
			"message": "必须指定门店（store = X-Branch-ID）。销售数据按门店隔离，" +
				"不提供「全部门店」这个选项 —— 那样拿到的是路由不明的数据。",
		})
		return
	}

	q := service.Query{
		StoreBranchID:   store,
		GroupBy:         c.DefaultQuery("dimension", service.DimCategory),
		DimensionFilter: c.Query("filter"),
	}

	from, to := c.Query("from"), c.Query("to")
	if from == "" || to == "" {
		preset := service.Preset(c.DefaultQuery("preset", string(service.PresetToday)))
		f, t := service.ResolveRange(preset, h.now())
		// 只在**没给**的一侧用预设值，避免覆盖调用方自定义区间
		if from == "" {
			from = f
		}
		if to == "" {
			to = t
		}
	}
	q.From, q.To = from, to

	q.Limit = defaultLimit
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			q.Limit = n
		}
	}

	rep, err := h.svc.Report(c.Request.Context(), q)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, rep)
}

// TodayByCategory GET /reports/sales/today
//
// 查询参数：
//
//	store  门店（X-Branch-ID）；**必填**，理由同 Report
//	date   YYYY-MM-DD；省略 = 今天（**以服务端时钟为准**，
//	       所以每日定时推送不会因为调用方时区/时钟不准而取错日期）
func (h *Handler) TodayByCategory(c *gin.Context) {
	store := strings.TrimSpace(c.Query("store"))
	if store == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "store_required",
			"message": "必须指定门店（store = X-Branch-ID），理由同 /reports/sales。",
		})
		return
	}
	rep, err := h.svc.TodayByCategory(c.Request.Context(), store, c.Query("date"))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, rep)
}

// fail 把错误映射为 HTTP 状态。
//
// 400 = 调用方给错了（维度不支持、日期格式不对）；
// 503 = 依赖不可用（cube 没配 / 源库不可达）。两者对调用方的处置完全不同：
// 前者改参数重试，后者等上游恢复 —— 混成 500 会让重试逻辑无从判断。
func (h *Handler) fail(c *gin.Context, err error) {
	if errors.Is(err, service.ErrInvalidDimension) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"code":    "invalid_dimension",
			"message": err.Error(),
		})
		return
	}
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
		"code":    "sales_source_unavailable",
		"message": err.Error(),
	})
}
