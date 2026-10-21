// Package service / routing_client.go —— 老路径 /stock/:product_id 的**统一判据**。
//
// # 为什么需要它
//
// `GET /stock/:product_id`（internal/cubehttp 转发的那条）此前**只走 cube**：
// 它不看"这个门店配了数据源吗"，直接问 cube-router 要库存。
//
// ⇒ 对**没配数据源**的门店（自营店 10）：cube-router 返 404
// `cube_source_not_configured` ⇒ 用户看到「库存服务暂不可用」，
// 而真相是"这个门店的库存在本域 PG 里"。
//
// ⇒ 同一个服务里出现"一个端点走 cube、另一个走本地"的分裂：
// 同一个门店、同一个商品，两个端点给两个答案。
//
// # 修法（不改 internal/cubehttp）
//
// 把 cubehttp 用的 cubeclient.Client **换成本域的路由版**：
// 它的 GetStock 走本域的 [StockService.GetLevel] —— 也就是**同一套判据**
// （有源 ⇒ cube；没配 ⇒ 本域 PG；问不到 ⇒ 503）。
//
// 其余方法（GetProduct / ListProducts …）原样转发给真 cube ——
// cubehttp 本期只用 GetStock，其余是为了满足接口。
package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/inventory/model"
)

// RoutingCubeClient 是"按数据源分流"的 cube 客户端。
//
// ⚠️ 嵌入 cubeclient.Client：未覆盖的方法**原样**转发给真 cube
// （少写一个方法就要手写一遍它的语义 ⇒ 迟早写错）。
type RoutingCubeClient struct {
	cubeclient.Client
	svc *StockService
}

// NewRoutingCubeClient 构造路由版客户端。
func NewRoutingCubeClient(svc *StockService, base cubeclient.Client) *RoutingCubeClient {
	return &RoutingCubeClient{Client: base, svc: svc}
}

// GetStock 取某门店某商品的库存快照（**按数据源分流**）。
//
// ⚠️ 响应形状与老路径**完全一致**（cubeclient.StockSnapshotDTO），
// 而**值**来自本域的路由判据（有源 ⇒ cube 的 live 快照；没配 ⇒ 本域 PG 的快照）。
func (c *RoutingCubeClient) GetStock(ctx context.Context, branchID, productID string) (*cubeclient.StockSnapshotDTO, error) {
	if c == nil || c.svc == nil {
		return nil, fmt.Errorf("inventory: 路由客户端未装配")
	}
	lvl, err := c.svc.GetLevel(ctx, branchID, productID)
	if err != nil {
		// ⚠️ "没有这条库存"⇒ 翻成 cube 的语义 ⇒ cubehttp 映成 404 stock_not_found
		//   （与老路径的对外行为一致）。
		if errors.Is(err, model.ErrStockLevelNotFound) {
			return nil, fmt.Errorf("%w: branch_id=%s product_id=%s",
				cubeclient.ErrStockNotFound, branchID, productID)
		}
		return nil, err
	}
	return &cubeclient.StockSnapshotDTO{
		ProductID:   lvl.ProductID,
		BranchID:    lvl.BranchID,
		Quantity:    lvl.Qty,
		AvgCostYuan: lvl.AvgCostYuan,
		UpdatedAt:   lvl.UpdatedAt,
	}, nil
}
