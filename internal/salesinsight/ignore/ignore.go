// Package ignore 提供"按门店维护的供应商忽略名单"。
//
// # 为什么从"三层自动判定"改成"人工名单"
//
// 原来每次生成报告都会跑一遍 ClassifySupplier（cust_flag / type / 名称关键词）。
// 这有两个问题：
//
//  1. **成本**：判定本身不花钱，但它是"每次都做"的智能活。
//     而名单是"配一次、用很久"的配置 —— 智能应该只发生在**维护时**，
//     而不是每次读报告时。这正是本包存在的理由。
//  2. **静默出错**：关键词没命中时，自用供应商会正常出现在榜单上，
//     **没有任何报错**，只是结论慢慢变得不可信。
//
// → 自动判定保留为 **Suggest**（维护名单时的便利工具，见 suggest.go），
//   退出运行时主链路。运行时只看名单。
//
// # ⚠️ 最重要的不变量：空名单必须**显式告警**
//
// 名单为空有两种可能：
//
//	a. 这家门店确实没有自用供应商（纯外采）；
//	b. 运营还没配（改了判定方式之后最容易出现这个）。
//
// 两种情况在数据上**完全一样**。若静默处理，(b) 会让自用供应商
// 全部涌进"供应商榜单"，而报告里没有任何异常信号 ——
// 店长会以为那些自营采购单元是外部优质货源。
//
// → 所以 Load 返回 configured 标志，调用方**必须**据此产生一条
//   可见的告警（见 build/suppliers.go 里对它的使用）。
package ignore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Entry 是名单里的一条。
type Entry struct {
	// Code 是供应商编码（supcust_no）。
	Code string `json:"code"`
	// Name 是供应商名称，仅供展示与人工核对。
	//
	// ⚠️ **判定只用 Code**。名称会变（"自购蔬菜"改成"蔬菜自营"），
	//   用名称匹配会让一份已经配好的名单静默失效。
	Name string `json:"name"`
	// Reason 是忽略原因，写给店长看（如"门店自营采购单元"）。
	Reason string `json:"reason,omitempty"`
}

// List 是某家门店的忽略名单。
type List struct {
	// BranchID 标明这份名单属于哪家门店。
	BranchID string `json:"branch_id"`
	// Entries 是被忽略的供应商。
	Entries []Entry `json:"entries"`
	// UpdatedAt 是最后修改时间（RFC3339），便于后台展示"谁在什么时候改的"。
	UpdatedAt string `json:"updated_at,omitempty"`
	// Configured 表示"运营是否显式保存过一次"。
	//
	// ⚠️ 这个字段是**判别"空名单"与"没配过"**的唯一依据，
	//   必须落盘 —— 否则每次保存都会退化成"没配过"，
	//   空名单的告警就永远不会消失。
	Configured bool `json:"configured"`
}

// Codes 返回编码集合，便于 O(1) 查表。
func (l List) Codes() map[string]bool {
	m := make(map[string]bool, len(l.Entries))
	for _, e := range l.Entries {
		if c := normalizeCode(e.Code); c != "" {
			m[c] = true
		}
	}
	return m
}

// normalizeCode 归一化编码。
//
// ⚠️ 编码在思迅里是字符串（"019"、"038"），前导 0 有意义 ——
//   "019" 与 "19" 是**两个不同的供应商**。
//   所以只去空白，不做任何数值转换。
func normalizeCode(s string) string { return strings.TrimSpace(s) }

