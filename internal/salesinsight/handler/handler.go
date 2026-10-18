// Package handler 是 sales-insight 的 HTTP 层。
//
// # 只做三件事：解析入参、调用 service、映射错误
//
// 它**不做**任何业务判断（该不该排除自用、能不能比环比、prompt 怎么写）——
// 那些都在 build/insight 包里。handler 里的业务逻辑无法被单测覆盖
// （要起 gin），所以它越薄越好。
package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/salesinsight/calendar"
	"github.com/YunBright/supertrade/internal/salesinsight/ignore"
	"github.com/YunBright/supertrade/internal/salesinsight/insight"
)

// Service 是本 handler 的依赖面。
type Service interface {
	// Generate 生成一期报告，返回结论与审计。
	Generate(ctx context.Context, in GenerateInput) (insight.Insight, insight.Audit, error)
}

// GenerateInput 是一次生成的入参。
type GenerateInput struct {
	// StoreBranchID 是门店（X-Branch-ID），必填。
	StoreBranchID string
	// Kind 是周期。
	Kind calendar.PeriodKind
	// TopN 是排行条数。
	TopN int
	// PrevSummary 是**调用方带来**的上一期结论摘要（可空）。
	//
	// ⚠️ 为什么由调用方传而不是本服务自己去读：
	//   摘要存在 wechat-bot 的 state store 里，而本服务是无状态推理服务 ——
	//   让它持有别人的数据，会让"谁拥有这份数据"变得不清楚。
	//   调用方本来就已经有这份数据（它自己存的），顺带传过来还省一次跨服务读。
	//
	//   留空是**正常分支**，不是错误：prompt 会明确写"没有上一期结论记录"，
	//   环比的数字部分由事实包的 Deltas 独立提供，不依赖这份摘要。
	PrevSummary *insight.PrevSummary
}

// Handler 是 HTTP handler。
type Handler struct {
	svc Service
	// ignores 是供应商忽略名单存储（按门店分文件）。
	//
	// ⚠️ 为 nil 时名单端点返回 503，**而不是**返回空名单 ——
	//   "没装配" 与 "这家店没配过" 对运营是完全不同的两件事，
	//   后者要引导他去配，前者是运维要修的故障。
	ignores *ignore.Store
	// dict 列出门店供应商字典（推荐时用）。
	//
	// ⚠️ 与 ignores 分开：ignores 是**配置存储**（本地文件），
	//   dict 是**数据查询**（cube）。两者生命周期与故障模式都不同，
	//   合成一个字段会让"磁盘写坏了"与"cube 查不到"报同一个错。
	dict DictProvider
	// dailyStoreReady 表示"已定稿销售事实库（PG）"是否已接入。
	//
	// ⚠️ 它必须在 health 里显式暴露，因为未接入时的**症状完全正常**：
	//   报告照出、数字大体对，只是同一个营业日两次查可能不同。
	//   没有这个字段，运维只有翻启动日志才能发现，
	//   而"落库改造上线了吗"恰恰是第一个要回答的问题。
	dailyStoreReady bool
}

// New 构造 Handler。svc 为 nil 时所有端点返回 503。
func New(svc Service) *Handler { return &Handler{svc: svc} }

// WithIgnores 注入供应商忽略名单存储（返回自身便于链式）。
func (h *Handler) WithIgnores(s *ignore.Store) *Handler {
	h.ignores = s
	return h
}

// WithDict 注入供应商字典查询（返回自身便于链式）。
func (h *Handler) WithDict(d DictProvider) *Handler {
	h.dict = d
	return h
}

// WithDailyStore 声明"已定稿销售事实库"是否已接入（只影响 health 输出）。
func (h *Handler) WithDailyStore(ready bool) *Handler {
	h.dailyStoreReady = ready
	return h
}

// RegisterRoutes 注册路由。
//
// ⚠️ 路径前缀不含 `/api/v1/<app>`：那一段由 nginx 剥掉后由 sidecar
//
//	转发（见 yun-bright.conf 与 dapr 路由约定）。
//	这里注册的是 sidecar `/v1.0/invoke/<app-id>/method/` 之后的路径。
func (h *Handler) RegisterRoutes(r gin.IRouter) {
	r.POST("/insights/generate", h.generate)
	r.GET("/insights/health", h.health)
	h.registerIgnoreRoutes(r)
}

