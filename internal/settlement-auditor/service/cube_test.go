// cube_test.go —— cube 数据访问层的日期解析/排序单测。
//
// 背景(2026-10-10):MSSQL live 源的 datetime 是 "09  1 2021 12:00AM" 形态,
// 字符串直接比较会把 12 月排成"最大"、2021 排在 2026 前面 —— 必须经
// parseCubeTime 解析后按时间比较。样本取自生产 sixun-ysx-fb 实测。
package service

import (
	"testing"
)

func TestParseCubeTime_MSSQLLiveSource(t *testing.T) {
	// 生产实测:月 2 位、日空格补位、12 小时制 AM/PM
	cases := []struct {
		in   string
		want string // "2006-01-02 15:04"
	}{
		{"09  1 2021 12:00AM", "2021-09-01 00:00"},
		{"10  8 2021 12:00AM", "2021-10-08 00:00"},
		{"12  8 2021 12:00AM", "2021-12-08 00:00"},
		{"12  8 2021 12:30PM", "2021-12-08 12:30"},
		{"01  2 2026 11:05PM", "2026-01-02 23:05"},
	}
	for _, tc := range cases {
		got, ok := parseCubeTime(tc.in)
		if !ok {
			t.Fatalf("parseCubeTime(%q) 应解析成功", tc.in)
		}
		if got.Format("2006-01-02 15:04") != tc.want {
			t.Errorf("parseCubeTime(%q) = %s, want %s", tc.in, got.Format("2006-01-02 15:04"), tc.want)
		}
	}
}

func TestParseCubeTime_ISOAndInvalid(t *testing.T) {
	if got, ok := parseCubeTime("2021-09-01"); !ok || got.Format("2006-01-02") != "2021-09-01" {
		t.Errorf("ISO 日期应可解析, got %v ok=%v", got, ok)
	}
	if got, ok := parseCubeTime("2021-09-01 08:30:00"); !ok || got.Hour() != 8 {
		t.Errorf("ISO datetime 应可解析, got %v ok=%v", got, ok)
	}
	if _, ok := parseCubeTime(""); ok {
		t.Errorf("空串应返 false")
	}
	if _, ok := parseCubeTime("不是日期"); ok {
		t.Errorf("垃圾串应返 false")
	}
}

func TestNormalizeCubeDate(t *testing.T) {
	if got := normalizeCubeDate("09  1 2021 12:00AM"); got != "2021-09-01" {
		t.Errorf("normalizeCubeDate(MSSQL) = %q, want 2021-09-01", got)
	}
	if got := normalizeCubeDate("2021-09-01"); got != "2021-09-01" {
		t.Errorf("normalizeCubeDate(ISO) = %q", got)
	}
	if got := normalizeCubeDate("  乱串  "); got != "乱串" {
		t.Errorf("解析失败应 trim 后原样保留, got %q", got)
	}
}

func TestSortSettlementRowsDesc(t *testing.T) {
	// 混合:不同账期 + 同账期多单 + MSSQL 原始格式串 + 无日期行
	rows := []SettlementRow{
		{ID: "CP002110080002", PeriodEnd: "10  8 2021 12:00AM", PeriodStart: "09  1 2021 12:00AM"},
		{ID: "CP002600105555", PeriodEnd: "2026-10-01", PeriodStart: "2026-09-01"},
		{ID: "CP002110090003", PeriodEnd: "10  9 2021 12:00AM", PeriodStart: "09  9 2021 12:00AM"},
		{ID: "CP002112080205", PeriodEnd: "12  8 2021 12:00AM", PeriodStart: "10  1 2021 12:00AM"},
		{ID: "CP002112080204", PeriodEnd: "12  8 2021 12:00AM", PeriodStart: "10  1 2021 12:00AM"},
		{ID: "CP_NO_DATE"},
	}
	sortSettlementRowsDesc(rows)
	want := []string{
		"CP002600105555", // 2026-10 最新
		"CP002112080205", // 2021-12-08 同账期,单号倒序
		"CP002112080204",
		"CP002110090003", // 2021-10-09
		"CP002110080002", // 2021-10-08
		"CP_NO_DATE",     // 无日期排最后
	}
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r.ID
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("排序结果[%d] = %s, want %s; 全序 %v", i, got[i], want[i], got)
		}
	}
}

func TestSortSettlementRowsDesc_EmptyAndNoMutation(t *testing.T) {
	sortSettlementRowsDesc(nil) // 不 panic
	one := []SettlementRow{{ID: "A", PeriodEnd: "2026-01-01"}}
	sortSettlementRowsDesc(one)
	if len(one) != 1 || one[0].ID != "A" {
		t.Errorf("单元素不应变化")
	}
}

// 回归锁:2026-10-10 生产故障 —— 日期串直接字符串比较,"12  8 2021" 会被当成
// 比 "1  5 2026" 更大(首字符后 "2" > " "),导致 2021 年的单排最前。
func TestSortSettlementRowsDesc_NotLexicographic(t *testing.T) {
	rows := []SettlementRow{
		{ID: "A", PeriodEnd: "1  5 2026 12:00AM"},  // 2026-01-05
		{ID: "B", PeriodEnd: "12  8 2021 12:00AM"}, // 2021-12-08(字符串比较下最大)
	}
	sortSettlementRowsDesc(rows)
	if rows[0].ID != "A" {
		t.Errorf("2026-01-05 应在 2021-12-08 之前(字符串比较会把 2021-12 排最前), got %s", rows[0].ID)
	}
}
