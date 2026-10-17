package weather

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	dapr "github.com/dapr/go-sdk/client"
)

// fakeDaprState 是一个**真正执行 ETag 语义**的 dapr state 假实现。
//
// # ⚠️ 为什么不做一个"总是成功"的假实现
//
//	那样测不出本文件最核心的性质：带 etag 写冲突时必须失败。
//	如果假实现无视 etag，那么"代码传了错误的 etag"、
//	"代码压根没用 SaveStateWithETag"这两类错**都不会变红**，
//	而它们在生产上的表现是**并发下静默丢计数** ——
//	实际发出量超过 1500，且没有任何信号。
type fakeDaprState struct {
	mu      sync.Mutex
	data    map[string][]byte
	etag    map[string]string
	version int

	getCalls  int
	saveCalls int
	saveETags []string // 每次写用的 etag（"" = 无条件覆盖）
	saveKeys  []string
	// failGet 让 GetState 恒失败（模拟组件未加载 / Redis 不可达）。
	failGet error
	// conflictsLeft 让接下来 N 次写返回 ETag 冲突。
	conflictsLeft int
	// failSave 让写恒失败（非 ETag 类错误）。
	failSave error
}

func newFakeDaprState() *fakeDaprState {
	return &fakeDaprState{data: map[string][]byte{}, etag: map[string]string{}}
}

func (f *fakeDaprState) GetState(_ context.Context, _, key string, _ map[string]string) (*dapr.StateItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.failGet != nil {
		return nil, f.failGet
	}
	v, ok := f.data[key]
	if !ok {
		return nil, nil // dapr 对不存在的 key 返回 (nil, nil)
	}
	return &dapr.StateItem{Key: key, Value: append([]byte(nil), v...), Etag: f.etag[key]}, nil
}

func (f *fakeDaprState) SaveStateWithETag(_ context.Context, _, key string, data []byte, etag string, _ map[string]string, _ ...dapr.StateOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saveCalls++
	f.saveETags = append(f.saveETags, etag)
	f.saveKeys = append(f.saveKeys, key)

	if f.failSave != nil {
		return f.failSave
	}
	if f.conflictsLeft > 0 {
		f.conflictsLeft--
		return errors.New("failed saving state: item.ETag mismatch")
	}
	cur, exists := f.etag[key]
	if exists && cur != etag {
		// 与 dapr Redis 组件一致：etag 不匹配 ⇒ 写被拒。
		return errors.New("failed saving state: item.ETag mismatch")
	}
	f.version++
	f.data[key] = append([]byte(nil), data...)
	f.etag[key] = strconv.Itoa(f.version)
	return nil
}

func (f *fakeDaprState) used(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.data[key]
	if !ok {
		return 0
	}
	st, err := decodeState(b)
	if err != nil {
		return -1
	}
	return st.Used
}

func newDaprStore(f *fakeDaprState) *DaprCounterStore {
	s := NewDaprCounterStore(DaprCounterStoreOptions{Client: f, Store: "weather-quota"})
	if s == nil {
		panic("NewDaprCounterStore 不应对非 nil client 返回 nil")
	}
	return s
}

const daprKey = "weather/quota/2026-10-17"

// ⚠️ **跨服务共享**必须落在 dapr state 上，而不是只在文件后端成立。
//
// 判别力：把两个 store 换成两个互不相干的 fakeDaprState → 第二个断言失败。
// 这是本次改造的核心判据 —— 和风按 API key 计费，各服务各数各的
// 就会让实际发出量变成 N×1500。
func TestDaprQuotaIsSharedAcrossServices(t *testing.T) {
	backend := newFakeDaprState()
	svcA := newDaprStore(backend)
	svcB := newDaprStore(backend) // 两个进程各自的 client，连的是同一个 Redis

	for i := 0; i < 3; i++ {
		if _, _, err := svcA.Incr(context.Background(), daprKey, 1500); err != nil {
			t.Fatal(err)
		}
	}
	if got, _, _ := svcB.Incr(context.Background(), daprKey, 1500); got != 4 {
		t.Fatalf("B 必须接着 A 的计数递增，实际 got=%d 期望 4", got)
	}
	if used := backend.used(daprKey); used != 4 {
		t.Errorf("后端里的计数应只有一份，实际 %d（两份即等于没共享）", used)
	}
}