// generateReq 是 POST /insights/generate 的请求体。
type generateReq struct {
	// Store 是门店（X-Branch-ID）。⚠️ 字段名是 store 不是 branch，
	// 与 salesagg 的口径一致 —— branch 在思迅体系里被占用过
	// （源库 branch_no 是门店区域），改名是为了让边界读得出来。
	//
	// ⚠️ 这里**故意不加** `binding:"required"`：
	//   加上之后，字段缺失会先被 gin's validator 拦下，返回
	//   `bad_request` + "Key: 'generateReq.Store' Error:Field validation ..."——
	//   那是把 Go 结构体字段名漏给运维看，而这条路径最可能的真实原因是
	//   "这个自建应用没绑门店"（调用方由门店反推应用）。缺口的显式检查
	//   在下面，两种情况都返回可操作的 store_required。
	Store string `json:"store"`
	// Kind 是 daily/weekly/monthly，默认 daily。
	Kind string `json:"kind"`
	// TopN 是排行条数，默认 10，上限 20。
	TopN int `json:"top_n"`
	// PrevSummary 是上一期结论摘要（可选）。
	PrevSummary *prevSummaryReq `json:"prev_summary,omitempty"`
	// IncludeAudit 是审计回传粒度："summary"（默认）或 "full"。
	IncludeAudit string `json:"include_audit,omitempty"`
}

// prevSummaryReq 是调用方回传的上期摘要。
//
// ⚠️ 独立于 insight.PrevSummary 定义，字段更少：调用方只需要传
// 结论本身，不该有能力指定 period_kind（那是**本服务**解析出来的，
// 由调用方指定就会出现"上期是月报、本期是日报"的错配）。
type prevSummaryReq struct {
	From       string   `json:"from"`
	To         string   `json:"to"`
	Summary    string   `json:"summary"`
	Highlights []string `json:"highlights,omitempty"`
}

// generate POST /insights/generate
//
// 生成一期报告。它是**同步**的：调用方（wechat-bot 的 dapr job 回调）
// 需要立刻拿到结论去渲染卡片并发送。
func (h *Handler) generate(c *gin.Context) {
	if h.svc == nil {
		c.JSON(http.StatusServiceUnavailable, errBody("service_not_ready",
			"AI 报告服务未就绪（推理引擎未装配）"))
		return
	}

	var req generateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errBody("bad_request", "参数不合法："+err.Error()))
		return
	}
	// ⚠️ 在这里挡一次，而不是等 service 报错：
	//   缺门店的症状是"拿到了全部分店的数据"，那是一份看起来完全正常的
	//   错误报告 —— 比报错危险得多。
	if strings.TrimSpace(req.Store) == "" {
		c.JSON(http.StatusBadRequest, errBody("store_required",
			"缺少门店（store = X-Branch-ID），该应用所属企业没有绑定门店"))
		return
	}

	kind := calendar.PeriodKind(strings.TrimSpace(req.Kind))
	switch kind {
	case "":
		kind = calendar.PeriodDaily
	case calendar.PeriodDaily, calendar.PeriodWeekly, calendar.PeriodMonthly:
	default:
		c.JSON(http.StatusBadRequest, errBody("bad_kind",
			"不支持的周期类型："+req.Kind+"（可选 daily/weekly/monthly）"))
		return
	}

	includeFull, err := parseIncludeAudit(req.IncludeAudit)
	if err != nil {
		c.JSON(http.StatusBadRequest, errBody("bad_include_audit", err.Error()))
		return
	}

	ins, audit, err := h.svc.Generate(c.Request.Context(), GenerateInput{
		StoreBranchID: strings.TrimSpace(req.Store),
		Kind:          kind,
		TopN:          req.TopN,
		PrevSummary:   prevSummaryOf(req.PrevSummary, kind),
	})
	if err != nil {
		// 取数类错误（源库不可达）才是 503；
		// 参数错在入口已经挡掉了。
		c.JSON(http.StatusServiceUnavailable, errBody("generate_failed", err.Error()))
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"insight": ins,
		"audit":   auditViewOf(audit, includeFull),
	})
}

