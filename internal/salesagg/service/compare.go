package service

import (
	"context"
	"fmt"
	"strings"
)

// compare.go —— 门店对比取数（Flutter「门店对比」页的数据端）。
//
// 与 Report 的根本差别在**鉴权模型**：
//
//   - Report：单店查询，门店来自 X-Branch-ID，门店隔离靠
//     branch → cube-router 路由链；
//   - Compare：一次请求涉及 2~3 个门店，模板全局共享（任何人都能选任何
//     模板），所以**逐门店鉴权只认 token claims.AccessibleBranches**，
//     X-Branch-ID 不参与。无权限门店的整列在服务端就地拦掉 ——
//     summary / cells 不出现该店任何 key，数值根本不离开后端。
//     这是「无权限列」语义的安全根基：模板人人可读，数据必须各自校验。

// CompareRow 是对比里一行的取数入参。
type CompareRow struct {
	Key   string
	Label string
	// Items 是门店 ID → 该店维度下的分组编码。
	// 缺 key / 空串 = 该店未设置编码，对应单元格为 null。
	Items map[string]string
}

// CompareInput 是一次对比取数的入参。
type CompareInput struct {
	Dimension string
	// Stores 是对比列，**顺序即响应 columns 的顺序**（前端按位置渲染表头）。
	Stores []string
	Rows   []CompareRow
	// Allowed 是调用方有权访问的门店集合（handler 从 token claims 算）。
	// 不在集合内的门店整列 forbidden，且**不发起任何 cube 调用** ——
	// 发了也是白发（数据不能给），却会在 cube 侧留下一条真实查询记录。
	Allowed map[string]bool
	From    string
	To      string
}

// 列状态。与 Flutter SalesCompareColumnStatus 的 wire 值一一对应，
// 未知值前端兜底为 unavailable —— 这里新增状态时前端必须同步 tryParse。
const (
	ColOK          = "ok"
	ColForbidden   = "forbidden"
	ColUnavailable = "unavailable"
)

// ErrSalesSourceUnavailable 是列状态 unavailable 的机器码。
// 语义：门店有权限，但它的数据源（cube / 源库）当前不可用。
const ErrSalesSourceUnavailable = "sales_source_unavailable"

// CompareColumn 是一个门店列的状态描述。
//
// Name 回显门店 ID：前端用本地门店表补显示名（后端没有门店名称表，
// 也不该为此加一个 —— 那是 userd/branch 的数据）。
type CompareColumn struct {
	Store  string `json:"store"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// ErrorCode 仅在非 ok 时可选给出；forbidden 没有（不是故障，是权限）。
	ErrorCode string `json:"error_code,omitempty"`
}

// CompareCell 是一行里某门店的单元格。字段口径与 ReportRow 一致：
// 数字全部是服务端格式化字符串；RefMarginPct 空串 = 成本覆盖率不足，不可算。
type CompareCell struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	NetAmount    string `json:"net_amount"`
	NetQty       string `json:"net_qty"`
	RefMargin    string `json:"ref_margin"`
	RefMarginPct string `json:"ref_margin_pct"`
}

// CompareRowOut 是对比结果的一行。Cells 按门店 ID 稀疏存放：
// 缺 key / nil = 该店没有匹配编码的销售数据（前端显示 --）。
type CompareRowOut struct {
	Key   string                  `json:"key"`
	Label string                  `json:"label"`
	Cells map[string]*CompareCell `json:"cells"`
}

// CompareDataOut 是一次对比取数的完整结果。
//
// 不变量（handler 的 HTTP 层与前端 UI 都依赖）：
//   - Columns 与请求门店一一对应（含非 ok 列），顺序即请求顺序；
//   - Summary 只含 ok 列的 key；Rows.Cells 同理 ——
//     无权限门店不出现任何数值，不可用门店同样没有。
type CompareDataOut struct {
	Dimension string             `json:"dimension"`
	From      string             `json:"from"`
	To        string             `json:"to"`
	Columns   []CompareColumn    `json:"columns"`
	Summary   map[string]Summary `json:"summary"`
	Rows      []CompareRowOut    `json:"rows"`
}

// CompareData 按门店逐个取数并拼装对比结果。
//
// 逐店**顺序**查询而不是并行：门店最多 3 个、单店实测 ~200ms，
// 并行省不到一秒却要引入 goroutine 错误聚合 —— 不值。真正的失败
// 也不上抛：单店 cube 故障只把**那一列**标记为 unavailable，
// 其余门店照常返回（对比的意义就是"有多少看多少"）。
func (s *Service) CompareData(ctx context.Context, in CompareInput) (*CompareDataOut, error) {
	if s.cube == nil {
		return nil, fmt.Errorf("salesagg: cube 未配置")
	}
	if !ValidDimension(in.Dimension) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidDimension, in.Dimension)
	}

	out := &CompareDataOut{
		Dimension: in.Dimension,
		From:      in.From,
		To:        in.To,
		Columns:   make([]CompareColumn, 0, len(in.Stores)),
		Summary:   map[string]Summary{},
		Rows:      make([]CompareRowOut, 0, len(in.Rows)),
	}
	for _, r := range in.Rows {
		out.Rows = append(out.Rows, CompareRowOut{
			Key:   r.Key,
			Label: r.Label,
			Cells: map[string]*CompareCell{},
		})
	}

	for _, store := range in.Stores {
		col := CompareColumn{Store: store, Name: store, Status: ColOK}
		if !in.Allowed[store] {
			col.Status = ColForbidden
			out.Columns = append(out.Columns, col)
			continue // 不发起任何 cube 调用
		}

		raw, err := s.cube.LoadSalesReport(ctx, store, in.Dimension, in.From, in.To, 0)
		if err != nil {
			col.Status = ColUnavailable
			col.ErrorCode = ErrSalesSourceUnavailable
			out.Columns = append(out.Columns, col)
			continue
		}
		rows := []RawReport{}
		if raw != nil {
			rows = raw.Rows
		}
		out.Summary[store] = buildSummary(rows)

		// 按编码建索引，逐行把该店的原始行映射成单元格。
		idx := make(map[string]RawReport, len(rows))
		for _, r := range rows {
			idx[r.Code] = r
		}
		for i, cr := range in.Rows {
			code := strings.TrimSpace(cr.Items[store])
			if code == "" {
				continue // 未设置编码 → cells 该 key 保持 nil
			}
			rawRow, ok := idx[code]
			if !ok {
				continue // 该店没有这个编码的销售数据 → 同样 nil
			}
			out.Rows[i].Cells[store] = compareCell(rawRow)
		}
		out.Columns = append(out.Columns, col)
	}
	return out, nil
}

// compareCell 把一个原始行转成对比单元格。
//
// 毛利口径与 Report 行级规则一致：coverage ≥ MinCostCoverage 才填
// margin / pct，否则留空串（前端把空串当"不可算"渲染占位符）。
func compareCell(r RawReport) *CompareCell {
	cell := &CompareCell{
		Code:      r.Code,
		Name:      r.Name,
		NetAmount: fmtAmount(r.NetAmount),
		NetQty:    fmtAmount(r.NetQty),
	}
	if cov := coverage(r.CostedRows, r.TotalRows); cov >= MinCostCoverage {
		m := r.NetAmount - r.RefCost
		cell.RefMargin = fmtAmount(m)
		if r.NetAmount != 0 {
			cell.RefMarginPct = fmtPct(m / r.NetAmount)
		}
	}
	return cell
}
