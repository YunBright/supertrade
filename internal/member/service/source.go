package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/member/model"
)

// # 门店 → 数据源 → cube 的读路径
//
// supertrade 是 ERP,也是**别的 ERP 的代理**:某门店在 cube-router 里配了数据源
// (富百家 00 → cube-sixun-ysx-fb、佳乐 01 → cube-sixun-hbposv7-jiale)时,
// **该门店的会员档案在外部系统里**,本域 PG 没有它的行。
//
// ⇒ 会员的读端点必须先问一句"我这个门店配了数据源吗":
//   - 配了 ⇒ 走 cube(cube-router → 该门店的 cube 实例)
//   - 没配 ⇒ 走本域 PG(自营店 10)
//
// ⚠️ 不做这一步的症状(2026-10-21 用户报):富百家 / 佳乐的会员档案列表
// 恒为空 ⇒ 前端显示「该门店还没有会员档案」—— 而真相是"会员在外部系统里"。
//
// # 为什么"问不到数据源"是 503 而不是回落本地
//
// 回落本地 ⇒ 有数据源的门店在链路故障时又变成"空列表"
// ⇒ 用户看到的还是那句"没有会员",而根因是路由挂了。
// ⇒ 宁可报 503(数据源暂不可用),也不要给一个**看起来正常**的空列表。

// SourceResolver 解析"这个门店配了哪个数据源"。
//
// 实现:internal/cubesource.CachedResolver(问 cube-router 的 /v1/branch-source)。
//
// ⚠️ 各服务**共用同一个实现**——判据必须只有一个,
// 否则同一个门店在不同服务里表现不一致(2026-10-21 审计的真实教训)。
type SourceResolver = cubesource.Resolver

// MemberReader 读 cube 里的会员档案。
//
// 实现:internal/cubeclient 的 DaprCubeClient。
type MemberReader interface {
	ListMembers(ctx context.Context, q cubeclient.MemberQuery) ([]cubeclient.MemberRow, int, error)
	GetMember(ctx context.Context, memberID string) (*cubeclient.MemberRow, error)
}

// SetCubeSource 注入数据源解析器与 cube 会员读端。
//
// ⚠️ **两个都注入**才走 cube 路径;只注入一个 ⇒ 仍走本地(装配漏了时的症状
// 是"有数据源的门店又变成空列表" ⇒ 装配侧必须两个一起注入)。
//
// ⚠️ 未注入(测试)⇒ 一律走本地 PG ⇒ 现有单测不受影响。
func (s *Service) SetCubeSource(resolver SourceResolver, reader MemberReader) {
	s.sources = resolver
	s.cube = reader
}

// EnsureLocalSource 在"该门店的会员档案来自外部系统"时返错(写端点用)。
//
// ⚠️ **该门店的会员档案来自外部系统**(富百家 / 佳乐)⇒ 本域只读,写操作一律拒(409)。
// 不拒的话:写进本域 PG 的一行"没人看的会员",而前台还显示"写成功了"。
//
// ⚠️ **解析失败(问不到路由)时放行**,不是拒:
//   - 读端点不能这么干(问不到就回落本地 = 显示一份空列表 ⇒ 把故障说成业务结论),
//     所以读路径把解析错误当 503;
//   - 写端点可以放行:写路径自己会再查一次会员(本域 PG 里没有这个会员 ⇒ 404 member_not_found),
//     那是**诚实**的失败(不是"写成功了却写在别处")。
//     ⇒ 顺带:cube-router 挂掉时不会挡住自营店(门店 10)的正常收银写入。
//
// ⚠️ 也覆盖"数据源被停用"的情况:那时**读**都读不了,更不该写。
func (s *Service) EnsureLocalSource(ctx context.Context, branchID string) error {
	src, useCube, err := s.resolveCubeSource(ctx, branchID)
	if err != nil {
		if errors.Is(err, model.ErrCubeSourceDisabled) {
			return err
		}
		// 问不到路由 ⇒ 放行(见上面的说明);只记一行日志,便于事后查。
		slog.Warn("member: 问不到门店数据源,写操作按本域处理", "branch_id", branchID, "err", err)
		return nil
	}
	if useCube {
		return fmt.Errorf("%w: 门店 %s 的会员档案来自外部数据源(%s),会员操作请在原系统进行",
			model.ErrExternalSourceReadOnly, branchID, src.CubeSourceName)
	}
	return nil
}

// resolveCubeSource 判定该门店的会员数据该走哪条路。
//
// 返回 (source, useCube, err):
//   - useCube=true  ⇒ 走 cube
//   - useCube=false ⇒ 走本域 PG
//   - err != nil    ⇒ 问不到 / 数据源被关停 ⇒ 上抛(503)
func (s *Service) resolveCubeSource(ctx context.Context, branchID string) (cubesource.Source, bool, error) {
	if s.sources == nil || s.cube == nil {
		// 未装配 ⇒ 本地(自营店场景)。
		return cubesource.Source{}, false, nil
	}
	src, err := s.sources.Resolve(ctx, branchID)
	if err != nil {
		// ⚠️ **权限类错误要原样上抛**:调用者没有该门店的 cube:read ⇒ 403,
		//   而"问不到路由"⇒ 503。两者混在一起会让"没权限"显示成"暂不可用",
		//   用户会去查数据源健康度(而真正的原因是权限)。
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
		return src, false, fmt.Errorf("%w: 门店 %s 的数据源 %s 已停用",
			model.ErrCubeSourceDisabled, branchID, src.CubeSourceName)
	}
	return src, true, nil
}

