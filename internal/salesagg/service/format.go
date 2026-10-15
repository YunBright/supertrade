package service

import (
	"fmt"
	"strconv"
	"strings"
)

// 本文件的格式化策略：**一律输出字符串，不输出 JSON number**。
//
// 理由：金额与数量要显示成 "18,896.38"（两位小数、千分位），
// 而 Go 的 float64 直接进 JSON 会变成 18896.380000000004 这类值。
// 前端拿到再格式化等于把精度的锅甩给前端，而**各端格式化规则必然漂移**
// （Flutter 与微信卡片对同一个数的小数位/千分位很容易不一致）。
//
// 所以在服务端定死成字符串，前端与卡片都只做展示。
//
// 另一层理由是**体积**：报表一次可能几百行，每行 6 个数字，
// 字符串比数字大一点，但换来"渲染端零计算"，值。

// fmtAmount 把金额/数量格式化成 "18,896.38"。
//
// 数量也走这里是因为源库的数量单位是称重重量，量级与金额同阶
// （实测当日净销量 774.95，7 天 11083.85）。
func fmtAmount(v float64) string {
	return groupThousands(fmt.Sprintf("%.2f", v))
}

// fmtPct 把 0.2534 格式化成 "25.3%"。
func fmtPct(v float64) string {
	return fmt.Sprintf("%.1f%%", v*100)
}

// groupThousands 给整数部分加千分位。
//
// 手写而不是引依赖：规则只有一条（每 3 位一个逗号），
// 为它引入一个格式化库会让"金额怎么显示"变成别人的配置。
func groupThousands(s string) string {
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	intPart := s
	frac := ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart = s[:i]
		frac = s[i:]
	}
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := b.String() + frac
	if neg {
		return "-" + out
	}
	return out
}

// numOf 把格式化后的字符串还原成数字，仅供排序用。
//
// 排序本可以在 Report 之前用原始 float 做（那样更快且无解析成本），
// 但 Report 的输入行已经被转成字符串了。parse 失败时返回 0 ——
// 它只影响排序次序，不影响任何对外的数字，解析失败不该让查询失败。
//
// ⚠️ 这是刻意接受的取舍：**fmtAmount 的输出必须能被 strconv 解析回来**。
// 改它之前记得这条（当前实现满足：逗号会被 ParseFloat 忽略，
// 因为这里先 Strip 掉了逗号）。
func numOf(s string) float64 {
	clean := strings.ReplaceAll(s, ",", "")
	v, err := strconv.ParseFloat(clean, 64)
	if err != nil {
		return 0
	}
	return v
}

// coverage 返回成本覆盖率，TotalRows 为 0 时返 0。
//
// 分母为 0 返回 0 而不是 1 或 NaN：0 会让调用方判定"不可算毛利率"
// （低于 MinCostCoverage），这是正确的默认 —— 没有行就无从判断覆盖率。
func coverage(costed, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(costed) / float64(total)
}
