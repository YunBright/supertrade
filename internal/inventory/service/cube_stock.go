// Package service / cube_stock.go —— 走 cube 的库存读路径。
//
// # cube 查询怎么拼
//
// cube 的 `stock` model 是 **live**（透传思迅 t_im_branch_stock 的当前值），
// 维度 = (product_id, branch_id, updated_at)，度量 = count / total_quantity / avg_cost …。
//
// ⚠️ cube 的**硬约束**（拼错就是 400 / 静默返空）：
//
//  1. **查询必须带 measure**，否则 400 `QUERY_INVALID: at least one measure required`。
//  2. **order 的 id 必须同时在 SELECT 的 dimensions 里**
//     （否则 GROUP BY 查询在 DB 层报 "not a grouped column"）。
//  3. **offset > 0 必须给 order**（否则 400：分页必须建立在稳定排序上）。
//  4. **要总数必须 total:true**（不给时 cube 恒返 0 —— 那是"没问"，不是"总共 0 条"）。
//
// # 为什么 dimension 只有 product_id
//
// cube 的查询是 **GROUP BY dimensions** ⇒ 选一个维度就一行一个商品，
// 排序列（product_id）因此天然是**全序** ⇒ 翻页不会重复 / 漏行。
//
// ⚠️ **不要**把 stock.branch_id 也选进来：
// 那是思迅的 branch_no（实测 "0001"），与本系统 branch_id（"00"）是**两套无关的编码**
// （见 internal/cubeclient.GetStock 的注释）⇒ 选进来会多一层无意义的分组。
// 门店隔离靠 X-Branch-ID → cube-router → branch_cube_sources → 该门店专属 cube 实例。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/inventory/model"
)

// cubeStockModel 是 cube 里的库存 model 名。
const cubeStockModel = "stock"

// listLevelsFromCube 走 cube 查库存快照列表（门店配了数据源时）。
//
// ⚠️ **分页 / 过滤 / 排序全部交给 cube**：前端只能排当前这一页，
// 而 cube 那边是全量口径 ⇒ 翻页顺序必须由 cube 保证。
func (s *StockService) listLevelsFromCube(ctx context.Context, q StockLevelQuery) ([]model.StockLevel, int64, error) {
	q = q.normalized()

	// ⚠️ **filters 必须是非 nil 的空切片**（不是 nil）：
	// cubeclient 会把 filters 原样写进 body，nil ⇒ "filters": null ⇒ cube 拒收。
	filters := make([]cubeclient.CubeFilter, 0, 2)
	if q.ProductID != "" {
		filters = append(filters, cubeclient.CubeFilter{
			Member:   "stock.product_id",
			Operator: cubeclient.OperatorEquals,
			Values:   []any{q.ProductID},
		})
	}
	if q.Keyword != "" {
		// ⚠️ 用 contains（大小写敏感）而不是 containsCI：
		// 本域 PG 的等价口径是 `product_id LIKE '%kw%'`（PG 的 LIKE 大小写敏感）
		// ⇒ 两边口径一致，别让同一个关键词在两个门店查出不同结果。
		filters = append(filters, cubeclient.CubeFilter{
			Member:   "stock.product_id",
			Operator: cubeclient.OperatorContains,
			Values:   []any{q.Keyword},
		})
	}

	data, total, err := s.cube.LoadCubeQueryPage(ctx, cubeStockModel, cubeclient.CubeQuery{
		// ⚠️ **必须带 measure**（cube 拒收"没有 measure"的查询）。
		Measures:   []string{"stock.total_quantity", "stock.avg_cost"},
		Dimensions: []string{"stock.product_id"},
		Filters:    filters,
		Limit:      q.PageSize,
		Offset:     (q.Page - 1) * q.PageSize,
		// ⚠️ order 的 id 必须在 SELECT 的 dimensions 里（硬约束 2）；
		//   且 offset>0 时必须给 order（硬约束 3）。
		//   唯一的维度就是排序键 ⇒ 天然全序（cube 按 dimension 做 GROUP BY）。
		Order: []cubeclient.CubeOrder{{ID: "stock.product_id", Order: "asc"}},
		// ⚠️ total:true 才有"不受 limit/offset 影响的总条数"（硬约束 4）。
		Total: true,
	})
	if err != nil {
		return nil, 0, err
	}

	rows := make([]model.StockLevel, 0, len(data))
	for _, raw := range data {
		rows = append(rows, s.stockLevelFromCubeRow(q.BranchID, raw))
	}
	return rows, int64(total), nil
}

