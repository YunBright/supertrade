// Package service / source.go —— 「这个门店配了哪个数据源」的判定。
//
// # 判据只有一个:**配没配数据源**
//
// supertrade 是 ERP,也是**别的 ERP 的代理**:某个门店一旦在 cube-router 里
// 配了数据源(`branch_cube_sources`:富百家 00 → cube-sixun-ysx-fb、
// 佳乐 01 → cube-sixun-hbposv7-jiale),**该门店的价格/促销就在外部系统里**
// ⇒ 读端点必须走那个 cube 实例;没配 ⇒ 走本域 PG(自营店 10)。
//
// # ⚠️ 判据**不是**「本地有没有数据」(2026-10-21 生产实测)
//
// 旧判据是"该门店本地有没有价格行"(`HasAnyPrices`)。
// 而门店 00 的价格**已经被主同步实例(00→00)灌进了本地 PG**
// (27565 行,快照时间 2026-10-20 19:24)
// ⇒ 查价命中本地 ⇒ 返回**两天前的快照价**,而不是思迅的当前价。
//
// ⇒ 两个真相 = 两个错法:
//   - 判据 A「本地有没有数据」⇒ 有源门店命中本地快照(旧价)
//   - 判据 B「配没配数据源」⇒ 有源门店走 cube(实时价)
//
// ⇒ 全域统一用判据 B(member / catalog / inventory / pricing 同一套),
// 实现见 internal/cubesource。
//
// # 三个状态必须严格区分
//
//	Configured=false            → 没配数据源(正常状态,自营店)⇒ 读本域 PG。
//	Configured=true, Enabled=false → 配了但被关停 ⇒ **503**,不是"没配"
//	  (回落成"没配"会让用户看到一份空列表,而真相是"这个门店现在查不了")。
//	解析失败(问不到路由)        → **503**;读端点**不能**回落成本地
//	  (回落 = 把一次链路故障显示成「这个商品没维护售价」)。
//
// # 装配
//
// ⚠️ **解析器与 cube 读端必须一起注入**(与 member 同款):
//
// 只注入解析器 ⇒ 走 cube 但没有读端;只注入读端 ⇒ 有数据源的门店又变成本地。
// 生产装配见 cmd/pricing/main.go。
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/pricing/model"
)

// SourceResolver 解析"这个门店配了哪个数据源"。
//
// 实现:internal/cubesource.CachedResolver(问 cube-router 的 /v1/branch-source,
// 带 60s 缓存 + stale-on-error)。
//
// ⚠️ 各服务**共用同一个实现**——判据必须只有一个,
// 否则同一个门店在不同服务里表现不一致(2026-10-21 审计的真实教训)。
type SourceResolver = cubesource.Resolver

// CubeQueryReader 是 pricing 读 cube 的最小能力(分页查询 + 总数)。
//
// ⚠️ cubeclient.Client 接口**不含**它(那个接口是给"查一个商品"用的),
// 所以这里单独声明一个窄接口,由装配侧把同一个 *cubeclient.DaprCubeClient
// 传进来 —— **不要建第二个 client**(两个 client = 两套超时/鉴权/路由参数)。
type CubeQueryReader interface {
	LoadCubeQueryPage(ctx context.Context, modelName string, q cubeclient.CubeQuery) ([]map[string]any, int, error)
}

// SetCubeSource 注入数据源解析器与 cube 读端。
//
// ⚠️ **两个都注入**才走 cube 路径;只注入一个 ⇒ 有数据源的门店仍走本地
// ⇒ 症状与"没接数据源"完全一样(查价返回旧快照),且**没有任何报错**。
//
// ⚠️ 未注入(单测 / 纯本地模式)⇒ 按"没配数据源"处理。
func (s *PriceService) SetCubeSource(resolver SourceResolver, reader CubeQueryReader) {
	if s == nil {
		return
	}
	s.sources = resolver
	s.reader = reader
}

// hasLocalStore 报"本地表服务是否装配"。
//
// ⚠️ 本地表没装配 ⇒ 该门店**不可能**有本地数据 ⇒ 读路径只能走 cube。
// 这不是"回落",而是"本地根本不存在"——装配漏了时的症状要**大声**,
// 不能变成一份看起来正常的空结果。
func (s *PriceService) hasLocalStore() bool {
	return s != nil && s.prices != nil && s.promos != nil
}

