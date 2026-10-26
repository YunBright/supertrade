package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YunBright/authkit/userinfo"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// fakePermissionsReader 是 BranchPermissionsReader 的测试替身。
//
// 行为:
//   - fn 返 nil error ⇒ 按 branches 返权限矩阵;
//   - fn 返 error ⇒ 模拟 userd 抖动(机器 token 交换被 cancel / 跨主机 invoke 失败)。
type fakePermissionsReader struct {
	fn func(userID, branchID string) (*userinfo.BranchPermissions, error)
	// calls 记录被调了几次(断言"缓存命中时不再打 userd")。
	calls int
}

func (f *fakePermissionsReader) GetBranchPermissions(_ context.Context, userID, branchID string) (*userinfo.BranchPermissions, error) {
	f.calls++
	if f.fn == nil {
		return nil, errors.New("fake: fn 未设置")
	}
	return f.fn(userID, branchID)
}

func permsWith(branches ...userinfo.BranchPermissionsRow) *userinfo.BranchPermissions {
	return &userinfo.BranchPermissions{Branches: branches}
}

// TestGetEffectiveScopes_StaleCacheFallbackOnUserdFailure 锁 stale-while-error：
//
//	2026-10-26 生产现场(16:49:01)：
//	  门店打开「每日作业」页 ⇒ 并发十几个请求，每个都要过 requireScope。
//	  权限缓存**恰好过期** + userd 那一瞬抖了(机器 token 交换 context canceled)
//	  ⇒ 全部并发请求一起拿到 ErrUserInfoUnavailable ⇒ **整页 503**。
//
//	⇒ 修法：userd 失败时，若有过期但仍在 staleTTL 内的缓存 ⇒ 用它兜底。
//
// 断言三件事：
//  1. userd 失败时仍返出**之前授权过的** scopes（不 503）；
//  2. 兜底**不刷新** TTL（下一轮仍会真的去问 userd，userd 恢复后自动回到新值）；
//  3. 超出 staleTTL ⇒ 宁可 503 也不放行（权限可能早已收回）。
func TestGetEffectiveScopes_StaleCacheFallbackOnUserdFailure(t *testing.T) {
	svc, _ := setupTestService(t)

	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return now })

	// 缓存 TTL 60s；stale 兜底窗口 5 分钟。
	svc.SetScopeTTL(60 * time.Second)
	svc.SetScopeStaleTTL(5 * time.Minute)

	// 第一次：userd 正常 ⇒ 拿到权限并进缓存。
	ok := &fakePermissionsReader{fn: func(string, string) (*userinfo.BranchPermissions, error) {
		return permsWith(userinfo.BranchPermissionsRow{
			BranchID: "00",
			Scopes:   []string{"freshmeat:view", "freshmeat:write"},
		}), nil
	}}
	svc.SetBranchPermissionsReader(ok)

	scopes, err := svc.GetEffectiveScopes(context.Background(), "u-1", "00")
	if err != nil {
		t.Fatalf("首次取权限应成功: %v", err)
	}
	if len(scopes) != 2 {
		t.Fatalf("首次应拿到 2 条 scope, got %v", scopes)
	}

	// 缓存过期(userd 此刻开始抖)。
	now = now.Add(2 * time.Minute)

	bad := &fakePermissionsReader{fn: func(string, string) (*userinfo.BranchPermissions, error) {
		return nil, errors.New("userinfo: 机器身份被拒: 用服务账号换机器 token 失败(app-id=userd): rpc error: code = Canceled desc = context canceled")
	}}
	svc.SetBranchPermissionsReader(bad)

	// ② userd 失败 ⇒ 用过期缓存兜底,**不 503**。
	scopes, err = svc.GetEffectiveScopes(context.Background(), "u-1", "00")
	if err != nil {
		t.Fatalf("userd 抖动时应走过期缓存兜底,不该报错: %v", err)
	}
	if len(scopes) != 2 || scopes[0] != "freshmeat:view" {
		t.Fatalf("兜底应返回先前授权过的 scopes, got %v", scopes)
	}

	// ③ 兜底**不刷新** TTL：再调一次仍会真的去问 userd（说明没把缓存续命）。
	before := bad.calls
	if _, err := svc.GetEffectiveScopes(context.Background(), "u-1", "00"); err != nil {
		t.Fatalf("第二次兜底同样应成功: %v", err)
	}
	if bad.calls != before+1 {
		t.Fatalf("兜底不应刷新 TTL(仍应每次打 userd): calls %d→%d", before, bad.calls)
	}

	// ④ 超出 staleTTL ⇒ 宁可 503 也不放行。
	now = now.Add(10 * time.Minute)
	if _, err := svc.GetEffectiveScopes(context.Background(), "u-1", "00"); !errors.Is(err, service.ErrUserInfoUnavailable) {
		t.Fatalf("超出 staleTTL 应返 ErrUserInfoUnavailable, got %v", err)
	}
}

