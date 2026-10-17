package weather

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	dapr "github.com/dapr/go-sdk/client"
)

// DefaultStateStoreName 是天气配额用的 dapr 组件名。
//
// ⚠️ 它必须与 dapr/components/weather-quota.yaml 的 `metadata.name` 完全一致，
//
//	而**不是** defaultStateStore ——
//	后者会指向别的组件（authkey / cube-statestore / wechat-bot 的），
//	于是配额被写进语义不相干的 key 空间，排查时完全看不出它在哪。
const DefaultStateStoreName = "weather-quota"

// ErrETagMismatch 表示并发写冲突（可重试）。
var ErrETagMismatch = errors.New("dapr state: ETag 不匹配")

// StateClient 是 DaprCounterStore 实际用到的 dapr state 方法子集。
//
// # 为什么要收窄成窄接口
//
//	dapr.Client 有几十个方法，测试里造不出一个假实现 ——
//	而 CAS（ETag 乐观锁）是本改造**最核心也最容易写错**的一段，
//	没有测试等于没写。只声明用到的这两个方法后，
//	dapr.Client 依然满足它，而测试只需要一个几十行的假实现。
//
// 导出它是为了让 cmd/sales-insight 能在装配层测「探不通就回退」。
type StateClient interface {
	GetState(ctx context.Context, storeName, key string, meta map[string]string) (*dapr.StateItem, error)
	SaveStateWithETag(ctx context.Context, storeName, key string, data []byte, etag string, meta map[string]string, so ...dapr.StateOption) error
}

// DaprCounterStore 用 dapr state（Redis 支撑）做跨服务共享计数。
//
// # 为什么是**跨服务**而不是本服务私有
//
//	和风按 API key 计费 —— 同一个 key 被 N 个服务用，就是**一份** 1500 的额度。
//	各服务各数各的 ⇒ 实际发出量 N×1500 ⇒ 402，而 402 的表现只是
//	"天气突然全无"，没有任何告警指向"你把配额用光了"。
//
// # ⚠️ 原子性靠 ETag 乐观锁，不是靠进程内 mutex
//
//	进程内锁只对本进程有效 —— 而本存储的意义就是跨进程/跨服务。
//	「读-判断-加一」三步必须在 Redis 侧原子完成，否则两个服务各自读到
//	used=1499 并都判定"还有余量"，实际超发。
//
//	Dapr 的 state API 提供 etag 参数：带 etag 写只在值未被改动时成功。
//
// 于是重试循环能把「读-判断-写」变成原子的 compare-and-swap。
type DaprCounterStore struct {
	// client 是 dapr client（只需 state 子集，见 StateClient）。
	client StateClient
	// store 是组件名。
	store string
	// maxRetries 是 CAS 重试次数。
	//
	// ⚠️ 必须有上限：无限重试会在高并发下把 6s 超时预算耗尽，
	// 症状变成"日报偶发变慢"。冲突率极低（实测日调用量是个位数），
	// 3 次足够。
	maxRetries int
}

// DaprCounterStoreOptions 是构造参数。
type DaprCounterStoreOptions struct {
	// Client 是 dapr client。
	//
	// 声明为窄接口 StateClient 而非 dapr.Client：
	// 生产传 dapr.Client（它满足），测试传几十行的假实现。
	Client StateClient
	// Store 是组件名；留空用 DefaultStateStoreName。
	Store string
	// MaxRetries 是 CAS 重试次数；<=0 用 3。
	MaxRetries int
}

// NewDaprCounterStore 构造。
//
// ⚠️ client 为 nil 返回 nil —— 调用方据此回退到本地后端，
// 而不是启动失败（天气是增强项，不该拖垮整个服务）。
func NewDaprCounterStore(opts DaprCounterStoreOptions) *DaprCounterStore {
	if opts.Client == nil {
		return nil
	}
	store := strings.TrimSpace(opts.Store)
	if store == "" {
		store = DefaultStateStoreName
	}
	retries := opts.MaxRetries
	if retries <= 0 {
		retries = 3
	}
	return &DaprCounterStore{
		client:     opts.Client,
		store:      store,
		maxRetries: retries,
	}
}

// ProbeKey 是连通性探针用的 key。
//
// ⚠️ 它**不含日期**且**永不写入** —— 探针必须是只读的，
// 否则"装配时探测一次"会白白吃掉一天 1 次配额（更糟的是会
// 在 key 空间里留下一个与计数混在一起的键）。
const ProbeKey = "weather/quota/__probe"

// Probe 探一次组件是否真的可用。
//
// # ⚠️ 为什么必须有它
//
//	NewDaprCounterStore 只检查 client 是否为 nil ——
//	而"组件 yaml 没被加载"这件事**只有真正发一次请求才会暴露**。
//	那种形态的后果不是报错，而是：
//
//	每天每份报告的天气都取不到 → facts 里留一句
//	「天气查询失败：dapr GetState(weather-quota)」
//
//	→ 运维看到的是"天气功能坏了"，而真相是配额守卫把天气**全日锁死**了。
//	（缺 dapr 组件 = 静默降级，不是报错 —— 与 authkey 那次同一个坑。）
//
//	所以在装配阶段就探一次：探不通就回退到本地后端 + 显式告警，
//	而不是把"零天气"当作正常。
//
// 注意它只读不写，所以不消耗配额，也不会与当日计数混淆。
func (s *DaprCounterStore) Probe(ctx context.Context) error {
	if s == nil || s.client == nil {
		return errors.New("dapr client 为空")
	}
	if _, err := s.client.GetState(ctx, s.store, ProbeKey, nil); err != nil {
		return fmt.Errorf("dapr state 组件 %q 不可用: %w", s.store, err)
	}
	return nil
}

