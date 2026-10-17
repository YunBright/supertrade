package weather

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixedClock(t *testing.T, day string) func() time.Time {
	t.Helper()
	d, err := time.ParseInLocation("2006-01-02 15:04:05", day+" 09:00:00", time.Local)
	if err != nil {
		t.Fatalf("解析测试日期失败: %v", err)
	}
	return func() time.Time { return d }
}

// newQuotaWithFile 造一个用文件后端的 Quota。
func newQuotaWithFile(t *testing.T, limit int, now func() time.Time) (*Quota, string) {
	t.Helper()
	dir := t.TempDir()
	return NewQuota(QuotaOptions{
		Store: NewFileCounterStore(dir),
		Limit: limit,
		Now:   now,
	}), dir
}

// ⚠️ 核心不变量：**超限时一个 HTTP 请求都不该发出**。
//
// 判别力来自反证：把 get() 里的 `p.Quota.AllowContext(ctx)` 整段删掉，
// 本用例的 gotHits 会从 0 变成 limit。
//
// ⚠️ 为什么这条最重要：配额保护失效是**完全静默**的
// （表现只是"某天天气突然全无"），没有任何错误码指向它。
func TestQuotaBlocksRequestsWhenExhausted(t *testing.T) {
	const limit = 5
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Write([]byte(`{"code":"200","now":{"text":"晴","temp":"30"}}`))
	}))
	t.Cleanup(srv.Close)

	q, _ := newQuotaWithFile(t, limit, fixedClock(t, "2026-10-17"))
	p := &QWeatherProvider{BaseURL: srv.URL, Key: "k", HTTP: srv.Client(), Quota: q}

	for i := 0; i < limit; i++ {
		if ok, err := q.Allow(); !ok {
			t.Fatalf("第 %d 次不该被拒: %v", i+1, err)
		}
	}
	if rem := q.Remaining(); rem != 0 {
		t.Errorf("灌满后 Remaining 应为 0，实际 %d", rem)
	}

	before := hits
	got, err := p.Fetch(t.Context(), "2026-10-16", validLoc())
	if err != nil {
		t.Fatalf("配额耗尽应降级为 Info 而非 error: %v", err)
	}
	if hits != before {
		t.Errorf("配额耗尽后仍发出了 %d 次请求 —— 配额保护形同虚设", hits-before)
	}
	// ⚠️ 配额耗尽时**无法拿到任何维度**（守卫在发请求前就拦下），
	//   所以整体降级是**正确**行为。
	//   曾在这里断言"实况摘要应保留"，那是把前提写错了：
	//   守卫的意义是"不浪费配额"，不是"尽量用完配额"。
	if got.Available {
		t.Errorf("配额耗尽时不应仍标记为可用（无任何真实数据）: %+v", got)
	}
	if got.Summary != "" {
		t.Errorf("降级时 Summary 必须为空串（绝不能编造天气），实际 %q", got.Summary)
	}
	if !strings.Contains(got.DegradedReason, "上限") {
		t.Errorf("DegradedReason 应说明是配额问题: %q", got.DegradedReason)
	}
	if !strings.Contains(got.DegradedReason, "5 / 5") {
		t.Errorf("应带已用/上限数字: %q", got.DegradedReason)
	}
}

// ⚠️ **跨重启不清零**（文件后端）。
//
// 判别力：把后端换成 MemoryCounterStore（每次进程新实例），本用例失败。
func TestQuotaSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	clock := fixedClock(t, "2026-10-17")

	q1 := NewQuota(QuotaOptions{Store: NewFileCounterStore(dir), Limit: 1500, Now: clock})
	for i := 0; i < 3; i++ {
		if ok, _ := q1.Allow(); !ok {
			t.Fatalf("第 %d 次不该被拒", i+1)
		}
	}

	q2 := NewQuota(QuotaOptions{Store: NewFileCounterStore(dir), Limit: 1500, Now: clock})
	if got := q2.Remaining(); got != 1497 {
		t.Fatalf("重启后计数被清零：Remaining=%d，期望 1497", got)
	}
}

