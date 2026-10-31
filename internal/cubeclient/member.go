// Package cubeclient / member.go —— 会员档案的 cube 读路径。
//
// # 什么时候会走这条路
//
// 门店在 cube-router 里配了数据源(富百家 00 → cube-sixun-ysx-fb,
// 佳乐 01 → cube-sixun-hbposv7-jiale)时，**该门店的会员档案在外部系统里**，
// 本域 PG 没有它的行 ⇒ 会员列表必须走 cube(cube-router → 该门店的 cube 实例)。
//
// 没配数据源的自营店(10)走本域 PG(member 服务自己的表)。
//
// # 调用链
//
//	member 服务 → cubeclient → dapr → supertrade-cube-router
//	                                  → branch_cube_sources(branch_id → cube 实例)
//	                                  → 该门店专属 cube 实例(DuckDB 里就是这家店的数据)
//
// 一个 cube 实例 = 一家门店 ⇒ 到达 cube 的查询天然只含本店数据，
// **不需要**再加门店维度过滤(思迅的 branch_no 与本系统 branch_id 是两套编码)。
package cubeclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// BranchSourcePath 是 cube-router 上「该门店配了哪个数据源」的路径。
const BranchSourcePath = "v1/branch-source"

// BranchSource 是「门店 → cube 数据源」的解析结果。
type BranchSource struct {
	// BranchID 是被查询的门店。
	BranchID string `json:"branch_id"`
	// Configured = 该门店在 branch_cube_sources 里有一行。
	Configured bool `json:"configured"`
	// Enabled = 那一行是否启用。
	//
	// ⚠️ **false 与"没配"是两件事**：
	//   - 没配(configured=false)⇒ 读本域 PG(自营店);
	//   - 配了但被关停(configured=true, enabled=false)⇒ **读不了**,
	//     应当报 503,而不是悄悄回落到本地(那会让用户看到"没会员")。
	Enabled bool `json:"enabled"`
	// CubeSourceName 是数据源对应的 dapr app-id。
	CubeSourceName string `json:"cube_source_name"`
}

// ResolveBranchSource 问 cube-router「这个门店配了哪个数据源」。
//
// ⚠️ 错误一律原样上抛:调用方必须区分"没配"(正常)与"问不到"(链路故障),
// 两者混在一起会让一次故障显示成"该门店没有会员"。
func (c *DaprCubeClient) ResolveBranchSource(ctx context.Context, branchID string) (BranchSource, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return BranchSource{}, fmt.Errorf("cubeclient: branch_id 必填")
	}
	// ⚠️ **必须把参数里的门店写进 ctx**:cube-router 的 /v1/branch-source 是从
	// X-Branch-ID header 取门店的(取不到直接 400 branch_required),
	// 而 invoke 只从 ctx 读门店 —— 后台流水线(runAudit 用 context.Background())的
	// ctx 里没有门店 ⇒ 每次解析都 400,所有"先问数据源再读"的路径全废
	// (2026-10-31 生产实踩:盘损失败 = 数据源路由暂不可用: Internal: Bad Request)。
	//
	// 已有值时**不覆盖**:HTTP 路径上 middleware 写入的是 header 原值
	// (可能是 "01,02" 多店形式),应收窄由下游权限判定做,这里不能改。
	if branchFromCtx(ctx) == "" {
		ctx = WithBranchID(ctx, branchID)
	}
	body, err := c.invoke(ctx, BranchSourcePath, map[string]any{})
	if err != nil {
		return BranchSource{}, err
	}
	var out BranchSource
	if err := json.Unmarshal(body, &out); err != nil {
		return BranchSource{}, fmt.Errorf("cubeclient: 解析 branch-source 响应失败: %w", err)
	}
	if out.BranchID == "" {
		out.BranchID = branchID
	}
	return out, nil
}

// MemberQuery 是查 cube member model 的条件。
type MemberQuery struct {
	// Field 是查询字段(白名单，见 memberQueryColumn)。
	Field string
	// Q 是关键字(模糊)。
	Q string
	// Limit / Offset 是分页。
	Limit  int
	Offset int
	// Sort 是排序字段(白名单，见 memberSortDimension)；空 = 默认 updated_at 倒序。
	Sort string
	// Order 是 asc / desc。
	Order string
}