// parseIncludeAudit 解析审计粒度。
//
// ⚠️ 未知值**报错**而不是当默认值：那一定是调用方写错了字段值
// （比如把 "FULL" 传进来），静默降级成 summary 的后果是
// "报告发出去了，但后台查不到推理上下文"，而排查方向会跑偏到
// "是不是没保存"，实际是没请求到。
func parseIncludeAudit(v string) (bool, error) {
	switch strings.TrimSpace(v) {
	case "", "summary":
		return false, nil
	case "full":
		return true, nil
	default:
		return false, fmt.Errorf("不支持的审计粒度：%q（可选 summary / full）", v)
	}
}

// prevSummaryOf 把请求里的上期摘要转成引擎用的形状。
//
// 空摘要（没传、或内容全空）返回 nil —— 让 prompt 走"没有上一期"
// 分支，而不是塞一个空的"上期结论"段。
func prevSummaryOf(p *prevSummaryReq, kind calendar.PeriodKind) *insight.PrevSummary {
	if p == nil {
		return nil
	}
	if strings.TrimSpace(p.Summary) == "" && len(p.Highlights) == 0 {
		return nil
	}
	return &insight.PrevSummary{
		PeriodKind: string(kind),
		From:       strings.TrimSpace(p.From),
		To:         strings.TrimSpace(p.To),
		Summary:    p.Summary,
		Highlights: p.Highlights,
	}
}

// MaxAuditFieldChars 是单个审计大字段（context/prompt/raw_output）的字符上限。
//
// ⚠️ 审计数据要落 state store，无界增长会把它写满 ——
//
//	insight.Audit 的注释里也提了这条铁律。
//
// 取 32K 的依据：事实包本身有 12000 字符的发送前自检（见 PRD 2.3），
// prompt 又内嵌了一次事实包，所以正常情况下三字段合计 < 40K；
// 32K/字段足够装下任何正常报告，又能挡住"某个 TopN 配成 20 +
// 商品名特别长"这类异常放大。超出时截断并置 AuditTruncated ——
// 宁可让后台显示"已截断"，也不要让运维发现"这期报告的审计不见了"。
const MaxAuditFieldChars = 32 * 1024

// auditView 是返回给调用方的审计视图。
//
// # 它同时服务两种粒度，而不是两个类型
//
//	full=false → 只有元信息（几百字节）
//	full=true  → 额外带 Context / Prompt / RawOutput
//
// ⚠️ 做成**同一个形状**而不是 full 时换成 insight.Audit：
//
//	两种形状会让调用方写两套解析分支，而两套分支的测试覆盖
//	一定会不同步 —— 于是"full 模式下少了一个字段"这种 bug
//	能安静地活下来。omitempty 让大字段在 summary 模式下自动消失。
//
// # 为什么 full 是同源同步返回，而不是另开一个端点或走 pub/sub
//
//	(a) 另开端点重跑一次生成 → audit 属于**第二次**推理，
//	    与已发出的报告不是同一份，run_id 对不上；
//	    而 PRD 3.4 要的正是"同一份事实包在不同 prompt 下的输出对比"——
//	    那要求 context / prompt / raw_output 严格来自同一次调用。
//	(b) 走 pub/sub 另推一份 → 至少一次投递、无补偿。
//	    丢一份审计在平时毫无征兆，偏偏在"模型升级后解析失败"
//	    那种最需要它的时刻丢失。
//
// 大小不是问题：dapr invoke 的上限是 MB 级，而这里的量是几十 KB。
type auditView struct {
	RunID string `json:"run_id"`
	// PeriodKind / From / To / BranchID 标明这是哪一期。
	//
	// ⚠️ 这四个字段**不是**可有可无的：调用方要用它们拼存储 key
	//   （aireport.ReportKey(branch, kind, period)）。
	//   缺了它们，调用方只能靠自己再解析一次周期 ——
	//   而两处解析用不同代码路径时，key 会不一致，
	//   症状是"报告存进去了但历史列表里查不到"。
	PeriodKind     string `json:"period_kind"`
	From           string `json:"from"`
	To             string `json:"to"`
	BranchID       string `json:"branch_id"`
	PromptVersion  string `json:"prompt_version,omitempty"`
	Degraded       bool   `json:"degraded"`
	DegradedReason string `json:"degraded_reason,omitempty"`
	LatencyMS      int64  `json:"latency_ms,omitempty"`
	PromptTokens   int    `json:"prompt_tokens,omitempty"`
	// CompletionTokens 与 PromptTokens 一样可能恒为 0（估算）。
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	Error            string `json:"error,omitempty"`
	// TokenEstimated 为 true 时，后台必须标注"token 为估算值"。
	TokenEstimated bool `json:"token_estimated"`

	// ---- 以下三个只在 include_audit=full 时出现 ----

	// Context 是本次推理的完整输入（事实包全文，JSON 字符串）。
	Context string `json:"context,omitempty"`
	// Prompt 是最终发出的 user prompt。
	Prompt string `json:"prompt,omitempty"`
	// RawOutput 是 LLM 原始返回（未经解析）。
	RawOutput string `json:"raw_output,omitempty"`
	// AuditTruncated 表示上面某个字段被截断了。
	AuditTruncated bool `json:"audit_truncated,omitempty"`
}

