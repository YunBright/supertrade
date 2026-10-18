// identity.go — 把「门店 → 可用的 bearer」串起来。
//
// 采集端点每被触发一次，就要为该门店现换一个 bearer 并塞进 ctx
// （cubeclient.WithBearer 会自动带给 cube）。
//
// # ⚠️ 为什么"缺凭据"必须在这里**拦下来**，而不是让 cube 去返 401
//
// 不拦的话症状是：
//
//	采集 503 → 日志里 cube 报 Unauthorized → 排查一路走到 cube / 网关 / token 有效期，
//	而真因是"这家门店压根没开采集"或"开了但凭据没落库"。
//
// 这正是本轮改造要消灭的那类症状（远大于真因的排查距离）。
// → 缺凭据时给一条**指向真因**的错误，并区分三种情况（见下方三分支）。
package collect

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/YunBright/supertrade/internal/cubeclient"
)

// ErrCollectNotEnabled 表示该门店未开启采集。
var ErrCollectNotEnabled = errors.New("该门店未开启每日销售数据采集")

// ErrCollectDisabled 表示门店采集被用户关停（凭据在，但状态是 disabled）。
var ErrCollectDisabled = errors.New("该门店的采集已被关停")

// BearerResolver 为某门店解析出一枚可用 bearer。
type BearerResolver interface {
	// Bearer 返回 access_token（不含 "Bearer " 前缀）。
	Bearer(ctx context.Context, branchID string) (string, error)
}

// credentialGetter 是"按门店取一行凭据"的最小能力面。
//
// # 为什么要抽成接口而不是直接用 *CredentialStore
//
// *CredentialStore 依赖 *gorm.DB，本仓没有 DB harness（sqlite 也跑不了
// 这套 PG 专有 schema）。若 Bearer 的字段是具体类型，下面这三条分支
// —— "没开过" / "被关停" / "状态无法识别" —— 就是**结构上测不到**的：
// 能编译、能跑 happy path，改错了没有任何用例会红。
//
// 而这三条正是本轮最要紧的（它们决定前端把用户引向"去开启"还是"找运维"）。
// 判据：**任何"决定用户下一步动作"的分支，都要能被不依赖 DB 的用例钉住。**
type credentialGetter interface {
	Get(ctx context.Context, branchID string) (*Credential, error)
}

// secretCipher 是"从凭据行解出明文 secret"的最小能力面（理由同上）。
type secretCipher interface {
	Secret(row *Credential) (string, error)
}

// StoreCipherIssuer 是 BearerResolver 的生产实现。
type StoreCipherIssuer struct {
	store  credentialGetter
	cipher secretCipher
	issuer TokenIssuer
}

// NewStoreCipherIssuer 构造。任一依赖缺失时构造仍成功，
// 但 Bearer 会返回**明确指出缺哪一件**的错误 —— 装配期就该炸的问题，
// 绝不能拖到凌晨 22:45 采集时才以"cube 401"的形式出现。
func NewStoreCipherIssuer(store *CredentialStore, c *Cipher, issuer TokenIssuer) *StoreCipherIssuer {
	return &StoreCipherIssuer{store: store, cipher: c, issuer: issuer}
}

var _ BearerResolver = (*StoreCipherIssuer)(nil)

