package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

func d(s string) decimal.Decimal {
	v, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return v
}

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

// fingerprint 把输出压成字符串,用于"两次运行逐字段相等"。
//
// ⚠️ 不直接 reflect.DeepEqual:decimal.Decimal 内部是 *big.Int,
// 值相等的两个实例指针不同 ⇒ 深比较会假失败。
func fingerprint(o Output) string {
	var b strings.Builder
	for _, m := range o.Matches {
		fmt.Fprintf(&b, "M|%s|%s|%s|%s|%s|%s|%s|%s|%s\n",
			m.Channel, m.BizDate.Format("20060102"), m.BankExternalID, m.CashKey,
			m.ExpectedAmount.String(), m.ActualAmount.String(), m.Fee.String(),
			m.Kind, m.DedupeKey)
	}
	for _, x := range o.Diffs {
		fmt.Fprintf(&b, "D|%s|%s|%s|%s|%s|%s|%s|%s|%s\n",
			x.Kind, x.Channel, x.CashRef, x.BankRef,
			x.Expected.String(), x.Actual.String(), x.DiffAmount.String(),
			x.Status, x.DedupeKey)
	}
	fmt.Fprintf(&b, "S|%s|%d|%s|%s|%s|%d|%d",
		o.Summary.MatchRate.String(), o.Summary.DiffCount,
		o.Summary.CashTotal.String(), o.Summary.BankTotal.String(), o.Summary.FeeTotal.String(),
		o.Summary.MatchedCount, o.Summary.EligibleCount)
	return b.String()
}

func baseInput() Input {
	return Input{
		BranchID:    "00",
		BizDateFrom: day("2026-10-30"),
		BizDateTo:   day("2026-10-31"),
		Cash: []CashAggregate{
			{BizDate: day("2026-10-30"), PayWay: "SWX", Method: model.MethodWechat, AmountNet: d("100"), TxnCount: 2},
			{BizDate: day("2026-10-30"), PayWay: "RMB", Method: model.MethodCash, AmountNet: d("50"), TxnCount: 3},
			{BizDate: day("2026-10-31"), PayWay: "SZF", Method: model.MethodBankCard, AmountNet: d("30"), TxnCount: 1},
		},
		Bank: []BankFlow{
			{ExternalID: "W1", Channel: model.MethodWechat, ValueDate: day("2026-10-31"), Amount: d("99"), Fee: d("1")},
			{ExternalID: "B1", Channel: model.MethodBankCard, ValueDate: day("2026-10-31"), Amount: d("30")},
		},
	}
}

// ① 确定性:同一输入跑两次,输出逐字段相等。
func TestRunDeterministic(t *testing.T) {
	out1 := Run(baseInput())
	out2 := Run(baseInput())
	assert.Equal(t, fingerprint(out1), fingerprint(out2), "同输入两次运行必须逐字段相等")

	// 乱序输入也必须得到同一结果(排序在引擎内部完成)。
	in := baseInput()
	in.Bank[0], in.Bank[1] = in.Bank[1], in.Bank[0]
	assert.Equal(t, fingerprint(out1), fingerprint(Run(in)), "输入顺序变化不应改变结果")
}

// ② 手续费单列:到账 99 + 手续费 1、应收 100 ⇒ fee_aware,不产生金额差异。
func TestRunFeeAwareKeepsFeeSeparate(t *testing.T) {
	in := Input{
		BranchID:    "00",
		BizDateFrom: day("2026-10-30"),
		BizDateTo:   day("2026-10-30"),
		Cash: []CashAggregate{
			{BizDate: day("2026-10-30"), PayWay: "SWX", Method: model.MethodWechat, AmountNet: d("100"), TxnCount: 1},
		},
		Bank: []BankFlow{
			{ExternalID: "W1", Channel: model.MethodWechat, ValueDate: day("2026-10-30"), Amount: d("99"), Fee: d("1")},
		},
	}
	out := Run(in)
	require.Len(t, out.Matches, 1)
	require.Len(t, out.Diffs, 0, "手续费已单列时不应再产生差异")
	m := out.Matches[0]
	assert.Equal(t, model.MatchKindFeeAware, m.Kind)
	assert.Equal(t, "100", m.ExpectedAmount.String())
	assert.Equal(t, "99", m.ActualAmount.String())
	assert.Equal(t, "1", m.Fee.String(), "手续费必须单列,不能并入金额")
	assert.Equal(t, "1", out.Summary.FeeTotal.String())
}