// resolveSource 解析该门店的数据源(不决定读路径)。
//
// 返回 (source, err):
//   - err == nil ⇒ 解析成功(src.Configured / src.Enabled 说明三态)
//   - err 为 ErrCubeForbidden / ErrCubeAuthRejected ⇒ **原样上抛**(403 / 401)
//   - 其余错误 ⇒ 包装成 ErrCubeSourceUnavailable(503)
//
// ⚠️ 解析失败**不能**在这里回落成本地:回落 = 把一次链路故障显示成
// 「这个商品没维护售价」/「该门店还没有价格」——那正是本次要修的症状。
func (s *PriceService) resolveSource(ctx context.Context, branchID string) (cubesource.Source, error) {
	branchID = strings.TrimSpace(branchID)
	src := cubesource.Source{BranchID: branchID}
	if branchID == "" {
		return src, fmt.Errorf("pricing: branch_id 必填")
	}
	if s.sources == nil {
		// 解析器未装配(纯本地 / 纯 cube 模式 / 单测)⇒ 按"没配数据源"处理。
		return src, nil
	}
	resolved, err := s.sources.Resolve(ctx, branchID)
	if err != nil {
		// ⚠️ 权限类错误**原样上抛**:调用者没有该门店的 cube:read ⇒ 403,
		// 而"问不到路由"⇒ 503。两者混在一起会让"没权限"显示成"暂不可用"。
		if errors.Is(err, cubeclient.ErrCubeForbidden) || errors.Is(err, cubeclient.ErrCubeAuthRejected) {
			return src, err
		}
		return src, fmt.Errorf("%w: %v", model.ErrCubeSourceUnavailable, err)
	}
	if resolved.BranchID == "" {
		resolved.BranchID = branchID
	}
	return resolved, nil
}

// readPath 判定该门店的价格该走哪条路。
//
// 返回 (source, useCube, err):
//   - useCube=true  ⇒ 走 cube(该门店配了数据源)
//   - useCube=false ⇒ 走本域 PG(没配数据源)
//   - err != nil    ⇒ 问不到 / 数据源被关停 ⇒ 上抛(503)
//
// ⚠️ **判据只看"配没配数据源"**,不看"本地有没有数据"。
func (s *PriceService) readPath(ctx context.Context, branchID string) (cubesource.Source, bool, error) {
	if strings.TrimSpace(branchID) == "" {
		// 没有门店 ⇒ 本地表按门店分区,无从谈起 ⇒ 只能走 cube。
		return cubesource.Source{}, true, nil
	}
	src, err := s.resolveSource(ctx, branchID)
	if err != nil {
		return src, false, err
	}
	if src.Configured && !src.Enabled {
		// ⚠️ 配了但被管理员关停 ⇒ "这个门店现在查不了",不是"没有数据源"。
		return src, false, fmt.Errorf("%w: 门店 %s 的数据源已停用",
			model.ErrCubeSourceDisabled, strings.TrimSpace(branchID))
	}
	if src.Configured {
		return src, true, nil
	}
	// 没配数据源(自营店)⇒ 本域 PG。
	// ⚠️ 本地表没装配时只能走 cube(纯 cube 模式):本地**根本不存在**,
	// 回落成"本地查不到"会把装配错误伪装成一份空结果。
	if !s.hasLocalStore() {
		return src, true, nil
	}
	return src, false, nil
}

// EnsureLocalSource 在"该门店的价格来自外部系统"时返错(写端点用)。
//
// ⚠️ **该门店的价格来自外部系统**(富百家 / 佳乐)⇒ 本域只读,写操作一律拒(409)。
// 不拒的话:写进本域 PG 的一行"没人看的价",而前台还显示"写成功了"。
//
// ⚠️ **解析失败(问不到路由)时放行**,不是拒:
//   - 读端点不能这么干(问不到就回落本地 = 把故障说成业务结论)⇒ 读路径当 503;
//   - 写端点可以放行:导入路径自己会再查一次(本域 PG 里没有这个门店的行 ⇒
//     导入 0 条),那是**诚实**的失败(不是"写成功了却写在别处")。
//     ⇒ 顺带:cube-router 挂掉时不会挡住自营店(门店 10)的正常导入。
func (s *PriceService) EnsureLocalSource(ctx context.Context, branchID string) error {
	src, err := s.resolveSource(ctx, branchID)
	if err != nil {
		if errors.Is(err, model.ErrCubeSourceDisabled) {
			return err
		}
		// 问不到路由 ⇒ 放行(见上面的说明);只记一行日志,便于事后查。
		slog.Warn("pricing: 问不到门店数据源,写操作按本域处理", "branch_id", branchID, "err", err)
		return nil
	}
	if src.Configured {
		// ⚠️ message **不暴露**内部 app-id(cube-sixun-ysx-fb 之类):
		// 那是运维细节,泄漏它对终端用户没有价值,却把内部拓扑摊开了。
		return fmt.Errorf("%w: 门店 %s 的价格来自外部数据源,请在原系统维护",
			model.ErrExternalSourceReadOnly, strings.TrimSpace(branchID))
	}
	return nil
}
