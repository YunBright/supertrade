package build

import (
	"context"
	"sort"
	"strings"

	"github.com/YunBright/supertrade/internal/salesinsight/fact"
	"github.com/YunBright/supertrade/internal/salesinsight/fact/supplier"
	"github.com/YunBright/supertrade/internal/salesinsight/ignore"
)

// buildSuppliers 组装供应商排行，并把自用/客户渠道**保留但排除**。
//
// # 为什么要先取**多于** TopN 的供应商再排除
//
// ⚠️ 这是本函数最容易做错的地方。
//
// 假设某个门店排名前 10 的供应商里有 4 个是自用直供，
// 若直接取 Top10 再排除，剩下只有 6 个真供应商 ——
// 而第 11、12 名可能是很好的外部货源，永远进不了榜单。
//
// 所以流程是：取 TopN × 3（上限 30），排除掉自用/客户，
// 再截到 TopN。这样榜单上永远是**真实供应商**。
//
// 排除掉的那些**保留在 ExcludedSuppliers 里**：
//  1. 店长需要知道"这部分销量走了自采渠道"，否则它从总量里凭空消失；
//  2. LLM 需要知道存在自采渠道，否则会把内部直供误读成供应商能力。
func (b *Builder) buildSuppliers(
	ctx context.Context,
	opt Options,
	f fact.Fact,
	topN int,
	sufficiency *fact.Sufficiency,
	// src 是**请求级**数据源（见 Builder.route）。
	// ⚠️ 必须由调用方传进来而不是在这里取 b.src：
	//   取 b.src 会绕过按区间的 PG/cube 路由，
	//   于是同一份报告里"品类来自 PG、供应商来自 cube"——
	//   那正是这个路由要消灭的口径分叉。
	src DataSource,
) ([]fact.Supplier, []fact.ExcludedSupplier) {
	cur, err := b.src.SupplierSales(ctx, opt.StoreBranchID, f.Meta.From, f.Meta.To, supplierScanCount(topN))
	if err != nil {
		sufficiency.Sufficient = false
		sufficiency.Issues = append(sufficiency.Issues, "供应商数据获取失败，本次不含供应商分析")
		return nil, nil
	}
	if len(cur) == 0 {
		// 没有供应商数据不算"不足" —— 可能这家门店全是自营。
		return nil, nil
	}

	prevByCode := map[string]float64{}
	if prev, err := b.src.SupplierSales(ctx, opt.StoreBranchID, f.Meta.PrevFrom, f.Meta.PrevTo, supplierScanCount(topN)); err == nil {
		for _, p := range prev {
			prevByCode[p.Code] = p.Amount
		}
	} else {
		sufficiency.Sufficient = false
		sufficiency.Issues = append(sufficiency.Issues, "上期供应商数据获取失败，供应商环比不可用")
	}

	// ---- 字典：一次性补齐所有编码的名称与类型 ----
	//
	// ⚠️ 不能按需逐个查：那是 N 次调用，而一次 in 查询就能拿全部。
	//
	// ⚠️ 这份字典**唯一**的用途是补供应商名称
	// （见 supplierDisplayName）—— 排除判定只认人工名单，与字典无关。
	// 2026-10-27 修复前，这里建了 dict 却**从未被读**，
	// 于是 cube 路径（Name 恒为空）全部退化成「供应商131」这种占位符。
	codes := make([]string, 0, len(cur))
	for _, c := range cur {
		codes = append(codes, c.Code)
	}
	dict := map[string]supplier.Record{}
	if recs, err := b.src.SupplierDict(ctx, opt.StoreBranchID, codes); err != nil {
		// ⚠️ 字典拿不到**不**降级为规则版 —— 降级会丢掉整个 LLM 解读，
		//   而此时我们只是**不知道名字**（用编码占位即可）。
		//
		// ⚠️ 告警文案必须说清**真实影响**：
		//   2026-10-17 起排除判定只认人工名单（ignore.IndexForSales），
		//   与字典**完全无关**。所以字典挂了并不会让自用供应商混进榜单 ——
		//   早先那句「无法识别自用渠道（供应商榜单可能包含内部采购）」
		//   与事实相反，会把店长和 LLM 引到错误的怀疑方向上。
		sufficiency.Sufficient = false
		sufficiency.Issues = append(sufficiency.Issues,
			"供应商字典获取失败，供应商名称暂用编码占位（不影响排除判定）")
	} else {
		for _, r := range recs {
			dict[r.Code] = r
		}
	}

	// ---- 逐条分类 ----
	//
	// ⚠️ 判定只认**人工名单**（ignore.List），不再跑三层自动判定。
	//
	// 改动理由：自动判定是"每次都做的智能活"，而名单是"配一次用很久"的配置。
	// 让智能只发生在**维护名单时**（见 ignore.Suggest），运行时就查表。
	kept := make([]fact.Supplier, 0, topN)
	var excluded []fact.ExcludedSupplier

	ignored, cfgErr := b.ignoredSuppliers(opt.StoreBranchID)
	switch {
	case cfgErr != nil:
		// ⚠️ 名单读不出来**不能**当成"空名单"继续 ——
		//   那会让自用供应商全部涌回榜单，而错误码会被折叠成
		//   一句"供应商榜单可能包含内部采购"。
		//   保守做法：保守保留（不排除任何供应商）+ 显式记为数据不足。
		//   代价是榜单可能含自采渠道，好过静默排除真供应商。
		sufficiency.Sufficient = false
		sufficiency.Issues = append(sufficiency.Issues,
			"供应商忽略名单读取失败，本次无法排除自营采购单元（榜单可能包含内部采购）")
	case !ignored.Configured:
		// ⚠️ **这条是本次改造最重要的分支**。
		//
		// "没配过"与"配了空的"在数据上完全一样，但含义相反：
		//   前者 = 运营还没配 → 自用供应商正涌进榜单
		//   后者 = 这家店确实全是外采 → 榜单本来就该是干净的
		//
		// 若静默处理，前者会让店长把门店自己的蔬菜采购
		// 当成"外部优质货源"来学习。
		// → 所以必须记为数据不足，让它在报告与后台里**可见**。
		sufficiency.Sufficient = false
		sufficiency.Issues = append(sufficiency.Issues,
			"本门店尚未配置供应商忽略名单，供应商榜单可能包含门店自营采购单元"+
				"（请在 App 的销售报表 → 供应商忽略名单 中配置）")
	}

	// 排除依据：销售编码 → 名单条目。
	//
	// ⚠️ 必须走 IndexForSales 对齐，而不是拿名单编码直接查表：
	//   名单录的是字典侧编码，流水侧可能少一个前导零
	//   （线上实测：名单 `029 猪肉自营` vs 流水 `29`），
	//   直接查表会让全店最大的供应商静默逃过排除。
	//   详见 ignore.IndexForSales 的三条硬约束。
	ignoredByCode := map[string]ignore.Entry{}
	if cfgErr == nil {
		ignoredByCode = ignore.IndexForSales(ignored.Entries, codes)
	}

	for _, c := range cur {
		// ⚠️ 名称必须**逐级回落**（见 supplierDisplayName）：
		//   cube 路径的销售行 Name 恒为空，早期实现直接落到
		//   "供应商"+编码 的占位符 —— 于是整份报告的供应商全是
		//   「供应商131」「供应商29」，店长和 LLM 都无从判断。
		name := supplierDisplayName(c.Name, dict, c.Code)

		e, isIgnored := ignoredByCode[c.Code]
		reason := ""
		if isIgnored {
			reason = strings.TrimSpace(e.Reason)
			if reason == "" {
				reason = "已加入本门店的供应商忽略名单"
			}
			if name == "" {
				// 名单条目上的名字是人工填的，可能是**唯一**可读的名字。
				name = strings.TrimSpace(e.Name)
			}
		}
		if name == "" {
			// 字典里没有记录时用编码占位，而不是丢弃 ——
			// 丢弃会让这家供应商的销量凭空从大盘里消失。
			name = "供应商" + c.Code
		}

		if isIgnored {
			excluded = append(excluded, fact.ExcludedSupplier{
				Code:   c.Code,
				Name:   name,
				Reason: reason,
				Rule:   string(fact.ExcludeManualList),
				Amount: c.Amount,
			})
			continue
		}

		s := fact.Supplier{
			Code:         c.Code,
			Name:         name,
			Amount:       c.Amount,
			Qty:          c.Qty,
			ProductCount: c.ProductCount,
		}
		if prevAmt, ok := prevByCode[c.Code]; ok {
			s.PrevAmount = prevAmt
			s.DeltaPct = pctChange(c.Amount, prevAmt)
			s.HasPrev = true
		}
		s.TopProducts = toTopProducts(c.TopItems)
		kept = append(kept, s)
	}

	sortSuppliersByAmountSlice(kept)
	if len(kept) > topN {
		kept = kept[:topN]
	}
	return kept, excluded
}

