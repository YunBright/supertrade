// Package cubesync / handler.go —— 同步触发端点。
//
// # 三个端点
//
//	POST /job/<job 名>    dapr scheduler 的回调(免鉴权窗口)—— **路径由 dapr 定**
//	POST /v1/sync/run     人工触发(免鉴权窗口)
//	GET  /v1/sync/status  上一次同步的结果(免鉴权窗口)
//
// (另有 POST /v1/sync/job 作旧别名,dapr 不打它;见 JobCallbackPath。)
//
// # ⚠️ 为什么都落在**免鉴权窗口**
//
// dapr scheduler 回调**不带任何凭据**(job 注册体只有 schedule/data/address 三项,
// dapr 的 job spec 也没有 headers 字段)。
// 挂在鉴权链内的后果是:每晚 03:30 的同步**每一次都 401**,
// 而症状极其安静 —— 服务照常启动、healthz 200、job 注册成功、
// 没有任何一行说"同步没跑过"。
// (同款故障在 sales-agg 上已经踩过一次,见该仓 registerDaprRoutes 的注释。)
//
// # ⚠️ 免鉴权窗口的代价与收口
//
// 开窗后**同网段任何主机**都能 POST 到这些端点。
// 唯一的收口是**共享密钥 SYNC_JOB_TOKEN**:
//
//   - 密钥由环境变量注入,不写在代码里;
//   - 比较用 crypto/subtle.ConstantTimeCompare(防时序侧信道);
//   - 密钥**不配置就拒绝启动**(Config.Validate),不是"启动后第一次同步才报错";
//   - 密钥**只授予"触发同步"这一项能力**,不是服务账号凭据 ——
//     拿到它的人只能让本机跑一次同步,拿不到 catalog 的写权限。
//
// ⚠️ **它确实会进 dapr scheduler 的存储**(job data 里带一份),
// 因为回调端点没有别的凭据来源 —— 调度器回调不带 header。
// 这与"绝不能把 client_secret 写进 job data"不冲突:
// 那条禁的是**服务账号密钥**(权限面大得多),
// 而这里是**单一用途的一次性触发口令**。
// 残余风险已记录在此,并由 nginx 挡住公网路径(见 deployer 的 nginx 配置)。
//
// # 将来:挪进鉴权链
//
// auth 有了 `cube:sync` 这类权限点之后,`/v1/sync/run` 应挪进鉴权链,
// 由真人/服务账号的 JWT 授权,而不是靠共享密钥。
// 在那之前,宁可"权限弱一点但能用",也不要"权限对但没人调得动"。
package cubesync

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/archivedto"
)

// TokenHeader 是携带共享密钥的头名。
const TokenHeader = "X-Sync-Token"

// JobCallbackPath 返回 dapr scheduler 回调本 app 时的**实际**路径。
//
// # ⚠️ 这个路径**不是我们能挑的** —— 它由 dapr 决定
//
// ✅ MEASURED 2026-10-22(dapr v1.18 源码 + 生产日志双向核对):
//
//	pkg/channel/http/http_channel.go → constructJobRequest:
//	    uri := h.baseAddress + "/job/" + strings.TrimPrefix(name, "/")
//
//	pkg/runtime/scheduler/internal/cluster/streamer.go → invokeApp:
//	    appChannel.TriggerJob(ctx, job.GetName(), job.GetData())
//
// ⇒ 回调恒为 **POST {app}/job/{job 名}**;而且
// **job spec 里的 address 字段被完全忽略**(invokeApp 只传 name + data)。
//
// # 为什么这段注释值这么多钱
//
// 之前这里写死 "/v1/sync/job",于是 dapr 每次回调都打到**没注册**的路径:
//
//	① gin 落进 NoRoute —— 但**全局中间件链照跑**
//	   ⇒ RequireAudience 发现没有 claims ⇒ 401
//	   `{"code":"unauthenticated","message":"缺少已签 token"}`(57 字节)
//	② dapr 把 401 折成 gRPC code 16,日志只有一句
//	   "unexpected status code returned from app ... status code returned: 16"
//
// ⇒ **带上正确的 token 也没用**:RequireAudience 在 NoRoute 之前就中止了。
// (实测 2026-10-22:带对 token 打 /job/<name> 仍然是 57 字节的 401。)
//
// 症状安静得可怕:服务正常、healthz 200、job 注册成功、
// 人工触发(/v1/sync/run)一切正常 —— **只有定时那一路从来没跑过**。
//
// ⇒ 判据:**回调路径必须与 job 名对上**,而且 job 名只有一处真相源
// (Handler.JobName)。两边各写各的字符串,就是下一次"同步没跑"的配方。
func JobCallbackPath(jobName string) string {
	return "/job/" + strings.TrimPrefix(strings.TrimSpace(jobName), "/")
}