// TestGetEffectiveScopes_StaleFallbackDisabled 锁"兜底可关闭"。
//
// 有些场景(比如审计要求权限实时准确)宁可 503 也不愿用旧权限 ⇒ 必须能关掉。
func TestGetEffectiveScopes_StaleFallbackDisabled(t *testing.T) {
	svc, _ := setupTestService(t)

	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return now })
	svc.SetScopeTTL(60 * time.Second)
	svc.SetScopeStaleTTL(0) // 关闭兜底

	ok := &fakePermissionsReader{fn: func(string, string) (*userinfo.BranchPermissions, error) {
		return permsWith(userinfo.BranchPermissionsRow{
			BranchID: "00",
			Scopes:   []string{"freshmeat:view"},
		}), nil
	}}
	svc.SetBranchPermissionsReader(ok)
	if _, err := svc.GetEffectiveScopes(context.Background(), "u-1", "00"); err != nil {
		t.Fatalf("首次取权限应成功: %v", err)
	}

	now = now.Add(2 * time.Minute)
	svc.SetBranchPermissionsReader(&fakePermissionsReader{fn: func(string, string) (*userinfo.BranchPermissions, error) {
		return nil, errors.New("userd 抖动")
	}})

	if _, err := svc.GetEffectiveScopes(context.Background(), "u-1", "00"); !errors.Is(err, service.ErrUserInfoUnavailable) {
		t.Fatalf("关闭兜底后 userd 失败应返 ErrUserInfoUnavailable, got %v", err)
	}
}

// TestGetEffectiveScopes_StaleFallbackDoesNotLeakAcrossBranches 锁"兜底不串店"。
//
// 缓存 key 是 (userID, branchID) ⇒ 店 00 的缓存**不能**用来放行店 01。
// 若串了 ⇒ 店员在 00 有权限,就能顺带在 01 拿到权限 ⇒ 越权。
func TestGetEffectiveScopes_StaleFallbackDoesNotLeakAcrossBranches(t *testing.T) {
	svc, _ := setupTestService(t)

	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return now })
	svc.SetScopeTTL(60 * time.Second)
	svc.SetScopeStaleTTL(5 * time.Minute)

	svc.SetBranchPermissionsReader(&fakePermissionsReader{fn: func(_, branchID string) (*userinfo.BranchPermissions, error) {
		return permsWith(userinfo.BranchPermissionsRow{
			BranchID: branchID,
			Scopes:   []string{"freshmeat:view"},
		}), nil
	}})
	if _, err := svc.GetEffectiveScopes(context.Background(), "u-1", "00"); err != nil {
		t.Fatalf("首次取权限应成功: %v", err)
	}

	now = now.Add(2 * time.Minute)
	svc.SetBranchPermissionsReader(&fakePermissionsReader{fn: func(string, string) (*userinfo.BranchPermissions, error) {
		return nil, errors.New("userd 抖动")
	}})

	// 店 00：有缓存 ⇒ 兜底成功。
	if _, err := svc.GetEffectiveScopes(context.Background(), "u-1", "00"); err != nil {
		t.Fatalf("店 00 应兜底成功: %v", err)
	}
	// 店 01：没有缓存 ⇒ 不能借用店 00 的 ⇒ 必须 503。
	if _, err := svc.GetEffectiveScopes(context.Background(), "u-1", "01"); !errors.Is(err, service.ErrUserInfoUnavailable) {
		t.Fatalf("店 01 无缓存,应返 ErrUserInfoUnavailable(不能借店 00 的), got %v", err)
	}
}
