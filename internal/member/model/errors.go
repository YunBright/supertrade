package model

import (
	"errors"
	"sort"
)

// ---- 业务错误 sentinel(handler 层映 4xx / 5xx) ----

var (
	// ErrMemberNotFound:按 (id, branch_id) 未命中会员。
	ErrMemberNotFound = errors.New("member: 会员不存在")
	// ErrMemberAlreadyExists:同 (id, branch_id) 已有会员(开卡冲突)。
	ErrMemberAlreadyExists = errors.New("member: 会员已存在")
	// ErrMemberTypeNotFound:会员类别不存在。
	ErrMemberTypeNotFound = errors.New("member: 会员类别不存在")
	// ErrPointsInsufficient:剩余积分不足(409)。
	ErrPointsInsufficient = errors.New("member: 剩余积分不足")
	// ErrAccountInsufficient:储值余额不足(退储超过余额,409)。
	ErrAccountInsufficient = errors.New("member: 储值余额不足")
	// ErrMemberStatusNotAllowed:会员状态不允许该操作(冻结/挂失/注销,409)。
	ErrMemberStatusNotAllowed = errors.New("member: 会员当前状态不允许该操作")
	// ErrInvalidInput:入参非法(400)。
	ErrInvalidInput = errors.New("member: 入参非法")
	// ErrInvalidQueryField:查询字段不在允许集合内(400)。
	ErrInvalidQueryField = errors.New("member: 查询字段非法")
	// ErrMemberTypeAlreadyExists:同 (id, branch_id) 已有该会员类别(409)。
	ErrMemberTypeAlreadyExists = errors.New("member: 会员类别已存在")
	// ErrMemberTypeInUse:该类别下还有会员,不能删(409)。
	ErrMemberTypeInUse = errors.New("member: 会员类别下仍有会员,不能删除")
	// ErrCubeSourceUnavailable:问不到"这个门店配了哪个数据源"(503)。
	//
	// ⚠️ 与"没配数据源"严格区分:没配是**正常状态**(自营店),
	// 问不到是**链路故障** ⇒ 后者必须让用户看到"暂不可用",
	// 而不是让它悄悄回落成"空列表"(那正是本次要修的那个症状)。
	ErrCubeSourceUnavailable = errors.New("member: 数据源路由暂不可用")
	// ErrCubeSourceDisabled:该门店的数据源被管理员停用(503)。
	ErrCubeSourceDisabled = errors.New("member: 该门店的数据源已停用")
	// ErrExternalSourceReadOnly:该门店的会员档案来自外部系统,本域只读(409)。
	ErrExternalSourceReadOnly = errors.New("member: 该门店的会员档案来自外部系统,本域只读")
)

// ---- 会员状态 ----

// 会员状态枚举(我们自己的,⛔ 不照搬思迅的 0/1/2/3/5)。
const (
	// MemberStatusNormal 是正常。
	MemberStatusNormal = "normal"
	// MemberStatusFrozen 是冻结。
	MemberStatusFrozen = "frozen"
	// MemberStatusLost 是挂失。
	MemberStatusLost = "lost"
	// MemberStatusCancelled 是注销。
	MemberStatusCancelled = "cancelled"
)

// ValidMemberStatus 判断状态值是否合法。
func ValidMemberStatus(s string) bool {
	switch s {
	case MemberStatusNormal, MemberStatusFrozen, MemberStatusLost, MemberStatusCancelled:
		return true
	}
	return false
}

// ---- 积分流水方向与原因 ----

// 积分流水方向。
const (
	// PointDirectionEarn 是获得(赠分 / 消费累计)。
	PointDirectionEarn = "earn"
	// PointDirectionDeduct 是扣减(冲减积分 / 积分支付)。
	PointDirectionDeduct = "deduct"
)

// 积分流水原因。
const (
	// PointReasonSale 是消费累计。
	PointReasonSale = "sale"
	// PointReasonManualDeduct 是手工冲减。
	PointReasonManualDeduct = "manual_deduct"
	// PointReasonManualGrant 是手工赠分。
	PointReasonManualGrant = "manual_grant"
)

// ---- 储值流水方向与原因 ----

// 储值流水方向。
const (
	// AccountDirectionRecharge 是充值。
	AccountDirectionRecharge = "recharge"
	// AccountDirectionRefund 是退储。
	AccountDirectionRefund = "refund"
)

// ---- 会员域 scope(方案 §7.1) ----

// 会员域 scope 常量。
//
// ⚠️ 与 auth 的 permissions seed **逐字相同** —— 差一个字符就是"所有人 403"。
const (
	// ScopeView 是查询会员/积分/消费/上一单。
	ScopeView = "member:view"
	// ScopeCardOpen 是开卡。
	ScopeCardOpen = "member:card-open"
	// ScopePointDeduct 是冲减积分。
	ScopePointDeduct = "member:point-deduct"
	// ScopePointManage 是赠分/调整。
	ScopePointManage = "member:point-manage"
	// ScopeAccountRecharge 是储值充值/退储。
	ScopeAccountRecharge = "member:account-recharge"
	// ScopeStatusManage 是冻结/挂失。
	ScopeStatusManage = "member:status-manage"
	// ScopePasswordManage 是改密/重置。
	ScopePasswordManage = "member:password-manage"
	// ScopeFaceManage 是人脸登记。
	ScopeFaceManage = "member:face-manage"
	// ScopeConfig 是**门店会员配置**的写权限(会员开关 / 积分率 / 抵扣率 /
	// 密码与人脸必填 / 会员类别增删改)。
	//
	// ⚠️ 会员类别也归它管(不另开 scope)——
	//
	//	理由一:会员类别与会员参数同属"会员配置",同一个人维护;
	//	理由二:auth 的 permissions seed 里 `member:config` 已经存在且已给 admin,
	//	另开 scope 要再加一条迁移 + 一轮生产授权,收益不抵成本。
	//
	// ⚠️ 与 auth 的 permissions seed **逐字相同** —— 差一个字符就是"所有人 403"。
	ScopeConfig = "member:config"
)

