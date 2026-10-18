package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/salesagg/cubeq"
)

// CubeQuerier 的生产实现：经 dapr service invocation 调 cube。
//
// 门店隔离完全靠 X-Branch-ID 路由链（branch → cube-router →
// branch_cube_sources → 该门店专属 cube 实例），**不按 cube 的
// branch_no 字段过滤** —— 思迅 branch_no 与本系统 branch_id 是两套
// 无关编码（cubeclient.GetStock 注释里有完整实测记录），按它过滤会永远匹配不上。
type DaprCubeQuerier struct {
	c *cubeclient.DaprCubeClient
}

// NewDaprCubeQuerier 构造生产实现。
func NewDaprCubeQuerier(c *cubeclient.DaprCubeClient) *DaprCubeQuerier {
	return &DaprCubeQuerier{c: c}
}

// LoadSalesReport 实现 CubeQuerier。
func (q *DaprCubeQuerier) LoadSalesReport(
	ctx context.Context, storeBranchID, groupBy, from, to string, limit int,
) (*RawSalesReport, error) {
	cq, err := BuildSalesReportQuery(groupBy, from, to, limit)
	if err != nil {
		return nil, err
	}
	if storeBranchID != "" {
		ctx = cubeclient.WithBranchID(ctx, storeBranchID)
	}
	data, err := q.c.LoadCubeQuery(ctx, "sales_report", cq)
	if err != nil {
		return nil, fmt.Errorf("salesagg: cube sales_report query: %w", err)
	}
	return &RawSalesReport{Rows: ParseSalesReportRows(groupBy, data)}, nil
}

var _ CubeQuerier = (*DaprCubeQuerier)(nil)

// LoadSalesTotals 实现 CubeQuerier：大盘总额，**无分组、不限量**。
//
// ⚠️ 这里刻意不带任何 Dimensions：
//
//	带了就等于"某一维度的子集"，于是未归类的商品整行消失，
//	总额会低于真实值 —— 而症状只是数字偏小，没有任何报错。
//
//	判据见 service_test.go 的 TestTotalsQueryHasNoDimension。
func (q *DaprCubeQuerier) LoadSalesTotals(
	ctx context.Context, storeBranchID, from, to string,
) (RawReport, error) {
	data, err := q.c.LoadCubeQuery(withBranch(ctx, storeBranchID), "sales_report",
		cubeq.Query(nil, from, to, 0))
	if err != nil {
		return RawReport{}, fmt.Errorf("salesagg: cube totals query: %w", err)
	}
	if len(data) == 0 {
		// 空结果与"有数据但全为 0"是两回事：前者不该报成功，
		// 否则顶部会显示 0 元，而真相是这一段压根没查到。
		return RawReport{}, fmt.Errorf("salesagg: cube totals query 返回空结果（%s~%s）", from, to)
	}
	row := data[0]
	return RawReport{
		NetAmount:  cubeq.Num(row, "sales_report.total_net_amount"),
		NetQty:     cubeq.Num(row, "sales_report.total_net_qty"),
		RefCost:    cubeq.Num(row, "sales_report.total_ref_cost"),
		CostedRows: int64(cubeq.Num(row, "sales_report.costed_rows")),
		TotalRows:  int64(cubeq.Num(row, "sales_report.count")),
	}, nil
}

// withBranch 注入 X-Branch-ID 路由头。
func withBranch(ctx context.Context, storeBranchID string) context.Context {
	if strings.TrimSpace(storeBranchID) == "" {
		return ctx
	}
	return cubeclient.WithBranchID(ctx, storeBranchID)
}