// ⚠️ **跨服务共享**（同一个 store 实例 = 同一个计数）。
//
// 这是本次改造的核心判据：和风按 API key 计费，
// 各服务各数各的 ⇒ 实际发出量 N×limit。
//
// 判别力：把两个 Quota 换成两个**互不相干**的 MemoryCounterStore，本用例失败。
func TestQuotaIsSharedAcrossServices(t *testing.T) {
	dir := t.TempDir()
	clock := fixedClock(t, "2026-10-17")

	// 两个"服务"：配置相同，指向同一后端（= 同一个 Redis key 空间）。
	svcA := NewQuota(QuotaOptions{Store: NewFileCounterStore(dir), Limit: 1500, Now: clock})
	svcB := NewQuota(QuotaOptions{Store: NewFileCounterStore(dir), Limit: 1500, Now: clock})

	for i := 0; i < 3; i++ {
		svcA.Allow()
	}
	// ⚠️ B 刚启动（计数 0），却必须**看到** A 已经用掉的 3 次。
	if got := svcB.Used(); got != 3 {
		t.Fatalf("B 未看到 A 的计数：Used=%d，期望 3", got)
	}
	svcB.Allow()
	if got := svcA.Used(); got != 4 {
		t.Fatalf("A 未看到 B 的计数：Used=%d，期望 4", got)
	}
}

// ⚠️ 前缀不同 ⇒ 各数各的 ⇒ 上限对不上真实额度。
//
// 这条锁住"前缀必须全平台一致"这条契约：
// keyPrefix 写错时**不报错**，只是各数各的，配额保护静默失效。
//
// 判别力：把 key() 里的 prefix 去掉，两个服务的计数会分叉 → 本用例失败。
func TestDifferentPrefixesDoNotShare(t *testing.T) {
	dir := t.TempDir()
	clock := fixedClock(t, "2026-10-17")

	svcA := NewQuota(QuotaOptions{
		Store: NewFileCounterStore(dir), Limit: 1500, Now: clock,
		Prefix: "weather/quota",
	})
	svcB := NewQuota(QuotaOptions{
		Store: NewFileCounterStore(dir), Limit: 1500, Now: clock,
		Prefix: "wrong/quota", // 模拟别人写错了前缀
	})
	svcA.Allow()
	svcA.Allow()
	if got := svcB.Used(); got != 0 {
		t.Errorf("不同前缀必须互不可见（这正是写错前缀时的静默失效形态），实际 Used=%d", got)
	}
}

func TestQuotaResetsNextDay(t *testing.T) {
	q, _ := newQuotaWithFile(t, 10, fixedClock(t, "2026-10-17"))
	for i := 0; i < 10; i++ {
		q.Allow()
	}
	if ok, _ := q.Allow(); ok {
		t.Fatal("当日用满后应拒绝")
	}

	// 次日：key 变了 ⇒ 天然归零。
	q2 := NewQuota(QuotaOptions{
		Store: NewFileCounterStore(q.store.(*FileCounterStore).Dir),
		Limit: 10,
		Now:   fixedClock(t, "2026-10-18"),
	})
	if got := q2.Remaining(); got != 10 {
		t.Errorf("次日计数应归零，Remaining=%d，期望 10", got)
	}
	if ok, err := q2.Allow(); !ok {
		t.Errorf("次日首次调用应放行: %v", err)
	}
}

// ⚠️ 并发下"读-判断-加一"必须原子，否则实际发出量会超限。
func TestQuotaIsAtomicUnderConcurrency(t *testing.T) {
	const (
		limit     = 100
		goroutine = 20
		perG      = 20 // 总共 400 次尝试，只有 100 次该成功
	)
	q, _ := newQuotaWithFile(t, limit, fixedClock(t, "2026-10-17"))

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
		t.Errorf("并发下放行次数=%d，期望恰好 %d（超发即配额保护失效）", allowed, limit)
	}
	if rem := q.Remaining(); rem != 0 {
		t.Errorf("放行满后 Remaining 应为 0，实际 %d", rem)
	}
}