// supplierDisplayName 取供应商的展示名。
//
// # 取名顺序（为什么是这个顺序）
//
//  1. **销售行自带名**：PG 路径在采集时补过一次字典，权威；
//  2. **字典精确命中**：cube 路径的 Name 恒为空 —— supplier 是
//     storage: duck 模型，与 live 的 sales_report **不能 join**
//     （cube 的 join checkStorage 在查询构造期就拒绝 live→duck），
//     所以必须另查一次字典来补名。
//
// ⚠️ **第 2 步必须精确匹配，不能补零/去零。**
//
// ✅ 实测（2026-10-27，门店 00）：字典表里
//
//	`29 猪肉自营` 与 `029 猪肉联营` 是**两个不同的供应商**，
//	`19 同辉纸厂` 与 `019 自购蔬菜` 同理。
//
// 而销售侧（t_rm_daysum.supcust_no）与字典侧（t_bd_supcust_info.supcust_no）
// 都是同一个源库里的 RTRIM 结果 ⇒ **同一供应商的编码逐字相同**，
// 精确匹配就能对上（实测 60 个销售编码 100% 命中字典）。
//
// 反过来，若在这里做"变体匹配"，销售侧 `29` 就可能被安上
// `029 猪肉联营` 的名字 —— **名字错了比没有名字更糟**：
// 没有名字店长知道要去查，错了名字会让店长按错误的供应商
// 去谈供货。
//
// 取不到时返回空串，由调用方决定回落（名单条目名 / 编码占位符）。
func supplierDisplayName(rowName string, dict map[string]supplier.Record, code string) string {
	if n := strings.TrimSpace(rowName); n != "" {
		return n
	}
	if rec, ok := dict[code]; ok {
		if n := strings.TrimSpace(rec.Name); n != "" {
			return n
		}
	}
	return ""
}

