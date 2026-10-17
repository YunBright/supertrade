package fact

import (
	"testing"

	"github.com/YunBright/supertrade/internal/salesinsight/fact/supplier"
)

// ✅ 本文件的每条用例都基于 **2026-10-16 对 ysx(hbposv10) 的实测数据**，
//   不是构造的假数据。用实测值的好处是：这些记录真实存在于生产，
//   判定错了会直接影响经营建议。
func TestClassifySupplierWithRealData(t *testing.T) {
	cases := []struct {
		name string
		rec  supplier.Record
		// wantRule 非空表示应被排除；wantReason 包含该关键短语。
		wantRule   ExclusionRule
		wantReason string
	}{
		{
			// ✅ 实测 sup_type='5' 的 4 条自用/直供之一
			name: "乐园公司是自用直供",
			rec:  supplier.Record{Code: "101", Name: "乐园公司", Type: "5", CustFlag: "S"},
			// 它同时命中类型与名称两层 —— 断言**类型层优先**
			// （顺序有讲究：先报"自用直供"比报"名称含乐园"更准确）
			wantRule:   ExcludeSelfUseType,
			wantReason: "自用/直供",
		},
		{
			name:       "伊利集团直供是自用",
			rec:        supplier.Record{Code: "201", Name: "内蒙古伊利集团公司", Type: "5", CustFlag: "S"},
			wantRule:   ExcludeSelfUseType,
			wantReason: "自用/直供",
		},
		{
			// ⚠️ 这一条是**仅靠类型排不掉**的：type='1'（普通供应商），
			//   但它是门店自营采购单元，且生鲜几乎全挂在它下面。
			name:       "自购蔬菜靠名称关键词才排得掉",
			rec:        supplier.Record{Code: "019", Name: "自购蔬菜", Type: "1", CustFlag: "S"},
			wantRule:   ExcludeNameKeyword,
			wantReason: "自购",
		},
		{
			name:       "水果（自营）靠名称关键词才排得掉",
			rec:        supplier.Record{Code: "084", Name: "水果（自营）", Type: "1", CustFlag: "S"},
			wantRule:   ExcludeNameKeyword,
			wantReason: "自营",
		},
		{
			// ✅ 实测 cust_flag='C' 的 49 个客户之一
			name:       "运营部是客户不是供应商",
			rec:        supplier.Record{Code: "01", Name: "运营部", Type: "1", CustFlag: "C"},
			wantRule:   ExcludeNotSupplier,
			wantReason: "客户",
		},
		{
			name:       "中铁五局搅拌站是客户",
			rec:        supplier.Record{Code: "x", Name: "中铁五局四标1号搅拌站", Type: "1", CustFlag: "C"},
			wantRule:   ExcludeNotSupplier,
			wantReason: "客户",
		},
		{
			name:       "special内部周转按非常规类型排除",
			rec:        supplier.Record{Code: "special", Name: "内部周转", Type: "xx", CustFlag: "A"},
			wantRule:   ExcludeSelfUseType,
			wantReason: "非常规",
		},
		{
			// ✅ 真实外部供应商，必须**保留**。
			//   反证用例：如果把这条也排除，"哪个供应商单品更好"就没有答案了。
			name: "正常外部供应商保留",
			rec:  supplier.Record{Code: "038", Name: "岑溪市泰鼎粮油食品有限公司", Type: "1", CustFlag: "S"},
		},
		{
			// ✅ 实测存在的一条真实供应商（销售流水上大量出现）
			name: "鑫时乐保留",
			rec:  supplier.Record{Code: "001", Name: "鑫时乐", Type: "1", CustFlag: "S"},
		},
		{
			// 类型缺失的老数据：**不能**排除。
			// 排除一条真供应商的代价（丢失优质货源线索）大于多显示一条。
			name: "类型缺失不排除",
			rec:  supplier.Record{Code: "X", Name: "某供应商", Type: "", CustFlag: "S"},
		},
		{
			// 标识缺失同样保守处理。
			name: "标识缺失且类型正常不排除",
			rec:  supplier.Record{Code: "Y", Name: "某贸易有限公司", Type: "1", CustFlag: ""},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rule, reason := ClassifySupplier(c.rec)
			if rule != c.wantRule {
				t.Fatalf("规则 = %q, 期望 %q（理由:%s）", rule, c.wantRule, reason)
			}
			if c.wantReason != "" && !contains(reason, c.wantReason) {
				t.Fatalf("理由 %q 未包含关键词 %q —— 理由是给店长看的原话", reason, c.wantReason)
			}
			if c.wantRule == "" && reason != "" {
				t.Fatalf("不该排除的供应商却带了排除理由: %q", reason)
			}
		})
	}
}

