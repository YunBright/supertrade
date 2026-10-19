package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	dapr "github.com/dapr/go-sdk/client"

	"github.com/YunBright/supertrade/internal/salesinsight/handler"
	"github.com/YunBright/supertrade/internal/salesinsight/weather"
)

// fakeState 是 package main 里独立的假 dapr state。
//
// ⚠️ 与 weather 包里那个**刻意重复**：两边测的是不同契约
//
//	（那边测 CAS 原子性，这边测装配层"探不通就回退"），
//	共享一个测试替身会把两个契约绑在一起，改一个就可能掩盖另一个。
type fakeState struct {
	mu      sync.Mutex
	data    map[string][]byte
	etag    map[string]string
	ver     int
	failGet error
}

func newFakeState() *fakeState {
	return &fakeState{data: map[string][]byte{}, etag: map[string]string{}}
}

func (f *fakeState) GetState(_ context.Context, _, key string, _ map[string]string) (*dapr.StateItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet != nil {
		return nil, f.failGet
	}
	v, ok := f.data[key]
	if !ok {
		return nil, nil
	}
	return &dapr.StateItem{Key: key, Value: append([]byte(nil), v...), Etag: f.etag[key]}, nil
}

func (f *fakeState) SaveStateWithETag(_ context.Context, _, key string, data []byte, etag string, _ map[string]string, _ ...dapr.StateOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, exists := f.etag[key]
	if exists && cur != etag {
		return errors.New("failed saving state: item.ETag mismatch")
	}
	f.ver++
	f.data[key] = append([]byte(nil), data...)
	f.etag[key] = strconv.Itoa(f.ver)
	return nil
}

func (f *fakeState) used(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.data[key]
	if !ok {
		return 0
	}
	st := struct {
		Used int `json:"used"`
	}{}
	if err := json.Unmarshal(b, &st); err != nil {
		return -1
	}
	return st.Used
}

func withQuotaEnv(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("WEATHER_QUOTA_FILE", dir)
	t.Setenv("WEATHER_QUOTA_STATE_STORE", weather.DefaultStateStoreName)
}

// ⚠️ **配额必须是整个 supertrade 的一份**，而不是每个服务一份。
//
// 这是用户提这次改造的根本理由：和风按 API key 计费，
// 同一个 key 被 N 个服务用就是一份 1500；各服务各数各的
// ⇒ 实际发出量 N×1500，而 402 的表现只是"天气突然全无"，零告警。
//
// 判别力：把 newQuotaGuard 里的 store 换成 NewFileCounterStore
// （即回到改造前）→ 本用例失败：第二个"服务"看不到第一个用掉的次数。
func TestNewQuotaGuardIsSharedAcrossProcesses(t *testing.T) {
	backend := newFakeState()
	withQuotaEnv(t, t.TempDir())

	// 两个进程各自的装配，连的是同一个 Redis 后端。
	svcA := newQuotaGuard(backend)
	svcB := newQuotaGuard(backend)

	if !svcA.Shared() || !svcB.Shared() {
		t.Fatalf("组件可用时必须用共享计数，实际 A=%q B=%q",
			svcA.Backend(), svcB.Backend())
	}

	before := svcA.Used()
	svcA.Allow()
	if got := svcB.Used(); got != before+1 {
		t.Fatalf("B 必须看到 A 用掉的配额：Used=%d，期望 %d", got, before+1)
	}
}

// ⚠️ **组件没加载时必须回退**，而不是让配额守卫把天气锁死一天。
//
// 缺 dapr 组件的形态不是报错，是"每天每份报告都取不到天气"，
// 只在报告文案里留一句「天气查询失败」—— 看起来像天气坏了，
// 真相是守卫每次 Incr 都失败、被保守判为已用尽。
//
// 判别力：删掉 newQuotaGuard 里的 probeErr 判断（只判 store != nil）
// → 本用例失败：Backend() 会是 "dapr:weather-quota"，
// 而天气会被锁死。这正是本次要堵的静默失效。
func TestNewQuotaGuardFallsBackWhenComponentMissing(t *testing.T) {
	backend := newFakeState()
	backend.failGet = errors.New(`code: ERR, message: couldn't find component "weather-quota"`)
	dir := t.TempDir()
	withQuotaEnv(t, dir)

	q := newQuotaGuard(backend)
	if q.Shared() {
		t.Fatalf("组件不可用时不得声称共享计数（backend=%q）—— "+
			"那会让天气被守卫锁死而没人知道", q.Backend())
	}
	if !strings.HasPrefix(q.Backend(), "file:") && q.Backend() != "file" {
		t.Errorf("应回退到文件后端，实际 %q", q.Backend())
	}
	// 回退后仍必须**可用**（能计数、能挡超限），不能是残废对象。
	if ok, err := q.Allow(); !ok || err != nil {
		t.Errorf("降级后配额守卫必须仍可用: ok=%v err=%v", ok, err)
	}
	if q.Used() < 1 {
		t.Errorf("降级后计数必须真的写下去，Used=%d", q.Used())
	}
	files, _ := os.ReadDir(dir)
	if len(files) == 0 {
		t.Error("降级计数文件必须落在 WEATHER_QUOTA_FILE 指定的目录里")
	}
}

