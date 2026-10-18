package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/salesagg/collect"
	"github.com/YunBright/supertrade/internal/salesdaily"
)

// DailyCollector 是采集端点依赖的采集器（由 *collect.Collector 实现）。
type DailyCollector interface {
	Run(ctx context.Context, storeBranchID, operDate string, source salesdaily.CollectSource) (collect.Result, error)
}

// DailyCollectHandler 是「每日销售事实落库」的 handler。
//
// # 为什么独立于 Handler / CompareHandler
//
// 本包已经有两套鉴权模型：
//
//	Handler         —— 带 store 的**单店即席查询**（X-Branch-ID → 透传给 cube）
//	CompareHandler  —— **忽略** X-Branch-ID，按 token claims 逐店鉴权
//
// 采集是第三种：**内部回调**。它由 dapr job 打过来，不带用户 token，
// 所以"用户能不能采这家店"这个问题在这里不成立 ——
// 边界是 dapr app 级 ACL（见 deployer 的 app-id 配置）与 X-Branch-ID 路由链。
//
// 塞进前两者任何一个都会让它看起来像有用户级鉴权而实际没有，
// 或者反过来 —— 所以单列，让依赖面与安全假设各自可见。
type DailyCollectHandler struct {
	daily DailyCollector
	// bearer 为该门店换机器 token（client credentials）。
	//
	// ⚠️ 为什么必须换而不是"没有 token 就算了"：
	// dapr 的 job 回调不带 Authorization 头（实测 job spec 无 headers 字段），
	// 而 cube-gateway 侧有 auth-validator（aud=userd）→ 无 token 必 401。
	// 这正是本轮改造前的真实故障：
	//	collect_failed: cube 拒绝调用者凭证: Unauthenticated: Unauthorized
	//
	// nil 时仍然可以构造（本包旧测试就这么用），但 Collect 会返回 503，
	// 因为"没有 bearer 解析器"在生产里意味着装配漏了。
	bearer            collect.BearerResolver
}

// NewDailyCollectHandler 构造。c 为 nil 时只注册路由，调用返回 503。
//
// bearer 可传 nil（单元测试场景）；生产必须传，
// 否则采集会以"cube 401"的形式失败，而真因是本服务没装配机器身份。
func NewDailyCollectHandler(c DailyCollector, bearer collect.BearerResolver) *DailyCollectHandler {
	return &DailyCollectHandler{daily: c, bearer: bearer}
}

// RegisterRoutes 注册采集路由。
//
// 路径是 nginx 剥掉 /api/v1/sales-agg/ 前缀后本服务看到的路径。
//
// # ⚠️ **同时注册 POST 与 GET**，两个都要，缺一不可
//
// ✅ MEASURED 2026-10-18（dapr scheduler 回调，baiyuan 生产日志）：
//
//	msg=http method=POST path=/job/wecom-membership-sync status=200
//
// dapr scheduler 一律用 **POST** 回调，body 就是注册时写的 job data。
// 所以只注册 GET 的话，定时任务每次都落到 405/404 ——
// 而症状是**采集永远没落过库**：服务照常启动、healthz 200、
// 报表照出（回退 cube），没有任何一行报错说"采集没跑"。
//
// GET 保留下来是给**人工重跑**用的（排查、补历史某天）：
// `GET /sales/daily/collect?store=00&oper_date=2026-10-16&source=corrected`。
// 它天然只对本机/内网可达者开放，公网侧由 nginx 挡住（见 main.go 注释）。
func (h *DailyCollectHandler) RegisterRoutes(r gin.IRouter) {
	r.POST("/sales/daily/collect", h.Collect)
	r.GET("/sales/daily/collect", h.Collect)
}

// collectReq 是采集参数。GET 从 query 读，POST 从 job data 的 JSON body 读。
//
// ⚠️ 两边**字段名不一样**，这是 job 注册侧定的，不是本 handler 能选的：
//
//	GET  ?store=00&source=initial
//	POST {"trigger":"dapr-job","job":"sales-daily-collect-00-initial",
//	      "branch":"00","source":"initial"}
//
// 所以 store 必须**两个名字都认**（store / branch）。
// 只认 store 的话，scheduler 回调会得到"必须指定门店"，
// 而门店明明就在 body 里 —— 排查时会以为是门店清单为空。
type collectReq struct {
	Store    string `json:"store"`
	Branch   string `json:"branch"`
	OperDate string `json:"oper_date"`
	Source   string `json:"source"`
}