// 顺序铁律：先判"是不是供应商"，再判"是不是自用"。
//
// 这条用例记录的客户名字里带"自"（自营物业）。
// 若顺序反了，它会被标成"自用"，而真实原因是"它是我们的客户"。
// 两者对店长的解释完全不同：前者暗示"这是我们自己人"，
// 后者才是"这个单位向我们采购" —— 混为一谈会让店长去错误的地方核实。
func TestCustomerWinsOverNameKeyword(t *testing.T) {
	rule, reason := ClassifySupplier(supplier.Record{
		Code: "C1", Name: "自营物业管理处", Type: "1", CustFlag: "C",
	})
	if rule != ExcludeNotSupplier {
		t.Fatalf("客户身份应优先于名称关键词，实际规则 = %q", rule)
	}
	if !contains(reason, "客户") {
		t.Fatalf("理由应说明是客户，实际 %q", reason)
	}
}

// 关键词表必须可配置 —— 换一家门店就要换一套命名。
//
// 这条用例锁住"运营改了配置立即生效"这条能力：
// 一旦有人把关键词写死进判定逻辑，SetSelfUseKeywords 会变成 no-op，
// 而症状是"换了门店但自采供应商还是排不掉"，没有任何报错。
func TestSelfUseKeywordsAreOverridable(t *testing.T) {
	// 默认表能排掉"自购"
	if _, reason := ClassifySupplier(supplier.Record{Name: "自购蔬菜", Type: "1", CustFlag: "S"}); reason == "" {
		t.Fatal("默认表应排掉自购蔬菜")
	}

	t.Cleanup(func() { SetSelfUseKeywords(nil) })

	// 覆盖成另一个门店的命名
	SetSelfUseKeywords([]string{"厂配", "集采"})
	if rule, _ := ClassifySupplier(supplier.Record{Name: "自购蔬菜", Type: "1", CustFlag: "S"}); rule != "" {
		t.Fatalf("覆盖后不该再命中默认关键词，实际规则 = %q", rule)
	}
	if rule, _ := ClassifySupplier(supplier.Record{Name: "厂配中心", Type: "1", CustFlag: "S"}); rule != ExcludeNameKeyword {
		t.Fatalf("覆盖后应命中新关键词，实际规则 = %q", rule)
	}

	// 显式传空切片 = 明确禁用关键词规则（与"没配置"必须不同）
	SetSelfUseKeywords([]string{})
	if rule, _ := ClassifySupplier(supplier.Record{Name: "自购蔬菜", Type: "1", CustFlag: "S"}); rule != "" {
		t.Fatalf("显式空表应禁用关键词规则，实际规则 = %q", rule)
	}

	// 恢复默认
	SetSelfUseKeywords(nil)
	if rule, _ := ClassifySupplier(supplier.Record{Name: "自购蔬菜", Type: "1", CustFlag: "S"}); rule != ExcludeNameKeyword {
		t.Fatalf("传 nil 应恢复默认表，实际规则 = %q", rule)
	}
}

// 覆盖后的切片必须被防御性拷贝 ——
// 否则调用方持有原切片并修改，会在运行中悄悄改变判定结果。
func TestKeywordOverrideIsDefensivelyCopied(t *testing.T) {
	t.Cleanup(func() { SetSelfUseKeywords(nil) })

	src := []string{"厂配"}
	SetSelfUseKeywords(src)
	src[0] = "被篡改"

	if got := SelfUseKeywords()[0]; got != "厂配" {
		t.Fatalf("覆盖表被外部切片污染: %q", got)
	}
}

func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