// getLevelFromCube 走 cube 查单个商品的库存快照（门店配了数据源时）。
func (s *StockService) getLevelFromCube(ctx context.Context, branchID, productID string) (*model.StockLevel, error) {
	dto, err := s.cube.GetStock(ctx, branchID, productID)
	if err != nil {
		// ⚠️ "确实没有这条库存"⇒ 404（与本域路径同一个错误）；
		//   链路故障（超时 / 5xx）原样上抛 ⇒ 503，不许折成"没有库存"。
		if errors.Is(err, cubeclient.ErrStockNotFound) || errors.Is(err, cubeclient.ErrCubeNotFound) {
			return nil, model.ErrStockLevelNotFound
		}
		return nil, err
	}
	now := s.now()
	return &model.StockLevel{
		BranchID:  branchID,
		ProductID: dto.ProductID,
		Qty:       dto.Quantity,
		// ⚠️ cube 侧的平均成本缺失时是 0（不是"没维护"）——
		//   本域的 AvgCostYuan 同样是"预留位"(当前恒为 0)，口径一致。
		AvgCostYuan: dto.AvgCostYuan,
		// ⚠️ server_version 是**本域 PG 的水位**，cube 侧没有 ⇒ 0。
		ServerVersion: 0,
		CreatedBy:     "cube",
		UpdatedBy:     "cube",
		CreatedAt:     now,
		UpdatedAt:     now,
	}, nil
}

// listMovesFromCube 走 cube 查库存流水（门店配了数据源时）。
//
// ⚠️ **cube 的 stock model 是 live 快照，没有流水** —— 思迅源库的库存历史
// 不在这个 model 里（cube 也没有对应的 model）。
//
// ⇒ 有源门店的流水**不在本域**，本域 PG 里那个门店的 stock_moves 从来没被写过
// （导入闸门拒了有源门店 ⇒ 见 EnsureLocalSource）。
//
// ⚠️ 因此这里返回**空页**，而**不是**回落读本域：
// 回落 = 把"本域没有这个门店的流水"说成"这个门店没有库存变动" ——
// 而真相是"这个门店的流水在外部系统里"。
func (s *StockService) listMovesFromCube(_ context.Context, _ StockMoveQuery) ([]model.StockMove, int64, error) {
	return []model.StockMove{}, 0, nil
}

// stockLevelFromCubeRow 把 cube 的一行转成本域的 model.StockLevel。
//
// ⚠️ **响应形状不变**：字段名 / 类型与本域 PG 路径完全一致
// （qty / avg_cost_yuan 都是 decimal ⇒ JSON 里是**字符串**，Dart 侧不会变成 double）。
//
// ⚠️ 数值用 cubeclient.ParseDecimal：思迅 live 源的 decimal 列经 go-mssqldb
// 以 []byte 返回 ⇒ JSON 序列化成 **base64**（如 "MC4wMDAw" = "0.0000"），
// 解析不了就当 0 ⇒ 库存恒 0（一个看起来正常的错数）。
func (s *StockService) stockLevelFromCubeRow(branchID string, raw map[string]any) model.StockLevel {
	now := s.now()
	return model.StockLevel{
		BranchID:      branchID,
		ProductID:     cubeText(raw["stock.product_id"]),
		Qty:           cubeclient.ParseDecimal(raw["stock.total_quantity"]),
		AvgCostYuan:   cubeclient.ParseDecimal(raw["stock.avg_cost"]),
		ServerVersion: 0,
		CreatedBy:     "cube",
		UpdatedBy:     "cube",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

// cubeText 把 cube 的标量转成**去空格的**文本。
//
// ⚠️ 思迅的 char 列普遍补空格（cube 侧只 RTRIM，这里再兜一层），
// 而 product_id 是**维度** ⇒ cube 回来的是字符串。
func cubeText(v any) string {
	if v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		s = fmt.Sprint(v)
	}
	return strings.TrimSpace(s)
}