// ⚠️ client 完全没有（裸跑，无 sidecar）时也必须回退而不是崩。
func TestNewQuotaGuardFallsBackWithoutClient(t *testing.T) {
	withQuotaEnv(t, t.TempDir())
	q := newQuotaGuard(nil)
	if q == nil {
		t.Fatal("不得返回 nil 守卫")
	}
	if q.Shared() {
		t.Errorf("无 client 时不得声称共享: %q", q.Backend())
	}
	if ok, err := q.Allow(); !ok || err != nil {
		t.Errorf("无 client 时守卫仍应可用: ok=%v err=%v", ok, err)
	}
}

// 组件可用时，探针**不得**留下计数键 —— 否则装配一次就白白吃掉 1 次配额。
func TestNewQuotaGuardProbeLeavesNoCounter(t *testing.T) {
	backend := newFakeState()
	withQuotaEnv(t, t.TempDir())
	newQuotaGuard(backend)

	backend.mu.Lock()
	defer backend.mu.Unlock()
	for k := range backend.data {
		if strings.Contains(k, "probe") {
			t.Errorf("探针写进了 key 空间: %s（每天凭空少 1 次配额）", k)
		}
	}
}

// ⚠️ wireBizTZ 默认必须是 Asia/Shanghai ——
// 现场 2026-10-19 22:50 推送 → 区间 2026-10-18 的根因就是这里。
//
// 反证：把默认改成 "UTC"（或任何其它）→ 本用例 fail。
//
// 期望值用字面量 "Asia/Shanghai"，不由 `time.Local.String()` 推导：
// 测试机器本身可能是 Local，但 daily 必须落在 CST。
func TestWireBizTZDefaultsToShanghai(t *testing.T) {
	t.Setenv("SALESINSIGHT_BIZ_TZ", "")
	svc := handler.NewServiceImpl(nil, nil)
	if err := wireBizTZ(svc); err != nil {
		t.Fatalf("wireBizTZ 失败: %v", err)
	}
	if got := svc.BizTZ().String(); got != "Asia/Shanghai" {
		t.Errorf("默认时区 = %v, 期望 Asia/Shanghai", got)
	}
}

// env SALESINSIGHT_BIZ_TZ 必须能覆盖默认 ——
// 与 fresh-meat 的 FRESHMEAT_BIZ_TZ 同语义，便于跨机房搬迁。
//
// 反证：删掉 envStr(envStr 那一段 → env 被忽略，Asia/Tokyo 不生效，
// 本用例 fail（仍走 Asia/Shanghai）。
func TestWireBizTZRespectsEnv(t *testing.T) {
	t.Setenv("SALESINSIGHT_BIZ_TZ", "Asia/Tokyo")
	svc := handler.NewServiceImpl(nil, nil)
	if err := wireBizTZ(svc); err != nil {
		t.Fatalf("wireBizTZ 失败: %v", err)
	}
	if got := svc.BizTZ().String(); got != "Asia/Tokyo" {
		t.Errorf("env 覆盖 = %v, 期望 Asia/Tokyo", got)
	}
}

// LoadLocation 失败时必须 fail-fast —— 不许静默退化成 time.Local
// 或 time.UTC（那会复现 2026-10-19 现场 bug）。
//
// 反证：删掉 err 检查，svc.WithBizTZ 不会被调用，本用例 fail（err == nil）。
func TestWireBizTZInvalidEnvErrors(t *testing.T) {
	t.Setenv("SALESINSIGHT_BIZ_TZ", "NotAReal/Zone")
	svc := handler.NewServiceImpl(nil, nil)
	if err := wireBizTZ(svc); err == nil {
		t.Errorf("无效时区应 fail-fast，实际 err = nil —— 那会落入 time.Local，" +
			"把现场 2026-10-19 bug 又带回来")
	}
	if svc.BizTZ() != nil {
		t.Errorf("加载失败时 bizTZ 字段必须保持 nil（不注入），实际 = %v",
			svc.BizTZ())
	}
}