func auditViewOf(a insight.Audit, full bool) auditView {
	v := auditView{
		RunID:            a.RunID,
		PeriodKind:       a.PeriodKind,
		From:             a.From,
		To:               a.To,
		BranchID:         a.BranchID,
		PromptVersion:    a.PromptVersion,
		Degraded:         a.Degraded,
		DegradedReason:   a.DegradedReason,
		LatencyMS:        a.LatencyMS,
		PromptTokens:     a.PromptTokens,
		CompletionTokens: a.CompletionTokens,
		Error:            a.Error,
		TokenEstimated:   true, // dapr ConverseAlpha1 不返回 Usage，一律估算
	}
	if !full {
		return v
	}
	v.Context, v.AuditTruncated = clipAuditField(v.AuditTruncated, a.Context)
	v.Prompt, v.AuditTruncated = clipAuditField(v.AuditTruncated, a.Prompt)
	v.RawOutput, v.AuditTruncated = clipAuditField(v.AuditTruncated, a.RawOutput)
	return v
}

// clipAuditField 按字符截断审计字段，返回 (截断结果, 是否发生了截断)。
//
// ⚠️ 按**字符**而不是字节截断：中文一个字 3 字节，按字节切会把
//
//	一个汉字劈成两半，存进 state store 后后台显示是乱码，
//	而审计数据的价值恰恰是"能读懂"。
func clipAuditField(alreadyTruncated bool, s string) (string, bool) {
	if len(s) <= MaxAuditFieldChars {
		return s, alreadyTruncated
	}
	r := []rune(s)
	return string(r[:MaxAuditFieldChars]) + "…[已截断]", true
}

// health GET /insights/health
//
// 它报的不只是"进程活着"，还有**这份日报还能不能生成**。
// 之所以要后者：日历表过期、LLM 不可用这些都会让日报静默降级，
// 而进程仍然健康 —— 没有这个字段，运维只能在群里看到"规则模式"四个字。
func (h *Handler) health(c *gin.Context) {
	out := gin.H{
		"status":                 "ok",
		"calendar_data_version":  calendar.DataVersion,
		"calendar_data_stale":    calendarStale(),
		"calendar_data_coverage": calendar.CoverageEnd,
		"prompt_version":         insight.PromptVersion,
		"service_ready":          h.svc != nil,
		// daily_store_ready=false 时销售数字**现查源库**：
		// 报告仍会出，但同一个营业日可能两次查到不同的数。
		"daily_store_ready": h.dailyStoreReady,
		"checked_at":        time.Now().Format(time.RFC3339),
	}
	c.JSON(http.StatusOK, out)
}

// calendarStale 判断内置节假日表是否已过期。
//
// ⚠️ 它的判据是"今天是否还在覆盖范围内"，而不是"今天是哪一年"：
//
//	表覆盖到 CoverageEnd 为止（含调休上班日，它们可能落在假期之后）。
//	过期后日历上下文会退化成"普通工作日"的朴素判定，
//	日报仍然能出，但**缺少最重要的解释维度** ——
//	所以它必须在 healthz 里显式暴露，而不是静默继续。
func calendarStale() bool {
	now := time.Now().Format(calendar.DateLayout)
	return now > calendar.CoverageEnd
}

// errBody 是统一错误信封。
//
// 形状与本仓其它服务一致（{"code":...,"message":...}），
// 前端与契约测试都按这个解析。
func errBody(code, msg string) gin.H {
	return gin.H{"code": code, "message": msg}
}
