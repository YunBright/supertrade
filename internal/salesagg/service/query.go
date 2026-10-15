package service

import (
	"fmt"
	"strings"

	"github.com/YunBright/supertrade/internal/cubeclient"
)

// RawRow 是从 cube 拿到的**未格式化**聚合行。
//
// 与 ReportRow 分开是因为两者的数字类型不同：这里用 float64（参与计算），
// ReportRow 用 string（只用于展示）。在服务层就把 float64 转成字符串会让
// 「排序要比较数值」变得需要重新 parse —— 那正是 format.go 里 numOf 存在的原因。
type RawReport struct {
	Code       string  // 维度编码
	Name       string  // 维度名称
	Extra      string  // 附加信息
	NetAmount  float64 // 净销售额
	NetQty     float64 // 净销量
	RefCost    float64 // 参考成本
	CostedRows int64   // 有成本支撑的行数
	TotalRows  int64   // 总行数
}

// RawSalesReport 是一次 cube 查询的原始返回。
type RawSalesReport struct {
	Rows []RawReport
}

// dimSpec 描述一个维度在 cube query 里要 select 的成员。
//
// 三个维度的成员名不同，但聚合 measure 完全相同 —— 所以只有维度成员需要分派。
type dimSpec struct {
	// codeMember / nameMember 是该维度的编码与名称成员。
	codeMember, nameMember string
	// extraMember 可选（商品维度有单位，类别维度有父类）。
	extraMember string
	// extraTransform 把 cube 返回的额外值整理成展示用字符串。
	extraTransform func(name, extra string) string
}

var dimSpecs = map[string]dimSpec{
	// ⭐ 类别维度按 **cls_parent(父类别)** 而不是末级类别。
	//
	// ✅ 实测 hbposepro 2026-10-14(与既有报表系统逐分对账):
	//   按父类 → 生鲜联营 4994.70/874.44、百货联营 2340.70/108.00,
	//              与对方报表分毫不差;
	//   按末级 → 「佳乐超市猪肉部」「谢志威床上用品」这类商品名,
	//              完全是另一套东西。
	//
	// ⚠️ 成员地址**必须带 model 前缀**:类别不是 sales_report 自己的维度,
	//   它在 item_cls_parent 这个 model 上(join 链:sales_report → item →
	//   item_cls → item_cls_parent)。写成 sales_report.xxx 会查得到、
	//   但那一列全空 —— 而全空的维度不会被"查不到"暴露,只会让金额凭空少一截。
	//   cube 返回的键本身就是 "<model>.<member>",见 cube 的 select 项构造。
	DimCategory: {
		codeMember: "item_cls_parent.id",
		nameMember: "item_cls_parent.name",
		// 刻意**不** select 末级类别名(item_cls.name)。
		//
		// cube 的 dimensions 全部进 GROUP BY —— 一旦把 item_clsname 也选上,
		// 同一个父类别(如「生鲜联营」)会按末级类别裂成十几行,汇总数直接失真。
		// 要看末级类别必须另走一次下钻查询,不能混在这个聚合里。
		//
		// extraMember 留空 → ReportRow.Extra 为空,前端在下钻入口处
		// 自行发起第二次查询。
	},
	DimProduct: {
		// 编码取 sales_report.item_no 而不是 item.id:
		// item 是 LEFT JOIN 过来的档案表,商品缺档案时 item.id 为 NULL,
		// 而 ParseSalesReportRows 会把「编码和名称都空」的行整行丢掉 ——
		// 那样卖出去的商品会凭空从报表里消失。item_no 在流水上实测非空,
		// 用它当 code 则缺档案的行仍保留(只是名字空),数不会少。
		codeMember:  "sales_report.item_no",
		nameMember:  "item.name",
		extraMember: "item.unit",
		extraTransform: func(name, extra string) string {
			if extra == "" {
				return ""
			}
			return extra // 单位直接展示,如 "kg" / "袋"
		},
	},
}