// IndexForSales 把名单对齐到「本期销售数据里实际出现的编码」，
// 返回 销售编码 → Entry 的映射（只含真正命中的）。
//
// # 为什么需要它
//
// 名单里的编码是**从字典侧**（t_bd_supcust_info）录的，
// 而运行时过滤用的是**流水侧**（t_rm_daysum.supcust_no）。
// 思迅这两个表对同一个供应商的前导零并不总是一致 ——
// 线上实测（2026-10-17，门店 00）：名单里是 `029 猪肉自营`，
// 流水里却是 `29 猪肉自营`，于是**全店最大的供应商（3378.3 元，占 26%）
// 静默逃过排除**，继续出现在"外部优质货源"榜单里，
// 而报告里没有任何异常信号。
//
// # ⚠️ 为什么不能简单地"两边都补零"
//
// 本包的既有约定明确记着：`019` 与 `19` 是两个不同的供应商。
// 无条件补零会把它们合并，等于用"匹配上了"换来"排错了"。
//
// → 所以对齐有三条硬约束：
//
//  1. **精确命中优先**：名单编码与销售编码逐字相同时直接采用；
//  2. **变体必须在销售数据里真实存在**才考虑补零/去零，
//     于是"019"与"19"都在流水里时各自保持原样，不会被合并；
//  3. **多个变体同时命中 = 判为歧义，不排除** ——
//     漏排除的代价（榜单含自采）远小于错排除的代价（真供应商凭空消失）。
//
// 歧义时保守不排除，与本包其它分支的取向一致。
func IndexForSales(entries []Entry, salesCodes []string) map[string]Entry {
	present := make(map[string]bool, len(salesCodes))
	widths := make(map[int]bool, 4)
	for _, c := range salesCodes {
		c = normalizeCode(c)
		if c == "" {
			continue
		}
		present[c] = true
		widths[len(c)] = true
	}

	out := make(map[string]Entry, len(entries))
	for _, e := range entries {
		code := normalizeCode(e.Code)
		if code == "" {
			continue
		}
		if present[code] { // 约束 1：精确命中
			out[code] = e
			continue
		}
		// 约束 2 + 3：候选变体必须在销售数据里存在，且唯一。
		//
		// ⚠️ 候选**必须去重**：同一编码带多个前导零时
		//   不同宽度会塌缩成同一个字符串（`0029` 在 w=2 与 w=3 下都是 `29`），
		//   不去重就会把"同一个候选"数成两个 → 误判歧义 → 静默不排除。
		var hits []string
		seenHit := make(map[string]bool, 4)
		for w := range widths {
			v := fitWidth(code, w)
			if v == code || !present[v] || seenHit[v] {
				continue
			}
			seenHit[v] = true
			hits = append(hits, v)
		}
		if len(hits) == 1 {
			out[hits[0]] = e
		}
	}
	return out
}

// fitWidth 把编码调整到指定宽度：不足补前导 0，过长去前导 0。
//
// ⚠️ 宽度**相等时必须原样返回**：若也走去零分支，
//   `029` 在 w=3 时会变成 `29`，与 w=2 的候选重复，
//   于是同一个候选被数成两个 → 误判为"歧义" → 静默不排除。
func fitWidth(code string, w int) string {
	switch {
	case w == len(code):
		return code
	case w < len(code):
		return strings.TrimLeft(code, "0")
	default:
		return strings.Repeat("0", w-len(code)) + code
	}
}

// Store 是名单的持久化。
//
// # 为什么按门店存成独立文件
//
// 本服务是**无状态**推理服务，不持有数据库连接。
// 而名单需要跨进程存活（否则重启后名单空了 → 结论静默变化）。
//
// 独立文件（而不是一个大 JSON）有三个好处：
//  1. 读改写只碰一个门店的数据，天然避免并发写冲突；
//  2. 人工排障时可以直接 cat 一个文件看，不用 jq 整个大文件；
//  3. 备份/迁移可以按门店做。
type Store struct {
	dir string
	mu  sync.Mutex
}

// NewStore 构造。dir 为空时退化为**内存模式**（仅测试/本地）。
func NewStore(dir string) *Store {
	return &Store{dir: strings.TrimSpace(dir)}
}

// Dir 返回存储目录。
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// ErrBranchRequired 表示缺门店。
var ErrBranchRequired = fmt.Errorf("缺少门店（branch id）")

