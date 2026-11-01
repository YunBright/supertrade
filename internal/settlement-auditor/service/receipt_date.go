// Package service / receipt_date.go —— 采购收货单「实际收货日期」提取（2026-10-31 用户口径）。
//
// # 业务背景
//
// 采购收货单存在**延后录入**：录单员休息、活动较忙时，货先到店、单后补录。
// 制单日期（oper_date / purchase_sheet.created_at）反映的是"录单那天"，
// 不是"货实际到店那天"。备注栏（purchase_sheet.remark，源列 other1）会写明
// 实际收货日期，例如：
//
//	2026.9.18号收货
//	2026.10.9号收货
//	2021.11.10（收货）
//	2021年11月12日上午收货
//	11.10收货
//
// ⇒ 销量窗口、流程判定都必须按**实际收货日期**起算，否则
// "2026-09-18 收到货、2026-10-21 才录单"的单会被判成"进货后无销售"
// （生产实踩：被拒审批 21170868728a4a57a9d3e82d14f42611）。
//
// 解析不出来就**兜底用制单日期** —— 与改动前的行为一致，不会更糟。
package service

import (
	"regexp"
	"strings"
	"time"
)

// 备注里的日期形态（实测生产数据覆盖的写法）：
//
//	YYYY.MM.DD / YYYY-MM-DD / YYYY/MM/DD / YYYY年M月D日
//	M.DD / M-DD / M/DD / M月D日
//
// 日后面可能跟「号」「日」，也可能直接跟「收货」「（收货）」。
var (
	receiptFullDateRe  = regexp.MustCompile(`(\d{4})\s*[年./\-]\s*(\d{1,2})\s*[月./\-]\s*(\d{1,2})\s*[日号]?`)
	receiptShortDateRe = regexp.MustCompile(`(\d{1,2})\s*[月./\-]\s*(\d{1,2})\s*[日号]?`)
)

// 备注里出现这些词，才认为日期是"收货日期"（避免把单号/电话里的数字当日期）。
var receiptKeywords = []string{"收货", "到货", "入库"}

// effectiveDate 返回采购单的**实际收货日期**（YYYY-MM-DD）。
//
// 提取失败 ⇒ 回退制单日期（CreatedAt）；两者都没有 ⇒ 空串（调用方按"无日期"处理）。
//
// reference 是"今天"（用于备注缺年份时推断年份）；空串时用 CreatedAt 推断。
func (r PurchaseSheetRow) effectiveDate(reference string) string {
	if d, ok := extractReceiptDate(r.Remark, r.CreatedAt, reference); ok {
		return d
	}
	return strings.TrimSpace(r.CreatedAt)
}

