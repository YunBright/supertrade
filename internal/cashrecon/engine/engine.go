// Package engine 是超市流水对账的**纯函数匹配引擎**。
//
// # 为什么必须是纯函数
//
//	preview(dry-run) 与 apply 走的是**同一段**匹配逻辑 —— 预览看到的匹配与差异,
//	落库时必须逐字段相同。任何 time.Now / rand / 数据库读取进到这里,
//	都会让"预览"与"实际"分叉,而出纳会先信预览、再被落库结果吓到。
//
// ⇒ 本包**只依赖 decimal 与标准库**;业务日、快照时间、人工处理历史全部由
// service 传入(见 Input)。同一份 Input 两次 Run 的输出逐字段相等(有单测锁住)。
//
// # 三条红线的落点
//
//  1. 差异只产出、不删除 —— 已有人工处理结果的差异按 dedupe_key 保留原状态
//     (Input.Prior),输出里带 CarriedOver 供 service 写审计日志。
//  3. 手续费单列 —— Match/Diff 永远同时保留 expected / actual / fee,
//     不允许把到账金额塞回"应收"凑平。
//
// # 金额口径
//
//	正数 = 收款,负数 = 冲销/退款(SUM 天然处理,txn_count 另计)。
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
)

// amountTolerance 是金额比对的容差(0.01 元)。
//
// 写死而不可配:差异报表的"匹配"口径必须唯一 —— 一旦按门店可配,
// 同一个差异在不同门店会被判成不同结论,而两边都"合理"。
var amountTolerance = decimal.New(1, -2)

// maxToleranceDays 是 T+N 到账窗口的上限。
//
// 超过 7 天还不到的渠道流水,真实原因通常不是"到账慢",而是漏导/漏配,
// 继续放大窗口只会把真正的差异藏进"还在路上"。
const maxToleranceDays = 7

// CashAggregate 是一个「门店 × 业务日 × 支付方式」的收银净额聚合。
//
// ⚠️ 不拉明细:cash-recon 只按日×方式聚合(单店日 5000 笔,逐笔分页会放大成万级行)。
type CashAggregate struct {
	BizDate   time.Time
	PayWay    string
	Method    string
	AmountNet decimal.Decimal
	TxnCount  int
}

// BankFlow 是一条银行/渠道到账流水。
type BankFlow struct {
	// ID 是本地 recon_bank_flows.id(可空;为空时 DedupeKey 落到 ExternalID)。
	ID            string
	ExternalID    string
	Channel       string
	ValueDate     time.Time
	Amount        decimal.Decimal
	Fee           decimal.Decimal
	SettleBatchNo string
}

// RuleSet 是本次匹配用到的规则(由 service 从 recon_rules 解析后传入)。
type RuleSet struct {
	// ArrivalToleranceDays = T+N 到账窗口;<=0 用默认 1,>7 截到 7。
	ArrivalToleranceDays int
}

// DiffState 是一条已存在差异的人工处理结果。
//
// 重算时按 DedupeKey 命中且状态属于 DiffSettledStatuses 的,**只保留人工字段**,
// 引擎字段(金额/引用)照常更新 —— 否则出纳每天的处理会被下一次 apply 冲掉。
type DiffState struct {
	DedupeKey      string
	Status         string
	ResolutionCode string
	ResolutionNote string
	HandledBy      string
	HandledAt      *time.Time
}

// Input 是一次匹配的全部输入。
type Input struct {
	BranchID    string
	BizDateFrom time.Time
	BizDateTo   time.Time
	Cash        []CashAggregate
	Bank        []BankFlow
	Rules       RuleSet
	Prior       []DiffState
}

// Match 是一条匹配结果。
type Match struct {
	Channel        string
	BizDate        time.Time
	BankFlowID     string
	BankExternalID string
	CashKey        string
	ExpectedAmount decimal.Decimal
	ActualAmount   decimal.Decimal
	// Fee 永远单列(红线 3)。
	Fee       decimal.Decimal
	Kind      string
	DedupeKey string
}

// Diff 是一条差异。
type Diff struct {
	Kind           string
	Channel        string
	CashRef        string
	BankRef        string
	Expected       decimal.Decimal
	Actual         decimal.Decimal
	DiffAmount     decimal.Decimal
	Status         string
	ResolutionCode string
	ResolutionNote string
	HandledBy      string
	HandledAt      *time.Time
	DedupeKey      string
	// CarriedOver 表示这条差异的状态来自上一批人工处理(不是本次引擎新判的 open)。
	CarriedOver bool
}

