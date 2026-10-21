// Package archiveroute —— 「归档导出 / 批量导入」端点的**统一判据**。
//
// # 它解决什么
//
//	supertrade 有一批**本域业务服务**(master-data / settlement-auditor /
//	stocktake / pos / fresh-meat / sales-agg):它们的数据**不在 cube 里**
//	(cube 只有商品/分类/供应商/条码/价格/促销/库存/会员/会员类别 +
//	 销售结算类 model)⇒ 这些域的读端点**不参与**「有源走 cube、无源走本地」
//	的路由(它们永远走本地)。
//
//	但**导入导出**仍要遵循同一套范式:
//	  - 没配 cube 源 ⇒ 导本地 / 写本地;
//	  - 配了 cube 源 ⇒ 导出**空数据** + 导入**拒绝(409 只读)**;
//	  - 问不到路由   ⇒ 导出 503(**不许**回落本地冒充成功)。
//
// # 为什么「配了源 ⇒ 导出空」是对的,「回落本地」是错的
//
//	回落 = 把「这个域在外部系统里没有数据」显示成「本地的旧数据」
//	⇒ 那是**假数据**,而且比空列表更危险:
//	  空列表会让人去问「数据在哪」,旧数据会被直接当成真的用下去。
//
// # 为什么判据必须只有一份
//
//	2026-10-21 审计的教训:catalog / pricing / inventory 三个服务对
//	「这个门店配了数据源吗」给了**三个**答案 ⇒ 同一个门店在不同服务里
//	表现不一致,而排查时每个服务看起来都"有道理"。
//
//	⇒ 本包是**唯一**判据,6 个服务共用(与 internal/cubesource 同一个实现类)。
package archiveroute

import (
	"context"
	"errors"
	"fmt"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubesource"
)

// Mode 是归档端点该走哪条路。
//
// ⚠️ 用**字符串**而不是 int:判据失败时返回的是零值 `""`,
//
//	而 `ModeLocal` 恰好也是"0 值邻居"——int 版里两者容易混淆
//	(调用方忘了判 err 就会把"判据失败"当成"走本地")。
type Mode string

const (
	// ModeLocal = 走本域 PG(该门店**没配** cube 数据源)。
	ModeLocal Mode = "local"
	// ModeExternal = 该门店**配了** cube 数据源 ⇒ 它的数据在外部系统里。
	//
	// ⚠️ 对本域服务来说,这不是"也能去 cube 查"——
	//   cube 里**根本没有**这些域(门店/收银员/班次/盘点/销售单/猪肉…)。
	//   ⇒ ModeExternal 的含义是"**这里没有**这个域的数据",
	//   所以导出给空、导入拒绝(409 只读)。
	ModeExternal Mode = "external"
)

// String 是 Mode 的可读名(日志用)。
func (m Mode) String() string { return string(m) }

// 判据失败的原因(调用方按它们映 HTTP 状态码)。
var (
	// ErrUnavailable:问不到路由(链路故障)⇒ 503 cube_unavailable。
	//
	// ⚠️ **不是**"没配数据源":两者混在一起会把一次故障显示成
	//   「该门店没有这个域的数据」——那是**假结论**。
	ErrUnavailable = errors.New("archiveroute: 数据源路由暂不可用")
	// ErrDisabled:配了源但被管理员停用 ⇒ 503 cube_source_disabled。
	//
	// ⚠️ 同样不是"没配":回落成"没配"会让用户看到一份空列表,
	//   而真相是"这个门店现在查不了"。
	ErrDisabled = errors.New("archiveroute: 该门店的数据源已停用")
	// ErrForbidden:调用方缺 cube:read ⇒ 403。
	ErrForbidden = errors.New("archiveroute: 没有该门店的数据源读权限")
	// ErrAuthRejected:登录态失效 ⇒ 401。
	ErrAuthRejected = errors.New("archiveroute: 登录态已失效")
	// ErrExternalReadOnly:该门店的数据在外部系统里,本域只读 ⇒ 409。
	//
	// ⚠️ **不是权限问题**:调用方**有**该门店的写权限,
	//	只是这份数据不在本域 ⇒ 写成 403 会让人去申请权限,
	//	而申请下来也还是 409(数据在别处)。
	ErrExternalReadOnly = errors.New("archiveroute: 该门店的数据来自外部数据源,本域只读")
)

