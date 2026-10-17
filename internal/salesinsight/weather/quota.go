package weather

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultQWeatherDailyLimit 是和风天气的**每日调用上限**（免费/基础订阅）。
//
// ⚠️ 它是**配额上限，不是速率上限** —— 和风按"每天 00:00 归零"计。
//
//	超过之后 API 会返回 402/403，且**当天不会自动恢复**：
//	这意味着一次意外的重试风暴（比如 dapr job 反复触发）就能把
//	当天剩余所有报告的天気全部拖死。
//
//	所以本包在**发起请求前**就检查配额，而不是事后统计。
//
// # ⚠️ 这是**整个 supertrade 的额度**，不是某个服务的
//
//	和风按 API key 计费 —— 同一个 key 被 N 个服务用，就是一份 1500。
//	若每个服务各自计数，实际发出量可以是 N×1500，
//	而 402 的表现只是"天气突然全无"，没有任何告警指向"配额用光了"。
//
//	→ 所以 Counter 必须是**跨服务共享**的（见 NewCounter 的存储选择）。
const DefaultQWeatherDailyLimit = 1500

// EnvQWeatherDailyLimit 是覆盖每日上限的环境变量名。
//
// 留空 = 用 DefaultQWeatherDailyLimit。配 0 或负数 = 回落默认值。
const EnvQWeatherDailyLimit = "WEATHER_QWEATHER_DAILY_LIMIT"

// quotaState 是共享的当日计数。
//
// # 为什么按「日期」分 key 而不是存一个字段
//
//	和风按 00:00 归零，而**时区是北京时区**。
//	把日期放进 key 里，跨日就是**换 key** —— 天然归零，
//	不需要任何"发现日期变了就重置"的逻辑（那类逻辑在并发下会漏）。
type quotaState struct {
	// Used 是当日已发起（**不是成功**）的请求数。
	//
	// ⚠️ 记"发起"而不是"成功"：配额按请求计费，
	// 一个超时或 500 的请求同样消耗配额。
	// 记"成功"会让网络故障时的真实消耗被低估 2~3 倍。
	Used int `json:"used"`
}

// CounterStore 是计数器的存储后端。
//
// # 为什么要抽象出这一层
//
//	主实现是 dapr state（跨服务共享，Redis 支撑）；
//	而**降级回退**需要一个进程内的实现 ——
//	Redis 抖动时，天气必须继续可用（它是增强项，不是必需项）。
type CounterStore interface {
	// Get 取当日计数。key 里已含日期，跨日由 key 自然切换。
	Get(ctx context.Context, key string) (quotaState, error)
	// Incr 原子地把计数加一，**并返回加一后的值**。
	//
	// ⚠️ 必须是**原子**的：跨服务场景下"读-判断-加一"三步
	// 若不原子，两个服务可能都读到 used=1499 并各自放行 → 实际超发。
	Incr(ctx context.Context, key string, limit int) (used int, over bool, err error)
}

// Quota 是每日调用配额守卫。
type Quota struct {
	store CounterStore
	// backend 是 store 的可读名（见 backendName）。
	backend string
	limit   int
	// prefix 是 key 前缀（形如 "weather/quota"）。
	//
	// ⚠️ 它必须**全平台一致** —— 与 dapr 组件的 keyPrefix 是两件事：
	//   组件的 keyPrefix 决定 dapr 把 key 拼成什么，
	//   这里的前缀决定"天气配额"这件事在 key 空间的哪个位置。
	//   两个服务用不同前缀 ⇒ 各数各的 ⇒ 上限对不上真实额度。
	prefix string
	// now 注入时钟（测试用）。生产为 time.Now。
	now func() time.Time
	// onAccountErr 记账失败时的回调（用于日志/告警）。
	// nil = 不回调。
	onAccountErr func(error)
}

// QuotaOptions 是构造参数。
type QuotaOptions struct {
	// Store 是后端。nil = 用内存版（仅测试/本地）。
	Store CounterStore
	// Limit 是每日上限。<=0 时用 DefaultQWeatherDailyLimit。
	Limit int
	// Prefix 是 key 前缀，留空用 DefaultQuotaKeyPrefix。
	Prefix string
	// Now 注入时钟。
	Now func() time.Time
	// OnAccountError 在"记账失败但仍放行"时被调用。
	//
	// ⚠️ 它的存在意义是：**配额保护正在静默失效**这件事必须可见。
	//   放行但记账失败 ⇒ 实际发出量会超过 limit，而没有任何其它信号。
	OnAccountError func(error)
}

// DefaultQuotaKeyPrefix 是计数 key 的默认前缀。
const DefaultQuotaKeyPrefix = "weather/quota"

// NewQuota 构造配额守卫。
func NewQuota(opts QuotaOptions) *Quota {
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultQWeatherDailyLimit
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	prefix := strings.TrimSpace(opts.Prefix)
	if prefix == "" {
		prefix = DefaultQuotaKeyPrefix
	}
	store := opts.Store
	if store == nil {
		store = NewMemoryCounterStore()
	}
	return &Quota{
		store:        store,
		backend:      backendName(store),
		limit:        limit,
		prefix:       strings.TrimSuffix(prefix, "/"),
		now:          now,
		onAccountErr: opts.OnAccountError,
	}
}

