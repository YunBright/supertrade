package build

import (
	"context"
	"fmt"
	"strings"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/salesinsight/fact/supplier"
)

// SupplierDictPage 分页列出某门店的供应商字典（供 App 维护忽略名单）。
//
// # 为什么不能直接复用 SupplierDict
//
//	SupplierDict 是"按**已知编码**补名称与类型"，codes 为空时直接返回空
//	（见它的实现开头）。而 App 打开配置页时手上**没有任何编码** ——
//	它要的是"这个门店有哪些供应商可勾"。
//
// # ⚠️ 搜索在本侧做，而不是下推到 cube
//
//	cube 的 supplier 是 duck 快照，其 member 上没有建 name 的
//	模糊索引；实测把 like 下推要么报 unknown filter，要么全表扫。
//	而单店字典只有 219 条 —— 先拉回候选再在内存里过滤，
//	代价可以忽略，换来的是"搜索一定能用"这个确定性。
//
// → 所以 q 非空时**按 limit 放大取**再过滤，
//
//	这样"搜到 0 条"只可能是真没有，而不会是"取少了"。
func (s *DaprSource) SupplierDictPage(
	ctx context.Context,
	storeBranchID string,
	offset, limit int,
	query string,
) ([]supplier.Record, error) {
	if limit <= 0 {
		limit = 300
	}
	if limit > MaxDictPageSize {
		limit = MaxDictPageSize
	}
	if offset < 0 {
		offset = 0
	}

	// 搜索时放大取数窗口：过滤发生在内存，窗口小了会漏结果。
	fetch := limit
	needle := strings.TrimSpace(query)
	if needle != "" {
		// 放大到 4 倍仍然会漏，但它只影响"翻页到底"，
		// 而 App 的用法是"看到候选就够"，不是"穷举全库"。
		fetch = limit * 4
		if fetch > MaxDictPageSize {
			fetch = MaxDictPageSize
		}
	}

	// ⚠️ withBranch 必须是：supplier 是 duck 模型，同样按 X-Branch-ID
	//   路由到这家门店的 DuckDB。漏了就是 400 branch_required，
	//   而症状会被误读成"这家店没有供应商"。
	data, err := s.c.LoadCubeQuery(withBranch(ctx, storeBranchID), "supplier", cubeclient.CubeQuery{
		Measures:   []string{"supplier.count"},
		Dimensions: []string{"supplier.id", "supplier.name", "supplier.type", "supplier.cust_flag", "supplier.supply_way"},
		// ⚠️ 字典表是全量快照（不是按日期），所以不给日期过滤 ——
		//   多传一个日期 range 会被 cube 当成"这段时间内存在的供应商"，
		//   于是长期停用但字典里还在的供应商会查不到。
		Limit: fetch,
	})
	if err != nil {
		return nil, fmt.Errorf("build: cube supplier dict page query: %w", err)
	}

	out := make([]supplier.Record, 0, len(data))
	for _, row := range data {
		code := str(row, "supplier.id")
		if code == "" {
			continue
		}
		rec := supplier.Record{
			Code:      code,
			Name:      str(row, "supplier.name"),
			Type:      str(row, "supplier.type"),
			CustFlag:  str(row, "supplier.cust_flag"),
			SupplyWay: str(row, "supplier.supply_way"),
		}
		if needle != "" && !matchesDictQuery(rec, needle) {
			continue
		}
		out = append(out, rec)
	}

	if offset >= len(out) {
		return []supplier.Record{}, nil
	}
	end := offset + limit
	if end > len(out) {
		end = len(out)
	}
	return out[offset:end], nil
}

// MaxDictPageSize 是单页字典条数上限（与 handler 层保持一致）。
const MaxDictPageSize = 500

// matchesDictQuery 判断一条记录是否命中搜索词。
//
// ⚠️ 同时匹配**编码与名称**：运营手里可能只有编码（从流水单上看到的一串数字）。
//
//	只匹配名称的话，那种搜索会返回空 ——
//	而"搜不到"在这个页面会被理解成"这家店没这个供应商"。
func matchesDictQuery(rec supplier.Record, needle string) bool {
	n := strings.ToLower(needle)
	return strings.Contains(strings.ToLower(rec.Name), n) ||
		strings.Contains(strings.ToLower(rec.Code), n)
}