// LegacyJobPath 是旧的手工别名。
//
// ⚠️ dapr **不会**打这个路径(它只打 JobCallbackPath 算出来的那条)。
//
// 保留它的理由只有一个:运维脚本 / 习惯用法可能还指着这里。
// 两条路径都由同一个共享密钥收口,多一个别名不扩大攻击面;
// 而"删掉之后老脚本 404"比"多留一个别名"更容易让人踩坑。
const LegacyJobPath = "/v1/sync/job"

// RunPath 是人工触发的路径。
const RunPath = "/v1/sync/run"

// StatusPath 是查询上次同步结果的路径。
const StatusPath = "/v1/sync/status"

// triggerReq 是触发请求体。
//
// ⚠️ job 回调的 body 就是注册时写的 job data,字段名由注册侧定。
// 这里**同时**认 trigger / job 两个名字(与 salesagg 的 collectReq 同款做法),
// 少认一个的症状是"回调 400 必须指定门店,而门店明明就在 body 里"。
//
// ⚠️ **不接受 source/target 门店覆盖**:门店由环境变量决定。
// 门店决定"数据从哪来、写到哪去",让请求体改它等于把跨店写入的口子开在 body 里。
type triggerReq struct {
	Trigger string   `json:"trigger"`
	Job     string   `json:"job"`
	Token   string   `json:"token"`
	Mode    string   `json:"mode"`
	Scopes  []string `json:"scopes"`
	// ForceFull 是"本轮无视水位,强制全量"。
	//
	// ⚠️ **只对人工触发生效** —— 定时回调不接受它:
	//   定时任务的搬什么由环境变量决定,不该被 body 改。
	//
	// # 为什么需要它
	//
	//	增量**导不出基础数据**:它只导"变过的行",
	//	没变过的行永远不会出现在增量结果里。
	//	⇒ 换门店 / 重建目标库之后,必须显式跑一次全量,
	//	  否则目标门店**永远缺着那些"没变过"的行**,而且没有任何报错。
	ForceFull bool `json:"force_full"`
}

// Handler 是同步端点的 handler。
type Handler struct {
	syncer *Syncer
	// jobToken 是共享密钥(明文,与 env 一致)。
	jobToken string
	// jobName 是本实例在 dapr scheduler 里的 job 名。
	//
	// ⚠️ 它决定回调路径(/job/<job 名>)—— 而 dapr **只认路径**,
	// 不认 job spec 里的 address ⇒ 名字与路由必须同源。
	// 取自 syncer 的 cfg(与注册 job 时用的是同一份 Config),
	// 这样"注册名"与"回调路径"在构造上就不可能分叉。
	jobName string
	logger  *slog.Logger

	// mu 保护下面两个字段。
	//
	// ⚠️ 同步是**长耗时**的(27565 行商品实测 ~100 秒),
	// 而 status 端点随时可能被查 ⇒ 必须有锁,否则是 data race。
	mu       sync.RWMutex
	last     *SyncReport
	lastErr  string
	lastRunA time.Time

	// runMu / running 是"同一时刻只跑一轮同步"的闸门。
	//
	// ⚠️ 定时回调与人工触发可能撞在一起 ⇒ 两轮并发导同一批数据,
	//   一边 upsert 一边被另一边覆盖,结果不可预期。
	runMu   sync.Mutex
	running bool
}

// backgroundSyncTimeout 是后台同步的超时上限。
//
// ⚠️ 取值依据:一次全量同步(门店 00 实测 27565 个商品)约 **100 秒**;
//
//	留 30 倍余量 ⇒ 50 分钟。超时后强制结束,避免一个卡死的同步
//	把"运行中"标记永久占住(那样之后所有触发都被忽略)。
const backgroundSyncTimeout = 50 * time.Minute