// MemberRow 是 cube member model 的一行(思迅原值，**未做语义归一化**)。
type MemberRow struct {
	ID         string
	CardFaceNo string
	CardType   string
	Name       string
	Gender     string
	Phone      string
	Mobile     string
	Birthday   *time.Time
	IDCardNo   string
	// Status 是思迅的 card_status 原值('0'/'1'/'2'/'3'/'5')，**不是**本域枚举。
	Status             string
	FeeAmountYuan      decimal.Decimal
	LimitedUseCount    int64
	PointsAccumulated  decimal.Decimal
	PointsUsed         decimal.Decimal
	RemainingPoints    decimal.Decimal
	BalanceYuan        decimal.Decimal
	ConsumedAmountYuan decimal.Decimal
	ConsumedCount      int64
	Favorite           string
	Remark             string
	BranchNo           string
	OperatorID         string
	OperatedAt         *time.Time
	ValidFrom          *time.Time
	ValidUntil         *time.Time
	OpenID             string
}

// memberDimensions 是查会员时要 SELECT 的维度(全限定名)。
//
// ⚠️ **与 cube 的 member model schema 逐字对应** —— 少一个就查不到那一列
// (cube 返 400 dimension not found,不是静默少一列)。
var memberDimensions = []string{
	"member.id", "member.card_face_no", "member.card_type", "member.name",
	"member.gender", "member.phone", "member.mobile", "member.birthday",
	"member.id_card_no", "member.status", "member.fee_amount_yuan",
	"member.limited_use_count", "member.points_accumulated", "member.points_used",
	"member.remaining_points", "member.balance_yuan", "member.consumed_amount_yuan",
	"member.consumed_count", "member.favorite", "member.remark", "member.branch_no",
	"member.operator_id", "member.operated_at", "member.valid_from",
	"member.valid_until", "member.open_id",
}

// memberQueryColumn 把查询字段映射到 cube 的维度(白名单)。
//
// ⛔ 不接受任意列名(那是注入面)⇒ 只认这几个。
func memberQueryColumn(field string) (string, bool) {
	switch field {
	case "card_id":
		return "member.id", true
	case "name":
		return "member.name", true
	case "phone":
		return "member.phone", true
	case "mobile":
		return "member.mobile", true
	case "remark":
		return "member.remark", true
	case "id_card_no":
		return "member.id_card_no", true
	}
	return "", false
}

// memberSortDimension 把排序字段映射到 cube 的维度(白名单)。
//
// ⚠️ **与 member/model.MemberSortColumn 同一套字段**(本域 PG 与 cube 两边同义)，
// 但映射目标不同:本域是列名、cube 是全限定维度名。
func memberSortDimension(field string) (string, bool) {
	switch field {
	case "id":
		return "member.id", true
	case "name":
		return "member.name", true
	case "mobile":
		return "member.mobile", true
	case "card_type_id":
		return "member.card_type", true
	case "status":
		return "member.status", true
	case "points_accumulated":
		return "member.points_accumulated", true
	case "points_used":
		return "member.points_used", true
	case "remaining_points":
		return "member.remaining_points", true
	case "balance_yuan":
		return "member.balance_yuan", true
	case "consumed_amount_yuan":
		return "member.consumed_amount_yuan", true
	case "consumed_count":
		return "member.consumed_count", true
	case "valid_from":
		return "member.valid_from", true
	case "valid_until":
		return "member.valid_until", true
	case "updated_at":
		return "member.operated_at", true
	}
	return "", false
}

// ListMembers 查会员列表(走 cube),返回当页行 + 总命中行数。
//
// ⚠️ total 来自 cube 的 `total:true`(不受 limit/offset 影响的总行数)——
// 不给就是 0(那是"没问",不是"总共 0 条")。
func (c *DaprCubeClient) ListMembers(ctx context.Context, q MemberQuery) ([]MemberRow, int, error) {
	filters := make([]CubeFilter, 0, 1)
	if v := strings.TrimSpace(q.Q); v != "" {
		col, ok := memberQueryColumn(q.Field)
		if !ok {
			return nil, 0, fmt.Errorf("%w: 查询字段 %q 不允许", ErrCubeBadRequest, q.Field)
		}
		filters = append(filters, CubeFilter{
			Member:   col,
			Operator: OperatorContains,
			Values:   []any{v},
		})
	}

	order := []CubeOrder{{ID: "member.operated_at", Order: "desc"}}
	if s := strings.TrimSpace(q.Sort); s != "" {
		dim, ok := memberSortDimension(s)
		if !ok {
			return nil, 0, fmt.Errorf("%w: 排序字段 %q 不允许", ErrCubeBadRequest, s)
		}
		dir := strings.ToLower(strings.TrimSpace(q.Order))
		if dir != "asc" {
			dir = "desc"
		}
		order = []CubeOrder{{ID: dim, Order: dir}}
	}
	// ⚠️ 末尾追加 id 兜底:排序列会并列 ⇒ 没有兜底键就不是全序
	//   ⇒ 翻页会重复/漏行(症状是"第 2 页又看到第 1 页的某条")。
	order = append(order, CubeOrder{ID: "member.id", Order: "asc"})

	data, total, err := c.LoadCubeQueryPage(ctx, "member", CubeQuery{
		// ⚠️ **必须带 measure**:cube 拒收"没有 measure"的查询
		// (QUERY_INVALID: at least one measure required)⇒ 少这一个字段,
		// 整个会员列表接口会 503,而原因看起来跟"会员"毫无关系。
		//
		// member.count 是 COUNT(*),按维度分组后每组恒为 1 ⇒ 不影响行数。
		Measures:   []string{"member.count"},
		Dimensions: memberDimensions,
		Filters:    filters,
		Limit:      q.Limit,
		Offset:     q.Offset,
		Order:      order,
		Total:      true,
	})
	if err != nil {
		return nil, 0, err
	}
	rows := make([]MemberRow, 0, len(data))
	for _, m := range data {
		rows = append(rows, memberRowFrom(m))
	}
	return rows, total, nil
}