// ⚠️ 必须走 **SaveStateWithETag + 真 etag**，不能是无条件覆盖。
//
// 判别力：把 Incr 里的 SaveStateWithETag 换成 SaveState
// （或把 etag 变量硬编码成 ""）→ 本用例失败。
func TestDaprIncrUsesETagCAS(t *testing.T) {
	backend := newFakeDaprState()
	s := newDaprStore(backend)
	ctx := context.Background()

	// 第一次：key 不存在 ⇒ etag 必须为空（无条件写）。
	if _, _, err := s.Incr(ctx, daprKey, 1500); err != nil {
		t.Fatal(err)
	}
	if backend.saveETags[0] != "" {
		t.Errorf("key 不存在时 etag 应为空，实际 %q", backend.saveETags[0])
	}
	// 第二次：key 存在 ⇒ 必须带真实 etag。
	if _, _, err := s.Incr(ctx, daprKey, 1500); err != nil {
		t.Fatal(err)
	}
	if backend.saveETags[1] == "" {
		t.Error("key 已存在时必须带 ETag 写，空 etag 等于无条件覆盖 ⇒ 并发下必然丢计数")
	}
}

// ⚠️ ETag 冲突必须**重试**，这是 CAS 成立的前提。
//
// 判别力：把 isETagConflict 的返回值改成恒 false → 本用例失败。
func TestDaprIncrRetriesOnETagConflict(t *testing.T) {
	backend := newFakeDaprState()
	backend.conflictsLeft = 2 // 前两次写冲突，第三次成功
	s := newDaprStore(backend)

	got, over, err := s.Incr(context.Background(), daprKey, 1500)
	if err != nil {
		t.Fatalf("冲突后应重试到成功: %v", err)
	}
	if over || got != 1 {
		t.Errorf("重试成功后 used=%d over=%v 期望 1/false", got, over)
	}
	if backend.saveCalls != 3 {
		t.Errorf("应发生 2 次冲突 + 1 次成功，实际写了 %d 次", backend.saveCalls)
	}
}

// ⚠️ 非 ETag 类错误（Redis 不可达）**不得**重试 ——
// 重试同一类错误只会白白吃掉天气那 6s 预算，症状是"日报偶发变慢"。
//
// 判别力：把 `if isETagConflict(err)` 改成恒 true → 本用例失败。
func TestDaprIncrDoesNotRetryNonETagErrors(t *testing.T) {
	backend := newFakeDaprState()
	backend.failSave = errors.New("connection refused")
	s := newDaprStore(backend)

	if _, _, err := s.Incr(context.Background(), daprKey, 1500); err == nil {
		t.Fatal("必须报错（守卫据此保守判为已用尽）")
	}
	if backend.saveCalls != 1 {
		t.Errorf("非 ETag 错误不得重试，实际写了 %d 次", backend.saveCalls)
	}
}

// ⚠️ 持续冲突必须有**上限**，否则高并发下会把超时预算耗光。
func TestDaprIncrGivesUpAfterMaxRetries(t *testing.T) {
	backend := newFakeDaprState()
	backend.conflictsLeft = 99
	s := newDaprStore(backend)

	_, _, err := s.Incr(context.Background(), daprKey, 1500)
	if err == nil {
		t.Fatal("持续冲突必须报错，不得当成成功放行")
	}
	if backend.saveCalls != 3 {
		t.Errorf("应恰好重试到上限（3 次），实际 %d 次", backend.saveCalls)
	}
	if got := err.Error(); !strings.Contains(got, "3") {
		t.Errorf("错误应说明重试了几次，便于定位: %v", err)
	}
}

// ⚠️ 超限时**一个写都不能发**。
//
// 判别力：删掉 `if used >= limit` 早返回 → 本用例失败
// （saveCalls 会从 0 变成 1，且 used 会继续增长）。
func TestDaprIncrDoesNotWriteWhenOverLimit(t *testing.T) {
	backend := newFakeDaprState()
	s := newDaprStore(backend)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		s.Incr(ctx, daprKey, 3)
	}
	before := backend.saveCalls
	got, over, err := s.Incr(ctx, daprKey, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !over {
		t.Error("已达上限必须报 over")
	}
	if got != 3 {
		t.Errorf("超限后 used 应停在 3，实际 %d", got)
	}
	if backend.saveCalls != before {
		t.Errorf("超限后不得再写（否则报错里的数字一路涨，看起来像故障），实际多写了 %d 次",
			backend.saveCalls-before)
	}
}