// ③ 容差边界:T=1 时 D+2 到账不匹配,产出 time_lag + single_sided_bank。
func TestRunToleranceBoundary(t *testing.T) {
	in := Input{
		BranchID:    "00",
		BizDateFrom: day("2026-10-30"),
		BizDateTo:   day("2026-10-30"),
		Rules:       RuleSet{ArrivalToleranceDays: 1},
		Cash: []CashAggregate{
			{BizDate: day("2026-10-30"), PayWay: "SWX", Method: model.MethodWechat, AmountNet: d("100"), TxnCount: 1},
		},
		Bank: []BankFlow{
			{ExternalID: "W1", Channel: model.MethodWechat, ValueDate: day("2026-11-01"), Amount: d("100")},
		},
	}
	out := Run(in)
	require.Len(t, out.Matches, 0, "D+2 超出 T=1 窗口,不应匹配")
	require.Len(t, out.Diffs, 2)

	kinds := map[string]bool{}
	for _, x := range out.Diffs {
		kinds[x.Kind] = true
	}
	assert.True(t, kinds[model.DiffKindTimeLag], "窗口内无流水的收银侧必须标 time_lag")
	assert.True(t, kinds[model.DiffKindSingleBank], "窗口外的银行流水是单边")
}

// ④ 未映射 pay_way ⇒ unmapped_pay_way,且不计入匹配率分母。
func TestRunUnmappedPayWayExcludedFromRate(t *testing.T) {
	in := Input{
		BranchID:    "00",
		BizDateFrom: day("2026-10-30"),
		BizDateTo:   day("2026-10-30"),
		Cash: []CashAggregate{
			{BizDate: day("2026-10-30"), PayWay: "LEP", Method: model.MethodUnmapped, AmountNet: d("20"), TxnCount: 1},
			{BizDate: day("2026-10-30"), PayWay: "CHG", Method: model.MethodChange, AmountNet: d("5"), TxnCount: 1},
			{BizDate: day("2026-10-30"), PayWay: "SWX", Method: model.MethodWechat, AmountNet: d("100"), TxnCount: 1},
		},
		Bank: []BankFlow{
			{ExternalID: "W1", Channel: model.MethodWechat, ValueDate: day("2026-10-30"), Amount: d("100")},
		},
	}
	out := Run(in)
	require.Len(t, out.Diffs, 1)
	assert.Equal(t, model.DiffKindUnmappedPayWay, out.Diffs[0].Kind)

	assert.Equal(t, 1, out.Summary.EligibleCount, "分母只数参与匹配的支付方式(未映射/找零/现金都不进)")
	assert.Equal(t, 1, out.Summary.MatchedCount)
	assert.Equal(t, "1", out.Summary.MatchRate.String())

	// 找零不计收款 ⇒ CashTotal = 20(未映射) + 100(微信),不含 5。
	assert.Equal(t, "120", out.Summary.CashTotal.String())
}

// ⑤ 重复流水 ⇒ duplicate,而不是两次匹配。
func TestRunDuplicateBankFlow(t *testing.T) {
	in := Input{
		BranchID:    "00",
		BizDateFrom: day("2026-10-30"),
		BizDateTo:   day("2026-10-30"),
		Cash: []CashAggregate{
			{BizDate: day("2026-10-30"), PayWay: "SWX", Method: model.MethodWechat, AmountNet: d("100"), TxnCount: 1},
		},
		Bank: []BankFlow{
			{ExternalID: "W1", Channel: model.MethodWechat, ValueDate: day("2026-10-30"), Amount: d("100")},
			{ExternalID: "W2", Channel: model.MethodWechat, ValueDate: day("2026-10-30"), Amount: d("100")},
		},
	}
	out := Run(in)
	require.Len(t, out.Matches, 1, "同金额多笔流水只允许匹配一次")
	require.Len(t, out.Diffs, 1)
	assert.Equal(t, model.DiffKindDuplicate, out.Diffs[0].Kind)
}

// ⑥ Prior 已处理差异 ⇒ 状态保留,并标记 CarriedOver。
func TestRunPriorPreservesManualResult(t *testing.T) {
	in := Input{
		BranchID:    "00",
		BizDateFrom: day("2026-10-30"),
		BizDateTo:   day("2026-10-30"),
		Cash: []CashAggregate{
			{BizDate: day("2026-10-30"), PayWay: "SWX", Method: model.MethodWechat, AmountNet: d("100"), TxnCount: 1},
		},
	}
	first := Run(in)
	require.Len(t, first.Diffs, 1)
	key := first.Diffs[0].DedupeKey

	handled := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	in.Prior = []DiffState{{
		DedupeKey:      key,
		Status:         model.DiffStatusHandled,
		ResolutionCode: "channel_delay",
		ResolutionNote: "次日到账,已核对",
		HandledBy:      "u1",
		HandledAt:      &handled,
	}}
	second := Run(in)
	require.Len(t, second.Diffs, 1)
	assert.Equal(t, model.DiffStatusHandled, second.Diffs[0].Status, "重算不得冲掉人工处理结果")
	assert.Equal(t, "channel_delay", second.Diffs[0].ResolutionCode)
	assert.True(t, second.Diffs[0].CarriedOver)
}