// Resolver 解析门店的数据源(= [cubesource.Resolver])。
type Resolver = cubesource.Resolver

// DecideExport 判定**导出**端点走哪条路。
//
// 返回值:
//
//	(ModeLocal, nil)     ⇒ 导本域 PG 的表。
//	(ModeExternal, nil)  ⇒ 导出**空数据**(该域在 cube 里没有,不许回落本地)。
//	(_, ErrUnavailable)  ⇒ 问不到路由 ⇒ 503。
//	(_, ErrDisabled)     ⇒ 数据源被停用 ⇒ 503。
//	(_, ErrForbidden)    ⇒ 缺 cube:read ⇒ 403。
//	(_, ErrAuthRejected) ⇒ 登录态失效 ⇒ 401。
//
// ⚠️ **resolver 未装配 ⇒ ModeLocal**(自营店场景)。
//
//	装配漏了的表现是"有数据源的门店又变成导本地旧数据",
//	所以装配侧必须真的注入(见各 cmd/*/main.go)。
func DecideExport(ctx context.Context, resolver Resolver, branchID string) (Mode, error) {
	src, err := resolve(ctx, resolver, branchID)
	if err != nil {
		return "", err
	}
	if !src.Configured {
		// 自营店(没配数据源)⇒ 本域 PG。
		return ModeLocal, nil
	}
	if !src.Enabled {
		// 配了但被管理员关停 ⇒ "这个门店现在查不了",不是"没有数据源"。
		return "", ErrDisabled
	}
	// 配了且启用 ⇒ 数据在外部系统里 ⇒ 本域导出**空**。
	return ModeExternal, nil
}

// DecideImport 判定**导入**端点走哪条路。
//
// 返回值:
//
//	(ModeLocal, nil)     ⇒ 写本域 PG 的表。
//	(ModeExternal, nil)  ⇒ **409 只读**(数据在外部系统里,本域不许写)。
//	(_, err)             ⇒ 解析失败 ⇒ 调用方**放行**(见下)。
//
// ⚠️ **解析失败时放行**,不是拒(与 member 的 EnsureLocalSource 同口径):
//
//	导入路径自己会再查一次目标数据(写前先查主键),写不进去会**诚实地**失败
//	(而不是"写成功了却写在别处")。
//	⇒ 顺带:cube-router 挂掉时不会挡住自营店(门店 10)的正常写入。
//
// ⚠️ **配了但被停用 ⇒ 仍然 409**:源已经配了 ⇒ 这家店的数据在外部系统里
//
//	⇒ 往本域 PG 写一份"没人看的记录"就是制造假数据。
func DecideImport(ctx context.Context, resolver Resolver, branchID string) (Mode, error) {
	src, err := resolve(ctx, resolver, branchID)
	if err != nil {
		return "", err
	}
	if src.Configured {
		// 配了源(不论启用与否)⇒ 本域只读。
		return ModeExternal, nil
	}
	return ModeLocal, nil
}

// resolve 解析门店的数据源,并把 cubeclient 的错误语义归一成本包的 sentinel。
//
// ⚠️ **权限类错误必须与"问不到"分开**:
//
//	调用方没有该门店的 cube:read ⇒ 403;
//	路由问不到 ⇒ 503。
//	两者混在一起会让"没权限"显示成"暂不可用",用户会去查数据源健康度
//	(而真正的原因是权限)。
func resolve(ctx context.Context, resolver Resolver, branchID string) (cubesource.Source, error) {
	if resolver == nil {
		// 未装配 ⇒ 本地(自营店场景)。见 DecideExport 的说明。
		return cubesource.Source{}, nil
	}
	src, err := resolver.Resolve(ctx, branchID)
	if err != nil {
		switch {
		case errors.Is(err, cubeclient.ErrCubeForbidden):
			return cubesource.Source{}, ErrForbidden
		case errors.Is(err, cubeclient.ErrCubeAuthRejected):
			return cubesource.Source{}, ErrAuthRejected
		default:
			return cubesource.Source{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
	}
	return src, nil
}