// ⚠️ 存量数据损坏时**不能**当成 0 —— 那等于把当日计数清零。
//
// 判别力：把 Incr 里 decodeState 的错误分支改成继续用 used=0 → 本用例失败。
func TestDaprIncrRejectsCorruptedStoredData(t *testing.T) {
	backend := newFakeDaprState()
	s := newDaprStore(backend)
	backend.data[daprKey] = []byte("{broken")

	_, _, err := s.Incr(context.Background(), daprKey, 1500)
	if err == nil {
		t.Fatal("损坏数据必须报错，不得当成 0 次已用")
	}
	if !strings.Contains(err.Error(), "损坏") {
		t.Errorf("错误应说明是数据损坏: %v", err)
	}
	if backend.saveCalls != 0 {
		t.Error("识别出损坏后不得继续写（那会覆盖掉现场）")
	}
}

// ⚠️ Get 也必须走同一条解码路径，不能对损坏数据返回 0。
func TestDaprGetRejectsCorruptedStoredData(t *testing.T) {
	backend := newFakeDaprState()
	backend.data[daprKey] = []byte(`{"used":-3}`)
	s := newDaprStore(backend)

	if _, err := s.Get(context.Background(), daprKey); err == nil {
		t.Error("负数计数必须报错")
	}
}

// ⚠️ **Probe 必须能探出"组件没加载"**。
//
// 这是 main.go 降级链的依据：少了这一步，"组件 yaml 没部署"
// 会在运行期表现为"每天每份报告都取不到天气"，
// 而看上去像是天气功能坏了，真相是配额守卫把天气锁死了。
//
// 判别力：把 Probe 的 GetState 调用删掉（恒返回 nil）→ 本用例失败。
func TestDaprProbeDetectsUnusableComponent(t *testing.T) {
	good := newFakeDaprState()
	if err := newDaprStore(good).Probe(context.Background()); err != nil {
		t.Errorf("正常组件必须探得通: %v", err)
	}

	bad := newFakeDaprState()
	bad.failGet = errors.New("error code 42: ERR_...")
	err := newDaprStore(bad).Probe(context.Background())
	if err == nil {
		t.Fatal("组件不可用时 Probe 必须报错，否则装配期就会误判为共享计数")
	}
	if !strings.Contains(err.Error(), "weather-quota") {
		t.Errorf("错误应带上组件名，否则配错组件时无从查起: %v", err)
	}
}

// ⚠️ Probe 必须是**只读**的 —— 装配时探一次不能凭空吃掉一天 1 次配额，
// 也不能往 key 空间里写进一个与计数混淆的键。
func TestDaprProbeDoesNotWrite(t *testing.T) {
	backend := newFakeDaprState()
	s := newDaprStore(backend)
	if err := s.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if backend.saveCalls != 0 {
		t.Errorf("Probe 不得写任何状态，实际写了 %d 次", backend.saveCalls)
	}
}

// client 为 nil 时必须返回 nil，让调用方回退而不是启动失败。
func TestNewDaprStoreReturnsNilWhenNoClient(t *testing.T) {
	if s := NewDaprCounterStore(DaprCounterStoreOptions{}); s != nil {
		t.Error("无 client 时必须返回 nil")
	}
	var nilStore *DaprCounterStore
	if err := nilStore.Probe(context.Background()); err == nil {
		t.Error("nil store 的 Probe 必须报错")
	}
}

