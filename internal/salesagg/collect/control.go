// control.go — 门店采集的**开启 / 关停编排**。
//
// # 为什么编排放在 sales-agg 而不是 Flutter 或 auth
//
//	Flutter → sales-agg（带用户 token）
//	            ├─ ① 校验 cube 数据源已绑定（本地 PG，最快、最新）
//	            ├─ ② 调 auth 代建服务账号（透传用户 token ⇒ 权限按真人判）
//	            ├─ ③ 加密落库（本地 PG，主密钥在本进程）
//	            └─ ④ 立即注册 job（不等下一次对账）
//
// 三件事必须**同一个事务性的流程**里做完：少任何一件都会留下"看起来开了、
// 实际采不到"的状态，而那种状态没有任何一行报错。
//
// # ⚠️ 顺序：先建号、拿到明文，再落库；落库成功才注册 job
//
// 反过来（先注册 job）的话，job 会在凭据就绪前就被触发，
// 症状是"开启成功后的第一次采集 503"，而用户会以为"开了但就是没数据"。
//
// # ⚠️ 建号成功但落库失败 ⇒ 必须能自愈
//
//	明文只在 auth 的响应里出现一次。若此刻落库失败，调用方**永久**没有明文；
//	而 client_id 是 UNIQUE，重建只会撞 409，错误与真因（落库失败）毫无关系。
//	→ 命中 409 时转去**轮换密钥**（auth 的 /service-accounts/:id/secret），
//	  换一枚新明文重新走落库。这是唯一能自愈的出路。
package collect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrCubeSourceNotConfigured 表示该门店还没绑定 cube 数据源。
//
// # 前端要靠它弹「去配置数据源」的链接
//
// 所以它必须是**可判别**的哨兵，而不是一个 fmt.Errorf 出来的字符串。
var ErrCubeSourceNotConfigured = errors.New("该门店尚未配置 cube 数据源")

// ErrCubeSourceDisabled 表示绑定了但被禁用。
var ErrCubeSourceDisabled = errors.New("该门店的 cube 数据源已被禁用")

// CubeSourceChecker 查询某门店绑定的 cube 数据源是否可用。
//
// # 窄接口（而不是直接持 *cube-router/service.Service）
//
// 它依赖 *gorm.DB，而本仓没有 DB harness —— 具体类型会让
// "前置校验拦住了开启"这条最要紧的分支**结构上测不到**。
//
// ⚠️ 实现方**必须**把 cube-router 的 model.ErrNotFound / model.ErrDisabled
// 翻译成本包的 ErrCubeSourceNotConfigured / ErrCubeSourceDisabled。
// 理由：handler 要判"给前端弹配置链接"还是"弹重试"，而让 Control 去
// errors.Is 另一个包的哨兵，会把本包绑死在 cube-router 的表结构上。
// 翻译点收在 NewCubeSourceChecker 这一处。
type CubeSourceChecker interface {
	ResolveCubeSource(ctx context.Context, branchID string) (string, bool, error)
}

// CubeSourceResolver 是 cube-router 服务在本包视角下的形状（避免 import 它）。
type CubeSourceResolver interface {
	ResolveCubeSource(ctx context.Context, branchID string) (string, bool, error)
}

// NewCubeSourceChecker 把 cube-router 的解析器包成本包认识的哨兵。
func NewCubeSourceChecker(r CubeSourceResolver) CubeSourceChecker {
	if r == nil {
		return nil
	}
	return cubeSourceCheckerAdapter{r: r}
}

// cubeSourceCheckerAdapter 是**唯一**同时认识两套错误词汇的地方。
type cubeSourceCheckerAdapter struct {
	r CubeSourceResolver
}

func (a cubeSourceCheckerAdapter) ResolveCubeSource(ctx context.Context, branchID string) (string, bool, error) {
	name, enabled, err := a.r.ResolveCubeSource(ctx, branchID)
	if err != nil {
		return "", false, translateCubeSourceErr(err)
	}
	return name, enabled, nil
}

