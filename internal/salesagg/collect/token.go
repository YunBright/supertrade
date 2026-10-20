// token.go — 用门店的服务账号凭据换 access token（client credentials）。
//
// # 为什么每次现换、且**不缓存**
//
// 三条约束叠在一起，结论是"别缓存"：
//
//  1. 一天只换两次（22:45 首采、次日 01:05 校正），缓存省不下什么；
//  2. auth 侧不签 refresh token（机器客户端到期现换，ServiceTTL 默认 1h），
//     所以"缓存"实际上就是"缓存一个 1 小时后就失效的东西"；
//  3. dapr scheduler 把 job 投递到**随机副本**（官方 scheduler.md），
//     而"关停采集"是立刻要生效的状态位 —— 一旦缓存了状态，
//     用户点了关闭后最长要等 TTL 过期才停，症状是"关了还在采"，
//     而关停的**全部意义**就是不再采集。
//
// → 每次执行现读凭据行、现换 token。多副本天然一致，零协调。
//
// # 为什么 app-id 是 login 而不是 userd
//
// 签 token 需要 JWT 私钥，而私钥只在 auth-login 进程里
// （cmd/authd/main.go 的 keyMgr / issuer / loginSvc 全在 `if runLogin` 块内；
// 生产是 auth-login / auth-userd 两个独立 systemd unit）。
// userd 引擎即使挂上 /auth/token 也会因为没有私钥而每次都失败。
//
// ⚠️ 而 gyy 上按 `app-id=login` 寻址曾必然失败：auth-login 的 systemd unit
// **漏了** `--config /opt/.dapr/config.yaml` ⇒ 注册不进 gyy 用的 consul。
// → 修法在**部署侧**（给 auth-login unit 补 --config），不在这里绕。
package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TokenAuthAppID 是签发 token 的 auth 引擎在 dapr 里的 app-id。
//
// ⚠️ 2026-10-20 改: "login" → "userd"。
//
// auth-login 与 auth-userd 已合并成**一个进程**、**一个 dapr app-id=userd**
// (见 deployer/systemd/auth.service)。app-id=login 不再注册进 Consul,
// 按它寻址必然 ERR_DIRECT_INVOKE。
//
// ⚠️ 私有 key 只在合并后的那个进程里(它同时负责签发与验签),
//    所以 /auth/token 只能经 app-id=userd 寻址。
const TokenAuthAppID = "userd"

// TokenIssuer 用 client credentials 换 access token。
type TokenIssuer interface {
	// AccessToken 为某门店换一枚 access token（不含 "Bearer " 前缀）。
	AccessToken(ctx context.Context, branchID, clientID, clientSecret string) (string, error)
}

// DaprTokenIssuer 经 dapr service invocation 调 auth 的 POST /auth/token。
type DaprTokenIssuer struct {
	sidecarBase string
	appID       string
	http        *http.Client
}

// NewDaprTokenIssuer 构造。sidecarBase 为空返回 nil（没有 sidecar）。
func NewDaprTokenIssuer(sidecarBase, appID string) *DaprTokenIssuer {
	base := strings.TrimRight(strings.TrimSpace(sidecarBase), "/")
	if base == "" {
		return nil
	}
	id := strings.TrimSpace(appID)
	if id == "" {
		id = TokenAuthAppID
	}
	return &DaprTokenIssuer{
		sidecarBase: base,
		appID:       id,
		http:        &http.Client{Timeout: 15 * time.Second},
	}
}

var _ TokenIssuer = (*DaprTokenIssuer)(nil)

// tokenURL 拼出 dapr service invocation 地址。
//
// ⚠️ "method/" 之后是**被调服务自己看到的路径**，不含它的 app-id。
func (i *DaprTokenIssuer) tokenURL() string {
	return i.sidecarBase + "/v1.0/invoke/" + i.appID + "/method/auth/token"
}

// tokenResp 是 POST /auth/token 的响应。
//
// ⚠️ 刻意**没有** RefreshToken 字段 —— 服务端不签发，
// 在这里声明它等于把"机器客户端有 refresh"这件事写进类型里，
// 而那是刻意的设计（见 auth 的 serviceaccount.go 顶部）。
type tokenResp struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

// tokenReq 是 client credentials 请求体。
type tokenReq struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// AccessToken 实现 TokenIssuer。
func (i *DaprTokenIssuer) AccessToken(ctx context.Context, branchID, clientID, clientSecret string) (string, error) {
	if i == nil {
		return "", fmt.Errorf("采集凭据：token 签发器未装配（没有 sidecar）")
	}
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return "", fmt.Errorf("采集凭据：门店 %s 的凭据不完整", branchID)
	}
	body, err := json.Marshal(tokenReq{ClientID: clientID, ClientSecret: clientSecret})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.tokenURL(), bytes.NewReader(body))
	setAPIToken(req)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := i.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("采集凭据：向 auth 换 token 失败（门店 %s）: %w", branchID, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode/100 != 2 {
		// ⚠️ 错误文案**不回显请求体**：client_secret 在里面。
		//   而这个错误会经采集端点的 503 响应回到日志被长期翻阅。
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return "", fmt.Errorf(
				"采集凭据：门店 %s 的服务账号被 auth 拒绝（%d）—— 若刚关停过采集，请等状态同步或检查账号是否已禁用",
				branchID, resp.StatusCode)
		}
		return "", fmt.Errorf("采集凭据：向 auth 换 token 返回 %d（门店 %s）", resp.StatusCode, branchID)
	}

	var tr tokenResp
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("采集凭据：解析 token 响应失败（门店 %s）: %w", branchID, err)
	}
	if strings.TrimSpace(tr.AccessToken) == "" {
		// 空 token 不能当成功：那会让后续 cube 调用不带 Authorization，
		// 而症状是"cube 401"，排查会跑到 cube 那边去，
		// 真因（换 token 没换到）却在 auth 侧。
		return "", fmt.Errorf("采集凭据：auth 返回了空的 access_token（门店 %s）", branchID)
	}
	return tr.AccessToken, nil
}