// Summary 是本次匹配的汇总。
type Summary struct {
	// MatchRate 是匹配率(0~1),分母 = 参与匹配的收银聚合条数。
	MatchRate decimal.Decimal
	DiffCount int
	CashTotal decimal.Decimal
	BankTotal decimal.Decimal
	FeeTotal  decimal.Decimal
	// MatchedCount / EligibleCount 让调用方不必反解 MatchRate(分母口径写死)。
	MatchedCount  int
	EligibleCount int
}

// Output 是匹配结果。
type Output struct {
	Matches []Match
	Diffs   []Diff
	Summary Summary
}

// matchingMethods 是**需要与银行/渠道流水勾对**的支付方式。
//
// ⚠️ 现金与找零**不参与**银行流水匹配:
//   - cash 走钱箱实盘(FR-04),不可能出现在银行流水里;
//   - change 是找零,**不计收款**。
//
// 若不排除,每一天都会为 cash 产出一条 single_sided_cash / time_lag,
// 真正的差异会被淹没在噪音里。
var matchingMethods = map[string]bool{
	model.MethodWechat:      true,
	model.MethodBankCard:    true,
	model.MethodStoredValue: true,
	model.MethodCredit:      true,
}

// toleranceDays 归一化 T+N。
func toleranceDays(r RuleSet) int {
	t := r.ArrivalToleranceDays
	switch {
	case t <= 0:
		return 1
	case t > maxToleranceDays:
		return maxToleranceDays
	default:
		return t
	}
}

// dateKey 把日期归一化成 yyyymmdd 整数,避免时区/时刻参与比较。
func dateKey(t time.Time) int {
	return t.Year()*10000 + int(t.Month())*100 + t.Day()
}

// inWindow 判断到账日是否落在 [bizDate, bizDate+T]。
//
// 按**日历日**比较(去掉时刻),因为业务日与到账日都是日期口径 ——
// 带时刻比较会让 23:59:59 到账漏出窗口。
func inWindow(bizDate, valueDate time.Time, days int) bool {
	to := bizDate.AddDate(0, 0, days)
	value := time.Date(valueDate.Year(), valueDate.Month(), valueDate.Day(), 0, 0, 0, 0, time.UTC)
	biz := time.Date(bizDate.Year(), bizDate.Month(), bizDate.Day(), 0, 0, 0, 0, time.UTC)
	return !value.Before(biz) && !value.After(to)
}

// cashKey 是收银侧聚合键(pay_way@yyyymmdd),对账工作台按它下钻。
func cashKey(c CashAggregate) string {
	return fmt.Sprintf("%s@%s", c.PayWay, c.BizDate.Format("20060102"))
}

// dedupeKey 是本条结果的幂等键。
//
// 口径:**同一批次内**同一个业务事实只应有一行;换批次重算是新批次,自然允许重建。
func dedupeKey(branch, channel, cashKey, bankExternal, kind string) string {
	sum := sha256.Sum256([]byte(branch + "|" + channel + "|" + cashKey + "|" + bankExternal + "|" + kind))
	return hex.EncodeToString(sum[:])
}

// diffAmount 返回 actual - expected(正数 = 多到账)。
func diffAmount(expected, actual decimal.Decimal) decimal.Decimal {
	return actual.Sub(expected)
}

// near 判断两个金额是否在容差内相等。
func near(a, b decimal.Decimal) bool {
	return a.Sub(b).Abs().Cmp(amountTolerance) <= 0
}

