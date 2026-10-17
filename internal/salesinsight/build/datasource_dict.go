package build

import (
	"context"
	"fmt"
	"strings"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/salesinsight/fact"
	"github.com/YunBright/supertrade/internal/salesinsight/fact/supplier"
)

// SupplierDict 实现 DataSource：按编码批量补供应商字典。
//
// # 为什么必须单独查一次（而不是 join）
//
// ✅ 实测（2026-10-16）：supplier 是 **storage: duck** 模型，
//
//	而 sales_report 是 **live**。cube 的 join.go checkStorage
//	只允许 live→live，live→duck 在**查询构造期**就被拒绝。
//
// 所以流程固定是两步：先按流水聚合拿到 Top-N 供应商编码，
// 再按这批编码查一次字典补名称与类型。两次查询都只传编码列表，
// 数据量很小。
//
// # 跨 storage 的时间不一致
//
// supplier 是 DuckDB 快照（按 refresh 周期刷新），
// sales_report 是实时打源库。所以这里的名称可能比销售流水晚几分钟。
// 对"哪个供应商单品更好"这种结论，这个延迟可以接受 ——
// 供应商名称不会在一天之内变。不接受的话得把 supplier 改成 live，
// 代价是每次查都打源库。
func (s *DaprSource) SupplierDict(ctx context.Context, storeBranchID string, codes []string) ([]supplier.Record, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	// 夹住查询规模：TopN 已经限制了供应商个数（≤20），
	// 这里再加一道是为了防止将来某个调用方传入全量编码。
	if len(codes) > maxDictCodes {
		codes = codes[:maxDictCodes]
	}

	vals := make([]any, 0, len(codes))
	for _, c := range codes {
		if c = strings.TrimSpace(c); c != "" {
			vals = append(vals, c)
		}
	}
	if len(vals) == 0 {
		return nil, nil
	}

	// ⚠️ withBranch 是必须的：supplier 是 duck 模型，同样按 X-Branch-ID
	//   路由到这家门店的 DuckDB。漏了就是 400 branch_required（见接口注释）。
	data, err := s.c.LoadCubeQuery(withBranch(ctx, storeBranchID), "supplier", cubeclient.CubeQuery{
		Measures:   []string{"supplier.count"},
		Dimensions: []string{"supplier.id", "supplier.name", "supplier.type", "supplier.cust_flag", "supplier.supply_way"},
		Filters: []cubeclient.CubeFilter{
			{Member: "supplier.id", Operator: "in", Values: vals},
		},
		Limit: len(vals) + 10,
	})
	if err != nil {
		return nil, fmt.Errorf("build: cube supplier dict query: %w", err)
	}
	out := make([]supplier.Record, 0, len(data))
	for _, row := range data {
		code := str(row, "supplier.id")
		if code == "" {
			continue
		}
		out = append(out, supplier.Record{
			Code:      code,
			Name:      str(row, "supplier.name"),
			Type:      str(row, "supplier.type"),
			CustFlag:  str(row, "supplier.cust_flag"),
			SupplyWay: str(row, "supplier.supply_way"),
		})
	}
	return out, nil
}

// maxDictCodes 是字典查询的编码数上限。
const maxDictCodes = 50

// StockByItems 实现 DataSource：批量查库存。
//
// # ⚠️ 为什么"查不到"和"库存为 0"必须区分
//
// ✅ 实测思迅体系里，有些商品从未进入库存系统（临时摊位的生鲜、
//
//	临时促销品）。它们在 t_im_branch_stock 里**没有行**。
//
// 若把"没有行"当成"库存为 0"，归因会把这些商品判成"缺货"，
// 店长会去补一批本来就不该管的货。所以这里返回 map 里
// **不出现**的 itemNo，在 fact.StockRecord.HasRow=false 时被正确处理。
//
// 这一条在 fact.attribution 的单测里有专门用例。
func (s *DaprSource) StockByItems(
	ctx context.Context, storeBranchID string, itemNos []string,
) (map[string]fact.StockRecord, error) {
	out := map[string]fact.StockRecord{}
	if len(itemNos) == 0 {
		return out, nil
	}
	if len(itemNos) > maxStockQuery {
		itemNos = itemNos[:maxStockQuery]
	}
	vals := make([]any, 0, len(itemNos))
	for _, c := range itemNos {
		if c = strings.TrimSpace(c); c != "" {
			vals = append(vals, c)
		}
	}
	if len(vals) == 0 {
		return out, nil
	}

	data, err := s.c.LoadCubeQuery(withBranch(ctx, storeBranchID), "stock", cubeclient.CubeQuery{
		Measures:   []string{"stock.total_quantity"},
		Dimensions: []string{"stock.product_id"},
		Filters: []cubeclient.CubeFilter{
			{Member: "stock.product_id", Operator: "in", Values: vals},
		},
		Limit: len(vals) + 10,
	})
	if err != nil {
		return nil, fmt.Errorf("build: cube stock query: %w", err)
	}
	for _, row := range data {
		itemNo := str(row, "stock.product_id")
		if itemNo == "" {
			continue
		}
		out[itemNo] = fact.StockRecord{
			ItemNo:   itemNo,
			Quantity: num(row, "stock.total_quantity"),
			// ⚠️ stock 模型有 low_stock_count measure，但它是**全店**聚合值，
			//   按单个 product_id 分组时 min_qty 为 NULL 的行会算出 0，
			//   无法区分"没设安全库存"和"设了但没超"。
			//   所以这里取 0（表示未设），由 ClassifyStock 保守处理：
			//   MinQty=0 时不会判为低库存。
			MinQty: 0,
			HasRow: true,
		}
	}
	return out, nil
}

// maxStockQuery 是库存查询的 item 数上限。
//
// 取值依据：只有 TopN（≤20）个商品需要库存判断，
// 60 留出余量以便将来扩展到"上期排行商品"也要查库存。
const maxStockQuery = 60