// ⚠️ "读-判断-加一"必须被 store 一次性完成，不是靠调用方在锁里包。
//
// 判别力（结构断言）：读源码确认 AllowContext 里**没有**自己加锁，
// 原子性由 store 负责 —— 因为跨服务时进程内锁毫无意义。
func TestAllowContextDelegatesAtomicityToStore(t *testing.T) {
	src := extractFunc(readSource(t, quotaGoPath(t)), "func (q *Quota) AllowContext")
	if strings.Contains(src, "q.mu.Lock") {
		t.Error("AllowContext 不得自己加锁：跨服务时进程内锁只护住本进程，" +
			"原子性必须由 store 提供")
	}
	if !strings.Contains(src, "q.store.Incr") {
		t.Error("AllowContext 必须把递增委托给 store.Incr（那是原子性所在）")
	}
	// ⚠️ 递增失败必须**保守判为已用尽** —— 读不到计数就放行
	//   正是配额失控的入口。
	if !strings.Contains(src, "return false") {
		t.Error("计数失败时必须 return false（保守方向）")
	}
}

// ⚠️ 计数数据损坏时**必须**保守判为已用尽，不能当成 0。
//
// 静默当成 0 = 把当日计数清零，是配额保护最隐蔽的失效方式
// （数据还在、格式合法、只是值不对，没有任何告警）。
//
// 判别力：把 decodeState 的 Unmarshal 错误分支改成返回零值，本用例失败。
func TestCorruptedCounterIsNotTreatedAsZero(t *testing.T) {
	_, err := decodeState([]byte("{not json"))
	if err == nil {
		t.Fatal("损坏数据必须报错，不得当成 0 次已用")
	}
	if !strings.Contains(err.Error(), "损坏") {
		t.Errorf("错误信息应说明是数据损坏: %v", err)
	}
	if _, err := decodeState([]byte(`{"used":-5}`)); err == nil {
		t.Error("负数计数必须报错（不该由本包写出，出现即数据异常）")
	}
}

// ⚠️ store 返回错误时必须**拒绝**，且错误可定位。
func TestStoreFailureBlocks(t *testing.T) {
	q := NewQuota(QuotaOptions{
		Store: &failingStore{},
		Limit: 1500,
		Now:   fixedClock(t, "2026-10-17"),
	})
	ok, err := q.Allow()
	if ok {
		t.Fatal("store 失败时不得放行 —— 那等于悄悄取消配额保护")
	}
	if err == nil || !strings.Contains(err.Error(), "计数失败") {
		t.Errorf("应给出可定位的原因: %v", err)
	}
	if got := q.Remaining(); got != -1 {
		t.Errorf("无法判断时 Remaining 应为 -1（未知），实际 %d", got)
	}
}

// failingStore 恒失败。
type failingStore struct{}

func (s *failingStore) Get(context.Context, string) (quotaState, error) {
	return quotaState{}, errors.New("redis down")
}
func (s *failingStore) Incr(context.Context, string, int) (int, bool, error) {
	return 0, false, errors.New("redis down")
}

// 默认上限必须是 1500，且配置项能覆盖。
func TestQuotaDefaultAndOverride(t *testing.T) {
	q := NewQuota(QuotaOptions{})
	if q.Limit() != DefaultQWeatherDailyLimit {
		t.Errorf("默认上限=%d，期望 %d", q.Limit(), DefaultQWeatherDailyLimit)
	}
	if DefaultQWeatherDailyLimit != 1500 {
		t.Errorf("和风每日上限约定为 1500，实际常量 %d", DefaultQWeatherDailyLimit)
	}
	for _, bad := range []int{0, -1, -100} {
		g := NewQuota(QuotaOptions{Limit: bad})
		if g.Limit() != DefaultQWeatherDailyLimit {
			t.Errorf("Limit=%d 应回落到默认值，实际 %d", bad, g.Limit())
		}
	}
	if NewQuota(QuotaOptions{Limit: 42}).Limit() != 42 {
		t.Error("合法上限应生效")
	}
	var nilQ *Quota
	if ok, err := nilQ.Allow(); !ok || err != nil {
		t.Errorf("nil 守卫应放行且不报错，实际 ok=%v err=%v", ok, err)
	}
	if nilQ.Remaining() != -1 || nilQ.Used() != -1 {
		t.Errorf("nil 守卫的 Unknown 值应为 -1")
	}
	nilQ.ResetToday()
}