// ⚠️ 并发下**不得丢计数**：ETag 冲突必须重试到成功，总数恰好等于上限。
//
// 判别力：把 SaveStateWithETag 换成无条件写 → used 会小于实际放行数。
// 这是"跨服务共享"这件事唯一有意义的判据：
// 配额保护一旦在并发下丢计数，实际发出量就悄悄超了 1500。
func TestDaprIncrLosesNothingUnderConcurrency(t *testing.T) {
	const (
		limit     = 60
		goroutine = 8
		perG      = 30 // 共 240 次尝试，只有 60 次该成功
	)
	backend := newFakeDaprState()
	s := newDaprStore(backend)
	q := NewQuota(QuotaOptions{Store: s, Limit: limit, Now: fixedClock(t, "2026-10-17")})

	var mu sync.Mutex
	allowed := 0
	var wg sync.WaitGroup
	for i := 0; i < goroutine; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				if ok, _ := q.Allow(); ok {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	if allowed != limit {
		t.Errorf("放行次数=%d，期望恰好 %d（多放行 = 超发配额失控）", allowed, limit)
	}
	if used := backend.used(daprKey); used != limit {
		t.Errorf("后端计数=%d，期望 %d（少了就是并发丢计数）", used, limit)
	}
}

// ⚠️ **`null` 会被 json.Unmarshal 静默当成 0**，必须显式拒绝。
//
// 这是 2026-10-17 线上排查发现的真实缺陷：
// 用 dapr HTTP state API 的 `POST /v1.0/state/{store}` 数组形式"读"状态，
// 那个接口其实是 bulk **set**（写），不带 value 就把 key 写成 `null`。
// 而 `json.Unmarshal([]byte("null"), &st)` **不报错**，st 停在零值 →
// Used=0 → 当日配额被静默清零，且数据"看起来正常"。
//
// 判别力：删掉 decodeState 里的 null 分支 → 本用例失败。
func TestDaprDecodeRejectsNullAndEmpty(t *testing.T) {
	for _, bad := range []string{"null", " null\n", "", "   "} {
		if _, err := decodeState([]byte(bad)); err == nil {
			t.Errorf("输入 %q 必须报错：null/空会被 Unmarshal 静默当成 Used=0，"+
				"那等于把当日配额清零", bad)
		} else if !errors.Is(err, ErrCorruptedState) {
			t.Errorf("输入 %q 的错误应带 ErrCorruptedState 哨兵（便于调用方 errors.Is 判定），got %v",
				bad, err)
		}
	}
	// 合法值仍必须照常解析，别把守卫写过头。
	st, err := decodeState([]byte(`{"used":7}`))
	if err != nil || st.Used != 7 {
		t.Errorf("合法值必须解析成功: used=%d err=%v", st.Used, err)
	}
	if st, err := decodeState([]byte(`{"used":0}`)); err != nil || st.Used != 0 {
		t.Errorf(`{"used":0}`+" 是合法的\"今天还没用过\"，不得报错: used=%d err=%v", st.Used, err)
	}
}

// ⚠️ 存量里已经是 null 时，守卫必须**保守拒绝**，而不是当成 0 继续发。
//
// 判别力：把 Incr 里 decodeState 的错误分支改掉 → 本用例失败。
func TestDaprIncrBlocksWhenStoredValueIsNull(t *testing.T) {
	backend := newFakeDaprState()
	backend.data[daprKey] = []byte("null")
	s := newDaprStore(backend)

	if _, _, err := s.Incr(context.Background(), daprKey, 1500); err == nil {
		t.Fatal("存量为 null 时必须报错（当成 0 就是把配额静默清零）")
	}
	if backend.saveCalls != 0 {
		t.Error("识别出 null 后不得继续写（那会覆盖掉现场，且把清零变成事实）")
	}

	// Get 同样必须报错，Remaining/Used 因此返回 -1（未知），而不是 0。
	if _, err := s.Get(context.Background(), daprKey); err == nil {
		t.Error("Get 对 null 也必须报错")
	}
	q := NewQuota(QuotaOptions{Store: s, Limit: 1500, Now: fixedClock(t, "2026-10-17")})
	if ok, _ := q.Allow(); ok {
		t.Error("计数不可信时不得放行")
	}
	if q.Used() != -1 {
		t.Errorf("计数不可信时 Used 应为 -1（未知），实际 %d —— 报 0 就是谎称\"今天还没用过\"", q.Used())
	}
}

// 装配层契约：main.go 的降级链依赖「探不通就回退」，
// 这里锁住"Probe 失败必须能被发现"这条，不允许被吞掉。
func TestIsETagConflictClassifies(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"failed saving state: item.ETag mismatch", true},
		{"ETAG MISMATCH", true},
		{"connection refused", false},
		{"state operation failed", false},
	}
	for _, c := range cases {
		if got := isETagConflict(fmt.Errorf("%s", c.msg)); got != c.want {
			t.Errorf("isETagConflict(%q)=%v 期望 %v", c.msg, got, c.want)
		}
	}
	if isETagConflict(nil) {
		t.Error("nil 不得被判为冲突")
	}
}