// Run 执行三遍匹配。确定性、幂等、无副作用。
func Run(in Input) Output {
	days := toleranceDays(in.Rules)
	prior := map[string]DiffState{}
	for _, p := range in.Prior {
		prior[p.DedupeKey] = p
	}

	// ---- 输入规整:排序是确定性的前提 ----
	cash := make([]CashAggregate, 0, len(in.Cash))
	for _, c := range in.Cash {
		if dateKey(c.BizDate) < dateKey(in.BizDateFrom) || dateKey(c.BizDate) > dateKey(in.BizDateTo) {
			continue
		}
		cash = append(cash, c)
	}
	sort.SliceStable(cash, func(i, j int) bool {
		if cash[i].BizDate.Equal(cash[j].BizDate) {
			if cash[i].Method == cash[j].Method {
				return cash[i].PayWay < cash[j].PayWay
			}
			return cash[i].Method < cash[j].Method
		}
		return cash[i].BizDate.Before(cash[j].BizDate)
	})

	bank := make([]BankFlow, len(in.Bank))
	copy(bank, in.Bank)
	sort.SliceStable(bank, func(i, j int) bool {
		if !bank[i].ValueDate.Equal(bank[j].ValueDate) {
			return bank[i].ValueDate.Before(bank[j].ValueDate)
		}
		if bank[i].Channel != bank[j].Channel {
			return bank[i].Channel < bank[j].Channel
		}
		if c := bank[i].Amount.Cmp(bank[j].Amount); c != 0 {
			return c < 0
		}
		if bank[i].ExternalID != bank[j].ExternalID {
			return bank[i].ExternalID < bank[j].ExternalID
		}
		return bank[i].ID < bank[j].ID
	})

	// ---- 重复流水:同渠道 + 同日 + 同金额 + 同手续费 ----
	//
	// 多笔完全相同的到账更可能是重复导入/重复结算,而不是两笔真实收款。
	// 只保留第一笔参与匹配,其余直接产出 duplicate 差异(不参与匹配)。
	dup := map[int]bool{}
	group := map[string][]int{}
	for i, f := range bank {
		k := fmt.Sprintf("%s|%d|%s|%s", f.Channel, dateKey(f.ValueDate), f.Amount.String(), f.Fee.String())
		group[k] = append(group[k], i)
	}
	for _, idxs := range group {
		for j := 1; j < len(idxs); j++ {
			dup[idxs[j]] = true
		}
	}

	// ---- 可匹配的收银聚合与银行流水的索引 ----
	eligible := make([]int, 0, len(cash))
	for i, c := range cash {
		if matchingMethods[c.Method] {
			eligible = append(eligible, i)
		}
	}
	bankForChannel := func(channel string, bizDate time.Time) int {
		n := 0
		for _, f := range bank {
			if f.Channel == channel && inWindow(bizDate, f.ValueDate, days) {
				n++
			}
		}
		return n
	}

	var matches []Match
	var diffs []Diff
	used := map[int]bool{}

	// ---- 第一遍:精确匹配(流水无手续费且到账 = 应收) ----
	// ---- 第二遍:手续费感知匹配(到账 = 应收 - 实际手续费) ----
	for _, ci := range eligible {
		c := cash[ci]
		if c.AmountNet.IsZero() {
			continue
		}
		for bi, f := range bank {
			if used[bi] || dup[bi] || f.Channel != c.Method || !inWindow(c.BizDate, f.ValueDate, days) {
				continue
			}
			var kind string
			switch {
			case f.Fee.IsZero() && near(f.Amount, c.AmountNet):
				kind = model.MatchKindExact
			case f.Fee.GreaterThan(decimal.Zero) && near(f.Amount, c.AmountNet.Sub(f.Fee)):
				kind = model.MatchKindFeeAware
			default:
				continue
			}
			used[bi] = true
			matches = append(matches, Match{
				Channel:        c.Method,
				BizDate:        c.BizDate,
				BankFlowID:     f.ID,
				BankExternalID: f.ExternalID,
				CashKey:        cashKey(c),
				ExpectedAmount: c.AmountNet,
				ActualAmount:   f.Amount,
				Fee:            f.Fee,
				Kind:           kind,
				DedupeKey:      dedupeKey(in.BranchID, c.Method, cashKey(c), bankRef(f), kind),
			})
			break
		}
	}

	// ---- 第三遍:差额/单边/延迟/未映射 ----
	for _, ci := range eligible {
		c := cash[ci]
		if c.AmountNet.IsZero() {
			continue
		}
		matched := false
		for _, m := range matches {
			if m.CashKey == cashKey(c) {
				matched = true
				break
			}
		}
		if matched {
			continue
		}

		// 找同渠道同窗口、差额最近且未被使用的银行流水 → amount_diff。
		best := -1
		var bestDiff decimal.Decimal
		for bi, f := range bank {
			if used[bi] || dup[bi] || f.Channel != c.Method || !inWindow(c.BizDate, f.ValueDate, days) {
				continue
			}
			d := diffAmount(c.AmountNet, f.Amount).Abs()
			if best < 0 || d.Cmp(bestDiff) < 0 {
				best, bestDiff = bi, d
			}
		}
		if best >= 0 {
			f := bank[best]
			used[best] = true
			diffs = append(diffs, Diff{
				Kind:       model.DiffKindAmountDiff,
				Channel:    c.Method,
				CashRef:    cashKey(c),
				BankRef:    bankRef(f),
				Expected:   c.AmountNet,
				Actual:     f.Amount,
				DiffAmount: diffAmount(c.AmountNet, f.Amount),
				DedupeKey:  dedupeKey(in.BranchID, c.Method, cashKey(c), bankRef(f), model.DiffKindAmountDiff),
			})
			continue
		}

		// 该渠道在窗口内一条流水都没有 ⇒ 不是"差额",是"还没到账"。
		if bankForChannel(c.Method, c.BizDate) == 0 {
			diffs = append(diffs, Diff{
				Kind:       model.DiffKindTimeLag,
				Channel:    c.Method,
				CashRef:    cashKey(c),
				Expected:   c.AmountNet,
				Actual:     decimal.Zero,
				DiffAmount: diffAmount(c.AmountNet, decimal.Zero),
				DedupeKey:  dedupeKey(in.BranchID, c.Method, cashKey(c), "", model.DiffKindTimeLag),
			})
			continue
		}

		diffs = append(diffs, Diff{
			Kind:       model.DiffKindSingleCash,
			Channel:    c.Method,
			CashRef:    cashKey(c),
			Expected:   c.AmountNet,
			Actual:     decimal.Zero,
			DiffAmount: diffAmount(c.AmountNet, decimal.Zero),
			DedupeKey:  dedupeKey(in.BranchID, c.Method, cashKey(c), "", model.DiffKindSingleCash),
		})
	}

	// 未映射 pay_way:显式差异,不猜、不计入匹配率分母。
	for _, c := range cash {
		if c.Method != model.MethodUnmapped {
			continue
		}
		diffs = append(diffs, Diff{
			Kind:       model.DiffKindUnmappedPayWay,
			Channel:    model.MethodUnmapped,
			CashRef:    cashKey(c),
			Expected:   c.AmountNet,
			Actual:     decimal.Zero,
			DiffAmount: diffAmount(c.AmountNet, decimal.Zero),
			DedupeKey:  dedupeKey(in.BranchID, model.MethodUnmapped, cashKey(c), "", model.DiffKindUnmappedPayWay),
		})
	}

	// 重复流水。
	for bi, f := range bank {
		if !dup[bi] {
			continue
		}
		diffs = append(diffs, Diff{
			Kind:       model.DiffKindDuplicate,
			Channel:    f.Channel,
			BankRef:    bankRef(f),
			Expected:   f.Amount,
			Actual:     f.Amount,
			DiffAmount: decimal.Zero,
			DedupeKey:  dedupeKey(in.BranchID, f.Channel, "", bankRef(f), model.DiffKindDuplicate),
		})
	}

	// 未配对的银行单边。
	for bi, f := range bank {
		if used[bi] || dup[bi] {
			continue
		}
		diffs = append(diffs, Diff{
			Kind:       model.DiffKindSingleBank,
			Channel:    f.Channel,
			BankRef:    bankRef(f),
			Expected:   decimal.Zero,
			Actual:     f.Amount,
			DiffAmount: diffAmount(decimal.Zero, f.Amount),
			DedupeKey:  dedupeKey(in.BranchID, f.Channel, "", bankRef(f), model.DiffKindSingleBank),
		})
	}

	// ---- Prior:保留人工处理结果 ----
	for i := range diffs {
		d := &diffs[i]
		d.Status = model.DiffStatusOpen
		p, ok := prior[d.DedupeKey]
		if !ok {
			continue
		}
		d.Status = p.Status
		d.ResolutionCode = p.ResolutionCode
		d.ResolutionNote = p.ResolutionNote
		d.HandledBy = p.HandledBy
		d.HandledAt = p.HandledAt
		d.CarriedOver = true
	}

	// ---- 汇总 ----
	var cashTotal, bankTotal, feeTotal decimal.Decimal
	for _, c := range cash {
		if c.Method == model.MethodChange {
			continue
		}
		cashTotal = cashTotal.Add(c.AmountNet)
	}
	for _, f := range bank {
		bankTotal = bankTotal.Add(f.Amount)
		feeTotal = feeTotal.Add(f.Fee)
	}

	// 排序:输出逐字段可比较(确定性单测按 dedupe_key 排序后比对)。
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].DedupeKey < matches[j].DedupeKey })
	sort.SliceStable(diffs, func(i, j int) bool { return diffs[i].DedupeKey < diffs[j].DedupeKey })

	summary := Summary{
		DiffCount:     len(diffs),
		CashTotal:     cashTotal,
		BankTotal:     bankTotal,
		FeeTotal:      feeTotal,
		MatchedCount:  len(matches),
		EligibleCount: len(eligible),
	}
	if summary.EligibleCount > 0 {
		summary.MatchRate = decimal.NewFromInt(int64(summary.MatchedCount)).
			Div(decimal.NewFromInt(int64(summary.EligibleCount))).
			Round(4)
	}
	return Output{Matches: matches, Diffs: diffs, Summary: summary}
}

// bankRef 是银行流水的稳定引用:优先 ExternalID,空则用本地 ID。
func bankRef(f BankFlow) string {
	if f.ExternalID != "" {
		return f.ExternalID
	}
	return f.ID
}