// ignoredSuppliers 读该门店的人工忽略名单。
//
// ⚠️ 返回值里 Configured 必须被调用方使用 ——
//
//	"没配过"与"配了空的"必须能被区分，否则前者会静默地
//	让自用供应商涌回榜单（见 buildSuppliers 里的 switch）。
func (b *Builder) ignoredSuppliers(branchID string) (ignore.List, error) {
	if b.ignores == nil {
		// 未装配名单存储 = 本服务不参与排除。
		// 标记为"未配置"，让上层产生告警而不是静默放行。
		return ignore.List{BranchID: branchID, Configured: false}, nil
	}
	return b.ignores.Load(branchID)
}

// supplierScanCount 决定扫多少个供应商。
//
// 取 TopN×3 且不超过 30：排除掉自用后仍能凑够 TopN 个真实供应商。
// 30 是上限，防止某个门店供应商极多时扫描过重。
func supplierScanCount(topN int) int {
	n := topN * 3
	if n > 30 {
		n = 30
	}
	if n < topN {
		n = topN
	}
	return n
}

// toTopProducts 把供应商维度下的商品明细转成 fact.TopProduct。
func toTopProducts(items []SupplierItem) []fact.TopProduct {
	if len(items) == 0 {
		return nil
	}
	out := make([]fact.TopProduct, 0, len(items))
	for _, it := range items {
		out = append(out, fact.TopProduct{
			ItemNo: it.ItemNo,
			Name:   it.Name,
			Amount: it.Amount,
			Qty:    it.Qty,
		})
	}
	return out
}

// sortSuppliersByAmountSlice 按金额降序排 fact.Supplier。
func sortSuppliersByAmountSlice(s []fact.Supplier) {
	sort.SliceStable(s, func(i, j int) bool { return s[i].Amount > s[j].Amount })
}
