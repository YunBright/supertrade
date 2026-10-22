// Package cubesource —— 「这个门店配了哪个数据源」的**统一**解析。
//
// # 它是什么
//
// supertrade 是 ERP，也是别的 ERP 的代理：某个门店一旦在 cube-router 里
// 配了数据源（`cube-<family>-<version>-<sub>`），**该门店的业务数据就在外部系统里**，
// 本域 PG 里没有它的行 ⇒ 读端点必须走那个 cube 实例；没配 ⇒ 走本域 PG。
//
// # 为什么要有这个包（而不是每个服务各问一次）
//
// 2026-10-21 审计发现：同一个问题「这个门店配了数据源吗」，三个服务给了三个答案：
//
//	catalog   → 「只有 cube」（无源门店直接 404，明明本地有 27565 个商品）
//	pricing   → 「本地有没有该门店的数据」（有源门店命中**每日快照** ⇒ 用旧价）
//	inventory → 「只有本地」（有源门店的库存是空的）
//
// ⇒ 判据必须是**一个**，否则同一个门店在不同服务里表现不一致，
// 而排查时每个服务看起来都"有道理"。
//
// # 判据的三个状态（必须严格区分）
//
//	Configured=false → 没配数据源（**正常状态**，自营店就是这样）⇒ 读本地。
//	Configured=true, Enabled=false → 配了但被管理员关停 ⇒ **503**，不是"没配"
//	  （回落成"没配"会让用户看到一份空列表，而真相是"这个门店现在查不了"）。
//	解析失败（问不到）→ **503**；读端点**不能**回落成本地
//	  （回落 = 把一次链路故障显示成「该门店还没有会员档案」）。
//
// # 缓存与 stale-on-error
//
// 解析要走一次 dapr 往返 ⇒ 加 60s TTL 缓存（与 cube-router 自己的缓存同量级）。
//
// ⚠️ **stale-on-error**：问不到路由时**沿用上一次的结果**。
// 路由配置很少变 ⇒ 一次网络抖动不该让整个门店的服务挂掉。
// 但**从未成功解析过** ⇒ 只能返错（没有"上一次"可沿用）。
package cubesource

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/YunBright/supertrade/internal/cubeclient"
)

// DefaultTTL 是解析结果的缓存时长。
//
// ⚠️ 与 cube-router 自己的 branch_cube_sources 缓存同量级(60s)——
// 路由改了以后最多 1 分钟生效，对"改个数据源"的运维动作来说足够。
const DefaultTTL = 60 * time.Second

// Source 是「门店 → 数据源」的解析结果。
type Source struct {
	// BranchID 是被解析的门店。
	BranchID string
	// Configured = 该门店在 branch_cube_sources 里有一行。
	Configured bool
	// Enabled = 那一行是否启用。
	//
	// ⚠️ **false 与"没配"是两件事**：
	//   - 没配（Configured=false）⇒ 读本域 PG；
	//   - 配了但被关停 ⇒ 读不了 ⇒ 应当报 503，而不是悄悄回落本地
	//     （那会让用户看到一份空列表）。
	Enabled bool
	// CubeSourceName 是数据源对应的 dapr app-id（没配时为空）。
	CubeSourceName string
}

// Resolver 解析门店的数据源。
//
// 实现见 [CachedResolver]；测试替身见各服务的 *_test.go。
type Resolver interface {
	Resolve(ctx context.Context, branchID string) (Source, error)
}

// Client 是解析所需的最小能力。
//
// ⚠️ cubeclient.DaprCubeClient 满足它（方法名一致）⇒ 装配时直接传同一个对象，
// 不需要再包一层适配器。
type Client interface {
	ResolveBranchSource(ctx context.Context, branchID string) (cubeclient.BranchSource, error)
}

// ---- 读路径的三态判据（2026-10-22 新增） ----
//
// ErrSourceUnavailable / ErrSourceDisabled 是**判据本身的两个失败结局**,
// 与各域 model 包里的 ErrCubeSourceUnavailable / ErrCubeSourceDisabled
// 同义(各域 model 的那两个是为了让 handler 映射到本域的错误码)。
//
// ⚠️ 为什么不把这两个 sentinel 放在各域 model 里、让 Decide 直接返回它们?
//
//	因为 Decide 住在 cubesource 包里,它不能 import 各域的 model 包
//	(那会变成 cubesource → catalog / pricing / transaction … 的反向依赖)。
//	⇒ Decide 返回 cubesource 的 sentinel,各域 service 再包一层成本域 sentinel。
//	这层包装只做一次,不会漂移。
var (
	// ErrSourceUnavailable:问不到"这个门店配了哪个数据源"(链路故障)⇒ 503。
	//
	// ⚠️ 与"没配数据源"严格区分:没配是**正常状态**(自营店),
	// 问不到是**链路故障**。
	// 回落成本地 = 把一次故障显示成"该门店没有数据"——
	// 那是 2026-10-21 生产事故的形状,禁止回落。
	ErrSourceUnavailable = errors.New("cubesource: 数据源路由暂不可用")
	// ErrSourceDisabled:配了数据源但被管理员停用 ⇒ 503。
	//
	// ⚠️ **不等于"没配"**:回落成"没配"会让用户看到一份空列表,
	// 而真相是"这个门店现在查不了"。
	ErrSourceDisabled = errors.New("cubesource: 该门店的数据源已停用")
)