// NewHandler 构造 Handler。
func NewHandler(syncer *Syncer, jobToken string, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	// ⚠️ job 名从 syncer 的 cfg 取 —— 与注册 job 时用的是同一份 Config。
	//   (调用方必须用**同一份** cfg 去 NewSyncer 与注册 job,
	//   否则名字与回调路径会分叉 ⇒ 定时回调永远 401。)
	jobName := ""
	if syncer != nil {
		jobName = syncer.cfg.JobName()
	}
	return &Handler{
		syncer:   syncer,
		jobToken: strings.TrimSpace(jobToken),
		jobName:  jobName,
		logger:   logger,
	}
}

// JobName 是本实例的 dapr job 名。
//
// ⚠️ **注册 job 时必须用这个值** —— 回调路径由它算出,
//   两边不一致 = dapr 打的路径没人接 = 定时同步静默消失。
func (h *Handler) JobName() string {
	if h == nil {
		return ""
	}
	return h.jobName
}

// JobCallbackPath 是本实例实际注册的 dapr 回调路径。
func (h *Handler) JobCallbackPath() string {
	return JobCallbackPath(h.JobName())
}

// RegisterDaprRoutes 注册免鉴权窗口的端点。
//
// ⚠️ 由 cmdbootstrap 在**鉴权中间件之前**调用,顺序是安全不变量 ——
// 改到中间件之后会让每晚的同步都 401(症状:服务正常、同步从未发生)。
//
// ⚠️ **回调路径必须是 /job/<job 名>**(dapr 只打这一条,见 JobCallbackPath)。
// 注册成别的路径的后果不是"404"那么简单:未匹配的路径会落进
// gin 的 NoRoute,而 NoRoute **照跑全局中间件链** ⇒ 被 RequireAudience
// 挡成 401 ⇒ dapr 折成 gRPC code 16 ⇒ 现场只看到一句
// "status code returned: 16",**连"路径不对"三个字都没有**。
func (h *Handler) RegisterDaprRoutes(r *gin.Engine) {
	// ① dapr scheduler 的回调(**dapr 唯一会打的路径**)。
	r.POST(h.JobCallbackPath(), h.handleJob)
	// ② 旧别名(人工 / 运维脚本);dapr 不打这里。
	r.POST(LegacyJobPath, h.handleJob)
	r.POST(RunPath, h.handleRun)
	r.GET(StatusPath, h.handleStatus)
}

// handleJob 是 dapr scheduler 的回调。
//
// ⚠️ scheduler 一律用 **POST** 回调(实测 2026-10-18),body 就是 job data。
//
// # ⚠️ **必须立刻 ack,不能在里面同步跑**
//
// dapr scheduler 的投递有超时。一次全量同步(门店 00 实测 27565 个商品)
// 要跑**几分钟** ⇒ 回调同步跑完会超过投递超时 ⇒ scheduler 判定失败 ⇒
// **重试** ⇒ 同一批数据被反复导(而且每次都从头再来)。
//
// ⇒ 回调只做"鉴权 + 受理",同步在后台跑;结果进日志与 status 端点。
// 人工触发(/v1/sync/run)仍然**同步**跑并直接返回报告 ——
// 那里调用方要的就是报告。
func (h *Handler) handleJob(c *gin.Context) {
	h.trigger(c, "job")
}

// handleRun 是人工触发。
func (h *Handler) handleRun(c *gin.Context) {
	h.trigger(c, "manual")
}

