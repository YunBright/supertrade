// Package service / source.go —— 「门店配了数据源 ⇒ 读路径走 cube」的判据。
//
// # 为什么要有这一层
//
// supertrade 是 ERP，也是**别的 ERP 的代理**：某个门店一旦在 cube-router 里
// 配了数据源（富百家 00 → cube-sixun-ysx-fb、佳乐 01 → cube-sixun-hbposv7-jiale），
// **该门店的库存就在外部系统里**，本域 PG 没有它的行。
//
// ⇒ 读端点必须先问一句"我这个门店配了数据源吗"：
//   - 配了 ⇒ 走 cube（cube-router → 该门店的 cube 实例）
//   - 没配 ⇒ 走本域 PG（自营店 10，本地有 23833 行导入的快照）
//
// ⚠️ 不做这一步的症状（2026-10-21 生产实测）：
// `GET /stock-levels` / `GET /stock-levels/:product_id` / `GET /stock-moves`
// **只查本地 PG** ⇒ 门店 00 / 01 的库存**查出来是空的**，
// 而它们的库存在 cube 里（cube 的 stock 是 live model，透传思迅源库的当前值）。
//
// # 判据的三个状态（必须严格区分）
//
//	Configured=false            → 没配数据源（**正常状态**，自营店就是这样）⇒ 读本地。
//	Configured=true, Enabled=false → 配了但被管理员关停 ⇒ **503**，不是"没配"
//	  （回落成"没配"会让用户看到一份空列表，而真相是"这个门店现在查不了"）。
//	解析失败（问不到路由）→ **503**；读端点**不能**回落成本地
//	  （回落 = 把一次链路故障显示成「该门店还没有库存」）。
//
// ⚠️ **判据只有一份**：member / catalog / pricing / inventory 共用
// internal/cubesource 的实现（见该包注释里 2026-10-21 的审计结论）——
// 三个服务给三个答案时，每个服务看起来都"有道理"，而用户看到的现象是错的。
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/YunBright/supertrade/internal/archiveroute"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/inventory/model"
)

// SourceResolver 解析"这个门店配了哪个数据源"。
//
// 实现：internal/cubesource.CachedResolver（问 cube-router 的 /v1/branch-source，
// 60s 缓存 + stale-on-error）。
type SourceResolver = cubesource.Resolver

// CubeStockReader 读 cube 里的库存快照。
//
// ⚠️ 实现：internal/cubeclient.DaprCubeClient。
//
//	LoadCubeQueryPage —— 列表形态（分页 / 过滤都在 cube 那边做）。
//	GetStock          —— 单个商品的快照。
type CubeStockReader interface {
	LoadCubeQueryPage(ctx context.Context, modelName string, q cubeclient.CubeQuery) ([]map[string]any, int, error)
	GetStock(ctx context.Context, branchID, productID string) (*cubeclient.StockSnapshotDTO, error)
}

// SetCubeSource 注入数据源解析器与 cube 读端。
//
// ⚠️ **两个都注入**才走 cube 路径；只注入一个 ⇒ 仍走本地
// （装配漏了时的症状是"有数据源的门店又变成空列表"
//
//	⇒ 装配侧必须两个一起注入）。
//
// ⚠️ 未注入（测试 / 老装配）⇒ 一律走本地 PG ⇒ 现有单测不受影响。
func (s *StockService) SetCubeSource(resolver SourceResolver, reader CubeStockReader) {
	s.sources = resolver
	s.cube = reader
}