// backendName 说出当前用的是哪个后端。
//
// # 为什么要暴露它
//
//	"配额保护在跑"和"配额保护真的在护住整个 supertrade"是两件事：
//	降级到文件/内存时前者仍为真，而后者为假。
//	运维第一句要问的就是"现在到底护住了几个进程"，
//	所以它必须能被程序读出来，而不是只出现在启动日志里。
func backendName(s CounterStore) string {
	switch v := s.(type) {
	case nil:
		return "none"
	case *DaprCounterStore:
		return "dapr:" + v.store
	case *FileCounterStore:
		return "file"
	case *MemoryCounterStore:
		return "memory"
	default:
		return fmt.Sprintf("%T", s)
	}
}

// Backend 返回后端标识（"dapr:weather-quota" / "file" / "memory" / "none"）。
//
// ⚠️ 只有 "dapr:*" 才满足"跨服务共享"；其余都是降级形态。
func (q *Quota) Backend() string {
	if q == nil {
		return "none"
	}
	return q.backend
}

// Shared 报告当前计数是否**跨服务**共享。
func (q *Quota) Shared() bool {
	return strings.HasPrefix(q.Backend(), "dapr:")
}

// ErrQuotaExceeded 表示今日配额已用尽。
var ErrQuotaExceeded = fmt.Errorf("和风天气每日调用次数已达上限")

// ErrNoStore 表示没有可用后端（装配错误）。
var ErrNoStore = fmt.Errorf("天气配额未装配存储后端")

// key 返回当日计数 key。
func (q *Quota) key() string {
	return q.prefix + "/" + q.now().Format("2006-01-02")
}

// Allow 报告"是否还允许再发一次请求"，并把计数 +1。
//
// ⚠️ 调用点必须在**真正发起 HTTP 之前**调用它：
//
//	先请求、后统计 = 超限了也已经浪费了配额；
//	超限时那一请求还会失败并产生一次无意义的降级日志。
func (q *Quota) Allow() (bool, error) {
	if q == nil {
		// 没有守卫不等于没有配额；保守放行（保持旧行为）。
		return true, nil
	}
	return q.AllowContext(context.Background())
}

// AllowContext 是 Allow 的带 context 版本。
//
// ⚠️ 生产应优先用它：storage 可能在等 Redis/网络，
// 而天气只有 6s 预算 —— 用 Background 会让一个卡住的存储
// 吃掉整个超时预算，症状是"日报偶发变慢"而不是"天气降级"。
func (q *Quota) AllowContext(ctx context.Context) (bool, error) {
	if q == nil {
		return true, nil
	}
	if q.store == nil {
		// ⚠️ 读不到后端时**不能**当成"还有余量" ——
		//   那正是配额失控的入口。保守判定为已用尽。
		return false, ErrNoStore
	}

	used, over, err := q.store.Incr(ctx, q.key(), q.limit)
	if err != nil {
		// ⚠️ 读/写失败时**不能**当成"还有余量" ——
		//   那正是配额失控的入口。保守判定为已用尽。
		return false, fmt.Errorf("天气配额计数失败: %w", err)
	}
	if over {
		return false, fmt.Errorf("%w（今日已用 %d / %d 次）", ErrQuotaExceeded, used, q.limit)
	}
	return true, nil
}

// Remaining 返回今日剩余可用次数（-1 = 未知）。
func (q *Quota) Remaining() int {
	if q == nil || q.store == nil {
		return -1
	}
	st, err := q.store.Get(context.Background(), q.key())
	if err != nil {
		return -1
	}
	r := q.limit - st.Used
	if r < 0 {
		return 0
	}
	return r
}

// Limit 返回每日上限。
func (q *Quota) Limit() int {
	if q == nil {
		return DefaultQWeatherDailyLimit
	}
	return q.limit
}

// Used 返回今日已用次数（-1 = 未知）。
func (q *Quota) Used() int {
	if q == nil || q.store == nil {
		return -1
	}
	st, err := q.store.Get(context.Background(), q.key())
	if err != nil {
		return -1
	}
	return st.Used
}

// ---- 内存后端（仅测试/本地；⚠️ 不跨服务、不跨重启）----

// MemoryCounterStore 是进程内计数。
//
// # ⚠️ 它**不是**跨服务安全的
//
//	生产必须用 dapr state。用它上线的样子是：
//	每个服务各有一份计数，实际发出量 = N × limit。
type MemoryCounterStore struct {
	mu    sync.Mutex
	byKey map[string]int
	// FailNext 让下一次操作返回错误（测试降级路径用）。
	failNext error
}

// NewMemoryCounterStore 构造内存后端。
func NewMemoryCounterStore() *MemoryCounterStore {
	return &MemoryCounterStore{byKey: map[string]int{}}
}

func (s *MemoryCounterStore) Get(_ context.Context, key string) (quotaState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return quotaState{Used: s.byKey[key]}, nil
}

