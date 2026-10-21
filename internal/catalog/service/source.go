// Package service / source.go —— 「门店 → 数据源」的判据(catalog 侧落地)。
//
// # 为什么 catalog 也要问"这个门店配了数据源吗"
//
// supertrade 是 ERP,也是**别的 ERP 的代理**:门店在 cube-router 的
// `branch_cube_sources` 里配了数据源(富百家 00 → cube-sixun-ysx-fb、
// 佳乐 01 → cube-sixun-hbposv7-jiale)时,**该门店的业务数据在外部系统里**,
// 本域 PG 里没有它的行 ⇒ 读端点必须走那个 cube 实例;
// 没配数据源的门店(自营店 10)⇒ 走本域 PG。
//
// ⚠️ 不做这一步的症状(2026-10-21 生产实测):商品选择控件的 4 条读端点
// **只走 cube** ⇒ 自营店 10 调它直接返
// `{"code":"cube_error","message":"cubeclient: cube 404 not found: Not Found"}`,
// 而门店 10 的本地表里明明有 27565 个商品(import 灌的)。
//
// # 判据必须全服务一致
//
// 同一套判据在 internal/member/service/source.go(会员域)已经落地。
// ⇒ 这里**复用** internal/cubesource 的解析器,不重新实现:
// 同一个门店在不同服务里给出不同的解释,排查时每个服务看起来都"有道理"。
//
// # 三个状态(必须严格区分)
//
//	Configured=false            → 没配数据源(**正常状态**)⇒ 读本域 PG。
//	Configured=true, Enabled=false → 配了但被关停 ⇒ 503,不是"没配"。
//	解析失败(问不到)             → 503;读端点**不**回落成本地
//	  (回落 = 把一次链路故障显示成"该门店没有商品")。
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubesource"
)

// SourceResolver 解析"这个门店配了哪个数据源"。
//
// 实现:internal/cubesource.CachedResolver(问 cube-router 的 /v1/branch-source,
// 60s TTL + stale-on-error)。
//
// ⚠️ 各服务**共用同一个实现** —— 判据必须只有一个。
type SourceResolver = cubesource.Resolver

// resolveSource 判定该门店的业务数据该走哪条路。
//
// 返回 (src, useCube, err):
//
//   - useCube=true  ⇒ 走 cube(该门店的数据在外部系统里)
//   - useCube=false ⇒ 走本域 PG(自营店)
//   - err != nil    ⇒ 问不到 / 数据源被关停 ⇒ 上抛(503 或权限错)
//
// ⚠️ **权限类错误原样上抛**:调用者缺 cube:read ⇒ 403,
// 而"问不到路由"⇒ 503。两者混在一起会让"没权限"显示成"暂不可用",
// 用户会去查数据源健康度(而真正的原因是权限)。
func resolveSource(ctx context.Context, resolver SourceResolver, branchID string) (cubesource.Source, bool, error) {
	if resolver == nil {
		// 未装配 ⇒ 由调用方决定默认走哪边(见各自的 useCube* 方法)。
		return cubesource.Source{}, false, nil
	}
	src, err := resolver.Resolve(ctx, branchID)
	if err != nil {
		if errors.Is(err, cubeclient.ErrCubeForbidden) || errors.Is(err, cubeclient.ErrCubeAuthRejected) {
			return cubesource.Source{}, false, err
		}
		return cubesource.Source{}, false, fmt.Errorf("%w: %v", model.ErrCubeSourceUnavailable, err)
	}
	if !src.Configured {
		// 自营店(没配数据源)⇒ 本域 PG。
		return src, false, nil
	}
	if !src.Enabled {
		// ⚠️ 配了但被管理员关停 ⇒ "这个门店现在查不了",不是"没有数据源"。
		return src, false, fmt.Errorf("%w: 门店 %s 的数据源已停用",
			model.ErrCubeSourceDisabled, branchID)
	}
	return src, true, nil
}

// SetSourceResolver 装配数据源解析器(catalog 读路径分流用)。
//
// ⚠️ 生产**必须**装配(cmd/catalog/main.go 里装 cubesource.CachedResolver):
// 没装 ⇒ 所有门店都走 cube ⇒ 自营店 10 又变回 2026-10-21 那个症状。
func (s *CatalogService) SetSourceResolver(r SourceResolver) {
	s.sources = r
}

