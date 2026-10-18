// authclient.go — ServiceAccountClient 的 dapr 实现（透传用户 bearer）。
//
// # 为什么透传**用户**的 bearer 而不是 sales-agg 自己的凭据
//
// 用户要求"用当前执行用户的 jwt token 创建服务账号"。这不只是流程约定，
// 也是安全要求：只有这样，auth 侧判的才是"这个真人有没有
// service-account:create / grant"，而不是"某台机器自称有权限"。
//
// 若 sales-agg 用自己的身份去建号，任何能调到本服务该端点的人
// 都能建出一台带 cube:read 的机器身份 —— 权限模型当场失效。
//
// # ⚠️ 只经 dapr app-id 寻址，不直连
//
// 直连 http://host:8082 也可行，但不走 dapr 就绕过了服务寻址边界。
// 本仓其它跨服务调用一律走 dapr，这里保持一致。
package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrServiceAccountExists 表示 auth 侧该 client_id 已存在。
//
// 它是**自愈路径的触发信号**：上一次建号成功但密钥没落库，
// 明文永久丢失，而 client_id 是 UNIQUE ⇒ 重建只会撞这个。
// → 命中它就转去轮换密钥（见 Control.createAccountWithRecovery）。
//
// ⚠️ 必须是可判别的哨兵：若把它包成普通字符串，"409 → 轮换"这条
// 自愈分支就永远不会执行，而后果是**门店永远开不了采集**，
// 且报错是"client_id 已存在"，与真因（落库失败）毫无关系。
var ErrServiceAccountExists = errors.New("服务账号已存在")

// AuthAccountAppID 是 auth **userd** 引擎的 app-id。
//
// ⚠️ 与 collect.AuthAppID 同值，两处各写一份字符串常量，改一边不会编译报错 ——
//	症状是"创建服务账号连不上"，而服务照常启动。
const AuthAccountAppID = AuthAppID

// AuthDeniedError 表示 auth **拒绝**了这次调用（403）。
//
// # 为什么单独一个类型，而不是包成普通 error
//
// auth 的 403 message 里是「你没有开启门店采集的权限，缺少：
// 00:service-account:grant、00:cube:read」——
// **缺哪几条权限**是用户唯一能照着去申请的东西。
//
// 普通 error 包装会把这句话埋在一堆前缀里，handler 只能把它当"未知失败"
// 统一回 400/503，前端于是只能弹一句"操作失败，请重试"，
// 而重试一万次也是同样结果。
//
// → 用这个类型把 code 与 message **原样**带到响应里。
type AuthDeniedError struct {
	// Code 是后端的业务码（通常是 "forbidden"）。
	Code string
	// Message 是后端原话（缺哪几条权限就在里面）。
	Message string
}

func (e *AuthDeniedError) Error() string {
	if e.Message == "" {
		return "auth 拒绝：缺少操作权限"
	}
	return "auth 拒绝：" + e.Message
}

// DaprServiceAccountClient 经 dapr 调 auth userd 的服务账号端点。
type DaprServiceAccountClient struct {
	sidecarBase string
	appID       string
	http        *http.Client
}

// NewDaprServiceAccountClient 构造。sidecarBase 为空返回 nil。
func NewDaprServiceAccountClient(sidecarBase, appID string) *DaprServiceAccountClient {
	base := strings.TrimRight(strings.TrimSpace(sidecarBase), "/")
	if base == "" {
		return nil
	}
	id := strings.TrimSpace(appID)
	if id == "" {
		id = AuthAccountAppID
	}
	return &DaprServiceAccountClient{
		sidecarBase: base,
		appID:       id,
		http:        &http.Client{Timeout: 20 * time.Second},
	}
}

var _ ServiceAccountClient = (*DaprServiceAccountClient)(nil)

// authURL 拼 dapr service invocation 地址。
func (c *DaprServiceAccountClient) authURL(path string) string {
	return c.sidecarBase + "/v1.0/invoke/" + c.appID + "/method" + path
}