// listMembersFromCube 走 cube 查会员列表(门店配了数据源时)。
//
// ⚠️ 排序/分页/过滤**全部交给 cube**:前端只能排当前这一页,
// 而 cube 那边是全量口径 ⇒ 翻页顺序必须由 cube 保证。
func (s *Service) listMembersFromCube(ctx context.Context, branchID string, in ListMembersInput) ([]model.Member, int64, error) {
	rows, total, err := s.cube.ListMembers(ctx, cubeclient.MemberQuery{
		Field:  in.Field,
		Q:      in.Q,
		Limit:  in.Limit,
		Offset: in.Offset,
		Sort:   in.Sort,
		Order:  in.Order,
	})
	if err != nil {
		return nil, 0, err
	}
	out := make([]model.Member, 0, len(rows))
	for _, r := range rows {
		out = append(out, memberFromCubeRow(branchID, r))
	}
	return out, int64(total), nil
}

// getMemberFromCube 走 cube 查单个会员。
func (s *Service) getMemberFromCube(ctx context.Context, branchID, memberID string) (*model.Member, error) {
	row, err := s.cube.GetMember(ctx, memberID)
	if err != nil {
		if cubeclient.IsCubeNotFound(err) {
			return nil, model.ErrMemberNotFound
		}
		return nil, err
	}
	m := memberFromCubeRow(branchID, *row)
	return &m, nil
}

// memberFromCubeRow 把 cube 的一行(思迅原值)转成本域的 model.Member。
//
// ⚠️ **语义归一化在这一层做**(cube 只搬源值):
//   - card_status → 本域状态枚举(与导入端同一张表,见 sixunCardStatusMap)
//   - card_type   → **类别名**(本域的 member_types 对该门店是空的,
//     前端用 `typeNameById[id] ?? id` 渲染 ⇒ 直接给名字就能显示对)
//   - use_num 的"不限次"哨兵(999998/999999)→ 0
//   - card_id 的脏值(前导空格 / 反引号)→ 规范化(与导入端同一套规则)
func memberFromCubeRow(branchID string, r cubeclient.MemberRow) model.Member {
	now := time.Now()
	// ⚠️ 状态:思迅 card_status → 本域枚举(与导入端同一张映射表)。
	// 认不出的码 ⇒ 保守按 normal(与导入端一致),不猜。
	status, known := memberStatusOf(r.Status)
	if !known {
		status = model.MemberStatusNormal
	}
	m := model.Member{
		ID:                 normalizeMemberID(r.ID),
		BranchID:           branchID,
		CardTypeID:         memberCardTypeName(r.CardType),
		CardFaceNo:         strings.TrimSpace(r.CardFaceNo),
		Name:               strings.TrimSpace(r.Name),
		Phone:              strings.TrimSpace(r.Phone),
		Mobile:             strings.TrimSpace(r.Mobile),
		Gender:             strings.TrimSpace(r.Gender),
		Birthday:           r.Birthday,
		Status:             status,
		ValidFrom:          r.ValidFrom,
		ValidUntil:         r.ValidUntil,
		Favorite:           strings.TrimSpace(r.Favorite),
		IDCardNo:           strings.TrimSpace(r.IDCardNo),
		Remark:             strings.TrimSpace(r.Remark),
		FeeAmountYuan:      r.FeeAmountYuan,
		LimitedUseCount:    memberLimitedUseCount(r.LimitedUseCount),
		PointsAccumulated:  r.PointsAccumulated,
		PointsUsed:         r.PointsUsed,
		BalanceYuan:        r.BalanceYuan,
		ConsumedAmountYuan: r.ConsumedAmountYuan,
		ConsumedCount:      int(r.ConsumedCount),
		OpenID:             strings.TrimSpace(r.OpenID),
		CreatedBy:          "cube",
		UpdatedBy:          strings.TrimSpace(r.OperatorID),
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if r.OperatedAt != nil {
		m.UpdatedAt = *r.OperatedAt
		m.CreatedAt = *r.OperatedAt
	}
	m.RemainingPoints = m.PointsAccumulated.Sub(m.PointsUsed)
	return m
}

// memberCardTypeName 把思迅的类别码翻成**类别名**。
//
// ⚠️ 未知码( hbposv7 的 'wx'=微信会员)⇒ 原样返回 ⇒ 前端显示 'wx'(不猜)。
func memberCardTypeName(code string) string {
	c := strings.TrimSpace(code)
	if name, ok := sixunCardTypeNames[c]; ok {
		return name
	}
	return c
}

// memberLimitedUseCount 把思迅的"不限次"哨兵转成本域的 0。
func memberLimitedUseCount(n int64) int {
	if n >= 999998 {
		return 0
	}
	return int(n)
}

// memberPointsRemaining 算剩余积分(累计 − 已用)。
//
// ⚠️ cube 侧也有 remaining_points(源库侧派生),但 hbposv7 恒 NULL
// ⇒ 这里**始终自己算**(累计 − 已用),两边口径一致,且不依赖某个库有没有派生列。
func memberPointsRemaining(accumulated, used decimal.Decimal) decimal.Decimal {
	return accumulated.Sub(used)
}
