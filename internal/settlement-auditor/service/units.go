// Package service / units.go —— 商品「组」与单位换算（2026-11-01 用户口径）。
//
// # 为什么需要「组」
//
// 思迅里整件进货、拆散零售的商品（大米/饮料/啤酒）销售日结只记**子商品**的销量，
// 进货却记在**父商品**上。不归并就是"进货 1 包、卖出 0" ⇒ 正常进货被判超量。
//
// 归并口径：把「1 父 = N 子」当成同一件商品的两个单位：
//
//	组库存 = Σ(成员库存 × 换算率)
//	组销量 = Σ(成员销量 × 换算率)
//	组净进货 = Σ(成员本次净进货 × 换算率)
//
// 换算率以**组基准单位**为准（基准 = 组里最上层的父商品，通常是整件/提），
// 这样「1 件」的进货量在组里就是 1，和「可售上限天数 × 日均销量」同一口径。
//
// # 数据来源
//
// 父子关系与换算数量只存在于商品档案（cube `item_combo` 模型，
// 源表 t_bd_item_combsplit）—— 销售/库存流水里没有拆分数据（见该 schema 注释）。
// ⚠️ 只采信 quantity > 0 的关系；`other1='6'`（组合打包）没有数量，不参与换算。
package service

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/settlement-auditor/model"
)

// ComboRelation 是一对「父商品 → 子商品 × 数量」的组合/拆分关系。
type ComboRelation struct {
	ParentID string
	ChildID  string
	// Quantity 是 1 个父商品等于多少个子商品（> 0 才可换算）。
	Quantity decimal.Decimal
}

// ProductUnitInfo 是商品档案里的单位与规格（换算兜底与单位一致性判定用）。
type ProductUnitInfo struct {
	// Unit 是商品档案上的**基本单位**名（思迅 product.unit，如 提/罐/kg）。
	Unit string
	// Spec 是规格原文（如 1*12 / 500g / 1.5L）。
	Spec string
}

// productGroupIndex 是「货号 → 所属组 + 换算率」的索引。
type productGroupIndex struct {
	// groupOf 是货号 → 组键。
	groupOf map[string]string
	// factorOf 是 组键 → (货号 → 换算率)：1 个货号 = factor 个**组基准单位**。
	factorOf map[string]map[string]decimal.Decimal
	// membersOf 是 组键 → 组成员（已排序）。
	membersOf map[string][]string
}

// convert 返回把一个货号的数量换算成组基准单位的倍数（未知货号按 1）。
func (g *productGroupIndex) convert(groupKey, productID string) decimal.Decimal {
	if g == nil {
		return decimal.NewFromInt(1)
	}
	if fs, ok := g.factorOf[groupKey]; ok {
		if f, ok := fs[productID]; ok && f.IsPositive() {
			return f
		}
	}
	return decimal.NewFromInt(1)
}

// group 返回货号所属组键（未知 ⇒ 货号自身当单成员组）。
func (g *productGroupIndex) group(productID string) string {
	if g == nil {
		return groupKeyOf([]string{productID})
	}
	if k, ok := g.groupOf[productID]; ok {
		return k
	}
	return groupKeyOf([]string{productID})
}

// members 返回组键的成员（未知 ⇒ 只含该货号，由调用方保证传入的是组键）。
func (g *productGroupIndex) members(groupKey string) []string {
	if g == nil {
		return nil
	}
	return g.membersOf[groupKey]
}

