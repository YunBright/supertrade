package handler

import (
	"context"
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
// 采集是第三种：**内部回调**。它由 wechat-bot 的 dapr job 打过来，
// 不带用户 token，所以"用户能不能采这家店"这个问题在这里不成立 ——
// 边界是 dapr app 级 ACL（见 deployer 的 app-id 配置）与 X-Branch-ID 路由链。
//
// 塞进前两者任何一个都会让它看起来像有用户级鉴权而实际没有，
// 或者反过来 —— 所以单列，让依赖面与安全假设各自可见。
type DailyCollectHandler struct {
	daily DailyCollector
}

// NewDailyCollectHandler 构造。c 为 nil 时只注册路由，调用返回 503。
func NewDailyCollectHandler(c DailyCollector) *DailyCollectHandler {
	return &DailyCollectHandler{daily: c}
}

// RegisterRoutes 注册采集路由。
//
// 路径是 nginx 剥掉 /api/v1/sales-agg/ 前缀后本服务看到的路径。
func (h *DailyCollectHandler) RegisterRoutes(r gin.IRouter) {
	r.GET("/sales/daily/collect", h.Collect)
}

// Collect GET /sales/daily/collect
//
// 采一个营业日的完整销售事实并落 PG（22:45 首采 / 次日 01:00 校正）。
//
// # 为什么是 GET 而不是 POST
//
// 这是一条**幂等**的"把某一天重新采一遍"命令：
// 它唯一的作用是覆盖同一个 (branch, date) 的那一行，
// 对 cube 与源库都没有副作用。
//
// dapr scheduler 的回调重试对 GET 更自然（POST 的重试在网关上
// 常需要显式幂等键），而"幂等"在这里由 (branch_id, oper_date)
// 这个主键保证，不依赖 HTTP 方法。
//
// # 查询参数
//
//	store     门店（X-Branch-ID）；**必填**，理由同 /reports/sales
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
//	corrected → 昨天（次日 01:00 采的是思迅刚补完的昨天）
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
	store := strings.TrimSpace(c.Query("store"))
	if store == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": "store_required",
			"message": "必须指定门店（store = X-Branch-ID）。销售数据按门店隔离，" +
				"不提供「全部门店」这个选项 —— 那样采到的是路由不明的数据。",
		})
		return
	}
	src, err := parseCollectSource(c.Query("source"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "bad_source",
			"message": err.Error(),
		})
		return
	}

	operDate, err := resolveOperDate(c.Query("oper_date"), src)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "bad_oper_date",
			"message": err.Error(),
		})
		return
	}

	res, err := h.daily.Run(c.Request.Context(), store, operDate, src)
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
