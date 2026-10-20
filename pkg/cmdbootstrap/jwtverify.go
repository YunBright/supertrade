// 应用层 JWT 验签的装配(2026-10-12)。
//
// 为什么要换掉原来的 claims.GinMiddleware():
//
//	原设计假定"Dapr sidecar 已经验过签,应用只解 payload"。这个假定在
//	**单上游 + 跨主机 service invocation** 的 nginx 拓扑下不成立:
//
//	  公网 → nginx → userd sidecar :3500 → /v1.0/invoke/<目标app>/method/...
//
//	appHttpPipeline 只作用于「**该 Configuration 所属 app** 的 sidecar → app」,
//	借道到别的 app 时不触发;真正执行的是**目标 app 自己 sidecar** 的 pipeline,
//	而 gyy 各服务的 Configuration 里并没有它。于是除 userd 外,所有服务实际
//	都是「解析即信任」。
//
//	生产实证(2026-10-12):构造一个 signature 是垃圾、payload 取自真 token 的 JWT,
//	打 /api/v1/settlement-auditor/settlement-approvals → **200 + 真实结算审批数据**;
//	同一个 token 打 /api/v1/userd/me → 401(那边确实有 bearer 中间件)。
//
//	补 appHttpPipeline 也走过,生产 canary 证伪:它会拦 sidecar 自己拉
//	/dapr/subscribe(sidecar 日志 "app returned http status code 401 from
//	subscription endpoint"),该服务全部 pubsub 事件静默停止投递。
//
// 所以:验签下沉到应用层,公钥从 **dapr state** 读(authkit/authn.StateSource)。
// 这样做的额外好处是"脱离 dapr 就无法验签"——没有 sidecar 的进程读不到公钥。
//
// 装配本体在 authn.NewFromDaprState(与 auth 仓共用同一份,避免分叉);
// 本文件只负责:env → 配置 → 全局单例 → healthz 上报。
package cmdbootstrap

import (
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/YunBright/authkit/authn"
)

// 验签相关的 env。默认值与 auth 侧 login.JWKSStoreName/JWKSStateKey 必须一致。
const (
	// EnvJWKSStore 是 dapr state 组件名(不是 app-id)。
	EnvJWKSStore = "AUTH_JWKS_STATE_STORE"
	// EnvJWKSKey 是 JWKS 在 state 里的键名。
	EnvJWKSKey = "AUTH_JWKS_STATE_KEY"
	// EnvJWKSIssuer 是期望的 iss claim;生产必须配,否则跨环境 token 可互认。
	EnvJWKSIssuer = "AUTH_JWKS_ISSUER"

	defaultJWKSStore  = "authkey"
	defaultJWKSKey    = "jwks/current"
	defaultJWKSIssuer = "https://yb.baiyuan.com"

	// 公钥后台刷新周期与"抖动容忍上限"。取值的含义见 authn.StateSourceConfig。
	//
	// StaleGrace < RefreshInterval 的量级是刻意的:允许连续若干次刷新失败
	// (redis 抖动)仍用缓存,但不允许无限期 —— 否则密钥吊销后旧公钥还在被信任。
	jwksRefreshInterval = 5 * time.Minute
	jwksStaleGrace      = 30 * time.Minute
)

var (
	verifierMu sync.RWMutex
	verifierV  *authn.Verifier
	setupV     *authn.DaprSetup
)

// AuthVerifier 返回当前装配的验签器;未装配时返回 nil。
func AuthVerifier() *authn.Verifier {
	verifierMu.RLock()
	defer verifierMu.RUnlock()
	return verifierV
}

// RevocationReloadError 返回最近一次**撤销列表**读取错误(nil = 健康),供 /healthz 上报。
//
// ⚠️ 它**不**等于"鉴权不可用"：撤销列表读不到时是 fail-open(照常放行),
// 所以它只说明"撤销暂时不生效"(登出后的 access token 仍可用到过期)。
//
// 为什么要单独暴露:撤销列表**读不到**与**读到空列表**在读侧看起来一样,
// 而后者是正常状态(还没人登出过)—— 没有这个字段,
// "撤销静默不生效"这件事在 /healthz 上完全看不出来。
//
// 可观测性要求:公钥读不到时服务是**fail-closed** 的(所有请求 401),
// 但 healthz 仍应 200(否则侧车会认为 app 挂了并反复重启,掩盖真正的故障面)。
// 所以要把"鉴权不可用"这件事显式放进 healthz,而不是靠 status code 表达。
func JWKSReloadError() error {
	verifierMu.RLock()
	src := setupV
	verifierMu.RUnlock()
	if src == nil || src.Source == nil {
		return nil
	}
	return src.Source.LastError()
}

// RevocationReloadError 见上。
func RevocationReloadError() error {
	verifierMu.RLock()
	src := setupV
	verifierMu.RUnlock()
	if src == nil {
		return nil
	}
	return src.RevocationError()
}

// SetAuthVerifier 注入验签器(供 NewRouter 使用)。
func SetAuthVerifier(v *authn.Verifier) {
	verifierMu.Lock()
	verifierV = v
	verifierMu.Unlock()
}

// SetupJWTVerifier 构造验签器并做首次预热,**失败则返回 error**。
//
// 调用方(Run)必须把 error 当致命错误处理:宁可起不来,也不要
// "起来了但所有请求 401" —— 后者会被误判成前端 token 过期 / 后端 bug,
// 排查成本高一个量级。
//
// 与 claims.GinMiddleware 的关键差别:那个中间件在验签失败时只是
// "不注入 claims",看起来还能跑;这里在**装配期**就失败,故障面更小、症状更明确。
func SetupJWTVerifier(logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	store := envOr(EnvJWKSStore, defaultJWKSStore)
	key := envOr(EnvJWKSKey, defaultJWKSKey)
	issuer := envOr(EnvJWKSIssuer, defaultJWKSIssuer)

	setup, err := authn.NewFromDaprState(authn.SetupConfig{
		StoreName:       store,
		Key:             key,
		Issuer:          issuer,
		Logger:          logger,
		RefreshInterval: jwksRefreshInterval,
		StaleGrace:      jwksStaleGrace,
		AppID:           os.Getenv("DAPR_APP_ID"),
		// 撤销列表（access token 的 jti 黑名单）。
		//
		// ⚠️ 键名用 authkit 的**共享常量**，不提供 env 覆盖：
		// 写侧（auth 的发布器）与读侧（这里）必须逐字一致，而 env 覆盖
		// 正是"写侧改了、读侧没改"这种漂移的来源。
		// 漂移的后果最危险：读侧读到**空列表** ⇒ 撤销**静默不生效**，零报错。
		//
		// 不配它 ⇒ 不做撤销检查（登出后 token 仍可用到过期）。
		RevocationKey: authn.DefaultRevocationKey,
	})
	if err != nil {
		return err
	}

	verifierMu.Lock()
	setupV = setup
	verifierV = setup.Verifier
	verifierMu.Unlock()

	logger.Info("JWT 应用层验签已就绪(公钥来源 dapr state)",
		"store", store, "key", key, "issuer", issuer)
	return nil
}

// CloseJWTVerifier 停掉 JWKS 后台刷新 goroutine。
func CloseJWTVerifier() {
	verifierMu.Lock()
	src := setupV
	setupV = nil
	verifierV = nil
	verifierMu.Unlock()
	src.Close()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
