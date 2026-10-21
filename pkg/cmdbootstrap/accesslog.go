// 请求访问日志 / 业务错误日志 / panic 兜底。
//
// # 为什么需要访问日志
//
// POS 联调时出现过一个问题:客户端报"未找到商品",而**后端日志里一条请求记录都没有** ——
// 只有 dapr sidecar 的 `A non-YAML Component file authkey.yaml.bak-... was detected`
// warning(那是无害的噪音)。
// ⇒ 排查时看不到"这个请求到底有没有到服务、走的哪个路径、什么状态码"。
//
// 根因:这些服务用 cmdbootstrap 统一起 gin engine,但**没装 gin 的访问日志中间件**
// (`gin.Default()` 的文本日志我们也不用 —— 那不是 slog JSON,grep 起来也费劲)。
//
// # 装在哪一层
//
// 访问日志装在**业务路由之前**(见 NewRouter 里的注释):
//
//   - 业务路由之前 ⇒ 业务端点的每条请求都有记录;
//   - /healthz 与 dapr 内部端点**之前** ⇒ 它们不在链上,不打日志
//     (健康轮询每 3 秒一次、sidecar 每 10 分钟重注册 job ⇒ 打进去就是刷屏)。
//
// # 绝不打印的东西
//
// Authorization / Cookie / 请求体 / query string —— 一个都不打。
// path 只取**路由模板**(c.FullPath()),路由没匹配上才退回 URL.Path
// (query string 被整个去掉,因为 query 里可能有 token / 口令)。
package cmdbootstrap

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/YunBright/authkit/claims"
	"github.com/gin-gonic/gin"
)

// accessLogMessage 是访问日志的固定 message,便于 `journalctl | grep`。
const accessLogMessage = "http access"

// branchHeader 是门店上下文 header(与 pkg/middleware.XBranchID 同名)。
//
// ⚠️ 只取**原值**不做解析:多店场景 header 可能是 "01,02" 或 "*",
// 日志要的是"调用方说了什么",不是"我们认为它是哪家店"。
const branchHeader = "X-Branch-ID"

// ---- 免访问日志的路径集合 ----

// accessLogSkipPaths 是"免访问日志"的路径集合。
//
// ⚠️ **跟着实际注册的路由走**,不维护一份硬编码的路径列表 ——
//
//	硬编码的那份迟早和实际注册的路径对不上
//	(哪个服务注册了 /events/*、哪个没注册,各不一样),
//	而"日志里少了一类请求"这种故障**没人会发现**。
//
// ⇒ 登记点只有两处(都在 bootstrap.go 里):
//
//	(1) 注册 /healthz 的时候;
//	(2) 调用 Options.RegisterDapr 之后 —— 把那批**新注册**的路由全登记进来。
var accessLogSkipPaths = struct {
	mu    sync.RWMutex
	paths map[string]bool
}{paths: map[string]bool{}}

// RegisterAccessLogSkipPaths 登记"免访问日志"的路径。
//
// ⚠️ 登记的是**路由模板**(如 "/events/:topic"),与 c.FullPath() 同形 ——
//
//	否则按模板登记、按具体路径判,永远判不中。
func RegisterAccessLogSkipPaths(paths ...string) {
	accessLogSkipPaths.mu.Lock()
	defer accessLogSkipPaths.mu.Unlock()
	for _, p := range paths {
		if p == "" {
			continue
		}
		accessLogSkipPaths.paths[p] = true
	}
}

// isAccessLogSkipped 判断某条路径要不要跳过访问日志。
//
// ⚠️ /healthz 是 **cmdbootstrap 自己**注册的公开探活(每个 app 都有),
//
//	所以硬编码在这里 —— 它不是"一份会和服务实际注册对不上的清单"。
//	探活轮询每 3 秒一次 ⇒ 记进去就是刷屏。
//
// ⚠️ 其余(各服务通过 Options.RegisterDapr 注册的 dapr 内部端点)走**登记表**:
//
//	登记的是**实际注册**的路由模板,不是硬编码列表
//	(哪个服务注册了 /events/*、哪个没注册,各不一样)。
func isAccessLogSkipped(path string) bool {
	if path == "/healthz" {
		return true
	}
	accessLogSkipPaths.mu.RLock()
	defer accessLogSkipPaths.mu.RUnlock()
	return accessLogSkipPaths.paths[path]
}

