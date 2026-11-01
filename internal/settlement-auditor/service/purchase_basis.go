// Package service / purchase_basis.go —— 多日合并付款下的「采购量」口径
// （2026-11-01 用户口径）。
//
// # 问题
//
// 一次付款审批常把 10 天的采购收货单**合并付款**（门店按周/旬结一次）。
// 此时单张 SKU 的进货量是**多日累加值**，直接拿它和「合理备货量」
// （= 周期销售数 × 倍数，一次备货的量）比，倍数天然虚高：
// 生产实踩（审批 e9f4a7b3c4024dc2a3116522ec32b3b8）香蕉-00518 十天进货 220、
// 合理备货量 49.304 ⇒ 判 4.5 倍拒单，而按日均只有 0.4 倍。
//
// ⇒ 对**确实跨多日收货**的 SKU，比较口径改成「日均采购量」
//
//	= 净进货量 / 有效收货日跨度；单日收货的 SKU 保持原样。
//
// # 三条硬口径
//
//  1. **收货日期取备注里的实际收货日**（见 receipt_date.go）：延后录单很常见，
//     制单日期不是到货日期 —— 按制单日算跨度会把 10 天压成 2 天。
//  2. **退货日不算收货日**：PI 的收货日 ≥ 2 个才算「有效多日」。
//     一天进货、一天退货 ⇒ 单日口径（那一天进货量就是一次备货量，
//     一刀切除以 2 会把真实的过量藏起来）。
//  3. **退货量从采购量里扣掉**：实际采购量 = PI 实收 − RO 退货。
//
// ⚠️ 这里的「天数」是**首尾收货日的自然日跨度（含首尾）**，不是"有几天进货"。
//
//	十天合并付款期间，哪怕只有 3 天真的到货，日均也是"这十天的平均采购节奏"，
//	这正是不该拿累加值去比单次合理备货量的原因。
package service

import (
	"strings"

	"github.com/shopspring/decimal"
)

// purchaseBasis 是单个 SKU 本次结算里的采购口径（不含销售/备货事实）。
type purchaseBasis struct {
	// NetQty 是净进货量 = PI 实收合计 − RO 退货合计（负数按 0 处理）。
	NetQty decimal.Decimal
	// ReturnedQty 是退货量（正数），只用于回显。
	ReturnedQty decimal.Decimal
	// Days 是有效收货日跨度（含首尾）；单日或非有效多日 = 1。
	Days int
	// FirstDate / LastDate 是首/末收货日（Days > 1 时才有意义，回显用）。
	FirstDate string
	LastDate  string
}

// CompareQty 返回与「合理备货量」比较用的进货量。
//
// 多日合并付款 ⇒ 日均；单日 ⇒ 净进货量本身。
func (b purchaseBasis) CompareQty() decimal.Decimal {
	if b.Days > 1 {
		// 保留 3 位：与信号里的其他数量口径（roundDecimal(_, 3)）一致，
		// 避免 18.999999 这种把结论吓人的尾数。
		return b.NetQty.Div(decimal.NewFromInt(int64(b.Days))).Round(3)
	}
	return b.NetQty
}

// buildPurchaseBasis 按商品聚合净进货量与有效收货日跨度。
//
// 参数:
//   - received:商品 → 本次结算内 PI 单实收合计；
//   - returned:商品 → 本次结算内 RO 单退货合计（**正数**，调用方已取绝对值）；
//   - productSheets:商品 → 它出现过的 PI 单 id；
//   - sheetDates:PI 单 id → **实际收货日期**（YYYY-MM-DD，见 receipt_date.go）。
//
// 只对 received 里有的商品出结果：只有退货、没有进货的 SKU 没有"采购量"可比。
func buildPurchaseBasis(
	received map[string]decimal.Decimal,
	returned map[string]decimal.Decimal,
	productSheets map[string][]string,
	sheetDates map[string]string,
) map[string]purchaseBasis {
	out := make(map[string]purchaseBasis, len(received))
	for pid, gross := range received {
		ret := returned[pid]
		net := gross.Sub(ret)
		if net.IsNegative() {
			net = decimal.Zero
		}
		b := purchaseBasis{NetQty: net, ReturnedQty: ret, Days: 1}
		// 有效收货日：只取解析得出的 PI 收货日；退货日期不参与（口径 2）。
		first, last := "", ""
		for _, sid := range productSheets[pid] {
			d := strings.TrimSpace(sheetDates[sid])
			if d == "" {
				continue
			}
			if first == "" || d < first {
				first = d
			}
			if last == "" || d > last {
				last = d
			}
		}
		if first != "" && last != "" && first != last {
			if days, ok := dateSpanInclusive(first, last); ok {
				b.Days = days
				b.FirstDate, b.LastDate = first, last
			}
		}
		out[pid] = b
	}
	return out
}

// dateSpanInclusive 返回两个 YYYY-MM-DD 之间的自然日跨度（含首尾）。
func dateSpanInclusive(from, to string) (int, bool) {
	f, okFrom := parseDateOnly(from)
	t, okTo := parseDateOnly(to)
	if !okFrom || !okTo {
		return 0, false
	}
	n := int(t.Sub(f).Hours()/24) + 1
	if n <= 0 {
		return 0, false
	}
	return n, true
}