// GetMember 查单个会员(按卡号)。
//
// ⚠️ 走的是**同一个 cube 实例**(该门店的)⇒ 天然只含本店数据。
//
// ⚠️ **不能用 equals 查卡号**:源库实测有 2 行卡号带**前导空格**
// (' 801598')、1 行以反引号开头('`800973'),而 cube 的 RTRIM 只去**尾部**空格
// ⇒ equals("801598") 匹配不到 " 801598"。
//
// ⇒ 用 contains 拿候选行,再在 Go 侧按**规范化后的卡号**精确挑
// (contains 会命中"801598"这种子串,可能带出别的卡 ⇒ 不能只取第一条)。
func (c *DaprCubeClient) GetMember(ctx context.Context, memberID string) (*MemberRow, error) {
	rows, _, err := c.ListMembers(ctx, MemberQuery{
		Field: "card_id",
		Q:     memberID,
		// 候选可能多于一条(子串命中)⇒ 多取一些再精确挑。
		Limit: 50,
	})
	if err != nil {
		return nil, err
	}
	want := normalizeCardID(memberID)
	for i := range rows {
		if normalizeCardID(rows[i].ID) == want {
			return &rows[i], nil
		}
	}
	return nil, fmt.Errorf("%w: member_id=%s", ErrCubeNotFound, memberID)
}

// normalizeCardID 规范化思迅的卡号(去前导空格 / 去反引号)。
//
// ⚠️ 与 supertrade 导入端的 normalizeMemberID **同款规则**
// (两边看到的必须是同一个卡号,否则"导入到 10"与"直接查 00"对不上)。
func normalizeCardID(v string) string {
	s := strings.TrimSpace(v)
	s = strings.TrimPrefix(s, "`")
	return strings.TrimSpace(s)
}

// memberRowFrom 把 cube 的一行转成 MemberRow。
//
// ⚠️ 数值列用 asDecimal:它在 JSON 里可能是 number 也可能是 **base64 字符串**
// (思迅 live 源的 decimal 经 go-mssqldb 出来是 []byte ⇒ JSON 序列化成 base64)⇒
// 两种都必须收(见 cubeclient.ParseDecimal 的同款语义)。
func memberRowFrom(m map[string]any) MemberRow {
	return MemberRow{
		ID:                 asStr(m["member.id"]),
		CardFaceNo:         asStr(m["member.card_face_no"]),
		CardType:           asStr(m["member.card_type"]),
		Name:               asStr(m["member.name"]),
		Gender:             asStr(m["member.gender"]),
		Phone:              asStr(m["member.phone"]),
		Mobile:             asStr(m["member.mobile"]),
		Birthday:           asTimePtr(m["member.birthday"]),
		IDCardNo:           asStr(m["member.id_card_no"]),
		Status:             asStr(m["member.status"]),
		FeeAmountYuan:      asDecimal(m["member.fee_amount_yuan"]),
		LimitedUseCount:    asInt64(m["member.limited_use_count"]),
		PointsAccumulated:  asDecimal(m["member.points_accumulated"]),
		PointsUsed:         asDecimal(m["member.points_used"]),
		RemainingPoints:    asDecimal(m["member.remaining_points"]),
		BalanceYuan:        asDecimal(m["member.balance_yuan"]),
		ConsumedAmountYuan: asDecimal(m["member.consumed_amount_yuan"]),
		ConsumedCount:      asInt64(m["member.consumed_count"]),
		Favorite:           asStr(m["member.favorite"]),
		Remark:             asStr(m["member.remark"]),
		BranchNo:           asStr(m["member.branch_no"]),
		OperatorID:         asStr(m["member.operator_id"]),
		OperatedAt:         asTimePtr(m["member.operated_at"]),
		ValidFrom:          asTimePtr(m["member.valid_from"]),
		ValidUntil:         asTimePtr(m["member.valid_until"]),
		OpenID:             asStr(m["member.open_id"]),
	}
}