// Get 取计数。
func (s *DaprCounterStore) Get(ctx context.Context, key string) (quotaState, error) {
	item, err := s.client.GetState(ctx, s.store, key, nil)
	if err != nil {
		return quotaState{}, fmt.Errorf("dapr GetState(%s): %w", s.store, err)
	}
	if item == nil || item.Value == nil || len(item.Value) == 0 {
		// 不存在 = 今天还没用过。**不是**错误。
		return quotaState{}, nil
	}
	return decodeState(item.Value)
}

// Incr 原子加一并返回新值。
//
// # CAS 循环
//
//  1. 读当前值 + ETag；
//
//  2. 若 used >= limit ⇒ 直接返回超限（**不写**，避免 used 无限增长）；
//
//  3. used++ 并带 ETag 写；ETag 不匹配说明别人改过 ⇒ 重读重试。
//
//     这是跨服务唯一能做到原子的方式（dapr 没有暴露 INCR 到 state API）。
func (s *DaprCounterStore) Incr(ctx context.Context, key string, limit int) (int, bool, error) {
	var lastErr error
	for attempt := 0; attempt < s.maxRetries; attempt++ {
		item, err := s.client.GetState(ctx, s.store, key, nil)
		if err != nil {
			return 0, false, fmt.Errorf("dapr GetState(%s): %w", s.store, err)
		}

		used := 0
		// ⚠️ key 不存在时 ETag 为空 —— 此时**不能**带 ETag 写，
		//   因为并发下两个服务都会"发现不存在"然后各自写，
		//   后写的覆盖先写的（丢计数）。那种丢失是静默的。
		//   → 所以无 ETag 时直接写；有 ETag 时走 CAS。
		etag := ""
		if item != nil && len(item.Value) > 0 {
			st, decErr := decodeState(item.Value)
			if decErr != nil {
				// ⚠️ 存量数据损坏时**不能**当成 0 ——
				//   那等于把当日计数清零，是配额保护最隐蔽的失效方式。
				return 0, false, decErr
			}
			used = st.Used
			etag = item.Etag
		}

		if used >= limit {
			// 越过上限后不再增长（见 MemoryCounterStore 的同类注释）。
			return used, true, nil
		}
		used++

		b, err := json.Marshal(quotaState{Used: used})
		if err != nil {
			return used, false, err
		}
		// ⚠️ 必须走 SaveStateWithETag：普通 SaveState 传 etag="" 等于
		//   **无条件覆盖**，那样 ETag 就白读了，并发下必然丢计数。
		if err := s.client.SaveStateWithETag(ctx, s.store, key, b, etag, nil); err != nil {
			// ⚠️ ETag 冲突才重试；其它错误（Redis 不可达等）直接返回 ——
			//   重试同一类错误只会拖长超时预算。
			if isETagConflict(err) {
				lastErr = err
				continue
			}
			return used, false, fmt.Errorf("dapr SaveState(%s): %w", s.store, err)
		}
		return used, false, nil
	}
	return 0, false, fmt.Errorf("dapr state CAS 重试 %d 次仍冲突: %w", s.maxRetries, lastErr)
}

// isETagConflict 判断是不是 ETag 冲突。
//
// ⚠️ 不做字符串精确匹配整句错误：dapr SDK / sidecar 版本会改措辞
// （实测见过 "failed saving state: item.ETag mismatch" 与
// "state operation failed" 等不同表述）。
// → 只判"含 ETag 相关关键词"，宁可多重试一次也不要把冲突
// 当成不可恢复错误。
func isETagConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// ⚠️ 只判"含 etag"（ToLower 后大小写已统一，再单独判 "eTag" 是死代码 ——
	//   它会让人误以为两种措辞被分别处理，而其实已被同一条覆盖）。
	return strings.Contains(msg, "etag")
}

// decodeState 解析计数。
// decodeState 解析计数。
//
// # ⚠️ `null` 是这里最容易漏掉的一种"看起来合法"的值
//
//	json.Unmarshal([]byte("null"), &st) **不报错** ——
//	它对结构体是空操作，只是把 st 留在零值，于是 st.Used == 0。
//	而 Used=0 的含义是"今天还没用过"，等于把当日配额**静默清零**，
//	正是本包最要防的那一种失效（计数看起来正常，实则从 0 重新发）。
//
//	现实来源(2026-10-17 线上实测):有人用 dapr HTTP state API 的
//	`POST /v1.0/state/{store}` 数组形式去"读"状态 —— 那个接口其实是
//	**bulk set(写)**,不带 value 就把该 key 写成 `null` 并顶高 version。
//	读必须用 `GET /v1.0/state/{store}/{key}`。
//
//	→ 所以这里显式把 null / 空输入判为损坏，走**保守方向**：
//	报错 ⇒ AllowContext 拒绝 ⇒ 天气降级但配额不会被悄悄放宽。
func decodeState(b []byte) (quotaState, error) {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return quotaState{}, fmt.Errorf("计数数据为空值(null/空): %w", ErrCorruptedState)
	}
	var st quotaState
	if err := json.Unmarshal(trimmed, &st); err != nil {
		return quotaState{}, fmt.Errorf("计数数据损坏: %w", ErrCorruptedState)
	}
	if st.Used < 0 {
		// 负数一定是脏数据（不该由本包写出）。
		return quotaState{}, fmt.Errorf("计数数据非法: used=%d: %w", st.Used, ErrCorruptedState)
	}
	return st, nil
}

// ErrCorruptedState 标记"计数不可信"。用哨兵错误而不是只靠文案，
// 是为了让调用方能 errors.Is 判定，且测试不必去匹配中文。
var ErrCorruptedState = errors.New("计数不可信")
