package ignore

import "testing"

// TestIndexForSales_线上真实故障形态 是本次修复的判别力主用例：
// 名单录的是字典侧编码 `029 猪肉自营`，流水侧是 `29`，
// 不对齐时全店最大的供应商（占 26%）静默逃过排除。
func TestIndexForSales_线上真实故障形态(t *testing.T) {
	entries := []Entry{{Code: "029", Name: "猪肉自营", Reason: "门店自营"}}
	sales := []string{"29", "131", "038"}

	got := IndexForSales(entries, sales)

	if _, ok := got["29"]; !ok {
		t.Fatalf("名单 `029` 应当对齐到流水里的 `29`；实际索引 = %v", keysOf(got))
	}
	if e := got["29"]; e.Reason != "门店自营" {
		t.Errorf("对齐后应保留原条目原因，实际 = %q", e.Reason)
	}
}

// TestIndexForSales_精确命中优先于补零 守住本包最硬的不变量：
// `019` 与 `19` 是两个不同的供应商，补零绝不能把它们合并。
func TestIndexForSales_精确命中优先于补零(t *testing.T) {
	entries := []Entry{{Code: "019", Name: "A"}}
	sales := []string{"019", "19", "131"}

	got := IndexForSales(entries, sales)

	if _, ok := got["019"]; !ok {
		t.Errorf("精确命中 `019` 必须被索引；实际 = %v", keysOf(got))
	}
	if _, ok := got["19"]; ok {
		t.Errorf("`19` 是另一个供应商，绝不能因为 `019` 在名单里而被排除；实际 = %v", keysOf(got))
	}
}

// TestIndexForSales_两个变体都在流水里时判为歧义不排除：
// 漏排除的代价是榜单含自采，错排除的代价是真供应商凭空消失 —— 后者更糟。
func TestIndexForSales_变体歧义时保守不排除(t *testing.T) {
	entries := []Entry{{Code: "29", Name: "X"}}
	// 流水里同时有 "29" 与 "029" 两个**不同**供应商
	sales := []string{"29", "029", "131"}

	got := IndexForSales(entries, sales)

	if len(got) != 1 {
		t.Fatalf("只应精确命中的那一条，实际索引 = %v", keysOf(got))
	}
	if _, ok := got["29"]; !ok {
		t.Errorf("精确命中应保留；实际 = %v", keysOf(got))
	}
	if _, ok := got["029"]; ok {
		t.Errorf("歧义变体不得被牵连排除；实际 = %v", keysOf(got))
	}
}

func TestIndexForSales_去零方向也能对齐(t *testing.T) {
	// 反向：名单录了 4 位，流水是 3 位
	entries := []Entry{{Code: "0029"}}
	got := IndexForSales(entries, []string{"29", "131"})
	if _, ok := got["29"]; !ok {
		t.Errorf("`0029` 应对齐到 `29`；实际 = %v", keysOf(got))
	}
}

func TestIndexForSales_流水里不存在的编码不产生索引(t *testing.T) {
	entries := []Entry{{Code: "029"}, {Code: ""}, {Code: "   "}}
	got := IndexForSales(entries, []string{"131", "038"})
	if len(got) != 0 {
		t.Errorf("流水里没有任何可对齐的编码时应为空，实际 = %v", keysOf(got))
	}
}

func keysOf(m map[string]Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