// salesReportMeasures 是所有维度共用的聚合 measure。
//
// ⚠️ costed_rows / count 一起返回不是为了好看:没有它们,
// 调用方无法区分「毛利率 100%」是真实的还是「成本全为 0」的假象。
// hbposv10 实测当日 64 个类别组里多组覆盖率 0%。
// 详见 service.go 包注释。
var salesReportMeasures = []string{
	"sales_report.total_net_amount",
	"sales_report.total_net_qty",
	"sales_report.total_ref_cost",
	"sales_report.costed_rows",
	"sales_report.count",
}

// BuildSalesReportQuery 构造 cube query。
//
// 抽成纯函数是为了能在不接 cube 的情况下断言"发出去的 query 长什么样"——
// 这是本仓对 cube 集成的既有纪律（见 cubeclient.query_shape_test.go）。
func BuildSalesReportQuery(groupBy, from, to string, limit int) (cubeclient.CubeQuery, error) {
	spec, ok := dimSpecs[groupBy]
	if !ok {
		return cubeclient.CubeQuery{}, fmt.Errorf("%w: %q", ErrInvalidDimension, groupBy)
	}
	dims := []string{spec.codeMember, spec.nameMember}
	if spec.extraMember != "" {
		dims = append(dims, spec.extraMember)
	}
	// 日期过滤：oper_date 在源库是 char(10) 字符串，且 cube 侧标了
	// time 类型。走 gte/lte 的字面值比较（不走 CAST），实测 7 天区间 225ms。
	filters := []cubeclient.CubeFilter{}
	if from != "" {
		filters = append(filters, cubeclient.CubeFilter{
			Member: "sales_report.oper_date", Operator: "gte", Values: []any{from},
		})
	}
	if to != "" {
		filters = append(filters, cubeclient.CubeFilter{
			Member: "sales_report.oper_date", Operator: "lte", Values: []any{to},
		})
	}
	return cubeclient.CubeQuery{
		Measures:   salesReportMeasures,
		Dimensions: dims,
		Filters:    filters,
		Limit:      limit,
	}, nil
}

// rowFloat 从 cube 返回的行里取 float64。
//
// ⚠️ MSSQL live 源的 decimal 经 go-mssqldb 以 []byte 返回，
// encoding/json 把它序列化成 base64（cubeclient.ParseDecimal 已有处理）。
// 这里统一走那个函数，不要自己写解析。
func rowFloat(row map[string]any, key string) float64 {
	return cubeclient.ParseDecimal(row[key]).InexactFloat64()
}

// rowStr 取字符串，nil / 缺字段返空串。
func rowStr(row map[string]any, key string) string {
	v, ok := row[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

// ParseSalesReportRows 把 cube 的返回行转成 RawReport。
func ParseSalesReportRows(groupBy string, data []map[string]any) []RawReport {
	spec := dimSpecs[groupBy]
	out := make([]RawReport, 0, len(data))
	for _, row := range data {
		code := rowStr(row, spec.codeMember)
		name := rowStr(row, spec.nameMember)
		extra := ""
		if spec.extraMember != "" {
			extra = rowStr(row, spec.extraMember)
		}
		if spec.extraTransform != nil {
			extra = spec.extraTransform(name, extra)
		}
		// 编码与名称都空的行是脏数据（类别字典缺项时可能发生），
		// 直接丢弃而不是留一行无名项 —— 报表里出现空白行比少一行更让人困惑。
		if code == "" && name == "" {
			continue
		}
		out = append(out, RawReport{
			Code:       code,
			Name:       name,
			Extra:      extra,
			NetAmount:  rowFloat(row, "sales_report.total_net_amount"),
			NetQty:     rowFloat(row, "sales_report.total_net_qty"),
			RefCost:    rowFloat(row, "sales_report.total_ref_cost"),
			CostedRows: int64(rowFloat(row, "sales_report.costed_rows")),
			TotalRows:  int64(rowFloat(row, "sales_report.count")),
		})
	}
	return out
}