// Decide 判定该门店的**读**端点该走哪条路。
//
// 返回 (useCube, err):
//
//   - useCube=true  ⇒ 走 cube(该门店配了数据源)
//   - useCube=false ⇒ 走本域 PG(没配数据源)
//   - err != nil    ⇒ 问不到路由 / 数据源被停用 ⇒ 上抛(503)
//
// # ⚠️ 三态必须严格区分,禁止回落
//
//	Configured=false              → 没配(正常)⇒ 本地。
//	Configured=true, Enabled=false → 配了但被关停 ⇒ **503**,不回落本地。
//	解析失败(问不到)               → **503**,不回落本地。
//
// 回落本地 = 把一次链路故障显示成"这个门店没有数据"。
// 2026-10-21 生产事故就是这么发生的:catalog 的读端点只走 cube,
// 自营店(门店 10)直接 404,而它本地明明有 27565 个商品。
//
// # 权限类错误**原样上抛**
//
// ErrCubeForbidden / ErrCubeAuthRejected ⇒ 调用者缺 cube:read ⇒ 403/401。
// 与"问不到路由"(503)混在一起会让"没权限"显示成"暂不可用",
// 用户会去查数据源健康度(而真正的原因是权限)。
//
// # resolver == nil ⇒ 走本地
//
// 解析器未装配(纯本地部署 / 单测)⇒ 没有"配没配"这个信息,
// 按"没配"处理。生产**必须**装配
// (cmd/<app>/main.go 里装 cubesource.NewCachedResolver)。
func Decide(ctx context.Context, resolver Resolver, branchID string) (bool, error) {
	branchID = strings.TrimSpace(branchID)
	if resolver == nil {
		return false, nil
	}
	src, err := resolver.Resolve(ctx, branchID)
	if err != nil {
		if errors.Is(err, cubeclient.ErrCubeForbidden) || errors.Is(err, cubeclient.ErrCubeAuthRejected) {
			return false, err
		}
		return false, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	if src.Configured && !src.Enabled {
		return false, fmt.Errorf("%w: 门店 %s 的数据源已停用", ErrSourceDisabled, branchID)
	}
	if src.Configured {
		return true, nil
	}
	return false, nil
}

// CachedResolver 是带缓存的 Resolver 实现。
type CachedResolver struct {
	client Client
	ttl    time.Duration
	now    func() time.Time
	logger *slog.Logger

	mu    sync.RWMutex
	cache map[string]cacheEntry
}

// cacheEntry 是缓存里的一项。
type cacheEntry struct {
	src       Source
	fetchedAt time.Time
}

// NewCachedResolver 构造 CachedResolver。
//
// ttl <= 0 ⇒ 用 [DefaultTTL]。logger == nil ⇒ 用 slog.Default()。
func NewCachedResolver(client Client, ttl time.Duration, logger *slog.Logger) *CachedResolver {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &CachedResolver{
		client: client,
		ttl:    ttl,
		now:    func() time.Time { return time.Now() },
		logger: logger,
		cache:  make(map[string]cacheEntry),
	}
}

// SetClock 注入时钟（测试用）。
func (r *CachedResolver) SetClock(fn func() time.Time) {
	if fn != nil {
		r.now = fn
	}
}

// Resolve 解析门店的数据源。
//
// ⚠️ 错误语义（调用方据此分流，**不要**在调用方再包一层"统一成不可用"）：
//
//	ErrCubeForbidden / ErrCubeAuthRejected → 调用者缺权限（403/401），**原样上抛**。
//	其它错误 → 问不到路由（503）。
//
// ⚠️ 问不到时若**有**历史结果 ⇒ 沿用它（stale-on-error），并记一行日志。
func (r *CachedResolver) Resolve(ctx context.Context, branchID string) (Source, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return Source{}, fmt.Errorf("cubesource: branch_id 必填")
	}
	if r == nil || r.client == nil {
		return Source{}, fmt.Errorf("cubesource: 未装配数据源解析器")
	}

	r.mu.RLock()
	entry, ok := r.cache[branchID]
	r.mu.RUnlock()
	if ok && r.now().Sub(entry.fetchedAt) < r.ttl {
		return entry.src, nil
	}

	raw, err := r.client.ResolveBranchSource(ctx, branchID)
	if err != nil {
		// ⚠️ 权限类错误**原样上抛**：调用方要区分"没权限"与"问不到"。
		if errors.Is(err, cubeclient.ErrCubeForbidden) || errors.Is(err, cubeclient.ErrCubeAuthRejected) {
			return Source{}, err
		}
		if ok {
			// 沿用上一次的结果（路由配置很少变 ⇒ 一次抖动不该让门店的服务挂掉）。
			r.logger.Warn("cubesource: 问不到数据源,沿用上次结果",
				"branch_id", branchID, "err", err,
				"stale_seconds", int(r.now().Sub(entry.fetchedAt).Seconds()))
			return entry.src, nil
		}
		return Source{}, err
	}

	src := Source{
		BranchID:       raw.BranchID,
		Configured:     raw.Configured,
		Enabled:        raw.Enabled,
		CubeSourceName: raw.CubeSourceName,
	}
	if src.BranchID == "" {
		src.BranchID = branchID
	}
	r.mu.Lock()
	r.cache[branchID] = cacheEntry{src: src, fetchedAt: r.now()}
	r.mu.Unlock()
	return src, nil
}

// Invalidate 主动失效某门店的缓存（配置变更后立即生效）。
//
// ⚠️ cube-router 的 admin CRUD 本身会让它自己的缓存失效（60s 内生效）；
// 本地缓存是第二层 ⇒ 需要"立即生效"时调它。
func (r *CachedResolver) Invalidate(branchID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, strings.TrimSpace(branchID))
}
