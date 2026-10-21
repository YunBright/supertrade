// Package cubesync / token.go —— 用服务账号换机器 token(client credentials)。
//
// # 为什么每次现换、且**不缓存**
//
// 三条约束叠在一起,结论是"别缓存":
//
//  1. 一天只跑一次(默认 03:30),缓存省不下什么;
//  2. auth 侧不签 refresh token(机器客户端到期现换,ServiceTTL 默认 1h),
//     所以"缓存"实际上就是"缓存一个 1 小时后就失效的东西";
//  3. dapr scheduler 把 job 投递到**随机副本**,而同步是幂等的重操作 ——
//     缓存 token 会让"换到的 token 过期"这件事延后到很晚才暴露。
//
// ⇒ 每次执行现换。多副本天然一致,零协调。
//
// # ⚠️ 机器 token 的 aud 必须同时含 cube-router 与 catalog
//
// auth 的 `jwt.service_audience` 默认值里同时有这两个
// (各业务服务用 `rbac.RequireAudience(<自己的 cmdbootstrap AppID>)` 把关,
// **cmdbootstrap AppID** 是 "cube-router" / "catalog",不是 dapr app-id)。
// ⇒ 一枚机器 token 就能同时打两个下游,不需要换两次。
//
// ⚠️ 但**权限是按门店解析**的:token 里的 scopes 恒为空数组,
// 真正的 scope 来自 userd 的 per-branch 权限矩阵。
// ⇒ 服务账号必须在**源门店**有 cube:read、在**目标门店**有 product:manage。
package cubesync

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

// TokenIssuer 用 client credentials 换 access token。
type TokenIssuer interface {
	// AccessToken 返回一枚 access token(**不含** "Bearer " 前缀)。
	AccessToken(ctx context.Context) (string, error)
}

// DaprTokenIssuer 经本机 sidecar 的 HTTP 调 auth 的 POST /auth/token。
type DaprTokenIssuer struct {
	sidecarBase  string
	appID        string
	clientID     string
	clientSecret string
	http         *http.Client
}

// NewDaprTokenIssuer 构造。
//
// sidecarBase 为空(没有 sidecar)时**仍然返回非 nil** ——
// 让调用方拿到一个"能明确报错"的对象,而不是一个 nil 指针。
// 症状区别很重要:空指针是 panic(看不出缺什么),
// 而这里会返回"没有 sidecar,无法换 token"。
func NewDaprTokenIssuer(sidecarBase, appID, clientID, clientSecret string) *DaprTokenIssuer {
	base := strings.TrimRight(strings.TrimSpace(sidecarBase), "/")
	id := strings.TrimSpace(appID)
	if id == "" {
		id = DefaultAuthAppID
	}
	return &DaprTokenIssuer{
		sidecarBase:  base,
		appID:        id,
		clientID:     strings.TrimSpace(clientID),
		clientSecret: strings.TrimSpace(clientSecret),
		http:         &http.Client{Timeout: 15 * time.Second},
	}
}

var _ TokenIssuer = (*DaprTokenIssuer)(nil)

// tokenURL 拼出 dapr service invocation 地址。
//
// ⚠️ "method/" 之后是**被调服务自己看到的路径**,不含它的 app-id。
func (i *DaprTokenIssuer) tokenURL() string {
	return i.sidecarBase + "/v1.0/invoke/" + i.appID + "/method/auth/token"
}

// tokenReq 是 client credentials 请求体。
type tokenReq struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// tokenResp 是 POST /auth/token 的响应。
//
// ⚠️ 刻意**没有** RefreshToken 字段 —— 服务端不签发,
// 在这里声明它等于把"机器客户端有 refresh"这件事写进类型里,而那是刻意的设计。
type tokenResp struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

// AccessToken 实现 TokenIssuer。
func (i *DaprTokenIssuer) AccessToken(ctx context.Context) (string, error) {
	if i == nil {
		return "", fmt.Errorf("cubesync: token 签发器未装配")
	}
	if i.sidecarBase == "" {
		return "", fmt.Errorf("cubesync: 没有 sidecar(DAPR_HTTP_PORT 未设置),无法向 auth 换机器 token")
	}
	if i.clientID == "" || i.clientSecret == "" {
		return "", fmt.Errorf("cubesync: 缺服务账号凭据(AUTH_SERVICE_CLIENT_ID / AUTH_SERVICE_CLIENT_SECRET)")
	}
	body, err := json.Marshal(tokenReq{ClientID: i.clientID, ClientSecret: i.clientSecret})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.tokenURL(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	setAPIToken(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := i.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("cubesync: 向 auth 换 token 失败: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode/100 != 2 {
		// ⚠️ 错误文案**不回显请求体**:client_secret 在里面。
		//   而这个错误会经同步端点的 503 响应回到日志被长期翻阅。
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return "", fmt.Errorf(
				"cubesync: 服务账号被 auth 拒绝(%d)—— 检查服务账号是否被禁用、client_secret 是否已轮换",
				resp.StatusCode)
		}
		return "", fmt.Errorf("cubesync: 向 auth 换 token 返回 %d", resp.StatusCode)
	}

	var tr tokenResp
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("cubesync: 解析 token 响应失败: %w", err)
	}
	if strings.TrimSpace(tr.AccessToken) == "" {
		// 空 token 不能当成功:那会让后续调用不带 Authorization,
		// 而症状是"下游 401",排查会跑到下游去,真因(没换到 token)却在 auth 侧。
		return "", fmt.Errorf("cubesync: auth 返回了空的 access_token")
	}
	return tr.AccessToken, nil
}
