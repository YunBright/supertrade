package service

import (
	"context"
	"fmt"

	"github.com/YunBright/supertrade/internal/cubeclient"
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