// resolveStore 合并 query 与 body 里的门店。
//
// 优先级：query 的 store > query 的 branch > body 的 store > body 的 branch。
//
// ⚠️ GET 与 POST 都要查一遍 query：scheduler 回调用 POST，但**参数可能在
// query 上**（人工用 POST + query 触发）。只读 body 会让那种调用丢掉
// oper_date，于是"重跑指定某天"悄悄变成"重跑今天"。
func resolveStore(c *gin.Context, body collectReq) string {
	if v := strings.TrimSpace(c.Query("store")); v != "" {
		return v
	}
	if v := strings.TrimSpace(c.Query("branch")); v != "" {
		return v
	}
	if v := strings.TrimSpace(body.Store); v != "" {
		return v
	}
	return strings.TrimSpace(body.Branch)
}

// param 取一个参数：query 优先，其次 JSON body 里已解析出来的值。
func param(c *gin.Context, body collectReq, queryKey, bodyVal string) string {
	if v := strings.TrimSpace(c.Query(queryKey)); v != "" {
		return v
	}
	return strings.TrimSpace(bodyVal)
}

// Collect 采一个营业日的完整销售事实并落 PG（22:45 首采 / 次日 01:05 校正）。
//
// # 为什么 POST 与 GET 都在
//
// **POST 是 dapr scheduler 的回调形态**（生产实测 method=POST），
// **GET 是给人工重跑**的（重采某一天、补历史）。见 RegisterRoutes 的说明。
//
// 两者语义完全相同：都是"把某一天重新采一遍"，**幂等**，
// 对 cube 与源库都没有副作用（只覆盖同一个 (branch_id, oper_date) 的那一行）。
//
// # 查询参数 / 请求体
//
//	store     门店；**必填**（GET 用 ?store=，POST 用 body 的 store 或 branch）
//	oper_date 营业日（YYYY-MM-DD）；可省，省略时由 source 推导
//	source    initial（默认）| corrected
//
// # 为什么 oper_date 可以省略
//
// dapr scheduler 的 job 数据是**注册时写死**的，
// 所以不可能把"今天"这种每天变化的值塞进去。
// 真正随时间变化的是"这次采集属于哪一天"，而它由 source 唯一决定：
//
//	initial   → 今天（22:45 收档时采当天）
//	corrected → 昨天（次日 01:05 采的是思迅刚补完的昨天）
//
// ⚠️ 与 collect.NormalizeOperDate 的"必填"**不矛盾**：
//
//	那里的严格性针对采集器**被直接调用**时 —— 那时无从推断要采哪一天。
//	这里是 HTTP 端点，而 source 已经把那一天说清楚了；
//	此时再要求调用方重复传一遍，只会让 job 配置与实际行为之间
//	多出一个"传错了但没人发现"的机会。
func (h *DailyCollectHandler) Collect(c *gin.Context) {
	if h.daily == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"code":    "not_ready",
			"message": "采集器未装配（本服务没有装载每日销售事实包的持久化）",
		})
		return
	}

	// ⚠️ body **允许不存在**：GET 没有 body，直接 ShouldBindJSON 会 EOF。
	//   解析失败只在 body 非空且非 JSON 时才算错 —— 静默忽略一个坏 body
	//   会让"参数写错了"变成"参数没传"，两者排查方向完全不同。
	var body collectReq
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    "bad_body",
				"message": "请求体不是合法 JSON：" + err.Error(),
			})
			return
		}
	}

	store := resolveStore(c, body)
	if store == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": "store_required",
			"message": "必须指定门店（GET 用 ?store=，scheduler 回调在 body 的 store/branch）。" +
				"销售数据按门店隔离，不提供「全部门店」这个选项 —— 那样采到的是路由不明的数据。",
		})
		return
	}
	src, err := parseCollectSource(param(c, body, "source", body.Source))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "bad_source",
			"message": err.Error(),
		})
		return
	}

	operDate, err := resolveOperDate(param(c, body, "oper_date", body.OperDate), src)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "bad_oper_date",
			"message": err.Error(),
		})
		return
	}

	// 为该门店换一枚机器 token 并放进 ctx。
	//
	// ⚠️ 必须在 Run **之前**换：Run 内部才会去查 cube，
	//   而没 token 的 cube 调用会 401，那条报错离真因（没开采集 /
	//   凭据坏了 / 服务账号被禁用）十万八千里。
	ctx, err := collect.WithBranchBearer(c.Request.Context(), h.bearer, store)
	if err != nil {
		c.JSON(collectStateStatus(err), gin.H{
			"code":    collectStateCode(err),
			"message": err.Error(),
			"branch":  store,
		})
		return
	}

	res, err := h.daily.Run(ctx, store, operDate, src)
	if err != nil {
		// 采集失败一律 503：它一定是对端不可达、查询超时或日期格式错，
		// 而不是参数错（参数错已在入口挡掉）。
		//
		// ⚠️ 错误文案里不得出现 DSN / 连接串 / 口令。
		//   这里只回传 err.Error()，所以**采集器内部的错误包装
		//   也不能带上连接串**（见 salesdaily.OpenDB 与 collect 的错误路径）。
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"code":    "collect_failed",
			"message": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"store":      res.BranchID,
		"oper_date":  res.OperDate,
		"source":     string(res.Source),
		"saved":      res.Saved,
		"categories": res.Categories,
		"suppliers":  res.Suppliers,
		"products":   res.Products,
	})
}

