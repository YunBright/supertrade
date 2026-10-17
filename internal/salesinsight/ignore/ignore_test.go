package ignore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/salesinsight/fact/supplier"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(t.TempDir())
}

// ⚠️ 门店 id 来自 HTTP 头，而它会被拼进**文件路径**。
//
// 这条测试是路径穿越的唯一防线。判别力：把 sanitizeFileName 直接
// `return s`（原样返回），下面每个 case 都会失败。
//
// ⚠️ 特别提醒：`\` 那条**不是形式主义** —— 在 Windows 上
// 反斜杠同样是路径分隔符，只挡 `/` 的实现在 Windows 上真的能穿越。
func TestBranchIDCannotTraversePath(t *testing.T) {
	st := newStore(t)
	cases := []string{
		"../../etc/passwd",
		`..\..\windows\system32\config`,
		"..",
		"a/b",
		"a\\b",
		"~/secret",
		".hidden",
		"",
		strings.Repeat("x", 500), // 超长 → 必须截断，否则文件名超 255 字节
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			branch := in
			if strings.TrimSpace(branch) == "" {
				// 空门店必须报"缺门店"，而不是写出个 unnamed 的文件。
				if _, err := st.Load(branch); err == nil {
					t.Fatal("空门店应返回 ErrBranchRequired")
				}
				return
			}
			if _, err := st.Save(branch, []Entry{{Code: "1", Name: "x"}}); err != nil {
				t.Fatalf("Save(%q) 失败: %v", in, err)
			}
			// 关键：文件必须落在 store 目录**内**。
			p := st.pathOf(strings.TrimSpace(branch))
			rel, err := filepath.Rel(st.Dir(), p)
			if err != nil || strings.HasPrefix(rel, "..") {
				t.Fatalf("路径逃逸: %q -> %q (rel=%q)", in, p, rel)
			}
			if _, err := st.Load(branch); err != nil {
				t.Fatalf("Load(%q) 失败: %v", in, err)
			}
		})
	}
}

// ⚠️ 前导 0 有业务含义："019" 与 "19" 是**两个不同的供应商**。
//
// 这条锁住"不得对编码做数值归一"。
// 判别力：把 normalizeCode 改成 strconv.Atoi 再转回，本用例立刻失败。
func TestCodesAreStringsWithMeaningfulLeadingZero(t *testing.T) {
	st := newStore(t)
	if _, err := st.Save("00", []Entry{
		{Code: "019", Name: "自购蔬菜"},
		{Code: "19", Name: "十九号供应商"},
	}); err != nil {
		t.Fatal(err)
	}
	l, err := st.Load("00")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Entries) != 2 {
		t.Fatalf("019 与 19 应是两条不同记录，实际 %d 条: %+v", len(l.Entries), l.Entries)
	}
	codes := l.Codes()
	if !codes["019"] || !codes["19"] {
		t.Errorf("两个编码都应在集合里: %+v", codes)
	}
	if codes["0019"] {
		t.Error("不应出现被数值归一过的编码 0019")
	}
}

// ⚠️ Configured 是区分「配了空的」与「没配过」的**唯一凭据**。
//
// 判别力：把 Save 里的 `Configured: true` 改成 false，本用例立刻失败。
func TestConfiguredDistinguishesEmptyFromNeverConfigured(t *testing.T) {
	st := newStore(t)

	// 从没配过
	l, err := st.Load("store-1")
	if err != nil {
		t.Fatal(err)
	}
	if l.Configured {
		t.Error("从未保存过的门店 Configured 必须为 false")
	}

	// 显式保存空名单
	if _, err := st.Save("store-1", nil); err != nil {
		t.Fatal(err)
	}
	l, err = st.Load("store-1")
	if err != nil {
		t.Fatal(err)
	}
	if !l.Configured {
		t.Error("显式保存空名单后 Configured 必须为 true（纯外采门店是合法状态）")
	}

	// 删除后回到"没配过"—— 删配置等于回到"不知道哪些是自用"。
	if err := st.Delete("store-1"); err != nil {
		t.Fatal(err)
	}
	l, err = st.Load("store-1")
	if err != nil {
		t.Fatal(err)
	}
	if l.Configured {
		t.Error("删除名单后应回到未配置状态，使空名单告警重新出现")
	}
}