// ---- 访问日志 ----

// AccessLog 是访问日志中间件。
//
// ⚠️ **持有自己的 *slog.Logger**(不直接用 slog.Default()):
//
//	一、测试要能往 buffer 里注入才能断言"到底记了什么";
//	二、包级全局 logger 一改,整个进程(所有 app)的日志格式都被带跑。
type AccessLog struct {
	logger *slog.Logger
}

// NewAccessLog 构造访问日志中间件。
//
// logger 传 nil ⇒ 用 slog.Default()。
func NewAccessLog(logger *slog.Logger) *AccessLog {
	if logger == nil {
		logger = slog.Default()
	}
	return &AccessLog{logger: logger}
}

// Middleware 返回 gin 中间件。
func (a *AccessLog) Middleware() gin.HandlerFunc {
	return accessLog(a.logger)
}

// accessLog 返回业务请求的访问日志中间件。
//
// 每条请求打**一行** JSON(slog),字段见 logAccess。
//
// ⚠️ 级别:5xx = Error,其余(含 4xx)= Info。
//
//	4xx 不升 Warn / Error:越权探测(401 / 403)会因此刷屏,
//	而"客户端打错路径"本来就不是服务端的问题。
func accessLog(logger *slog.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		logAccess(logger, c, c.Writer.Status(), time.Since(start))
	}
}

// logAccess 打一条访问日志。
//
// ⚠️ 字段里**没有** Authorization / Cookie / 请求体 / query string。
func logAccess(logger *slog.Logger, c *gin.Context, status int, dur time.Duration) {
	path := requestPath(c)
	if isAccessLogSkipped(path) {
		return
	}
	attrs := []any{
		"method", c.Request.Method,
		"path", path,
		"status", status,
		"duration_ms", dur.Milliseconds(),
		"ip", c.ClientIP(),
		"bytes", responseBytes(c),
		"sub", requestSub(c),
		"branch", c.GetHeader(branchHeader),
	}
	if status >= http.StatusInternalServerError {
		logger.Error(accessLogMessage, attrs...)
		return
	}
	logger.Info(accessLogMessage, attrs...)
}

// ---- 业务错误日志 ----

// errorLog 返回业务错误日志中间件。
//
// 触发条件:handler 调了 `c.Error(err)`(gin 的标准错误收集方式)。
//
// ⚠️ 本仓各服务目前多数是直接 `c.AbortWithStatusJSON({code,message})`,
//
//	那种写法 cmdbootstrap **看不到** error ⇒ 只有访问日志(status 可见)。
//	等各服务改用 c.Error() 之后,这里会额外给出一条带 code 的 ERROR 行。
//
// ⚠️ 只打 code / message / method / path —— 不打请求体、不打请求头。
func errorLog(logger *slog.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(c *gin.Context) {
		c.Next()
		for _, e := range c.Errors {
			if e == nil || e.Err == nil {
				continue
			}
			if isClientDisconnect(e.Err) {
				continue
			}
			logger.Error("business error",
				"code", businessErrorCode(e),
				"message", e.Err.Error(),
				"method", c.Request.Method,
				"path", requestPath(c),
			)
		}
	}
}