// SetLocalDB 装配本域 PG(没配数据源的门店走本地时用)。
func (s *CatalogService) SetLocalDB(db *gorm.DB) {
	s.db = db
}

// useCubeForRead 判定 picker 的读端点该走哪条路。
//
// ⚠️ **解析器未装配 ⇒ 走 cube**(保持这个端点既有的行为)。
// 单测里只装 fake cube、不装解析器 ⇒ 既有断言全部照旧;
// 生产一定装解析器 ⇒ 按门店分流。
func (s *CatalogService) useCubeForRead(ctx context.Context, branchID string) (bool, error) {
	if s.sources == nil {
		return true, nil
	}
	_, useCube, err := resolveSource(ctx, s.sources, branchID)
	return useCube, err
}

// SetSourceResolver 装配数据源解析器(导入闸门 + 导出分流用)。
func (s *ArchiveService) SetSourceResolver(r SourceResolver) {
	s.sources = r
}

// SetExporter 装配上游导出端点客户端(cube-router 的 GET /v1/export)。
//
// ⚠️ 没装 ⇒ 有源门店的导出返 503(而不是回落本地):
// 回落 = 把"上游导不出来"显示成"这个门店没有商品"。
func (s *ArchiveService) SetExporter(e ArchiveExporter) {
	s.exporter = e
}

// useCubeForRead 判定档案的读/导该走哪条路。
//
// ⚠️ **解析器未装配 ⇒ 走本地**(保持 /products/export 既有的行为:
// 它本来就是读本地表的)。生产一定装解析器 ⇒ 按门店分流。
func (s *ArchiveService) useCubeForRead(ctx context.Context, branchID string) (bool, error) {
	if s.sources == nil {
		return false, nil
	}
	_, useCube, err := resolveSource(ctx, s.sources, branchID)
	return useCube, err
}

// UseCubeForRead 判定该门店的档案该走哪条路(handler 层用)。
//
// ⚠️ 与 CatalogService.useCubeForRead 的**默认值不同**:
// 这里未装配解析器 ⇒ 本地(离线商品库本来就是读本地表的),
// picker 那边未装配 ⇒ cube(它本来就是转发 cube 的)。
// 两边都只影响"装配漏了 / 单测"的情形;生产一定装解析器 ⇒ 同一个判据。
func (s *ArchiveService) UseCubeForRead(ctx context.Context, branchID string) (bool, error) {
	return s.useCubeForRead(ctx, branchID)
}

// EnsureLocalSource 在"该门店的商品档案来自外部系统"时返错(写端点用)。
//
// ⚠️ **该门店的商品档案来自外部系统**(富百家 / 佳乐)⇒ 本域只读,写操作一律拒(409)。
// 不拒的话:写进本域 PG 的一行"没人看的商品",而前台还显示"写成功了"。
//
// ⚠️ **解析失败(问不到路由)时放行**,不是拒:
//
//   - 读端点不能这么干(问不到就回落本地 = 显示一份空列表 ⇒ 把故障说成业务结论),
//     所以读路径把解析错误当 503;
//   - 写端点可以放行:导入路径自己会兜底(写入的行没人读,总比"正常导入被路由抖动挡住"好),
//     而且 cube-router 挂掉时不会挡住自营店(门店 10)的正常收银写入。
//
// ⚠️ 也覆盖"数据源被停用"的情况:那时**读**都读不了,更不该写。
//
// ⚠️ **message 里不出现内部 app-id**(如 cube-sixun-ysx-fb):
// 那是运维细节,不该出现在给终端用户的错误文案里。
func (s *ArchiveService) EnsureLocalSource(ctx context.Context, branchID string) error {
	if s.sources == nil {
		// 未装配 ⇒ 不拦(单测 / 装配漏了时不挡住正常导入)。
		return nil
	}
	_, useCube, err := resolveSource(ctx, s.sources, branchID)
	if err != nil {
		if errors.Is(err, model.ErrCubeSourceDisabled) {
			return err
		}
		// 问不到路由 ⇒ 放行(见上面的说明);只记一行日志,便于事后查。
		slog.Warn("catalog: 问不到门店数据源,写操作按本域处理", "branch_id", branchID, "err", err)
		return nil
	}
	if useCube {
		// ⚠️ **不把数据源名写进 message**(见上面的说明)。
		return fmt.Errorf("%w: 门店 %s 的商品档案来自外部数据源,请在原系统维护",
			model.ErrExternalSourceReadOnly, branchID)
	}
	return nil
}