// ⚠️ 名单损坏必须报错，**不能**当成空名单。
//
// 判别力：把 Load 里的 json.Unmarshal 错误分支改成返回空 List，本用例失败。
func TestCorruptedListIsNotSilentlyEmpty(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	if err := os.WriteFile(filepath.Join(dir, "supplier-ignore-00.json"),
		[]byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := st.Load("00")
	if err == nil {
		t.Fatalf("名单损坏必须报错（否则自用供应商静默回到榜单），实际返回 %+v", l)
	}
	if !strings.Contains(err.Error(), "损坏") {
		t.Errorf("错误信息应说明是文件损坏: %v", err)
	}
}

// 名单按门店隔离。
func TestListsAreIsolatedPerBranch(t *testing.T) {
	st := newStore(t)
	if _, err := st.Save("A", []Entry{{Code: "1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Save("B", []Entry{{Code: "2"}}); err != nil {
		t.Fatal(err)
	}
	a, _ := st.Load("A")
	b, _ := st.Load("B")
	if len(a.Entries) != 1 || a.Entries[0].Code != "1" {
		t.Errorf("A 名单: %+v", a.Entries)
	}
	if len(b.Entries) != 1 || b.Entries[0].Code != "2" {
		t.Errorf("B 名单: %+v", b.Entries)
	}
}

// 去重、去空、排序：编码集合与顺序必须稳定。
//
// ⚠️ 判据只针对**编码**，不比对整个文件字节 —— Name/Reason 是可改的展示字段，
//   它们变化本来就该产生 diff（否则运营改了名字却看不出改动生效了）。
//   真正需要稳定的是"同内容不会每次都产生无意义 diff"，那由 dedupe 的排序保证。
func TestSaveDeduplicatesAndSorts(t *testing.T) {
	st := newStore(t)
	in := []Entry{
		{Code: " 084 ", Name: "水果（自营）"},
		{Code: "019", Name: "自购蔬菜"},
		{Code: "019", Name: "重复项"}, // 同编码重复
		{Code: "   ", Name: "空编码"},  // 应被丢弃
	}
	if _, err := st.Save("00", in); err != nil {
		t.Fatal(err)
	}

	l, _ := st.Load("00")
	if len(l.Entries) != 2 {
		t.Fatalf("去重去空后应剩 2 条，实际 %d: %+v", len(l.Entries), l.Entries)
	}
	if l.Entries[0].Code != "019" || l.Entries[1].Code != "084" {
		t.Errorf("应按编码排序且去空白: %+v", l.Entries)
	}
	// ⚠️ 编码前后的空白必须被去掉，否则 " 084" 与 "084" 会成为两条不同记录，
	//   而榜单排除用的是原样编码 —— 结果是"配了却不生效"。
	if l.Entries[1].Code != "084" {
		t.Errorf("编码未去空白，会与榜单匹配不上: %q", l.Entries[1].Code)
	}

	// 换个顺序保存，编码序列应完全一致。
	if _, err := st.Save("00", []Entry{{Code: "084"}, {Code: "019"}}); err != nil {
		t.Fatal(err)
	}
	l2, _ := st.Load("00")
	if len(l2.Entries) != 2 || l2.Entries[0].Code != "019" || l2.Entries[1].Code != "084" {
		t.Errorf("保存顺序不应影响最终编码序列: %+v", l2.Entries)
	}
}

// 原子写：不留 .tmp。
func TestSaveLeavesNoTempFile(t *testing.T) {
	st := newStore(t)
	if _, err := st.Save("00", []Entry{{Code: "1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.pathOf("00") + ".tmp"); err == nil {
		t.Error("保存完成后不应残留 .tmp 文件")
	}
}

// 缺门店必须报可识别的错误，而不是写出 unnamed 文件。
func TestEmptyBranchRejected(t *testing.T) {
	st := newStore(t)
	for _, b := range []string{"", "   "} {
		if _, err := st.Load(b); err != ErrBranchRequired {
			t.Errorf("Load(%q) 错误 = %v，期望 ErrBranchRequired", b, err)
		}
		if _, err := st.Save(b, []Entry{{Code: "1"}}); err != ErrBranchRequired {
			t.Errorf("Save(%q) 错误 = %v，期望 ErrBranchRequired", b, err)
		}
		if err := st.Delete(b); err != ErrBranchRequired {
			t.Errorf("Delete(%q) 错误 = %v，期望 ErrBranchRequired", b, err)
		}
	}
}

// nil Store 不得 panic（装配顺序可能先调它）。
func TestNilStoreSafe(t *testing.T) {
	var st *Store
	if st.Dir() != "" {
		t.Error("nil Store 的 Dir 应为空串")
	}
}

// ---- Suggest ----

// 推荐只作为**工具**，不参与运行时判定。
//
// 它必须覆盖三层里那些实测真正命中的形态，否则运营还得手工找。
func TestSuggestCoversRealWorldCases(t *testing.T) {
	recs := []supplier.Record{
		{Code: "01", Name: "运营部", Type: "1", CustFlag: "C"},                       // 客户
		{Code: "101", Name: "乐园公司", Type: "5", CustFlag: "S"},                    // 自用直供
		{Code: "019", Name: "自购蔬菜", Type: "1", CustFlag: "S"},                    // 名称关键词
		{Code: "084", Name: "水果（自营）", Type: "1", CustFlag: "S"},                  // 名称关键词
		{Code: "special", Name: "内部周转", Type: "xx", CustFlag: "A"},                // 异常类型
		{Code: "038", Name: "岑溪市泰鼎粮油食品有限公司", Type: "1", CustFlag: "S"},   // 真供应商
		{Code: "200", Name: "梧州祥鑫贸易", Type: "1", CustFlag: "S"},                // 真供应商
		{Code: "999", Name: "", Type: "", CustFlag: ""},                              // 全空
	}
	got := Suggest(recs)
	byCode := map[string]Suggestion{}
	for _, s := range got {
		byCode[s.Code] = s
	}

	for _, want := range []string{"01", "101", "019", "084", "special"} {
		if _, ok := byCode[want]; !ok {
			t.Errorf("%s 应被推荐（实测三层能识别它）", want)
		}
	}
	for _, unwanted := range []string{"038", "200", "999"} {
		if _, ok := byCode[unwanted]; ok {
			t.Errorf("%s 是真供应商/数据不全，不应被推荐", unwanted)
		}
	}
	// 置信度降序，便于运营先看最可能的。
	for i := 1; i < len(got); i++ {
		if got[i-1].Confidence < got[i].Confidence {
			t.Errorf("推荐应按置信度降序: %v", got)
			break
		}
	}
	// 每条推荐都要有理由（运营要能看懂为什么）。
	for _, s := range got {
		if strings.TrimSpace(s.Reason) == "" {
			t.Errorf("%s 的推荐缺少理由", s.Code)
		}
	}
}

// ⚠️ cust_flag='C' 的优先级必须高于名称关键词。
//
// 反例："自营物业"既是客户又命中"自营"关键词。
// 两者对运营的解释完全不同（"我们自己人" vs "它向我们采购，不是供货方"）。
//
// 判别力：把 suggestOne 里的 cust_flag 检查移到函数末尾，本用例失败。
func TestSuggestCustomerBeatsNameKeyword(t *testing.T) {
	got := Suggest([]supplier.Record{
		{Code: "X1", Name: "自营物业", Type: "1", CustFlag: "C"},
	})
	if len(got) != 1 {
		t.Fatalf("应推荐 1 条，实际 %d", len(got))
	}
	if got[0].Rule != "cust_flag" {
		t.Errorf("命中客户时应以 cust_flag 为依据（解释是「它向我们采购」），实际 rule=%q", got[0].Rule)
	}
}

// 名称关键词层置信度必须**低于**类型层：它最脆弱。
func TestSuggestConfidenceOrdering(t *testing.T) {
	got := Suggest([]supplier.Record{
		{Code: "T", Name: "乐园公司", Type: "5", CustFlag: "S"},
		{Code: "N", Name: "自购蔬菜", Type: "1", CustFlag: "S"},
	})
	if len(got) != 2 {
		t.Fatalf("应推荐 2 条，实际 %d", len(got))
	}
	if got[0].Rule != "type" {
		t.Errorf("类型命中应排在名称命中之前，实际首条 rule=%q", got[0].Rule)
	}
	if got[1].Confidence >= got[0].Confidence {
		t.Errorf("名称关键词层置信度必须低于类型层: %v", got)
	}
}

// 空输入不得 panic。
func TestSuggestEmptyInput(t *testing.T) {
	if got := Suggest(nil); len(got) != 0 {
		t.Errorf("空输入应返回空结果，实际 %+v", got)
	}
}