// Bearer 实现 BearerResolver。
//
// # ⚠️ 三种"拿不到凭据"必须给**三条不同的错误**
//
//	没有凭据行       → 没开过采集（用户该去开）
//	状态是 disabled  → 用户主动关停过（不该去重开，该提示已关停）
//	client_id 对不上 → 配置被改坏/串店（该找运维，不是找用户）
//
// 混成一句"采集凭据不可用"的话，前端 alert 会把三种情况都指向
// "去开启采集"，而第三种情况用户开了也还是坏 —— 死循环。
func (s *StoreCipherIssuer) Bearer(ctx context.Context, branchID string) (string, error) {
	if s == nil || s.store == nil || s.cipher == nil || s.issuer == nil {
		return "", fmt.Errorf("采集凭据：解析器未完整装配（store/cipher/issuer 缺一）")
	}
	branchID = strings.TrimSpace(branchID)

	row, err := s.store.Get(ctx, branchID)
	if err != nil {
		return "", fmt.Errorf("采集凭据：读取门店 %s 的凭据失败: %w", branchID, err)
	}
	if row == nil {
		return "", fmt.Errorf("%w（门店 %s）。请先在「每日销售数据」里开启采集", ErrCollectNotEnabled, branchID)
	}
	switch CredentialStatus(row.Status) {
	case StatusDisabled:
		return "", fmt.Errorf("%w（门店 %s）", ErrCollectDisabled, branchID)
	case StatusNotConfigured:
		return "", fmt.Errorf("%w（门店 %s，状态尚未配置）", ErrCollectNotEnabled, branchID)
	case StatusEnabled:
		// 继续。
	default:
		// ⚠️ 未知状态**拒绝**而不是当 enabled：
		//   放行意味着一个拼错的状态串（比如大小写不同）会静默开始采集，
		//   而用户以为自己是关着的 —— 关停采集的全部意义就是不再采。
		return "", fmt.Errorf("采集凭据：门店 %s 的凭据状态无法识别：%q", branchID, row.Status)
	}

	// 一致性闸门：库里的 client_id 必须仍等于当前门店推导值。
	// 不一致说明有人改了门店 id、或数据被手工改过 ——
	// 拿旧 client_id 去换 token 会拿到一台**别的门店**的机器身份。
	want, err := ClientIDFor(row.BranchID)
	if err != nil {
		return "", err
	}
	if row.ClientID != want {
		return "", fmt.Errorf(
			"采集凭据：门店 %s 的 client_id 不一致（库里 %q，按门店应为 %q）—— 请重新开启一次采集以重建凭据",
			row.BranchID, row.ClientID, want)
	}

	secret, err := s.cipher.Secret(row)
	if err != nil {
		return "", err
	}
	return s.issuer.AccessToken(ctx, branchID, row.ClientID, secret)
}

// WithBranchBearer 为某门店换一枚 token 并放进 ctx。
//
// ⚠️ 错误原样返回（不吞、不包装成通用错误）：
//
//	handler 要据此判"是未开启/已关停"还是"是真的出错"，
//	因为前两者不该被统计成"采集失败"—— 前者在报表里应当显示
//	"该门店未开启采集"，而不是一个 503 加一句"去重试"。
//
// ⚠️⚠️ **必须自己补上 "Bearer " 前缀**（2026-10-19 端到端实测抓出来的）：
//
//	cubeclient.WithBearer 的约定是"值 = Authorization 头的完整内容"
//	（它直接塞进 gRPC metadata "authorization"，sidecar 转成 HTTP 头）。
//	本仓其余九个调用方无一例外传 `Header.Get("Authorization")`，
//	即都带前缀；而 [BearerResolver.Bearer] 按契约返回**裸 token**。
//	直接透传 ⇒ 目标 app 收到 `Authorization: eyJ...`（没有 Bearer 前缀）
//	⇒ 验签中间件判"缺少已签 token" → 401 → dapr 转成 gRPC Unauthenticated
//	⇒ 报成 `cube 拒绝调用者凭证`。
//
//	症状极具误导性：它把人一路引向 cube / 网关 / token 有效期 / 权限，
//	而真因（少一个前缀）不在这条链路上的任何一行日志里。
//
//	→ 前缀在**这个边界**上补：解析器保持"返回裸 token"的契约
//	  （换 token 的返回值本就是裸的），由负责放进 HTTP 语义位置的一方补。
func WithBranchBearer(ctx context.Context, r BearerResolver, branchID string) (context.Context, error) {
	if r == nil {
		return ctx, fmt.Errorf("采集凭据：bearer 解析器未装配")
	}
	tok, err := r.Bearer(ctx, strings.TrimSpace(branchID))
	if err != nil {
		return ctx, err
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		// 空 token 不能当成功：那会让 cube 收到**完全没有** Authorization，
		// 症状同样是 Unauthenticated，而真因（解析器给了空串）被吞掉。
		return ctx, fmt.Errorf("采集凭据：门店 %s 的 bearer 解析器返回了空 token", branchID)
	}
	if !strings.HasPrefix(strings.ToLower(tok), "bearer ") {
		tok = "Bearer " + tok
	}
	return cubeclient.WithBearer(ctx, tok), nil
}