// postJSON 发一个带用户 bearer 的 POST。
//
// ⚠️ bearer 形如 "Bearer xxx"，这里**原样转发**，不重新拼 "Bearer" 前缀 ——
//	调用方（handler）拿到的就是 gin request 的 Authorization 头。
//
// ⚠️ 出错时**不回显请求体**：创建/轮换的请求体里有 client_secret。
//	而这个错误会经 HTTP 响应与日志被长期翻阅。
func (c *DaprServiceAccountClient) postJSON(ctx context.Context, url, userBearer string, body any) (map[string]any, error) {
	if c == nil {
		return nil, fmt.Errorf("auth 服务账号客户端未装配（没有 sidecar）")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if b := strings.TrimSpace(userBearer); b != "" {
		req.Header.Set("Authorization", b)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("调 auth 失败: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))
		_ = resp.Body.Close()
	}()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	switch {
	case resp.StatusCode == http.StatusConflict:
		// auth 的错误信封：{"code":"conflict","message":"client_id 已存在"}
		var env struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &env)
		if env.Code == "conflict" || strings.Contains(env.Message, "已存在") {
			return nil, fmt.Errorf("%w（%s）", ErrServiceAccountExists, env.Message)
		}
		return nil, fmt.Errorf("auth 返回 409: %s", env.Message)
	case resp.StatusCode/100 != 2:
		// auth 的错误信封：{"code":"forbidden","message":"..."}
		var env struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &env)
		if resp.StatusCode == http.StatusForbidden {
			// ⚠️ 403 要**原样**带到调用方：message 里是"缺哪几条权限"，
			//   那是用户唯一能照着去申请的东西。
			return nil, &AuthDeniedError{Code: env.Code, Message: env.Message}
		}
		if env.Message == "" {
			return nil, fmt.Errorf("auth 返回 %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("auth 返回 %d：%s", resp.StatusCode, env.Message)
	}

	var out map[string]any
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &out); err != nil {
			return nil, fmt.Errorf("解析 auth 响应失败: %w", err)
		}
	}
	return out, nil
}

// createScopedBody 对应 auth 的 CreateScopedServiceAccountReq。
type createScopedBody struct {
	ClientID    string `json:"client_id"`
	DisplayName string `json:"display_name"`
	Branches    []struct {
		BranchID    string   `json:"branch_id"`
		Permissions []string `json:"permissions"`
	} `json:"branches"`
}

// CreateScoped 实现 ServiceAccountClient。
func (c *DaprServiceAccountClient) CreateScoped(ctx context.Context, userBearer, clientID, displayName, branchID string, permissions []string) (string, error) {
	var body createScopedBody
	body.ClientID = clientID
	body.DisplayName = displayName
	b := struct {
		BranchID    string   `json:"branch_id"`
		Permissions []string `json:"permissions"`
	}{BranchID: branchID, Permissions: permissions}
	body.Branches = append(body.Branches, b)

	out, err := c.postJSON(ctx, c.authURL("/service-accounts"), userBearer, body)
	if err != nil {
		return "", err
	}
	return secretOf(out), nil
}

// ResetSecret 实现 ServiceAccountClient（自愈路径）。
func (c *DaprServiceAccountClient) ResetSecret(ctx context.Context, userBearer, clientID string) (string, error) {
	out, err := c.postJSON(ctx, c.authURL("/service-accounts/"+clientID+"/secret"), userBearer, map[string]any{})
	if err != nil {
		return "", err
	}
	secret := secretOf(out)
	if secret == "" {
		// 空明文必须当失败：返回它会让调用方把空串当凭据落库，
		// 而采集会一直 401，真因（轮换没真返回明文）被完全掩盖。
		return "", fmt.Errorf("轮换密钥后 auth 未返回明文 —— 拒绝继续（否则会落一个空凭据）")
	}
	return secret, nil
}

// SetDisabled 实现 ServiceAccountClient。
func (c *DaprServiceAccountClient) SetDisabled(ctx context.Context, userBearer, clientID string, disabled bool) error {
	_, err := c.postJSON(ctx, c.authURL("/service-accounts/"+clientID+"/disabled"), userBearer,
		map[string]any{"disabled": disabled})
	return err
}

// secretOf 从响应里取 client_secret。
func secretOf(out map[string]any) string {
	if out == nil {
		return ""
	}
	if v, ok := out["client_secret"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}