// ServiceAccountClient 调 auth 的服务账号端点（**透传用户 bearer**）。
//
// # 为什么透传用户 token 而不是服务自己的身份
//
// 用户要求"用当前执行用户的 jwt token 创建服务账号"。这不只是流程要求，
// 也是**安全要求**：只有这样，auth 侧判的才是"这个真人有没有
// service-account:create / grant"，而不是"某台机器自称是管理员"。
//
// 若 sales-agg 用自己的凭据去建号，那么任何能调到 sales-agg 这个端点的人
// 都能建出一台有 cube:read 的机器身份 —— 权限模型当场失效。
type ServiceAccountClient interface {
	// CreateScoped 代建服务账号。userBearer 含 "Bearer " 前缀。
	CreateScoped(ctx context.Context, userBearer, clientID, displayName, branchID string, permissions []string) (secret string, err error)
	// ResetSecret 轮换密钥，返回新明文（用于建号成功但落库失败的自愈）。
	ResetSecret(ctx context.Context, userBearer, clientID string) (secret string, err error)
	// SetDisabled 禁用/启用服务账号（关停走禁用而非删除）。
	SetDisabled(ctx context.Context, userBearer, clientID string, disabled bool) error
}

// CollectorPermissions 是采集所需授予服务账号的权限点。
//
// ⚠️ 只读不写。采集只做 cube 查询，不碰任何写操作 ——
// 给一台自动运行的机器身份写权限，等于让它能在无人值守时改数据。
var CollectorPermissions = []string{"cube:read"}

// JobRegistrar 注册/删除某门店的采集 job。
type JobRegistrar interface {
	RegisterBranchJobs(ctx context.Context, branchID string) error
	DeleteBranchJobs(ctx context.Context, branchID string) error
}

// credentialStore 是凭据存储的窄接口。
//
// # 为什么不用具体的 *CredentialStore
//
// 它依赖 *gorm.DB，而本仓没有 DB harness（sqlite 跑不了这套 PG 专有 schema）。
// 若 Control 直接持具体类型，下面这些分支就**结构上测不到**：
//
//	"前置校验没过 ⇒ 绝不建号" / "重复开启 ⇒ 什么都不做" /
//	"auth 拒绝 ⇒ 本地状态不能改" / "关停保留凭据行"
//
// 而它们正是"用户点了开启/关闭之后系统处于什么状态"的全部语义。
// 判据：**任何"决定状态"的分支，都要能被不依赖 DB 的用例钉住。**
type credentialStore interface {
	Get(ctx context.Context, branchID string) (*Credential, error)
	Put(ctx context.Context, row *Credential) error
	SetStatus(ctx context.Context, branchID string, st CredentialStatus) error
}

// Control 是开启/关停编排器。
type Control struct {
	cubes    CubeSourceChecker
	accounts ServiceAccountClient
	store    credentialStore
	cipher   *Cipher
	jobs     JobRegistrar
	now      func() time.Time
}

// NewControl 构造。now 为 nil 时用 time.Now。
func NewControl(cubes CubeSourceChecker, accounts ServiceAccountClient, store credentialStore, c *Cipher, jobs JobRegistrar, now func() time.Time) *Control {
	if now == nil {
		now = time.Now
	}
	return &Control{cubes: cubes, accounts: accounts, store: store, cipher: c, jobs: jobs, now: now}
}

// ControlState 是某门店的采集开关状态（给前端展示用）。
type ControlState struct {
	BranchID string           `json:"branch_id"`
	ClientID string           `json:"client_id"`
	Status   CredentialStatus `json:"status"`
	// CubeSourceConfigured / CubeSourceEnabled 让前端**不用**先发一次
	// "能不能开"的预检请求才知道该不该显示开关。
	CubeSourceConfigured bool `json:"cube_source_configured"`
	CubeSourceEnabled    bool `json:"cube_source_enabled"`
}

// State 查某门店当前状态。
func (c *Control) State(ctx context.Context, branchID string) (*ControlState, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("门店 id 不能为空")
	}
	clientID, err := ClientIDFor(branchID)
	if err != nil {
		return nil, err
	}
	st := &ControlState{BranchID: branchID, ClientID: clientID, Status: StatusNotConfigured}
	if c.cubes != nil {
		if _, enabled, err := c.cubes.ResolveCubeSource(ctx, branchID); err == nil {
			st.CubeSourceConfigured, st.CubeSourceEnabled = true, enabled
		}
	}
	if c.store != nil {
		row, err := c.store.Get(ctx, branchID)
		if err != nil {
			return nil, err
		}
		if row != nil {
			st.Status = CredentialStatus(row.Status)
			st.ClientID = row.ClientID
		}
	}
	return st, nil
}