// resolveCubeSource 判定该门店的库存该走哪条路。
//
// 返回 (source, useCube, err)：
//   - useCube=true  ⇒ 走 cube
//   - useCube=false ⇒ 走本域 PG
//   - err != nil    ⇒ 问不到 / 数据源被关停 ⇒ 上抛（503）
func (s *StockService) resolveCubeSource(ctx context.Context, branchID string) (cubesource.Source, bool, error) {
	if s.sources == nil || s.cube == nil {
		// 未装配 ⇒ 本地（自营店场景）。
		return cubesource.Source{}, false, nil
	}
	src, err := s.sources.Resolve(ctx, branchID)
	if err != nil {
		// ⚠️ **权限类错误要原样上抛**：调用者没有该门店的 cube:read ⇒ 403，
		//   而"问不到路由"⇒ 503。两者混在一起会让"没权限"显示成"暂不可用"，
		//   用户会去查数据源健康度（而真正的原因是权限）。
		if errors.Is(err, cubeclient.ErrCubeForbidden) || errors.Is(err, cubeclient.ErrCubeAuthRejected) {
			return cubesource.Source{}, false, err
		}
		return cubesource.Source{}, false, fmt.Errorf("%w: %v", model.ErrCubeSourceUnavailable, err)
	}
	if !src.Configured {
		// 自营店（没配数据源）⇒ 本域 PG。
		return src, false, nil
	}
	if !src.Enabled {
		// ⚠️ 配了但被管理员关停 ⇒ "这个门店现在查不了"，不是"没有数据源"。
		return src, false, fmt.Errorf("%w: 门店 %s 的数据源已停用",
			model.ErrCubeSourceDisabled, branchID)
	}
	return src, true, nil
}

// EnsureLocalSource 在"该门店的库存来自外部系统"时返错（写端点用）。
//
// ⚠️ **该门店的库存来自外部系统**（富百家 / 佳乐）⇒ 本域只读，导入一律拒（409）。
// 不拒的话：往本域 PG 写一份"没人看的库存"，而前台还显示"写成功了"。
//
// # 判据用 archiveroute（与其它服务的归档导入**同一份**代码）
//
// ⚠️ inventory 的**读路径**不能直接用 archiveroute：
//
//	archiveroute 的 DecideExport 只有「本地 / 外部(= 这里没有这个域的数据)」
//	两档，而 inventory 的库存**就在 cube 里** ⇒ "外部"对读路径的含义是
//	「**去 cube 查**」，不是「导出空」。
//	⇒ 读路径用本文件的三态判据（与 member 域同款），
//	  导入闸门用 archiveroute.DecideImport（与 master-data / settlement-auditor 同款）。
//	两者的**解析器**是同一个（internal/cubesource）⇒ 状态永远一致。
//
// ⚠️ **解析失败（问不到路由）时放行**，不是拒：
//   - 读端点不能这么干（问不到就回落本地 = 显示一份空列表
//     ⇒ 把故障说成业务结论），所以读路径把解析错误当 503；
//   - 写端点可以放行：导入路径自己会再查一次（本域 PG 里没有这个商品 ⇒ 跳过），
//     那是**诚实**的失败（不是"写成功了却写在别处"）。
//     ⇒ 顺带：cube-router 挂掉时不会挡住自营店（门店 10）的正常导入。
//
// ⚠️ 也覆盖"数据源被停用"的情况：源**已经配了** ⇒ 这家店的数据在外部系统里
// ⇒ 往本域 PG 写一份"没人看的库存"就是制造假数据 ⇒ 仍然 409。
func (s *StockService) EnsureLocalSource(ctx context.Context, branchID string) error {
	mode, err := archiveroute.DecideImport(ctx, s.sources, branchID)
	if err != nil {
		// 问不到路由 ⇒ 放行（见上面的说明）；只记一行日志，便于事后查。
		slog.Warn("inventory: 问不到门店数据源,写操作按本域处理", "branch_id", branchID, "err", err)
		return nil
	}
	if mode == archiveroute.ModeExternal {
		// ⚠️ message **不带**数据源名（那是内部 app-id，不该出现在对外文案里）。
		return fmt.Errorf("%w: 门店 %s 的库存来自外部系统,库存维护请在原系统进行",
			model.ErrExternalSourceReadOnly, branchID)
	}
	return nil
}