// businessErrorCode 取业务错误的机器可读 code。
//
// 优先用错误自带的 `Code()`(本仓没有共享 apierror 包,各服务各写一份),
// 退而用 gin 的错误分类,最后兜一个常量。
func businessErrorCode(e *gin.Error) string {
	var coded interface{ Code() string }
	if errors.As(e.Err, &coded) {
		if code := coded.Code(); code != "" {
			return code
		}
	}
	switch e.Type {
	case gin.ErrorTypePrivate:
		return "private"
	case gin.ErrorTypePublic:
		return "public"
	case gin.ErrorTypeBind:
		return "bind"
	case gin.ErrorTypeRender:
		return "render"
	}
	return "business_error"
}

// isClientDisconnect 判断错误是不是"客户端断开连接"。
//
// gin 的 panic 兜底在 broken pipe 时会把错误塞进 c.Errors;
// 那是网络层的连接断开,不是业务错误 ⇒ 不该打成 business error。
func isClientDisconnect(err error) bool {
	var ne *net.OpError
	if !errors.As(err, &ne) {
		return false
	}
	msg := strings.ToLower(ne.Error())
	return strings.Contains(msg, "broken pipe") || strings.Contains(msg, "connection reset by peer")
}

// ---- panic 兜底 ----

// recoveryMiddleware 返回 panic 兜底中间件(替掉 `gin.Recovery()`)。
//
// ⚠️ **为什么不用 gin.Recovery()**:
//
//	gin 的 Recovery 用 httputil.DumpRequest 把**请求头**打到 stderr
//	(ReleaseMode 下不打头,DebugMode 下打;Authorization 被 gin 自己 mask 掉,
//	但 **Cookie 没有**),而且是纯文本、不是 slog JSON
//	⇒ 日志形态不统一 + 有漏令牌 / 漏 Cookie 的风险。
//
//	这里改成:只打 slog JSON(Error),字段里**没有**请求头 / 请求体。
//	⇒ 令牌、口令、Cookie 一个都不会进日志。
//
// ⚠️ 堆栈照打(runtime/debug.Stack):panic 现场是排障的第一手资料,
//
//	它只含函数名与文件路径,不含任何请求数据。
func recoveryMiddleware(logger *slog.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	return func(c *gin.Context) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			logger.Error("panic recovered",
				"method", c.Request.Method,
				"path", requestPath(c),
				"err", rec,
				"stack", string(debug.Stack()),
			)
			c.AbortWithStatus(http.StatusInternalServerError)
		}()
		c.Next()
	}
}

// ---- 小工具 ----

// requestPath 取要记进日志的路径。
//
// ⚠️ **优先用路由模板**(c.FullPath()),不用原始 URL:
//
//  1. 原始 URL 的 query string 里可能有 token / 口令 ⇒ 一个字都不打;
//  2. 路由模板按**接口**聚合 —— 一条 "/items/:id" 能统计出调用量,
//     而原始路径会把每个 id 拆成一行(几万个 id ⇒ 日志没法看)。
//
// ⚠️ 路由没匹配上(404)时 c.FullPath() 为空 ⇒ 退回 URL.Path(仍不含 query)。
//
//	不退回就只剩空串,现场根本看不出"客户端调了哪个地址"。
func requestPath(c *gin.Context) string {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return ""
	}
	if p := c.FullPath(); p != "" {
		return p
	}
	return c.Request.URL.Path
}

// requestSub 取调用方的 auth user id(claims.sub)。
//
// ⚠️ **只从 context 取,绝不自己解析 token** ——
//
//	解析 token 意味着在日志路径上多一份"读 secret"的逻辑,
//	而且解析失败时很容易顺手放行。取不到就是空字符串。
func requestSub(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	cl, ok := claims.FromContext(c.Request.Context())
	if !ok || cl == nil {
		return ""
	}
	return cl.Sub
}

// responseBytes 取响应体字节数。
//
// gin 在"什么都没写"时返回 -1 ⇒ 归一成 0,免得日志里出现 -1。
func responseBytes(c *gin.Context) int {
	if c == nil || c.Writer == nil {
		return 0
	}
	n := c.Writer.Size()
	if n < 0 {
		return 0
	}
	return n
}