// Enable 开启某门店的每日销售数据采集。
//
// # 幂等
//
//	已是 enabled ⇒ 直接返回当前状态，不重复建号、不重复注册。
//	（前端提供重试按钮，同一请求会到达多次。）
//
// # 重新启用**不重新分发密钥**
//
//	凭据行还在（关停=禁用≠删除），所以第二次开启只是把状态翻回 enabled
//	并通知 auth 取消禁用。明文不用再要一次 —— 它从一开始就没存在别处过。
func (c *Control) Enable(ctx context.Context, userBearer, branchID, createdBy string) (*ControlState, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("门店 id 不能为空")
	}
	clientID, err := ClientIDFor(branchID)
	if err != nil {
		return nil, err
	}
	if c.store == nil || c.cipher == nil {
		return nil, fmt.Errorf("采集凭据未装配（本服务没有主密钥，无法保存服务账号密钥）")
	}

	// ① 前置：cube 数据源必须已绑定且启用。
	//
	// ⚠️ 这一步不能省，也不能放到"开启之后再说"：
	//   没绑定数据源就开启成功，用户会以为"开了但就是没数据"，
	//   而真因（数据源没配）在整条链路上不会留下任何痕迹。
	if err := c.requireCubeSource(ctx, branchID); err != nil {
		return nil, err
	}

	// ② 已是开启 ⇒ 幂等返回。
	if row, err := c.store.Get(ctx, branchID); err != nil {
		return nil, err
	} else if row != nil && CredentialStatus(row.Status) == StatusEnabled {
		return c.State(ctx, branchID)
	}

	// ③ 重新启用：取消 auth 侧的禁用。密钥**不轮换**（明文我们还有）。
	if row, err := c.store.Get(ctx, branchID); err == nil && row != nil &&
		CredentialStatus(row.Status) == StatusDisabled {
		if err := c.accounts.SetDisabled(ctx, userBearer, clientID, false); err != nil {
			return nil, fmt.Errorf("启用失败：取消 auth 侧禁用时出错: %w", err)
		}
		if err := c.store.Put(ctx, row); err != nil {
			return nil, err
		}
		if c.jobs != nil {
			if err := c.jobs.RegisterBranchJobs(ctx, branchID); err != nil {
				return nil, fmt.Errorf("凭据已启用，但定时任务注册失败: %w", err)
			}
		}
		return c.State(ctx, branchID)
	}

	// ④ 全新开启：建号 → 加密落库 → 注册 job。
	secret, err := c.createAccountWithRecovery(ctx, userBearer, clientID, branchID)
	if err != nil {
		return nil, err
	}
	cipherText, err := c.cipher.Seal(secret, branchID)
	if err != nil {
		return nil, fmt.Errorf("加密服务账号密钥失败: %w", err)
	}
	now := c.now()
	if err := c.store.Put(ctx, &Credential{
		BranchID:     branchID,
		ClientID:     clientID,
		SecretCipher: cipherText,
		Status:       string(StatusEnabled),
		CreatedBy:    createdBy,
		EnabledAt:    &now,
	}); err != nil {
		// ⚠️ 这一刻账号已经存在、而我们手里这枚明文即将随请求结束消失。
		//   不做任何事的话，门店永远开不了采集（重建撞 409）。
		//   → 明确提示"请重试"，重试会走轮换密钥那条自愈路径。
		return nil, fmt.Errorf(
			"服务账号已创建，但保存密钥失败（请**重试**一次，重试会自动轮换密钥）：%w", err)
	}

	if c.jobs != nil {
		if err := c.jobs.RegisterBranchJobs(ctx, branchID); err != nil {
			return nil, fmt.Errorf("凭据已保存，但定时任务注册失败（请重试）: %w", err)
		}
	}
	return c.State(ctx, branchID)
}

