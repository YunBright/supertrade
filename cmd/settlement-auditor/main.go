// Package main 是 settlement-auditor dapr app 入口(结算付款审批机器一审,PRD §FR-2)。
//
// 启动:`dapr run --app-id settlement-auditor --app-port 8109 -- go run ./cmd/settlement-auditor`
//
// 业务域:H5 提交付款审批 → 机器审核流水线(cube 核对 + 信号分级 + LLM 研判)→
// 通过则发 wecom.approval.command 命令由 wechat-bot 代提交企微审批(人工终审),
// 拒绝则通知申请人;建议盘点时调 stocktake 建计划单 + taskcard 待办。
//
// 数据存储:**只支持 PostgreSQL**。POSTGRES_DSN 必填,缺失直接启动失败。
//
// env(除 POSTGRES_DSN 外均有默认/可空):
//
//	SETTLEMENT_AUDITOR_CORP_ID                wcorp 友好 id(wecom 命令路由键)
//	SETTLEMENT_AUDITOR_WECOM_TEMPLATE_ID      企微审批模板 id
//	SETTLEMENT_AUDITOR_AMOUNT_TOLERANCE       数值容差(默认 0.01 元;单头 vs 明细勾稽)
//	SETTLEMENT_AUDITOR_PAYMENT_TOLERANCE      付款取整容差(默认 1 元;实际付款按元取整)
//	SETTLEMENT_AUDITOR_SALE_WINDOW_DAYS       销售回看窗口(默认 30)
//	SETTLEMENT_AUDITOR_QTY_FACTOR             数量偏离倍数(默认 3)
//	SETTLEMENT_AUDITOR_STOCKTAKE_APP_ID       stocktake dapr app-id(默认 stocktake)
//	SETTLEMENT_AUDITOR_STOCKTAKE_NOTIFY_USERS 盘点待办接收人(逗号分隔企微 userid)
//	SETTLEMENT_AUDITOR_WXH5_BASE_URL          wx-h5 基础地址(taskcard url)
//	SETTLEMENT_AUDITOR_APPLY_CONTROL_IDS      模板三个 Text 控件 id(逗号分隔,默认 Text-1,Text-2,Text-3)
//	SETTLEMENT_AUDITOR_AI_CONCLUSION_CONTROL_ID 模板「AI审核结论」多行文本控件 id(可选;非空时 apply_data 写入 LLM 结论)
//	SETTLEMENT_AUDITOR_PAYMENT_INFO_CONTROL_ID 模板「付款信息」多行文本控件 id(可选;2026-10-28 新增)
//	SETTLEMENT_AUDITOR_PAYMENT_INFO_CONTROL_TYPE 该控件类型(默认 Textarea)
//	SETTLEMENT_AUDITOR_BANK_ACCOUNT_APP_ID      bank-account 服务的 dapr app-id(默认 supertrade-bank-account)
//	SETTLEMENT_AUDITOR_CONVERSATION           dapr conversation component(默认 conversation)
//	SETTLEMENT_AUDITOR_LLM_TIMEOUT            LLM 超时(默认 30s)
package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/YunBright/authkit/daprclient"
	"github.com/YunBright/authkit/rbac"
	"github.com/YunBright/authkit/userinfo"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubesource"
	settlementauditor "github.com/YunBright/supertrade/internal/settlement-auditor"
	"github.com/YunBright/supertrade/internal/settlement-auditor/handler"
	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
	"github.com/YunBright/supertrade/internal/settlement-auditor/service"
	"github.com/YunBright/supertrade/pkg/cmdbootstrap"
	"github.com/YunBright/supertrade/pkg/middleware"
)

func main() {
	cmdbootstrap.Run(cmdbootstrap.Options{
		AppID: "settlement-auditor",
		Port:  cmdbootstrap.AppPort(":8109"),
		OnStart: func() error {
			return initApp()
		},
		RegisterDapr: registerDaprRoutes,
		Register:     registerRoutes,
	})
}

var (
	appDB      *gorm.DB
	appSvc     *service.Service
	appArchive *service.ArchiveService
)