// buildProductGroups 按组合/拆分关系给**本次审批涉及的商品**建组。
//
// 只处理与涉及商品**同连通分量**的关系：审批不涉及的商品不会被拉进来
// （否则一个常见父商品就能把半个商品档案带进查询）。
//
// ⚠️ 换算率以「组里最上层的父商品」为基准（整件/提），于是「1 件」= 1。
// 找不到唯一父（环形关系/脏数据）时退回按货号字典序取最小者当基准 ——
// 口径仍然是确定的，只是"1 件"可能变成"1 罐"，但**判据是比值，量级不影响结论**。
func buildProductGroups(products []string, relations []ComboRelation) *productGroupIndex {
	idx := &productGroupIndex{
		groupOf:   make(map[string]string, len(products)),
		factorOf:  make(map[string]map[string]decimal.Decimal),
		membersOf: make(map[string][]string, len(products)),
	}
	if len(products) == 0 {
		return idx
	}
	wanted := make(map[string]struct{}, len(products))
	for _, p := range products {
		p = strings.TrimSpace(p)
		if p != "" {
			wanted[p] = struct{}{}
		}
	}
	// 邻接表：有效关系（quantity > 0）双向展开，边上带着"谁是父"的方向。
	type edge struct {
		other      string
		quantity   decimal.Decimal
		otherIsKid bool // true = other 是子商品
	}
	adj := make(map[string][]edge, len(relations)*2)
	for _, r := range relations {
		parent := strings.TrimSpace(r.ParentID)
		child := strings.TrimSpace(r.ChildID)
		if parent == "" || child == "" || parent == child || !r.Quantity.IsPositive() {
			continue
		}
		adj[parent] = append(adj[parent], edge{other: child, quantity: r.Quantity, otherIsKid: true})
		adj[child] = append(adj[child], edge{other: parent, quantity: r.Quantity, otherIsKid: false})
	}
	// 关系表很小（实测 500 余行）⇒ 直接 BFS 每个涉及商品。
	for pid := range wanted {
		if _, ok := idx.groupOf[pid]; ok {
			continue
		}
		// 1) 收集连通分量（只沿有效关系走）。
		comp := []string{pid}
		seen := map[string]struct{}{pid: {}}
		queue := []string{pid}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, e := range adj[cur] {
				if _, ok := seen[e.other]; ok {
					continue
				}
				seen[e.other] = struct{}{}
				comp = append(comp, e.other)
				queue = append(queue, e.other)
			}
		}
		sort.Strings(comp)
		key := groupKeyOf(comp)
		// 2) 基准 = 分量里**不是子商品**的节点（整件/提那一层），取字典序最小。
		children := make(map[string]struct{}, len(comp))
		for _, node := range comp {
			for _, e := range adj[node] {
				if e.otherIsKid {
					children[e.other] = struct{}{}
				}
			}
		}
		base := ""
		for _, node := range comp {
			if _, isKid := children[node]; !isKid {
				base = node
				break
			}
		}
		if base == "" {
			base = comp[0]
		}
		// 3) 从基准出发算换算率（每个成员 = 多少个基准单位）。
		factors := map[string]decimal.Decimal{base: decimal.NewFromInt(1)}
		queue = []string{base}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			cf := factors[cur]
			for _, e := range adj[cur] {
				if _, ok := factors[e.other]; ok {
					// 同一对商品重复配置时以先到者为准（关系表是人工维护的，
					// 重复行不代表有第二个换算口径）。
					continue
				}
				if e.otherIsKid {
					// 1 cur(父) = quantity 个 other(子) ⇒ 1 子 = cf / quantity 基准单位。
					//
					// ⚠️ **不四舍五入**：换算率要一路乘到上百上千的数量上，
					// 1/12 截成 0.0833 会让 120 罐变成 9.996 件（差 0.4%），
					// 组口径的库存/销量比较就失真了。
					factors[e.other] = cf.Div(e.quantity)
				} else {
					// 1 other(父) = quantity 个 cur(子) ⇒ 1 父 = cf × quantity 基准单位。
					factors[e.other] = cf.Mul(e.quantity)
				}
				queue = append(queue, e.other)
			}
		}
		idx.membersOf[key] = comp
		idx.factorOf[key] = factors
		for _, node := range comp {
			idx.groupOf[node] = key
		}
	}
	// 本次涉及但不在任何关系里的商品：单成员组（换算率 1）。
	for pid := range wanted {
		if _, ok := idx.groupOf[pid]; ok {
			continue
		}
		key := groupKeyOf([]string{pid})
		idx.groupOf[pid] = key
		idx.membersOf[key] = []string{pid}
		idx.factorOf[key] = map[string]decimal.Decimal{pid: decimal.NewFromInt(1)}
	}
	return idx
}

// groupKeyOf 生成组的稳定键：成员货号排序后用 "|" 连接。
//
// ⚠️ 组键会落库（stocking_floors 的唯一键）⇒ 超长时截断 + 摘要，
// 避免 PostgreSQL 索引行超限；摘要保证不同成员集合不会撞到同一个键。
func groupKeyOf(members []string) string {
	uniq := make([]string, 0, len(members))
	seen := make(map[string]struct{}, len(members))
	for _, m := range members {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		uniq = append(uniq, m)
	}
	sort.Strings(uniq)
	joined := strings.Join(uniq, "|")
	if len(joined) <= 150 {
		return joined
	}
	sum := md5.Sum([]byte(joined))
	return "h:" + hex.EncodeToString(sum[:])[:16]
}

// parseItemSizeFactor 从规格原文里解析「一件等于多少个基本单位」。
//
// 只认 `A*B` / `A*B*C` 这种明确的装箱写法（实测商品档案里大量存在
// 1*12 / 1*24 / 1*24*32）。重量/容量写法（500g / 1.5L / -4斤箱）**不解析**：
//
//	文档里没有说清"一件"是袋还是箱，硬猜会把下限放大，反而弱化审核。
//
// 解析不出 ⇒ 1（下限 = 一个基本单位，最保守）。
func parseItemSizeFactor(spec string) decimal.Decimal {
	s := strings.ToLower(strings.TrimSpace(spec))
	s = strings.NewReplacer("×", "*", "Ｘ", "*", "x", "*", " ", "", "　", "").Replace(s)
	if !strings.Contains(s, "*") {
		return decimal.NewFromInt(1)
	}
	parts := strings.Split(s, "*")
	factor := decimal.NewFromInt(1)
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n <= 0 || n > 10000 {
			return decimal.NewFromInt(1)
		}
		factor = factor.Mul(decimal.NewFromInt(int64(n)))
	}
	if !factor.IsPositive() || factor.GreaterThan(decimal.NewFromInt(10000)) {
		return decimal.NewFromInt(1)
	}
	return factor
}

