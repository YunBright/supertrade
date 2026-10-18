package build

import (
	"context"
	"errors"
	"testing"
)

// TestDeltas_上期有数据时必须给出环比 是本次修复的判别力主用例。
//
// 2026-10-17 线上实测：Deltas 全仓从未被赋值 ⇒ has_prev 恒 false，
// 于是头条写「无上期数据，不做环比」，而同一次生成的品类却带着
// 真实环比（蛋类 +36.1%）—— 报告自相矛盾，LLM 把它当成一条 warning。
func TestDeltas_上期有数据时必须给出环比(t *testing.T) {
	fs := newFake()
	fs.catCur = []CategoryRow{
		{Name: "蛋类", Amount: 8000},
		{Name: "百货", Amount: 5000},
	}
	fs.catPrev = []CategoryRow{
		{Name: "蛋类", Amount: 6000},
		{Name: "百货", Amount: 4000},
	}

	f, err := NewBuilder(fs, nil).Build(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}

	if !f.Deltas.HasPrev {
		t.Fatal("上期有数据时 has_prev 必须为 true —— 它恒为 false 会让头条与品类环比互相矛盾")
	}
	if f.Deltas.PrevNetAmount != 10000 {
		t.Errorf("上期销售额 = %v, 期望 10000", f.Deltas.PrevNetAmount)
	}
	if f.Deltas.PrevNetQty != 0 {
		t.Errorf("上期销量（本用例未给数量）= %v, 期望 0", f.Deltas.PrevNetQty)
	}
	// 13000 vs 10000 ⇒ +30%
	if f.Deltas.AmountPct < 29.9 || f.Deltas.AmountPct > 30.1 {
		t.Errorf("环比 = %v, 期望约 +30%%", f.Deltas.AmountPct)
	}
	if f.Deltas.AmountAbs != 3000 {
		t.Errorf("环比绝对值 = %v, 期望 3000", f.Deltas.AmountAbs)
	}
}

// TestDeltas_上期取不到时保守不环比：
// has_prev 必须与"上期是否真的取到数据"一致，否则用户以为这家店第一天开张。
func TestDeltas_上期取不到时保守不环比(t *testing.T) {
	fs := newFake()
	fs.catCur = []CategoryRow{{Name: "蛋类", Amount: 8000}}
	fs.catPrev = nil

	f, err := NewBuilder(fs, nil).Build(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if f.Deltas.HasPrev {
		t.Error("上期无数据时 has_prev 必须为 false")
	}
	if f.Deltas.PrevNetAmount != 0 || f.Deltas.AmountPct != 0 {
		t.Errorf("无上期时数字应全为 0，实际 = %+v", f.Deltas)
	}
}

// TestDeltas_上期品类失败但总额成功时两条轨道各说各话是允许的，
// 但**措辞必须精确** —— 报告里不能同时出现
// 「销售额 +30%（上期 10000 元）」与「本次不做环比」。
//
// 这条是 2026-10-17 把大盘改成独立查询后新出现的语义：
// 总额与品类是两次独立查询，各自有成败。
func TestDeltas_上期品类失败时大盘环比仍可用(t *testing.T) {
	fs := &prevOnlyFailSource{fakeSource: newFake()}
	fs.catCur = []CategoryRow{{Name: "蛋类", Amount: 8000, CostedRow: 100, TotalRow: 100}}
	fs.catPrev = []CategoryRow{{Name: "蛋类", Amount: 10000}}

	f, err := NewBuilder(fs, nil).Build(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("上期品类失败不应让整体失败: %v", err)
	}
	// 品类侧：没有环比
	if f.Categories[0].HasPrev {
		t.Error("上期品类查询失败时品类环比必须不可用")
	}
	// 大盘侧：总额查询成功 ⇒ 环比仍然成立
	if !f.Deltas.HasPrev {
		t.Error("上期总额查询成功时大盘环比应可用 —— 两次查询是独立的")
	}
	// ⚠️ 措辞：只说品类不可用，不得说"本次不做环比"
	issues := f.DataSufficiency.Issues
	if !containsSub(issues, "品类环比不可用") {
		t.Errorf("应说明「品类环比不可用」；实际 = %v", issues)
	}
	if containsSub(issues, "本次不做环比") {
		t.Errorf("大盘环比仍成立时不得出现「本次不做环比」；实际 = %v", issues)
	}
}

// TestDeltas_上期总额失败时大盘环比不可用：
// 总额查询失败会退回"按品类求和"，而那个值天生偏低（被 topN 截断、
// 被丢弃空类目行），拿它当基数算出的环比是假的。
func TestDeltas_上期总额失败时大盘环比不可用(t *testing.T) {
	fs := &prevTotalsFailSource{fakeSource: newFake()}
	fs.catCur = []CategoryRow{{Name: "蛋类", Amount: 8000, CostedRow: 100, TotalRow: 100}}
	// 故意给上期品类**真实**数据：若仍算出大盘环比，
	// 说明它用的是降级求和值而不是"承认上期不可用"。
	fs.catPrev = []CategoryRow{{Name: "蛋类", Amount: 10000, CostedRow: 100, TotalRow: 100}}

	f, err := NewBuilder(fs, nil).Build(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("Build 不应中止: %v", err)
	}
	if !f.Categories[0].HasPrev {
		t.Error("上期品类有数据时品类环比应可用（用来证明失败只发生在总额侧）")
	}
	if !containsSub(f.DataSufficiency.Issues, "大盘总额查询失败") {
		t.Errorf("总额查询失败必须显式降级告警；实际 = %v", f.DataSufficiency.Issues)
	}
	if f.Deltas.HasPrev {
		t.Error("上期总额取不到时大盘环比不得成立 —— 降级求和值偏低，算出的环比是假的")
	}
}

// prevTotalsFailSource 只在上期的**总额**查询时报错。
type prevTotalsFailSource struct {
	*fakeSource
}

func (p *prevTotalsFailSource) TotalsSales(ctx context.Context, b, from, to string) (TotalsRow, error) {
	if from == "2026-10-14" {
		return TotalsRow{}, errors.New("上期总额不可用")
	}
	return p.fakeSource.TotalsSales(ctx, b, from, to)
}

// TestDeltas_上期有行但金额为0时不硬算环比：
// prev==0 时环比在数学上无意义（pctChange 返回 0），
// 此时给出 has_prev=true 会渲染成"环比 +0.0%（上期 0.00 元）"——
// 一个看起来正常、实则无意义的结果。
func TestDeltas_上期有行但金额为0时不硬算环比(t *testing.T) {
	fs := newFake()
	fs.catCur = []CategoryRow{{Name: "蛋类", Amount: 8000}}
	fs.catPrev = []CategoryRow{{Name: "蛋类", Amount: 0}}

	f, err := NewBuilder(fs, nil).Build(context.Background(), testOptions())
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if f.Deltas.HasPrev {
		t.Error("上期金额为 0 时没有可用基数，has_prev 应为 false")
	}
	if f.Deltas.AmountPct != 0 {
		t.Errorf("无基数时环比必须为 0（不是 +Inf），实际 = %v", f.Deltas.AmountPct)
	}
}