// trigger 是两个触发端点的共同实现。
func (h *Handler) trigger(c *gin.Context, kind string) {
	if h.syncer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"code":    "not_ready",
			"message": "同步器未装配",
		})
		return
	}

	var body triggerReq
	// ⚠️ body **允许不存在**:GET / query 形态没有 body,
	//   直接 ShouldBindJSON 会 EOF —— 而"body 为空"是合法形态。
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    "bad_body",
				"message": "请求体不是合法 JSON:" + err.Error(),
			})
			return
		}
	}

	if !h.authorized(c, body) {
		// ⚠️ 401 而不是 403:这里缺的是"能证明你是谁"的凭据,
		//   而不是"你不够格"。两者的语义差别在前端引导上不一样。
		c.JSON(http.StatusUnauthorized, gin.H{
			"code":    "sync_token_required",
			"message": "缺少或错误的 " + TokenHeader + "(可用请求头 / ?token= / body 的 token 字段携带)",
		})
		return
	}

	// 覆盖项:只在**人工触发**时允许,定时任务的参数由环境变量决定。
	cfg := h.syncer.cfg
	if kind == "manual" {
		mode := strings.TrimSpace(body.Mode)
		if mode != "" {
			if mode != "incremental" && mode != "overwrite" {
				c.JSON(http.StatusBadRequest, gin.H{
					"code":    "bad_mode",
					"message": "mode 只能是 incremental / overwrite",
				})
				return
			}
			cfg.Mode = mode
		}
		if len(body.Scopes) > 0 {
			scopes, err := validateScopes(body.Scopes)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{
					"code":    "bad_scopes",
					"message": err.Error(),
				})
				return
			}
			cfg.Scopes = scopes
		}
		// ⚠️ 强制全量**只认人工触发**(见 triggerReq.ForceFull 的注释)。
		if body.ForceFull {
			cfg.ForceFull = true
		}
	}

	syncer := h.syncer.WithConfig(cfg)

	// ① 定时回调:**立刻 ack**,同步放后台。
	//
	// ⚠️ 后台同步必须用**自己的 context** ——
	//   c.Request.Context() 在响应写回时就被取消了,
	//   拿它跑后台任务 ⇒ 任务刚起步就被 cancel ⇒ 同步"跑完了但一行没导"。
	//   (症状极安静:回调返 200、日志说"同步完成"、表里什么都没变。)
	if kind == "job" {
		if !h.beginRun() {
			c.JSON(http.StatusOK, gin.H{
				"code":    "sync_busy",
				"message": "已有一轮同步在跑,本次触发被忽略(避免同一批数据被并发导两遍)",
			})
			return
		}
		go h.runBackground(syncer, cfg, "job")
		c.JSON(http.StatusOK, gin.H{
			"code":    "accepted",
			"message": "已受理,同步在后台执行;结果见 GET " + StatusPath,
		})
		return
	}

	// ② 人工触发:同步跑并直接返回报告(调用方要的就是报告)。
	//
	// ⚠️ **人工触发也必须抢"运行中"标记**(定时回调已经抢了)。
	//
	//	**为什么有了增量水位之后它从"重复劳动"升级成"永久漏数据"**:
	//
	//	两轮并发跑,一轮读到的水位 W_old 比另一轮**新**(因为后者先成功了)。
	//	⇒ 读着旧水位的那轮导的是**更宽**的窗口(W_old, …],
	//	  而它**失败**了 ⇒ 那段窗口没被导入。
	//	⇒ 另一轮成功 ⇒ 水位推进到 W_new > W_old
	//	⇒ **下一轮只会导 (W_new, …]** ⇒ (W_old, W_new] 这段**永远补不回来**。
	//
	//	⇒ 所以人工触发与定时回调**不得并发**。撞上了就回 sync_busy。
	if !h.beginRun() {
		c.JSON(http.StatusConflict, gin.H{
			"code":    "sync_busy",
			"message": "已有一轮同步在跑,本次触发被忽略(并发两轮会把增量水位推进过头 ⇒ 漏数据)",
		})
		return
	}
	defer h.endRun()

	rep, err := syncer.Sync(c.Request.Context())
	h.remember(rep, err)
	if err != nil {
		h.logger.Error("cubesync: 同步失败", "kind", kind, "err", err,
			"source_branch", cfg.SourceBranchID, "target_branch", cfg.TargetBranchID)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"code":    "sync_failed",
			"message": err.Error(),
			"report":  rep,
		})
		return
	}
	inserted, updated, skipped := rep.Total()
	h.logger.Info("cubesync: 同步完成",
		"kind", kind,
		"source_branch", cfg.SourceBranchID,
		"target_branch", cfg.TargetBranchID,
		"inserted", inserted, "updated", updated, "skipped", skipped,
		"duration_ms", rep.DurationMS)
	c.JSON(http.StatusOK, gin.H{
		"report": rep,
	})
}

