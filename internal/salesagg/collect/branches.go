package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Branch 是一个在营门店。
type Branch struct {
	ID          string
	DisplayName string
}

// BranchLister 列出**在营**门店。
//
// # 为什么门店列表来自 auth，而不是 wechat-bot 的应用配置
//
// 两个候选源都存在，但它们回答的不是同一个问题：
//
//	auth 的 GET /auth/branches  —— 「哪些门店在营业」（is_active）
//	wechat-bot 的自建应用配置   —— 「谁在 App 里点了开启每日销售统计」
//
// 后者是**推送配置**：一个应用可以绑了门店但没开统计，
// 也可以门店根本还没配应用。若拿它当采集名单，
// "这个门店今天没采" 的原因就变成了两种完全不同的东西
// （还没配 App / 配了但没开统计），而它们都不该影响数据落库。
//
// → 采集是**在营门店级**的事，与谁想看报告无关。
type BranchLister interface {
	ListActiveBranches(ctx context.Context) ([]Branch, error)
}

// LoginAppID 是 auth 的 **login 引擎**在 dapr 里的 app-id。
//
// ⚠️ 它必须与 deployer/systemd/auth-login.service 里的 `--app-id login` 完全一致。
//
//	两边各写一份字符串常量，改了一边**不会**编译报错 ——
//	症状是"采集任务一条都注册不上"，而服务照常启动、报表照出。
const LoginAppID = "login"

// AuthBranchLister 经 dapr service invocation 读 auth 的门店清单。
//
// # 为什么是 login 引擎而不是 userd
//
// auth 内部是**三个独立的 gin engine**（见 auth 的 internal/router 包注释）：
//
//	login    —— 公网入口 :8081，无 token 校验；JWKS / login / refresh / SSE
//	userd    —— app-id=userd；/users/* /admin/* /me
//	internal —— app-id=auth-internal，S2S-only
//
// `/auth/branches` 挂在 **login** engine 上（无 token，设计用途是
// 注册页/选门店页），所以必须打 app-id=login。
// 打 userd 会 404 —— 而 404 在这里很容易被误读成"auth 里没有门店"，
// 于是所有采集任务一条都不注册，且没有任何报错。
//
// ⚠️ 直连 http://host:8081 也可行，但**不走 dapr** 就意味着
//
//	绕过了 accessControl 白名单这条边界。本仓其它跨服务调用
//	（如 wechat-bot 调 sales-agg）一律走 dapr，这里保持一致。
type AuthBranchLister struct {
	sidecarBase string
	appID       string
	http        *http.Client
}

// NewAuthBranchLister 构造。sidecarBase 为空时返回 nil（没有 sidecar）。
func NewAuthBranchLister(sidecarBase, appID string) *AuthBranchLister {
	base := strings.TrimRight(strings.TrimSpace(sidecarBase), "/")
	if base == "" {
		return nil
	}
	id := strings.TrimSpace(appID)
	if id == "" {
		id = LoginAppID
	}
	return &AuthBranchLister{
		sidecarBase: base,
		appID:       id,
		http:        &http.Client{Timeout: 10 * time.Second},
	}
}

var _ BranchLister = (*AuthBranchLister)(nil)

// branchesURL 拼出 dapr service invocation 的地址。
//
// ⚠️ 形如 `/v1.0/invoke/<app-id>/method/<app 看到的路径>`：
//
//	"method/" 之后是**被调服务自己看到的路径**，
//	不含它的 app-id，也不含 nginx 剥掉的 /api/v1 前缀。
func (l *AuthBranchLister) branchesURL() string {
	return l.sidecarBase + "/v1.0/invoke/" + l.appID + "/method/auth/branches"
}

// branchItem 是 auth 公开端点的响应项。
type branchItem struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// ListActiveBranches 实现 BranchLister。
func (l *AuthBranchLister) ListActiveBranches(ctx context.Context) ([]Branch, error) {
	if l == nil {
		return nil, fmt.Errorf("collect: 门店列表未配置（没有 sidecar）")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.branchesURL(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("collect: 读取门店列表: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("collect: 读取门店列表（app-id=%s）返回 %d", l.appID, resp.StatusCode)
	}
	var items []branchItem
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, fmt.Errorf("collect: 解析门店列表: %w", err)
	}

	out := make([]Branch, 0, len(items))
	for _, it := range items {
		// ⚠️ id 为空的行必须丢：
		//   它会成为 job 名的一部分（"sales-daily-collect--initial"），
		//   而那会和别的空 id 行**撞名** —— 两个门店共用一个 job，
		//   表现是只有一个门店在采，而注册过程全部成功。
		id := strings.TrimSpace(it.ID)
		if id == "" {
			continue
		}
		out = append(out, Branch{ID: id, DisplayName: strings.TrimSpace(it.DisplayName)})
	}
	// ⚠️ 排序是为了让注册顺序稳定：
	//   auth 若换了个返回顺序，日志与 diff 就每次都不一样，
	//   而"顺序变了"会被误读成"门店集合变了"。
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