// Load 读某家门店的名单。
//
// ⚠️ 门店编码必须做**文件名安全化** —— 它来自 HTTP 头，
//
//	未经处理就拼进路径会产生路径穿越（`../../etc/passwd`）。
//	而"门店 id"这种字段在校验层很容易被漏掉。
func (s *Store) Load(branchID string) (List, error) {
	branchID = normalizeCode(branchID)
	if branchID == "" {
		return List{}, ErrBranchRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.dir == "" {
		return s.loadMemory(branchID)
	}
	b, err := os.ReadFile(s.pathOf(branchID))
	if err != nil {
		if os.IsNotExist(err) {
			// 没有文件 = 从没配过。这**不是**"配了空的"。
			return List{BranchID: branchID, Configured: false}, nil
		}
		return List{}, err
	}
	var l List
	if err := json.Unmarshal(b, &l); err != nil {
		// ⚠️ 名单损坏时**不能**当成"空名单" —— 那会让所有自用供应商
		//   涌回榜单，且没有错误码指向原因。返回错误，让调用方显式降级。
		return List{}, fmt.Errorf("门店 %s 的忽略名单文件损坏: %w", branchID, err)
	}
	l.BranchID = branchID
	return l, nil
}

// Save 写某家门店的名单。
//
// ⚠️ 保存时会把 Configured 置为 true —— 这是"运营显式配过"的唯一凭据。
func (s *Store) Save(branchID string, entries []Entry) (List, error) {
	branchID = normalizeCode(branchID)
	if branchID == "" {
		return List{}, ErrBranchRequired
	}
	// 去重 + 去空 + 排序，保证同样内容存两次得到同样的文件
	// （否则 diff 会一直显示"有变化"，人工排障时徒增噪音）。
	cleaned := dedupe(entries)
	l := List{
		BranchID:   branchID,
		Entries:    cleaned,
		Configured: true,
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.dir == "" {
		s.saveMemory(branchID, l)
		return l, nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return List{}, err
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return List{}, err
	}
	// 原子写：中断不能留下半截 JSON —— 那会让下次 Load 直接报错，
	// 名单在两次编辑之间整个不可用。
	tmp := s.pathOf(branchID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return List{}, err
	}
	return l, os.Rename(tmp, s.pathOf(branchID))
}

// Delete 清空某家门店的名单（"重新开始配"的入口）。
//
// ⚠️ 删除后 Configured 变回 false —— 于是空名单告警会重新出现，
//   这是刻意的：删掉配置就等于回到"不知道哪些是自用"的状态。
func (s *Store) Delete(branchID string) error {
	branchID = normalizeCode(branchID)
	if branchID == "" {
		return ErrBranchRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir == "" {
		memStores.mu.Lock()
		delete(memStores.data, s.memKey(branchID))
		memStores.mu.Unlock()
		return nil
	}
	if err := os.Remove(s.pathOf(branchID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// pathOf 返回某门店的文件路径。
//
// ⚠️ 这里做文件名安全化：只允许字母数字与 `-_`，
// 其余字符（含 `.`、`/`、`\`）替换为 `_`。
//
// 门店 id 来自 HTTP 头，而"路径穿越"这类问题在校验层很容易被漏 ——
// 它不像 SQL 注入那样有明显的语法特征，看上去只是个普通字符串拼接。
func (s *Store) pathOf(branchID string) string {
	return filepath.Join(s.dir, "supplier-ignore-"+sanitizeFileName(branchID)+".json")
}

// sanitizeFileName 把不安全的文件名字符替换掉。
//
// ⚠️ 判定条件里必须包含 `\`，因为在 Windows 上反斜杠同样是路径分隔符 ——
//   只挡 `/` 的实现在 Windows 上是**真的能穿越**的。
func sanitizeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		return "_"
	}
	// 截断：文件名有 255 字节上限，而门店 id 没有长度约束。
	if len(out) > 100 {
		return out[:100]
	}
	return out
}

// dedupe 清洗名单：去空、去重、按编码排序。
func dedupe(in []Entry) []Entry {
	seen := make(map[string]bool, len(in))
	out := make([]Entry, 0, len(in))
	for _, e := range in {
		c := normalizeCode(e.Code)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		e.Code = c
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// ---- 内存模式（仅测试/本地）----

var memStores = struct {
	mu   sync.Mutex
	data map[string]List
}{data: map[string]List{}}

func (s *Store) loadMemory(branchID string) (List, error) {
	memStores.mu.Lock()
	defer memStores.mu.Unlock()
	l, ok := memStores.data[s.memKey(branchID)]
	if !ok {
		return List{BranchID: branchID, Configured: false}, nil
	}
	return l, nil
}

func (s *Store) saveMemory(branchID string, l List) {
	memStores.mu.Lock()
	defer memStores.mu.Unlock()
	memStores.data[s.memKey(branchID)] = l
}

func (s *Store) memKey(branchID string) string {
	if s.dir == "" {
		return "mem:" + branchID
	}
	return s.dir + "|" + branchID
}
