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
//	SETTLEMENT_AUDITOR_AMOUNT_TOLERANCE       金额容差(默认 0.01)
//	SETTLEMENT_AUDITOR_SALE_WINDOW_DAYS       销售回看窗口(默认 30)
//	SETTLEMENT_AUDITOR_QTY_FACTOR             数量偏离倍数(默认 3)
//	SETTLEMENT_AUDITOR_STOCKTAKE_APP_ID       stocktake dapr app-id(默认 stocktake)
//	SETTLEMENT_AUDITOR_STOCKTAKE_NOTIFY_USERS 盘点待办接收人(逗号分隔企微 userid)
//	SETTLEMENT_AUDITOR_WXH5_BASE_URL          wx-h5 基础地址(taskcard url)
//	SETTLEMENT_AUDITOR_APPLY_CONTROL_IDS      模板三个 Text 控件 id(逗号分隔,默认 Text-1,Text-2,Text-3)
//	SETTLEMENT_AUDITOR_AI_CONCLUSION_CONTROL_ID 模板「AI审核结论」多行文本控件 id(可选;非空时 apply_data 写入 LLM 结论)
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

	"github.com/YunBright/authkit/userinfo"
	dapr "github.com/dapr/go-sdk/client"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubeclient"
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
	appDB  *gorm.DB
	appSvc *service.Service
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
		&model.AuditSignal{},
		&model.AuditLog{},
		&model.LLMVerdict{},
		&model.ApprovalStocktakeLink{},
		&model.ApprovalApproverPreference{},
	); err != nil {
		return fmt.Errorf("auto migrate: %w", err)
	}

	cfg := service.Config{
		CorpID:               os.Getenv("SETTLEMENT_AUDITOR_CORP_ID"),
		TemplateID:           os.Getenv("SETTLEMENT_AUDITOR_WECOM_TEMPLATE_ID"),
		AmountToleranceYuan:  envFloat("SETTLEMENT_AUDITOR_AMOUNT_TOLERANCE", 0.01),
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
	appSvc = service.New(appDB, cfg)

	// dapr client(pub/sub + conversation + invoke 共用)。
	daprCli, err := dapr.NewClient()
	if err != nil {
		return fmt.Errorf("dapr client: %w (确认 dapr run 已起)", err)
	}

	// pub/sub publisher(fail-fast:命令/事件是本服务核心输出)。
	pub, err := service.NewDaprPublisherFromEnv()
	if err != nil {
		return fmt.Errorf("dapr publisher: %w (确认 dapr run 已起)", err)
	}
	appSvc.SetPublisher(pub)
	slog.Info("dapr publisher enabled")

	// cube 查询客户端(cube-router live 查询)。
	appSvc.SetCube(service.NewDaprCubeQuerier(cubeclient.NewDaprCubeClient(
		daprCli, cubeclient.DefaultCubeAppID(), cubeclient.DefaultCubeQueryPath())))
	slog.Info("cube client enabled")

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

	// userinfo(auth-internal,per-branch scopes;同 fresh-meat 链路)。
	ui, err := userinfo.New("auth-internal")
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

	slog.Info("settlement-auditor app initialized", "db_driver", "postgres", "tables", 6)
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
}

// registerDaprRoutes 在 auth 中间件**之前**注册 dapr 订阅端点
// (/dapr/subscribe、/events/*、/job/*;sidecar 调用不带 Authorization 头)。
func registerDaprRoutes(r *gin.Engine) {
	h := handler.New(appSvc)
	h.RegisterSubscribeRoutes(r, slog.Default())
}

// forwardBearerToOutgoing 把 caller 的 Authorization 注入 ctx,供下游
// dapr 调用(cubeclient → cube-router,userinfo → auth-internal)转发。
func forwardBearerToOutgoing() gin.HandlerFunc {
	return func(c *gin.Context) {
		bearer := c.Request.Header.Get("Authorization")
		if bearer != "" {
			ctx := c.Request.Context()
			ctx = cubeclient.WithBearer(ctx, bearer)
			ctx = userinfo.WithBearer(ctx, bearer)
			c.Request = c.Request.WithContext(ctx)
		}
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