// extractReceiptDate 从备注提取实际收货日期。
//
// 返回值 ok=false 表示"备注里没有可用的收货日期"，调用方应兜底制单日期。
//
// # 为什么必须带上下文的"收货/到货/入库"
//
// 备注是自由文本，可能同时出现送货单号、电话、"上月结清"等。只按日期形态抓，
// 会把无关日期当收货日 ⇒ 窗口起点错位 ⇒ 销量结论错。宁可解析不出来（回退制单日期）。
func extractReceiptDate(remark, createdAt, reference string) (string, bool) {
	remark = normalizeRemark(remark)
	if remark == "" {
		return "", false
	}
	// 先定"年份锚点"：优先制单日期，其次 reference（今天）。
	anchor, anchorOK := parseDateOnly(createdAt)
	if !anchorOK {
		anchor, anchorOK = parseDateOnly(reference)
	}
	hasKeyword := false
	for _, kw := range receiptKeywords {
		if strings.Contains(remark, kw) {
			hasKeyword = true
			break
		}
	}
	if !hasKeyword {
		return "", false
	}

	type candidate struct {
		date        string // YYYY-MM-DD
		contextual  bool   // 日期附近有"收货/到货/入库"
	}
	var cands []candidate
	for _, m := range receiptFullDateRe.FindAllStringSubmatchIndex(remark, -1) {
		y, mo, d := remark[m[2]:m[3]], remark[m[4]:m[5]], remark[m[6]:m[7]]
		if date, ok := buildDate(y, mo, d, anchor, anchorOK); ok {
			cands = append(cands, candidate{date: date, contextual: dateHasKeyword(remark, m[1])})
		}
	}
	for _, m := range receiptShortDateRe.FindAllStringSubmatchIndex(remark, -1) {
		// 完整日期已匹配过的片段不重复解析（如 "2026.10.27" 里的 "10.27"）。
		if coveredByFullDate(remark, m[0], m[1]) {
			continue
		}
		mo, d := remark[m[2]:m[3]], remark[m[4]:m[5]]
		if date, ok := buildDate("", mo, d, anchor, anchorOK); ok {
			cands = append(cands, candidate{date: date, contextual: dateHasKeyword(remark, m[1])})
		}
	}
	if len(cands) == 0 {
		return "", false
	}
	// 优先取"紧邻收货/到货/入库"的那个；都没有则取最后一个（备注常把结论放句尾）。
	for i := len(cands) - 1; i >= 0; i-- {
		if cands[i].contextual {
			return cands[i].date, true
		}
	}
	return cands[len(cands)-1].date, true
}

// dateHasKeyword 判断日期结束位置之后的文本里是否出现收货类关键词。
//
// 窗口取 12 个字符：覆盖 "号收货"、"（收货）"、"上午收货"、"后到店收货" 这类写法，
// 又不至于把下一句无关日期的话拽进来。
func dateHasKeyword(s string, end int) bool {
	if end >= len(s) {
		return false
	}
	tail := []rune(s[end:])
	if len(tail) > 12 {
		tail = tail[:12]
	}
	seg := string(tail)
	for _, kw := range receiptKeywords {
		if strings.Contains(seg, kw) {
			return true
		}
	}
	return false
}

// coveredByFullDate 判断 [start,end) 是否落在某个完整日期匹配之内。
func coveredByFullDate(s string, start, end int) bool {
	for _, m := range receiptFullDateRe.FindAllStringIndex(s, -1) {
		if start >= m[0] && end <= m[1] {
			return true
		}
	}
	return false
}

// buildDate 组装并校验 YYYY-MM-DD。
//
//   - year 为空（短日期形态）⇒ 用 anchor 的年份；若得到的日期晚于 anchor（如
//     制单日是 2027-01-03、备注写 "12.28"）⇒ 视为上一年的日期；
//   - 非法月/日（2 月 30 日这类）⇒ 解析失败。
func buildDate(year, month, day string, anchor time.Time, anchorOK bool) (string, bool) {
	y := atoi(year)
	mo := atoi(month)
	d := atoi(day)
	if mo < 1 || mo > 12 || d < 1 || d > 31 {
		return "", false
	}
	if y == 0 {
		if !anchorOK {
			return "", false
		}
		y = anchor.Year()
	}
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if t.Month() != time.Month(mo) || t.Day() != d {
		return "", false
	}
	if len(year) == 0 && anchorOK && t.After(anchor) {
		t = t.AddDate(-1, 0, 0)
	}
	return t.Format("2006-01-02"), true
}

// atoi 把数字串转 int（非法 ⇒ 0）。
func atoi(s string) int {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// parseDateOnly 解析 "YYYY-MM-DD"（也接受 cube 的 datetime 串）。
func parseDateOnly(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, ok := parseCubeTime(s); ok {
		return t, true
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// normalizeRemark 统一全角/半角分隔符，便于正则匹配。
func normalizeRemark(s string) string {
	r := strings.NewReplacer(
		"（", "(", "）", ")", "：", ":", "，", ",",
		"．", ".", "／", "/", "－", "-", "～", "~",
	)
	return r.Replace(strings.TrimSpace(s))
}