// beginRun 抢占"运行中"标记。
//
// ⚠️ 为什么需要:定时回调 + 人工触发可能撞在一起。
//
//	两轮并发导同一批数据 ⇒ 一边 upsert 一边被另一边覆盖,结果不可预期。
//	⇒ 已有一次在跑时,后来的直接被忽略(而不是排队 —— 排队会让
//	scheduler 的投递超时,反而更糟)。
func (h *Handler) beginRun() bool {
	h.runMu.Lock()
	defer h.runMu.Unlock()
	if h.running {
		return false
	}
	h.running = true
	return true
}

// endRun 释放"运行中"标记。
func (h *Handler) endRun() {
	h.runMu.Lock()
	defer h.runMu.Unlock()
	h.running = false
}

// runBackground 在后台跑一轮同步。
//
// ⚠️ 用 context.Background() 而不是请求的 ctx(见调用处的说明)。
func (h *Handler) runBackground(syncer *Syncer, cfg Config, kind string) {
	defer h.endRun()
	ctx, cancel := context.WithTimeout(context.Background(), backgroundSyncTimeout)
	defer cancel()

	rep, err := syncer.Sync(ctx)
	h.remember(rep, err)
	if err != nil {
		h.logger.Error("cubesync: 后台同步失败", "kind", kind, "err", err,
			"source_branch", cfg.SourceBranchID, "target_branch", cfg.TargetBranchID)
		return
	}
	inserted, updated, skipped := rep.Total()
	h.logger.Info("cubesync: 后台同步完成",
		"kind", kind,
		"source_branch", cfg.SourceBranchID,
		"target_branch", cfg.TargetBranchID,
		"inserted", inserted, "updated", updated, "skipped", skipped,
		"duration_ms", rep.DurationMS)
}

// handleStatus 返回上一次同步的结果。
func (h *Handler) handleStatus(c *gin.Context) {
	if h.syncer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"code":    "not_ready",
			"message": "同步器未装配",
		})
		return
	}
	if !h.authorized(c, triggerReq{}) {
		c.JSON(http.StatusUnauthorized, gin.H{
			"code":    "sync_token_required",
			"message": "缺少或错误的 " + TokenHeader,
		})
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	body := gin.H{
		"source_branch_id": h.syncer.cfg.SourceBranchID,
		"target_branch_id": h.syncer.cfg.TargetBranchID,
		"mode":             h.syncer.cfg.Mode,
		"scopes":           h.syncer.cfg.Scopes,
		"last_run_at":      h.lastRunA,
	}
	if h.last != nil {
		body["report"] = h.last
	}
	if h.lastErr != "" {
		body["last_error"] = h.lastErr
	}
	c.JSON(http.StatusOK, body)
}

// authorized 校验共享密钥。
//
// 凭据来源(header > body > query),任一匹配即放行。
func (h *Handler) authorized(c *gin.Context, body triggerReq) bool {
	if h.jobToken == "" {
		// 没配密钥 ⇒ 一律拒绝(fail-closed)。
		// 放行等于"这个端点对整个内网开放"。
		return false
	}
	given := strings.TrimSpace(c.GetHeader(TokenHeader))
	if given == "" {
		given = strings.TrimSpace(body.Token)
	}
	if given == "" {
		given = strings.TrimSpace(c.Query("token"))
	}
	if given == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(given), []byte(h.jobToken)) == 1
}

// remember 记下最近一次结果(status 端点要读)。
func (h *Handler) remember(rep *SyncReport, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last = rep
	h.lastRunA = time.Now()
	if err != nil {
		h.lastErr = err.Error()
	} else {
		h.lastErr = ""
	}
}

// validateScopes 校验 scope 名。
//
// ⚠️ 未知 scope **报错**而不是忽略:
// 忽略的症状是"这个 scope 什么都没导",与"这个 scope 本来就没数据"完全一样。
func validateScopes(in []string) ([]string, error) {
	known := map[string]bool{}
	for _, s := range archivedto.AllScopes {
		known[s] = true
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !known[s] {
			return nil, errors.New("不认识的资料域:" + s + "(可选:" + strings.Join(archivedto.AllScopes, ",") + ")")
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, errors.New("scopes 不能为空")
	}
	return out, nil
}