func initApp() error {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("settlement-auditor: POSTGRES_DSN 必填(只支持 PostgreSQL,不提供 SQLite fallback)")
	}

	var err error
	appDB, err = settlementauditor.OpenPostgres(dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	if err := appDB.AutoMigrate(
		&model.ApprovalInstance{},
		&model.ApprovalSettlement{},
		// 发起人上传的附件(图片/文件)——机器审核是异步的,字节要活到 pass 那一刻。
		&model.ApprovalAttachment{},
		// 手工收付款明细(陈列费/水电费/…)—— 收款项冲减付款金额。
		&model.ApprovalItem{},
		&model.AuditSignal{},
		&model.AuditLog{},
		&model.LLMVerdict{},
		&model.ApprovalStocktakeLink{},
		&model.ApprovalApproverPreference{},
		&model.SettlementPayState{},
		&model.StocktakeNotifyGroup{},
		// 收款账号快照(2026-10-28 用户口径:选中收款账号后按行写进企微「付款信息」控件)。
		//
		// ⚠️ **独立表**,刻意不放进 ApprovalInstance ——
		// AuditInput.Approval 是 ApprovalInstance 整体,账号信息混进去
		// 就等于进了 LLM 输入面(见 internal/settlement-auditor/service/payee_account.go)。
		&model.ApprovalPayeeAccount{},
		// 机器审核「流程定制」配置(2026-10-28 用户口径):
		// 供应商流程 / 付款项流程 / 正则表达式维护。
		//
		// ⚠️ 这三张表的内容**不进 AuditInput** ⇒ 不进 LLM 输入面。
		&model.AuditSupplierFlow{},
		&model.AuditPaymentItemFlow{},
		&model.AuditRegexPattern{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}

	cfg := service.Config{
		CorpID:               os.Getenv("SETTLEMENT_AUDITOR_CORP_ID"),
		TemplateID:           os.Getenv("SETTLEMENT_AUDITOR_WECOM_TEMPLATE_ID"),
		AmountToleranceYuan:  envFloat("SETTLEMENT_AUDITOR_AMOUNT_TOLERANCE", 0.01),
		PaymentToleranceYuan: envFloat("SETTLEMENT_AUDITOR_PAYMENT_TOLERANCE", 1),
		SaleWindowDays:       envInt("SETTLEMENT_AUDITOR_SALE_WINDOW_DAYS", 30),
		QtyDeviationFactor:   envFloat("SETTLEMENT_AUDITOR_QTY_FACTOR", 3),
		StocktakeAppID:       os.Getenv("SETTLEMENT_AUDITOR_STOCKTAKE_APP_ID"),
		StocktakeNotifyUsers: envCSV("SETTLEMENT_AUDITOR_STOCKTAKE_NOTIFY_USERS"),
		WxH5BaseURL:          os.Getenv("SETTLEMENT_AUDITOR_WXH5_BASE_URL"),
	}
	if ids := envCSV("SETTLEMENT_AUDITOR_APPLY_CONTROL_IDS"); len(ids) == 3 {
		cfg.ApplyControlIDs = [3]string{ids[0], ids[1], ids[2]}
	}
	if ts := envCSV("SETTLEMENT_AUDITOR_APPLY_CONTROL_TYPES"); len(ts) == 3 {
		cfg.ApplyControlTypes = [3]string{ts[0], ts[1], ts[2]}
	}
	cfg.AIConclusionControlID = os.Getenv("SETTLEMENT_AUDITOR_AI_CONCLUSION_CONTROL_ID")
	// 「附件」控件(企微「结算付款」模板里的 File 型控件)。
	//
	// ⚠️ id **不设默认值**:它是模板里的那一个控件,随模板走 ⇒ 猜错 id
	//    只会让企微报"控件不存在"。空 ⇒ 不带附件(退回旧行为:只写四个文本控件)。
	cfg.AttachmentControlID = os.Getenv("SETTLEMENT_AUDITOR_ATTACHMENT_CONTROL_ID")
	cfg.AttachmentControlType = os.Getenv("SETTLEMENT_AUDITOR_ATTACHMENT_CONTROL_TYPE")
	// 「付款信息」多行文本控件(2026-10-28 用户口径)。
	//
	// ⚠️ id **不设默认值**:它是企微后台那张模板里的控件,id 随模板走
	// ⇒ 猜错 id 只会让企微报"控件不存在"。
	// 空 ⇒ 不写这个控件(退回旧行为,提交不受影响)。
	cfg.PaymentInfoControlID = os.Getenv("SETTLEMENT_AUDITOR_PAYMENT_INFO_CONTROL_ID")
	cfg.PaymentInfoControlType = os.Getenv("SETTLEMENT_AUDITOR_PAYMENT_INFO_CONTROL_TYPE")
	// 审批流程节点声明(与企微管理后台里「付款审批」模板的流程设置对应)。
	//
	// ⚠️ 企微的公开 API 读不到流程配置(gettemplatedetail 只给表单控件),
	//    所以这份声明只能由 app 侧持有 —— **改模板流程时这里要一起改**。
	//    解析失败直接退出:配置错了要让进程起不来,而不是等到店长发起时才发现。
	flow, err := service.ParseApprovalFlow(os.Getenv("SETTLEMENT_AUDITOR_APPROVAL_FLOW"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "SETTLEMENT_AUDITOR_APPROVAL_FLOW 非法: %v\n", err)
		os.Exit(1)
	}
	cfg.ApprovalFlow = flow
	appSvc = service.New(appDB, cfg)

	// dapr client(pub/sub + conversation + invoke 共用)。
	//
	// ⚠️ 用 daprclient.New() 而不是 dapr.NewClient():
	//   go-sdk 的 api-token 拦截器会**整块替换** outgoing metadata,
	//   把本客户端转发给 cube 的 Authorization 头抹掉
	//   (2026-10-20 全网加 DAPR_API_TOKEN 之后暴露的)。
	//   详见 authkit/daprclient 的包注释。
	daprCli, err := daprclient.New()
	if err != nil {
		return fmt.Errorf("创建 dapr 客户端失败: %w (确认 dapr run 已起)", err)
	}

	// pub/sub publisher(fail-fast:命令/事件是本服务核心输出)。
	pub, err := service.NewDaprPublisherFromEnv()
	if err != nil {
		return fmt.Errorf("dapr publisher: %w (确认 dapr run 已起)", err)
	}
	appSvc.SetPublisher(pub)
	slog.Info("dapr publisher enabled")

	// cube 查询客户端(cube-router live 查询)。
	cubeCli := cubeclient.NewDaprCubeClient(
		daprCli, cubeclient.DefaultCubeAppID(), cubeclient.DefaultCubeQueryPath())
	appSvc.SetCube(service.NewDaprCubeQuerier(cubeCli))
	slog.Info("cube client enabled")

	// ---- 归档导出/导入的「这个门店配了哪个数据源」解析器 ----
	//
	// ⚠️ **只用它解析数据源,不拿它查业务数据**:
	//   审批实例 / 核对信号 / 推理日志是**本域自己产生的**,cube 里没有这些 model。
	//
	// ⚠️ 解析器带 60s 缓存 + stale-on-error:每次导出都打一次 dapr 往返不划算,
	//   而一次网络抖动不该让整个门店的导出挂掉。
	appArchive = service.NewArchiveService(appDB)
	appArchive.SetSourceResolver(cubesource.NewCachedResolver(cubeCli, cubesource.DefaultTTL, slog.Default()))
	slog.Info("settlement-auditor: 归档导出/导入已启用", "app_id", cubeclient.DefaultCubeAppID())

	// LLM 研判(同步调 Dapr Conversation API,仿 fresh-meat)。
	convName := os.Getenv("SETTLEMENT_AUDITOR_CONVERSATION")
	if convName == "" {
		convName = "conversation"
	}
	llmTimeout := 30 * time.Second
	if env := os.Getenv("SETTLEMENT_AUDITOR_LLM_TIMEOUT"); env != "" {
		if d, err := time.ParseDuration(env); err == nil && d > 0 {
			llmTimeout = d
		}
	}
	appSvc.SetVerdictFn(service.DaprConversationVerdictFn(daprCli, convName, llmTimeout))
	slog.Info("dapr conversation client enabled", "component", convName, "timeout", llmTimeout)

	// stocktake 盘点建单客户端(内部端点,任务 1c 提供)。
	appSvc.SetStocktake(service.NewDaprStocktakeClient(daprCli, appSvc.Config().StocktakeAppID))

	// 供应商收款账号客户端(2026-10-28):按 id 回查开户行/账号/户名。
	//
	// ⚠️ 取回的账号信息**只落 approval_payee_accounts**,不进 LLM 输入面
	// (见 internal/settlement-auditor/service/payee_account.go)。
	//
	// ⚠️ 调的是 bank-account 服务的**公开读端点**(不是内部端点):
	// 公开端点有 per-branch scope 守门 ⇒ 调用者身份被校验。
	// ⇒ 申请人的 JWT 必须透传 ⇒ 用 daprclient.New() 而非 dapr.NewClient()
	// (go-sdk 的 api-token 拦截器会整块替换 outgoing metadata,把
	//  authorization 抹掉 —— 2026-10-20 全网加 DAPR_API_TOKEN 后暴露的)。
	bankAccountAppID := os.Getenv("SETTLEMENT_AUDITOR_BANK_ACCOUNT_APP_ID")
	if bankAccountAppID == "" {
		bankAccountAppID = "supertrade-bank-account"
	}
	appSvc.SetBankAccount(service.NewDaprBankAccountClient(daprCli, bankAccountAppID))
	slog.Info("bank-account client enabled", "app_id", bankAccountAppID)

	// userinfo(userd,per-branch scopes;同 fresh-meat 链路)。
	// 2026-10-12:auth-internal 已合并进 userd,调用方统一打 app-id=userd。
	ui, err := userinfo.New("userd")
	if err != nil {
		return fmt.Errorf("userinfo client: %w (确认 dapr run 已起)", err)
	}
	appSvc.SetUserInfo(ui)
	warmupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ui.Warmup(warmupCtx); err != nil {
		slog.Warn("userinfo warmup 失败 (auth-internal 链路未预热),业务首次调用走 cold path", "err", err)
	} else {
		slog.Info("userinfo warmup ok (auth-internal 链路预热完成)")
	}

	// 注册 dapr job:扫卡死实例(@every 5m)。best-effort,失败仅 warn。
	registerRescanJob()

	slog.Info("settlement-auditor app initialized", "db_driver", "postgres", "tables", 7)
	return nil
}

// registerRescanJob 经 dapr sidecar HTTP API 注册 rescan job(best-effort)。
//
// 两个坑(与 wechat-bot wcomsync/job.go 同款教训,2026-10-10 实踩):
//  1. 同名 POST 返回 409 不是 upsert —— 必须先 DELETE(忽略 404/连接失败)。
//  2. data 不能省略:空 data 的 job 触发时 daprd 1.18 proto marshal 空
//     structpb.Value 报 "none of the oneof fields is set" 并每秒重试风暴。
func registerRescanJob() {
	port := os.Getenv("DAPR_HTTP_PORT")
	if port == "" {
		port = "3500"
	}
	url := fmt.Sprintf("http://localhost:%s/v1.0-alpha1/jobs/settlement-auditor-rescan", port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 先删后建(幂等):DELETE 是尽力清理,失败不阻断后续 POST。
	if req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil); err == nil {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}

	body := []byte(`{"schedule":"@every 5m","data":{"trigger":"dapr-job","job":"settlement-auditor-rescan"}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		slog.Warn("register rescan job: build request", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Warn("register rescan job failed(卡死实例将不会自动重跑)", "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		slog.Warn("register rescan job non-2xx", "status", resp.StatusCode)
		return
	}
	slog.Info("rescan job registered", "schedule", "@every 5m")
}

func registerRoutes(r *gin.Engine) {
	if appSvc == nil {
		panic("settlement-auditor: appSvc 未初始化,可能是 OnStart 失败")
	}
	r.Use(middleware.XBranchID())
	r.Use(forwardBearerToOutgoing())
	h := handler.New(appSvc)
	h.RegisterRoutes(r)
	// 归档导出/导入(独立的契约 + 独立的守卫 scope)。
	//
	// ⚠️ 守门复用 service 层的 effective scopes(带 60s 缓存),
	//   与本域既有的 settlement:view / settlement:audit 同一套口径。
	handler.NewArchiveHandler(appArchive, rbac.ScopeResolver(
		func(c *gin.Context, userID, branchID, scope string) (bool, error) {
			return appSvc.HasEffectiveScope(c.Request.Context(), userID, branchID, scope)
		})).RegisterRoutes(r)
}

// registerDaprRoutes 在 auth 中间件**之前**注册 dapr 订阅端点
// (/dapr/subscribe、/events/*、/job/*;sidecar 调用不带 Authorization 头)。
func registerDaprRoutes(r *gin.Engine) {
	h := handler.New(appSvc)
	h.RegisterSubscribeRoutes(r, slog.Default())
	// 兜底清理端点。收尾清理时未必持有 token，所以必须在这个
	// **免鉴权窗口**注册；公网安全靠 nginx 的
	// `location ^~ /api/v1/settlement-auditor/internal/ { return 404; }`。
	handler.RegisterPurgeRoutes(r, appDB)
}

// forwardBearerToOutgoing 把 caller 的 Authorization 注入 ctx,供下游
// dapr 调用(cubeclient → cube-router,userinfo → userd)转发。
//
// ⚠️ X-Branch-ID 也要写进 **cubeclient 的 ctx key**:
//
//	归档端点要问 cube-router「这个门店配了哪个数据源」,
//	而 cube-router 的 /v1/branch-source 挂着 cube:read 守门
//	⇒ JWT 与门店都要被透传过去,否则 401 / 路由到错的门店。
func forwardBearerToOutgoing() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		if bearer := c.Request.Header.Get("Authorization"); bearer != "" {
			ctx = cubeclient.WithBearer(ctx, bearer)
			ctx = userinfo.WithBearer(ctx, bearer)
		}
		if branchID := strings.TrimSpace(c.Request.Header.Get("X-Branch-ID")); branchID != "" {
			ctx = cubeclient.WithBranchID(ctx, branchID)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// ---- env helpers ----

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envCSV(key string) []string {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