// collectStateStatus 把"拿不到机器身份"这类**状态错误**映射成状态码。
//
// # ⚠️ 为什么不能一律 503
//
// 503 的语义是"服务不可用，请稍后重试"。而"这家店没开启采集"重试一万次
// 也是同样结果 —— 用户会一直点重试，而真因是"你还没开这个功能"。
//
// 409 + 独立 code 让前端能**分流**：显示"去开启采集"的引导，
// 而不是弹一个"重试"按钮把人引向错误的动作。
//
// ⚠️ 而"凭据解不开/换不到 token"仍然回 503：
// 那是真的服务侧故障，重试是对的。
func collectStateStatus(err error) int {
	switch {
	case errors.Is(err, collect.ErrCollectNotEnabled):
		return http.StatusConflict
	case errors.Is(err, collect.ErrCollectDisabled):
		return http.StatusConflict
	default:
		return http.StatusServiceUnavailable
	}
}

// collectStateCode 给状态错误一个稳定的 code 串。
//
// ⚠️ 前端按 code 分流，文案按 message 走 —— 所以 code 必须区分
// "没开过"与"被关停"，而文案可以复用（两者引导都是"去开启采集"）。
func collectStateCode(err error) string {
	switch {
	case errors.Is(err, collect.ErrCollectNotEnabled):
		return "collect_not_enabled"
	case errors.Is(err, collect.ErrCollectDisabled):
		return "collect_disabled"
	default:
		return "collect_identity_unavailable"
	}
}

// resolveOperDate 决定这一次采集属于哪一天。
//
// ⚠️ 显式传了 oper_date 时**以它为准**，即使与 source 推出的那天不同。
//
//	因为人工重跑（"把 10-03 重采一遍"）就是靠它 ——
//	而那种场景下"今天是 10-18"毫无参考价值。
//	两侧不一致本身不是错误，只是要让人说得清自己传了什么。
func resolveOperDate(v string, src salesdaily.CollectSource) (string, error) {
	if s := strings.TrimSpace(v); s != "" {
		return collect.NormalizeOperDate(s)
	}
	now := time.Now()
	if src == salesdaily.CollectSourceCorrected {
		// ⚠️ 校正采集修的是**昨天**：思迅日结补的是昨天的账。
		//	按"今天"校正会覆盖一个刚开门、几乎没销售的今天，
		//	而昨天那份残缺的首采**永远留在库里** ——
		//	症状是月报某天明显偏低，且没有任何标记。
		return collect.PrevOperDate(now), nil
	}
	return now.Format(collect.DateLayout), nil
}

// parseCollectSource 解析采集来源。
//
// ⚠️ 未知值**报错**而不是当默认值：那一定是调用方写错了
//
//	（比如把 "init" 传进来），而静默当成 initial 的后果是
//	**一次校正采集被记成首采** —— 库里再没有痕迹能说清
//	"这个数字到底被校正过没有"。
//	而"没校正过"与"校正过"在数值上长得一模一样。
func parseCollectSource(v string) (salesdaily.CollectSource, error) {
	switch s := strings.TrimSpace(v); s {
	case "":
		// 默认首采：人工重跑某一天时用得上（那时不关心来源标签）。
		return salesdaily.CollectSourceInitial, nil
	case string(salesdaily.CollectSourceInitial):
		return salesdaily.CollectSourceInitial, nil
	case string(salesdaily.CollectSourceCorrected):
		return salesdaily.CollectSourceCorrected, nil
	default:
		return "", fmt.Errorf("不支持的采集来源：%q（可选 initial / corrected）", s)
	}
}