// 文件后端：原子写、不留 tmp、损坏报错。
func TestFileCounterStoreBehaviour(t *testing.T) {
	dir := t.TempDir()
	s := NewFileCounterStore(dir)
	ctx := context.Background()

	// 目录不存在时自建。
	if _, _, err := s.Incr(ctx, "weather/quota/2026-10-17", 10); err != nil {
		t.Fatalf("首次应自建目录: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(files) != 0 {
		t.Errorf("不应残留 .tmp: %v", files)
	}
	// 文件名不得含路径分隔符（key 里的 `/` 是路径穿越入口）。
	files, _ = filepath.Glob(filepath.Join(dir, "*"))
	for _, f := range files {
		if strings.Contains(filepath.Base(f), "/") || strings.Contains(filepath.Base(f), `\`) {
			t.Errorf("文件名含分隔符: %s", f)
		}
	}

	// 损坏文件 → 报错。
	bad := s.pathOf("weather/quota/2026-10-18")
	if err := writeFile(bad, "{broken"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "weather/quota/2026-10-18"); err == nil {
		t.Error("损坏文件必须报错")
	}
	// 损坏文件上的 Incr 也必须报错（不能当成 0 继续写）。
	if _, _, err := s.Incr(ctx, "weather/quota/2026-10-18", 10); err == nil {
		t.Error("损坏文件上的 Incr 必须报错（当成 0 就是把计数清零）")
	}
}

// ⚠️ 前缀超长时必须**换一个有界摘要**做文件名，而不是把错误交给文件系统。
//
// 判别力：把 pathOf 里 len(name) > maxQuotaFileNameLen 的分支删掉，
// 本用例在 Windows 上会以 ENAMETOOLONG 失败（Linux 上 255 字节上限更难触发）。
func TestFileCounterStoreBoundsFileNameLength(t *testing.T) {
	dir := t.TempDir()
	s := NewFileCounterStore(dir)

	longA := strings.Repeat("a", 200)
	longB := strings.Repeat("b", 200)

	if _, _, err := s.Incr(context.Background(), longA+"/2026-10-17", 10); err != nil {
		t.Fatalf("超长前缀必须仍能写入，不得把错误留给文件系统: %v", err)
	}
	if _, _, err := s.Incr(context.Background(), longB+"/2026-10-17", 10); err != nil {
		t.Fatalf("超长前缀必须仍能写入: %v", err)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(files) != 2 {
		t.Fatalf("两个不同前缀必须落到不同文件，实际 %d 个: %v", len(files), files)
	}
	for _, f := range files {
		if strings.Contains(filepath.Base(f), strings.Repeat("a", 20)) {
			t.Errorf("超长前缀应退化为摘要文件名，实际仍用可读名: %s", f)
		}
	}
	// 摘要模式下计数仍必须正确（不能因为换了名字就串了）。
	st, err := s.Get(context.Background(), longA+"/2026-10-17")
	if err != nil || st.Used != 1 {
		t.Errorf("摘要模式下计数应正确: used=%d err=%v", st.Used, err)
	}
}

// 超限后计数不再增长 —— 报错信息里的数字不该一路涨上去。
func TestOverLimitCountStopsGrowing(t *testing.T) {
	s := NewMemoryCounterStore()
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		s.Incr(ctx, "k", 5)
	}
	used, over, _ := s.Incr(ctx, "k", 5)
	if !over || used != 5 {
		t.Errorf("超限后应停在 5，实际 used=%d over=%v", used, over)
	}
}

// ---- 源码结构断言的辅助 ----

func quotaGoPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "quota.go")
}

func readSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

func extractFunc(src, start string) string {
	i := strings.Index(src, start)
	if i < 0 {
		return ""
	}
	rest := src[i+len(start):]
	if j := strings.Index(rest, "\nfunc "); j >= 0 {
		return rest[:j]
	}
	return rest
}