// createAccountWithRecovery 建号；遇到 409（上次建号成功但没落库）时转去轮换密钥。
func (c *Control) createAccountWithRecovery(ctx context.Context, userBearer, clientID, branchID string) (string, error) {
	if c.accounts == nil {
		return "", fmt.Errorf("auth 服务账号客户端未装配")
	}
	secret, err := c.accounts.CreateScoped(ctx, userBearer, clientID,
		"门店 "+branchID+" 每日销售采集", branchID, CollectorPermissions)
	if err == nil {
		return secret, nil
	}
	if !errors.Is(err, ErrServiceAccountExists) {
		return "", fmt.Errorf("创建服务账号失败: %w", err)
	}
	// 409：账号已存在 ⇒ 上次建号成功但落库失败。
	// 轮换密钥换一枚新明文，比"删了重建"好 —— 后者需要 admin 角色，
	// 而这条路径要能被**普通店长**在自己门店上自愈。
	secret, rerr := c.accounts.ResetSecret(ctx, userBearer, clientID)
	if rerr != nil {
		return "", fmt.Errorf(
			"服务账号已存在但密钥不可得，轮换也失败（请找管理员协助）: %w", rerr)
	}
	return secret, nil
}

// Disable 关停某门店的采集（**禁用，不删除**）。
//
// # 为什么禁用而不是删除
//
//	auth 侧删账号会连带清 user_branches / user_branch_permissions，
//	而明文 secret 只在创建时返回一次（库里只有 bcrypt，回显不出来）
//	⇒ 删除后重新开启必须重新分发密钥。
//	禁用是纯状态位翻转，重新开启是无损往返。
//
// # 顺序：先禁 auth，再改本地，最后删 job
//
//	反过来（先删 job）的话，用户点了关闭之后仍可能有一次正在跑的采集
//	成功落库 —— 而他看到的是"已经关了"，于是会以为数据停止更新是出错了。
func (c *Control) Disable(ctx context.Context, userBearer, branchID string) (*ControlState, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("门店 id 不能为空")
	}
	clientID, err := ClientIDFor(branchID)
	if err != nil {
		return nil, err
	}
	if c.store == nil {
		return nil, fmt.Errorf("采集凭据未装配")
	}

	row, err := c.store.Get(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		// 本来就没开过：幂等成功。
		// ⚠️ 不能报错 —— 前端有重试按钮，重复点关闭不该变成错误。
		return c.State(ctx, branchID)
	}
	if CredentialStatus(row.Status) == StatusDisabled {
		return c.State(ctx, branchID)
	}

	if c.accounts != nil {
		if err := c.accounts.SetDisabled(ctx, userBearer, clientID, true); err != nil {
			return nil, fmt.Errorf("关停失败：禁用 auth 侧服务账号时出错: %w", err)
		}
	}
	if err := c.store.SetStatus(ctx, branchID, StatusDisabled); err != nil {
		return nil, err
	}
	if c.jobs != nil {
		if err := c.jobs.DeleteBranchJobs(ctx, branchID); err != nil {
			return nil, fmt.Errorf("凭据已关停，但定时任务删除失败（请重试）: %w", err)
		}
	}
	return c.State(ctx, branchID)
}

// requireCubeSource 校验门店的 cube 数据源可用，返回本包的哨兵错误。
func (c *Control) requireCubeSource(ctx context.Context, branchID string) error {
	if c.cubes == nil {
		return fmt.Errorf("cube 数据源查询未装配，无法校验是否可以开启采集")
	}
	_, enabled, err := c.cubes.ResolveCubeSource(ctx, branchID)
	if err != nil {
		return fmt.Errorf("%w（门店 %s）。%s", err, branchID, cubeSourceHint(err))
	}
	if !enabled {
		return fmt.Errorf("%w（门店 %s）", ErrCubeSourceDisabled, branchID)
	}
	return nil
}

// cubeSourceHint 按哨兵给出不同的下一步提示。
//
// ⚠️ 两种情况给的提示**必须不同**：让"数据源被禁用"的用户去点「配置数据源」
//   是纯粹的误导 —— 他配了也没用，那要管理员启用。
func cubeSourceHint(err error) string {
	if errors.Is(err, ErrCubeSourceDisabled) {
		return "请在「数据源」里启用该门店的 cube 数据源后重试（若找不到该门店的映射，请联系管理员）。"
	}
	return "请先在「数据源」里为该门店绑定 cube 数据源后重试。"
}
