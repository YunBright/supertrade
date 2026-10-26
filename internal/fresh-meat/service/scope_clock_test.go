package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YunBright/authkit/userinfo"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// fakeClockReader 是一个"永远成功"的权限读取器（配合可控时钟用）。
type fakeClockReader struct {
	calls int
}

func (f *fakeClockReader) GetBranchPermissions(_ context.Context, userID, branchID string) (*userinfo.BranchPermissions, error) {
	f.calls++
	return &userinfo.BranchPermissions{Branches: []userinfo.BranchPermissionsRow{{
		BranchID: branchID,
		Scopes:   []string{"freshmeat:view"},
	}}}, nil
}

// TestGetEffectiveScopes_CacheTTLUsesInjectedClock 锁一个**极隐蔽**的 bug：
//
//	缓存 entry 的时间戳是用 **s.now()**（可注入时钟）打的，
//	但过期判断用的是 **time.Since**（真实墙钟）。
//	⇒ 两个时钟不同源 ⇒ 差值可能是**负数**（注入的"现在"比真实时间靠后）
//	⇒ 负数 < TTL 恒成立 ⇒ **缓存永远不过期**。
//
// 症状：改了 TTL / 注入时钟后，缓存行为完全失真，而且**不报错**。
// 这个 bug 2026-10-26 在 fresh-meat 的 stale-while-error 改动里被抓出来；
// 同一模式在 stocktake / settlement-auditor 域也各有一份，一并修了。
//
// 断言：注入时钟往前推超过 TTL ⇒ 缓存**必须**过期（重新打 userd）。
func TestGetEffectiveScopes_CacheTTLUsesInjectedClock(t *testing.T) {
	svc, _ := setupTestService(t)

	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return now })
	svc.SetScopeTTL(60 * time.Second)

	reader := &fakeClockReader{}
	svc.SetBranchPermissionsReader(reader)

	// 第一次 ⇒ 打 userd 并缓存。
	if _, err := svc.GetEffectiveScopes(context.Background(), "u-1", "00"); err != nil {
		t.Fatalf("首次取权限应成功: %v", err)
	}
	if reader.calls != 1 {
		t.Fatalf("首次应打 userd 一次, got %d", reader.calls)
	}

	// TTL 内 ⇒ 命中缓存,不再打 userd。
	if _, err := svc.GetEffectiveScopes(context.Background(), "u-1", "00"); err != nil {
		t.Fatalf("TTL 内取权限应成功: %v", err)
	}
	if reader.calls != 1 {
		t.Fatalf("TTL 内应命中缓存(不重复打 userd), calls=%d", reader.calls)
	}

	// 注入时钟往前推 2 分钟(超过 60s TTL)⇒ **必须**过期。
	//
	// ⚠️ 如果这里还是用 time.Since(entry.at)：
	//    entry.at 是注入的 2026-10-26 10:00，真实墙钟是 2026-10-26 17:xx
	//    ⇒ time.Since(entry.at) 为**负数** ⇒ 恒 < TTL ⇒ 缓存永不过期
	//    ⇒ reader.calls 停在 1 ⇒ 本断言失败。
	now = now.Add(2 * time.Minute)
	if _, err := svc.GetEffectiveScopes(context.Background(), "u-1", "00"); err != nil {
		t.Fatalf("TTL 过后取权限仍应成功: %v", err)
	}
	if reader.calls != 2 {
		t.Fatalf("TTL 过后应重新打 userd（缓存必须按注入时钟过期）, calls=%d", reader.calls)
	}
}

// TestGetEffectiveScopes_StaleFallbackDoesNotExtendLife 锁"兜底不续命"。
//
// 用过期缓存兜底**不能**把缓存续命成永久有效 —— 否则一旦某次权限被收回，
// 兜底会一直放行（超出 staleTTL 也照样放行）。
func TestGetEffectiveScopes_StaleFallbackDoesNotExtendLife(t *testing.T) {
	svc, _ := setupTestService(t)

	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return now })
	svc.SetScopeTTL(60 * time.Second)
	svc.SetScopeStaleTTL(5 * time.Minute)

	svc.SetBranchPermissionsReader(&fakeClockReader{})
	if _, err := svc.GetEffectiveScopes(context.Background(), "u-1", "00"); err != nil {
		t.Fatalf("首次取权限应成功: %v", err)
	}

	// 缓存过期 + userd 持续失败 ⇒ 每次都走兜底。
	now = now.Add(2 * time.Minute)
	bad := &fakePermissionsReader{fn: func(string, string) (*userinfo.BranchPermissions, error) {
		return nil, errors.New("userd 抖动")
	}}
	svc.SetBranchPermissionsReader(bad)

	for i := 0; i < 3; i++ {
		if _, err := svc.GetEffectiveScopes(context.Background(), "u-1", "00"); err != nil {
			t.Fatalf("第 %d 次兜底应成功: %v", i+1, err)
		}
	}

	// 兜底期间每次都应真的去问 userd（= 没续命）。
	if bad.calls != 3 {
		t.Fatalf("兜底不应续命（每次都应打 userd）, calls=%d", bad.calls)
	}

	// 再推 10 分钟 ⇒ 超出 staleTTL(5min) ⇒ 必须 503。
	now = now.Add(10 * time.Minute)
	if _, err := svc.GetEffectiveScopes(context.Background(), "u-1", "00"); !errors.Is(err, service.ErrUserInfoUnavailable) {
		t.Fatalf("超出 staleTTL 应返 ErrUserInfoUnavailable, got %v", err)
	}
}
