// Package cubeq 是 sales-agg 访问 cube `sales_report` 时**共用**的查询原语。
//
// # 为什么要抽出来，而不是各写各的
//
// 2026-10-17 之前，本仓有两处各自定义"销售聚合查哪些 measure、怎么过滤日期"：
//
//   - internal/salesagg/service/query.go —— 每日销售统计卡片与门店对比报表
//   - internal/salesinsight/build/datasource.go —— AI 销售日报
//
// 后果不是"代码重复不好看"，而是**同一个营业日能查出两个数**：
// 线上实测门店 00 / 2026-10-16，品类口径 2024.6 元 vs 供应商口径
// 13003.8 元 —— 差 6 倍，**零报错**。两处一旦漂移（多一个 measure、
// 少一个成本口径、日期过滤写法不同），没有任何机制会发现。
//
// → 落 PG 改造把这个坑的最后一层也填掉：采集只由 sales-agg 做一次，
//
//	两个消费方读同一行。为此先把**它们共同依赖的那一层**合并：
//	measure 集合、日期过滤、查询骨架只有一份定义。
//
// # 这里只放"怎么查"，不放"查什么维度"
//
// 维度成员（category / product / supplier 的字段地址）刻意**不**在这里统一，
// 而是留在各自的调用方：报表要的是"父类别"，采集要的还包括
// "商品所属父类别"与"供应商 × 商品"这种报表不会用的组合。
// 强行合并维度会让两个调用方都开始依赖对方的字段需求。
package cubeq

import (
	"fmt"
	"strings"

	"github.com/YunBright/supertrade/internal/cubeclient"
)

// SalesMeasures 是销售聚合共用的度量集合。
//
// ⚠️ costed_rows / count 一起取不是为了好看：没有它们，
//
//	调用方无法区分「毛利率 100%」是真实的还是「成本全为 0」的假象
//	（hbposv10 实测当日 64 个类别组里多组覆盖率 0%）。
//	详见 internal/salesagg/service 的包注释。
var SalesMeasures = []string{
	"sales_report.total_net_amount",
	"sales_report.total_net_qty",
	"sales_report.total_ref_cost",
	"sales_report.costed_rows",
	"sales_report.count",
}

// DateFilters 构造日期闭区间过滤。
//
// ⚠️ 走 gte/lte 的字面值比较而不 CAST：oper_date 在源库是 char(10)
//
//	字符串，CAST 会让百万行全表扫描（实测 7 天区间 225ms）。
//
// ⚠️ from/to 为空时不加对应条件 —— 但**调用方不要**依赖这一点去"查全量"：
//
//	源库最早的数据是门店上线那天，全表扫描没有业务意义，
//	而漏加过滤的后果是几百万行全量返回。
func DateFilters(from, to string) []cubeclient.CubeFilter {
	var f []cubeclient.CubeFilter
	if from != "" {
		f = append(f, cubeclient.CubeFilter{
			Member: "sales_report.oper_date", Operator: "gte", Values: []any{from},
		})
	}
	if to != "" {
		f = append(f, cubeclient.CubeFilter{
			Member: "sales_report.oper_date", Operator: "lte", Values: []any{to},
		})
	}
	return f
}

// Query 构造一个 sales_report 聚合查询。
//
// # limit <= 0 表示**不限量**
//
// cubeclient 只在 `Limit > 0` 时才把 limit 放进 body，
// 所以 0 与不传等价。落 PG 采集必须走这条（0）——
// 带上限的快照会让"求和 = 大盘"这个前提不成立，
// 而月报正是靠它做跨日聚合（见 salesdaily 包的注释）。
func Query(dims []string, from, to string, limit int) cubeclient.CubeQuery {
	return cubeclient.CubeQuery{
		Measures:   SalesMeasures,
		Dimensions: dims,
		Filters:    DateFilters(from, to),
		Limit:      limit,
	}
}

// Num 从 cube 返回的一行里取 float64。
//
// ⚠️ MSSQL live 源的 decimal 经 go-mssqldb 以 []byte 返回，
//
//	encoding/json 会把它序列化成 base64。必须走 ParseDecimal，
//	不要自己 parse 字符串 —— 那会在某些驱动版本上静默得到 0。
func Num(row map[string]any, key string) float64 {
	return cubeclient.ParseDecimal(row[key]).InexactFloat64()
}

// Str 从 cube 返回的一行里取字符串（trim 过），缺字段返空串。
func Str(row map[string]any, key string) string {
	v, ok := row[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}
