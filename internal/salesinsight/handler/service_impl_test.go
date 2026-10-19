package handler

import (
	"testing"
	"time"
)

// ⚠️ WithBizTZ 必须把 now 换成「带 location 的 time.Now」——
//
// 否则早班 0:30 CST = 前一日 16:30 UTC，UTC 视角下 daily 算成「昨日」，
// 22:50 推送时落「今天」也是巧合（同一天 14:50 UTC 同样落在 19 号）。
//
// 反证 1: 删掉 s.now = func() ...（保留 bizTZ 字段）→ now() 仍走
//
//	time.Now 默认 UTC，本用例 fail —— s.now().Location() == UTC，
//	不等 Asia/Shanghai。
//
// 反证 2: 把 s.now = func() 换成 s.now = time.Now（覆盖式）→
//
//	位置仍为 UTC，本用例 fail（位置变了但未注入 location）。
//
// 反证 3: 把 time.Now().In(loc) 改成 time.Now()（漏 .In）→
//
//	位置仍为 UTC，本用例 fail。
func TestWithBizTZInjectsLocationIntoNow(t *testing.T) {
	cst, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation(Asia/Shanghai) 失败: %v", err)
	}

	s := NewServiceImpl(nil, nil).WithBizTZ(cst)

	if s.BizTZ() != cst {
		t.Fatalf("BizTZ() = %v, 期望 %v", s.BizTZ(), cst)
	}
	got := s.now()
	if got.Location().String() != cst.String() {
		t.Errorf("now().Location() = %v, 期望 %v（Asia/Shanghai）",
			got.Location().String(), cst.String())
	}
}

// WithBizTZ(loc=nil) 不应 panic，且 bizTZ 字段保持 nil ——
// 让 nil-tolerance 走"未注入"分支（与 fresh-meat 同语义）。
//
// 反证：把 nil 短路改成 s.bizTZ = loc（即 nil 也覆盖）→
// WithBizTZ(nil) 后 BizTZ() 是 nil，但 s.now 仍被改写；
// 调用 s.now() 时拿 nil location 调 .In() 会 panic。
//
// 本用例仅验短路与字段未变：把 nil 短路删掉 → s.bizTZ == nil 仍成立，
// 但 s.now 已被改写，下次 WithBizTZInjectsLocationIntoNow 测试通过就漏了。
// 所以这条**只测短路逻辑**，那条测「注入生效」一起看。
func TestWithBizTZNilIsNoop(t *testing.T) {
	s := NewServiceImpl(nil, nil).WithBizTZ(nil)
	if s.BizTZ() != nil {
		t.Errorf("WithBizTZ(nil) 后 BizTZ() = %v, 期望 nil", s.BizTZ())
	}
	// 短路之后 now 仍应是「没注入 location 的默认」——
	// 不应是 Asia/Shanghai（那条路径只走 WithBizTZ(cst)）。
	// 不强制断言等于 UTC 是因为裸跑时 time.Now() 的 location
	// 可能是 Local（Windows / distroless）。
	if got := s.now(); got.Location().String() == "Asia/Shanghai" {
		t.Errorf("WithBizTZ(nil) 不应改 now，但 now().Location() = %v",
			got.Location().String())
	}
}

// WithNow 覆盖 WithBizTZ：保留两套构造链，让测试可固定时钟。
//
// 反证：把 WithNow 改成只在 s.now == nil 时赋值 → 测试想覆盖
// WithBizTZ 注入后的 now 时会失败；但本测试期望 now 由 WithNow 决定，
// 不论 WithBizTZ 是否被调用。
//
// ⚠️ 时区无关测试依赖 —— 用字面量 2026-10-16 钉死。
func TestWithNowOverridesBizTZ(t *testing.T) {
	cst, _ := time.LoadLocation("Asia/Shanghai")
	want := time.Date(2026, 10, 16, 22, 50, 0, 0, time.UTC)

	s := NewServiceImpl(nil, nil).
		WithBizTZ(cst).
		WithNow(func() time.Time { return want })

	got := s.now()
	if !got.Equal(want) {
		t.Errorf("WithNow 应覆盖 WithBizTZ, got=%v want=%v", got, want)
	}
	if got.Location().String() != time.UTC.String() {
		t.Errorf("WithNow 注入后 location 由 now 自带（测试期望 UTC），got=%v",
			got.Location().String())
	}
}