// ---- 排序字段(会员档案列表的"按列排序") ----

// 允许的会员排序字段。
//
// ⚠️ 这些是**契约名**(前端传的),不是列名 —— 列名映射见 memberSortExpr。
const (
	// MemberSortID 是按会员号排。
	MemberSortID = "id"
	// MemberSortName 是按姓名排。
	MemberSortName = "name"
	// MemberSortMobile 是按手机排。
	MemberSortMobile = "mobile"
	// MemberSortCardType 是按会员类别排。
	MemberSortCardType = "card_type_id"
	// MemberSortStatus 是按状态排。
	MemberSortStatus = "status"
	// MemberSortPointsAccumulated 是按累计积分排。
	MemberSortPointsAccumulated = "points_accumulated"
	// MemberSortPointsUsed 是按已用积分排。
	MemberSortPointsUsed = "points_used"
	// MemberSortRemainingPoints 是按**剩余积分**排(派生列)。
	MemberSortRemainingPoints = "remaining_points"
	// MemberSortBalance 是按储值余额排。
	MemberSortBalance = "balance_yuan"
	// MemberSortConsumedAmount 是按累计消费排。
	MemberSortConsumedAmount = "consumed_amount_yuan"
	// MemberSortConsumedCount 是按消费次数排。
	MemberSortConsumedCount = "consumed_count"
	// MemberSortValidFrom 是按有效期起排。
	MemberSortValidFrom = "valid_from"
	// MemberSortValidUntil 是按有效期止排。
	MemberSortValidUntil = "valid_until"
	// MemberSortUpdatedAt 是按最后修改时刻排(**默认**)。
	MemberSortUpdatedAt = "updated_at"
)

// memberSortExpr 把排序字段映射到 ORDER BY 表达式。
//
// ⛔ 不接受任意列名/表达式(那是 SQL 注入面)⇒ 只认白名单。
//
// ⚠️ 映射到的是**表达式**而不是裸列名:剩余积分是派生列
// (points_accumulated − points_used,本域不落列),
// 想按它排就必须在 SQL 里现算。
var memberSortExpr = map[string]string{
	MemberSortID:                "id",
	MemberSortName:              "name",
	MemberSortMobile:            "mobile",
	MemberSortCardType:          "card_type_id",
	MemberSortStatus:            "status",
	MemberSortPointsAccumulated: "points_accumulated",
	MemberSortPointsUsed:        "points_used",
	MemberSortRemainingPoints:   "(points_accumulated - points_used)",
	MemberSortBalance:           "balance_yuan",
	MemberSortConsumedAmount:    "consumed_amount_yuan",
	MemberSortConsumedCount:     "consumed_count",
	MemberSortValidFrom:         "valid_from",
	MemberSortValidUntil:        "valid_until",
	MemberSortUpdatedAt:         "updated_at",
}

// MemberSortColumn 把排序字段映射到 ORDER BY 表达式。
//
// ⛔ 不接受任意列名(那是 SQL 注入面)⇒ 只认白名单。
func MemberSortColumn(field string) (string, bool) {
	expr, ok := memberSortExpr[field]
	return expr, ok
}

// MemberSortFields 是全部可排序字段(供上层做白名单校验与文档化)。
func MemberSortFields() []string {
	out := make([]string, 0, len(memberSortExpr))
	for k := range memberSortExpr {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- 查询字段(图3 的"查询条件"下拉) ----

// 允许的会员查询字段。
const (
	// QueryFieldCardID 是按会员卡号(= members.id)查。
	QueryFieldCardID = "card_id"
	// QueryFieldName 是按姓名查。
	QueryFieldName = "name"
	// QueryFieldPhone 是按电话号码查。
	QueryFieldPhone = "phone"
	// QueryFieldMobile 是按手机号查。
	QueryFieldMobile = "mobile"
	// QueryFieldRemark 是按备注查。
	QueryFieldRemark = "remark"
	// QueryFieldIDCardNo 是按身份证查。
	QueryFieldIDCardNo = "id_card_no"
)

// QueryFieldColumn 把查询字段映射到列名。
//
// ⛔ 不接受任意列名(那是 SQL 注入面)⇒ 只认白名单。
func QueryFieldColumn(field string) (string, bool) {
	switch field {
	case QueryFieldCardID:
		return "id", true
	case QueryFieldName:
		return "name", true
	case QueryFieldPhone:
		return "phone", true
	case QueryFieldMobile:
		return "mobile", true
	case QueryFieldRemark:
		return "remark", true
	case QueryFieldIDCardNo:
		return "id_card_no", true
	}
	return "", false
}
