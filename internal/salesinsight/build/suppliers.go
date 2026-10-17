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
	codes := make([]string, 0, len(cur))
	for _, c := range cur {
		codes = append(codes, c.Code)
	}
	dict := map[string]supplier.Record{}
	if recs, err := b.src.SupplierDict(ctx, opt.StoreBranchID, codes); err != nil {
		// ⚠️ 字典拿不到**不**降级为规则版 —— 而是把这些供应商标成"未知"。
		//   降级为规则模式会丢掉整个 LLM 解读，而代价仅仅是
		//   无法排除自用（此时我们选择**保守保留**它们，见下）。
		sufficiency.Sufficient = false
		sufficiency.Issues = append(sufficiency.Issues,
			"供应商字典获取失败，本次无法识别自用渠道（供应商榜单可能包含内部采购）")
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

	// 排除依据：编码 → 原因（可能为空串）。
	// 仅用 Code 匹配，名称会变（见 ignore.Codes 的说明）。
	reasonByCode := map[string]string{}
	if cfgErr == nil {
		for _, e := range ignored.Entries {
			reasonByCode[strings.TrimSpace(e.Code)] = strings.TrimSpace(e.Reason)
		}
	}

	for _, c := range cur {
		name := strings.TrimSpace(dict[c.Code].Name)
		if name == "" {
			// 没有字典记录时用编码占位，而不是丢弃 ——
			// 丢弃会让这家供应商的销量凭空从大盘里消失。
			name = "供应商" + c.Code
		}

		_, isIgnored := reasonByCode[c.Code]
		if isIgnored {
			reason := reasonByCode[c.Code]
			if reason == "" {
				reason = "已加入本门店的供应商忽略名单"
			}
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

// ignoredSuppliers 读该门店的人工忽略名单。
//
// ⚠️ 返回值里 Configured 必���被调用方使用 ——
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