// isPackUnit 判断采购单位名是否是「整件/整箱」这类包装单位。
//
// 只有采购单位是包装单位时，规格里的 `1*12` 才代表"1 件 = 12 个基本单位"；
// 如果采购本来就是按基本单位录的（袋/瓶/罐），规格不能拿来放大下限。
func isPackUnit(unit string) bool {
	switch strings.TrimSpace(unit) {
	case "件", "箱", "整箱", "提", "包", "组", "袋", "盒":
		// ⚠️ 袋/盒 同时是常见基本单位 —— 但若商品档案规格写着 1*12，
		// 说明它就是"一袋 12 小包"这类包装，按包装单位处理是合理的。
		return true
	}
	return false
}

// ---- 品类档位（可售上限天数）----

// categoryCapDays 按品名关键词自动判定品类档位与可售上限天数。
//
// # 用户口径（2026-11-01）
//
//	「保质期维度可以添加，但仍然是自动标记，人工修正会更好，要不然等于虚设」。
//
// ⇒ 用**品名关键词**给出默认档位（不需要人录），人工可在后台改；
// 判不准的一律落 general（45 天），偏宽松但不至于无限——真正的兜底是
// 「进货下限」与「盘点」。
//
// ⚠️ 顺序很重要：先冷冻、再冷藏、再生鲜，最后才看饮料/日用——
// 「速冻水饺」不能被"水"判成饮料。
func categoryCapDays(name string) (class string, capDays int, shelfLifeDays int) {
	n := strings.ToLower(strings.TrimSpace(name))
	switch {
	case containsAny(n, "冷冻", "速冻", "雪糕", "冰淇淋", "冰棍", "汤圆", "水饺", "馄饨", "冻品"):
		return model.ProfileClassFrozen, 30, 180
	case containsAny(n, "酸奶", "牛奶", "鲜奶", "豆奶", "熟食", "卤", "冷藏", "低温",
		"沙拉", "芝士", "黄油", "火腿", "香肠", "豆腐", "豆干", "酱菜"):
		return model.ProfileClassChilled, 20, 30
	case containsAny(n, "菜", "青菜", "白菜", "菠菜", "芹菜", "韭菜", "葱", "蒜", "姜",
		"菇", "藕", "番茄", "西红柿", "黄瓜", "冬瓜", "南瓜", "苦瓜", "丝瓜", "豆角",
		"土豆", "红薯", "山药", "萝卜", "肉", "排骨", "鸡", "鸭", "鱼", "虾", "蟹",
		"蛤", "贝", "蛋", "水果", "苹果", "梨", "香蕉", "橘", "橙", "柚", "桃",
		"葡萄", "西瓜", "哈密瓜", "火龙果", "芒果", "荔枝", "龙眼", "草莓", "蓝莓",
		"猕猴桃", "木瓜", "柠檬", "李子", "枣"):
		return model.ProfileClassFresh, 7, 7
	case containsAny(n, "纸巾", "抽纸", "卷纸", "卫生纸", "湿巾", "洗衣", "洗发", "沐浴",
		"牙膏", "牙刷", "清洁", "洗涤", "洗洁精", "皂", "垃圾袋", "电池", "灯泡",
		"拖把", "抹布", "保鲜膜", "一次性"):
		return model.ProfileClassDaily, 90, 0
	case containsAny(n, "饮料", "可乐", "雪碧", "芬达", "啤酒", "白酒", "红酒", "黄酒",
		"果汁", "矿泉水", "汽水", "苏打", "咖啡", "凉茶", "茶饮", "桶装水", "纯净水"):
		return model.ProfileClassBeverage, 45, 365
	default:
		return model.ProfileClassGeneral, 45, 0
	}
}

func containsAny(s string, keywords ...string) bool {
	for _, k := range keywords {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// pctText 把比例渲染成百分数（"12.3"），用于价格涨幅。
func pctText(ratio decimal.Decimal) string {
	return ratio.Mul(decimal.NewFromInt(100)).Round(1).String()
}

// factorText 渲染换算率（去掉无意义的尾零）。
func factorText(d decimal.Decimal) string {
	return strings.TrimRight(strings.TrimRight(d.StringFixed(4), "0"), ".")
}

// productGroupLabel 渲染组的中文描述，如「加多宝凉茶*12罐/提-4891599332223(组:2 个商品)」。
func productGroupLabel(name, pid string, memberCount int) string {
	label := ProductLabel(map[string]string{pid: name}, pid)
	if memberCount > 1 {
		label = fmt.Sprintf("%s(组合商品 %d 个)", label, memberCount)
	}
	return label
}