func (s *MemoryCounterStore) Incr(_ context.Context, key string, limit int) (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	used := s.byKey[key]
	// ⚠️ 越过 limit 时**不再增长**：否则并发下 used 会一路涨上去，
	//   报错信息里"已用 1732 / 1500"看起来像出了故障，
	//   而真相只是"有人一直在重试"。
	if used >= limit {
		return used, true, nil
	}
	used++
	s.byKey[key] = used
	return used, false, nil
}

// FailNext 让下一次操作返回 err（测试用）。
func (s *MemoryCounterStore) FailNext(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = err
}

// ---- 文件后端（单机保底 / 离线自测）----

// FileCounterStore 把计数写文件。
//
// # 什么时候用它而不是 dapr state
//
//	**没有 dapr sidecar 的裸跑**（本地开发、一次性排障）。
//
//	线上**不要**用它：它是每服务一份的（见 MemoryCounterStore 的同类警告），
//	而且 dapr 不可用时它会把"跨服务计数"降级成"每服务计数"——
//	那比降级成内存更危险，因为它看起来是持久化的。
type FileCounterStore struct {
	// Dir 是计数文件目录；留空则落到调用方给的路径。
	Dir string
	mu  sync.Mutex
}

// NewFileCounterStore 构造文件后端。
func NewFileCounterStore(dir string) *FileCounterStore {
	return &FileCounterStore{Dir: strings.TrimSpace(dir)}
}

// maxQuotaFileNameLen 是可读文件名部分的字节上限。
const maxQuotaFileNameLen = 96

func (s *FileCounterStore) pathOf(key string) string {
	// ⚠️ 必须用**完整 key** 做文件名，不能只取最后一段。
	//
	//	只取最后一段（日期）时，"weather/quota/2026-10-17" 与
	//	"wrong/quota/2026-10-17" 落到**同一个文件** ——
	//	两个前缀静默共用一份计数，等于前缀约定失效却零报错，
	//	和 authkey 组件"keyPrefix 写错 → 静默 404"是同一类陷阱。
	name := sanitizeQuotaFileName(key)
	if len(name) > maxQuotaFileNameLen {
		// 文件名超长在多数文件系统上直接 ENAMETOOLONG，
		// 而 Incr 失败会被**保守判为已用尽** → 天气全日不可用。
		// 所以这里换成一个有界摘要，而不是把错误留给文件系统抛。
		sum := sha256.Sum256([]byte(key))
		name = "h" + hex.EncodeToString(sum[:])
	}
	return filepath.Join(s.Dir, "quota-"+name+".json")
}

func (s *FileCounterStore) Get(_ context.Context, key string) (quotaState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.pathOf(key))
	if err != nil {
		if os.IsNotExist(err) {
			// 文件不存在 = 今天还没用过。
			return quotaState{}, nil
		}
		return quotaState{}, err
	}
	if len(b) == 0 {
		return quotaState{}, nil
	}
	var st quotaState
	if err := json.Unmarshal(b, &st); err != nil {
		// ⚠️ 文件损坏时**不能**静默当成 0 ——
		//   那等于把当日计数清零，是配额保护最隐蔽的失效方式。
		return quotaState{}, fmt.Errorf("计数文件内容损坏: %w", err)
	}
	return st, nil
}

func (s *FileCounterStore) Incr(_ context.Context, key string, limit int) (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	used := 0
	if b, err := os.ReadFile(s.pathOf(key)); err == nil && len(b) > 0 {
		var st quotaState
		if err := json.Unmarshal(b, &st); err != nil {
			return 0, false, fmt.Errorf("计数文件内容损坏: %w", err)
		}
		used = st.Used
	} else if err != nil && !os.IsNotExist(err) {
		return 0, false, err
	}

	if used >= limit {
		return used, true, nil
	}
	used++
	if s.Dir != "" {
		if err := os.MkdirAll(s.Dir, 0o755); err != nil {
			return used, false, err
		}
	}
	b, err := json.Marshal(quotaState{Used: used})
	if err != nil {
		return used, false, err
	}
	// ⚠️ 原子写：先临时文件再 rename。
	//   直接覆盖写在中途崩溃时会留下**半截 JSON**，
	//   而 Get 对损坏文件返回错误 → 计数被当成"已用尽" → 天气全日不可用。
	tmp := s.pathOf(key) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return used, false, err
	}
	return used, false, os.Rename(tmp, s.pathOf(key))
}

// sanitizeQuotaFileName 去掉文件名里的路径分隔符。
func sanitizeQuotaFileName(s string) string {
	return strings.NewReplacer("/", "_", `\`, "_", "..", "_").Replace(s)
}

// ResetToday 把当日计数清零（测试用；生产上不存在这个需求）。
func (q *Quota) ResetToday() {
	if q == nil || q.store == nil {
		return
	}
	if ms, ok := q.store.(*MemoryCounterStore); ok {
		ms.mu.Lock()
		delete(ms.byKey, q.key())
		ms.mu.Unlock()
	}
}
