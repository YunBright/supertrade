// Package service / import.go —— 会员档案的**导入**(思迅源库 → member 本地表)。
//
// # 它是什么
//
// 把思迅商云 V10 源库(cube 侧)的会员档案写进 `member.members` / `member.member_types`。
// 数据流:
//
//	cube-sync(dapr app) → cube-router(导出,scope=members/member_types)
//	                    → member(POST /import) → members / member_types 表
//
// # ⚠️ **本域的积分 / 储值 / 消费 / 状态是 POS 业务的事实源**
//
//	`sale.paid` 订阅给会员加积分、手工赠分/充值也改这些列 ⇒
//	**导入绝不能覆盖已存在会员的财务/状态列**。
//
//	⇒ 语义分两档(见 ImportRequest.Mode):
//	  - `incremental`(默认):新会员用思迅值初始化(bootstrap 基线);
//	    已存在会员**只更新身份字段**(姓名/电话/性别/备注/有效期/爱好/身份证/卡面号/类别),
//	    财务列与状态列**一个都不碰**。
//	  - `overwrite`:连财务列与状态列一起覆盖 —— **必须由运维显式选择**。
//
// # ⚠️ 为什么余额解析失败要**跳过该行**
//
//	思迅的 `residual_amt` 是**密文/脏值**(实测样例:"@><:6642675" / "86220.." / ","),
//	还有 764 行是空的。
//	⇒ 解析不出来时**不能静默当 0**:那会让一个**有储值余额**的会员变成"余额 0",
//	  而"余额 0"没有任何人会去查(静默截断比报错危险)。
//	⇒ 本实现:余额解析失败 ⇒ 该行**不落库** + 计入 skipped + 给一条 warning。
//
// # ⚠️ 会员类别**不自动建**
//
//	会员类别是**门店自己维护的字典**(本域:MT-STD-10 会员卡 / MT-SAV-10 储值卡 …),
//	而思迅的类别码是 0/1/2 ⇒ 两边 id 对不上,只能按**类别名**对齐。
//	⇒ 源库里有的类别、本域没有 ⇒ **不新建**(那是越权:类别归门店),
//	  会员行也**跳过**(它挂不到任何类别上)。
package service

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/member/model"
)

// importActor 是导入写入的审计人(照抄 inventory 的 CreatedBy:"import")。
const importActor = "import"

// maxImportWarnings 是导入报告里**最多**带多少条 warning。
//
// ⚠️ 不是"随便截断":一次全量导入有 4128 行,脏值可能成百上千条。
//   全量带回来会把响应体撑大、把真正的"第一条"淹没 ⇒ 截断 + 一条汇总说明。
const maxImportWarnings = 100

// maxImportErrors 是导入报告里**最多**带多少条逐行失败明细。
const maxImportErrors = 200

// 思迅 V10 的会员类别码 → **类别名**。
//
// ⚠️ 依据:源库 card_type 的实测取值(0/1/2)+ 仓内 model 的注释
//   ("思迅 ysx 2=VIP储值卡")。本仓**没有**权威文档,见报告的"不确定"说明。
var sixunCardTypeNames = map[string]string{
	"0": "会员卡",
	"1": "储值卡",
	"2": "VIP储值卡",
}

// 思迅 V10 的 card_status → 本域状态枚举。
//
// ⚠️ `3` 的语义**没有权威文档**:
//
//	实测这 21 张卡的 vip_end_date **全部未过期**(2041-10-02 ~ 2046-03-01)
//	⇒ 肯定不是"已过期"类。思迅 V10 的 3 常表示"冻结/停用"类,
//	但本仓查不到出处 ⇒ **保守按 frozen 处理**(冻结可解冻,写 normal 则是"放开了卡")。
//
// ⚠️ 绝不写 normal:猜错的代价是"一张本该停用的卡在 POS 上能正常消费"。
var sixunCardStatusMap = map[string]string{
	"0": model.MemberStatusNormal,
	"1": model.MemberStatusFrozen,
	"2": model.MemberStatusLost,
	"3": model.MemberStatusFrozen,
	"5": model.MemberStatusCancelled,
}

// 思迅 `use_num` 的"不限次"哨兵 ⇒ 本域的 0(0 = 不限次)。
//
// ⚠️ 999998 / 999999 **不是**次数,是"不限"⇒ 直接当次数会把 POS 的
//   "还能用 999998 次"显示给收银员。
var sixunUnlimitedUseSentinels = map[string]bool{
	"999998": true,
	"999999": true,
}

// MemberImportInput 是会员档案导入的入参。
type MemberImportInput struct {
	// SourceBranchID 是**来源门店**(仅用于审计与报告;数据本身由 cube 决定)。
	SourceBranchID string
	// TargetBranchID 是**目标门店**(= 请求的 X-Branch-ID)。
	//
	// ⚠️ 只从 header 取:body 里带一份门店就能跨店写入,那是**跨店写**的口子。
	TargetBranchID string
	// Mode 是 incremental / overwrite。
	Mode string
	// Data 是导入的行。
	Data archivedto.ImportData
}

// ImportMembers 导入会员档案(类别 + 主档)。
//
// ⚠️ **目标门店只从 header 取**(调用方传进来的)——
// body 里带一份门店就能跨店写入。
func (s *Service) ImportMembers(ctx context.Context, in MemberImportInput) (*archivedto.ImportResult, error) {
	target := strings.TrimSpace(in.TargetBranchID)
	if target == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", model.ErrInvalidInput)
	}
	mode := strings.TrimSpace(in.Mode)
	if mode == "" {
		mode = "incremental"
	}
	if mode != "incremental" && mode != "overwrite" {
		return nil, fmt.Errorf("%w: mode 只能是 incremental / overwrite", model.ErrInvalidInput)
	}

	rep := &archivedto.ImportResult{
		SourceBranchID: strings.TrimSpace(in.SourceBranchID),
		TargetBranchID: target,
		Mode:           mode,
	}

	// ① 类别先导 —— 会员引用类别,反了就成"引用了不存在的对象"。
	typeRow, typeWarn, typeErr := s.importMemberTypes(ctx, target, mode, in.Data.MemberTypes)
	// ② 会员主档。
	memRow, memWarn, memErr := s.importMembers(ctx, target, mode, in.Data.Members)

	rep.ByScope = []archivedto.ImportScopeRow{typeRow, memRow}
	rep.Warnings = append(rep.Warnings, typeWarn...)
	rep.Warnings = append(rep.Warnings, memWarn...)
	rep.Errors = append(rep.Errors, typeErr...)
	rep.Errors = append(rep.Errors, memErr...)
	return rep, nil
}

// importMemberTypes 导入会员类别字典(**只对齐,不新建**)。
//
// # ⚠️ 为什么不新建类别
//
//	会员类别是**门店自己维护的字典**:门店可以有自己的折扣率、是否储值。
//	思迅的类别码(0/1/2)与本域 id(MT-STD-10 …)是两套体系 ⇒ 只能按**名字**对齐。
//	源库有、本域没有的类别 ⇒ 跳过 + warning(不新建:那是替门店做决定)。
//
// # ⚠️ incremental 与 overwrite 的差别
//
//	incremental(默认):**不覆盖**门店自己维护的字段(只对齐命中)。
//	overwrite:覆盖 discount_rate / allow_stored_value / allow_points_pay。
func (s *Service) importMemberTypes(ctx context.Context, branchID, mode string,
	rows []archivedto.ImportMemberTypeRow) (archivedto.ImportScopeRow, []string, []archivedto.ImportErrorItem) {

	res := archivedto.ImportScopeRow{Scope: archivedto.ScopeMemberTypes, Received: len(rows)}
	if len(rows) == 0 {
		return res, nil, nil
	}
	warns := &warnCollector{}
	errs := &errCollector{}

	existing, err := s.ListTypes(ctx, branchID)
	if err != nil {
		return res, nil, nil
	}
	byName := make(map[string]model.MemberType, len(existing))
	for _, t := range existing {
		byName[strings.TrimSpace(t.Name)] = t
	}

	now := s.now()
	for _, row := range rows {
		name := strings.TrimSpace(row.Name)
		if name == "" {
			res.Skipped++
			warns.add("member_types:有一行没有类别名,已跳过")
			errs.add(archivedto.ScopeMemberTypes, row.ID, "类别名为空")
			continue
		}
		cur, ok := byName[name]
		if !ok {
			// ⚠️ **不新建类别** —— 见文件头。
			res.Skipped++
			warns.add(fmt.Sprintf("member_types:类别 %q 在本门店不存在,已跳过(不自动建类别:类别归门店维护)", name))
			errs.add(archivedto.ScopeMemberTypes, row.ID, "本门店没有同名会员类别")
			continue
		}
		if mode == "overwrite" {
			updates := map[string]any{
				"allow_stored_value": parseBoolLoose(row.AllowStoredValue),
				"allow_points_pay":   parseBoolLoose(row.AllowPointsPay),
				"updated_by":         importActor,
				"updated_at":         now,
			}
			if d, ok := parseDecimalLoose(row.DiscountRate); ok && d.GreaterThan(decimal.Zero) {
				updates["discount_rate"] = d
			}
			if err := s.db.WithContext(ctx).
				Model(&model.MemberType{}).
				Where("id = ? AND branch_id = ?", cur.ID, branchID).
				Updates(updates).Error; err != nil {
				slog.Error("member: 导入会员类别失败", "type_id", cur.ID, "err", err)
				res.Skipped++
				errs.add(archivedto.ScopeMemberTypes, cur.ID, err.Error())
				continue
			}
		}
		// incremental:只对齐命中,不覆盖门店自己维护的值。
		res.Updated++
	}
	return res, warns.list(), errs.list()
}

// importMembers 导入会员主档。
//
// ⚠️ **已存在的会员只更新身份字段**(incremental)——
// 积分 / 储值 / 消费 / 状态是本域 POS 业务的事实源,导入无权覆盖。
func (s *Service) importMembers(ctx context.Context, branchID, mode string,
	rows []archivedto.ImportMemberRow) (archivedto.ImportScopeRow, []string, []archivedto.ImportErrorItem) {

	res := archivedto.ImportScopeRow{Scope: archivedto.ScopeMembers, Received: len(rows)}
	if len(rows) == 0 {
		return res, nil, nil
	}
	warns := &warnCollector{}
	errs := &errCollector{}

	// 本门店的类别(按名字索引)—— 会员引用类别。
	types, err := s.ListTypes(ctx, branchID)
	if err != nil {
		return res, nil, nil
	}
	byName := make(map[string]model.MemberType, len(types))
	knownNames := make(map[string]bool, len(types))
	for _, t := range types {
		byName[strings.TrimSpace(t.Name)] = t
		knownNames[strings.TrimSpace(t.Name)] = true
	}

	now := s.now()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, row := range rows {
			id := normalizeMemberID(row.ID)
			if id == "" {
				res.Skipped++
				warns.add("members:有一行没有会员号,已跳过")
				errs.add(archivedto.ScopeMembers, row.ID, "会员号为空")
				continue
			}

			// ① 类别:按**名字**对齐(思迅码 → 类别名 → 本域类别)。
			typeName := memberTypeNameOf(row.CardType, knownNames)
			if typeName == "" {
				res.Skipped++
				warns.add(fmt.Sprintf("members:会员 %s 的 card_type=%q 认不出类别,已跳过", id, row.CardType))
				errs.add(archivedto.ScopeMembers, id, "card_type 认不出类别")
				continue
			}
			mt, ok := byName[typeName]
			if !ok {
				res.Skipped++
				warns.add(fmt.Sprintf("members:会员 %s 的类别 %q 在本门店不存在,已跳过(不自动建类别)", id, typeName))
				errs.add(archivedto.ScopeMembers, id, "本门店没有对应的会员类别")
				continue
			}

			// ② 状态:思迅 card_status → 本域枚举。
			status, known := memberStatusOf(row.Status)
			if !known {
				warns.add(fmt.Sprintf("members:会员 %s 的 card_status=%q 认不出,按 %s 处理", id, row.Status, status))
			}

			// ③ 余额:**解析失败 ⇒ 该行不落库**(见文件头)。
			balance, okBal := parseDecimalLoose(row.BalanceYuan)
			if !okBal {
				res.Skipped++
				warns.add(fmt.Sprintf("members:会员 %s 的余额 %q 解析不了,该行未落库(不静默当 0)", id, row.BalanceYuan))
				errs.add(archivedto.ScopeMembers, id, "余额解析失败: "+row.BalanceYuan)
				continue
			}

			// ④ 其余数值列:解析不了按 0 + warning(不跳整行 ——
			//    余额是"钱的多少",其余是"次数/比率",错的代价小得多)。
			pointsAcc, ok1 := parseDecimalLoose(row.PointsAccumulated)
			if !ok1 {
				pointsAcc = decimal.Zero
				warns.add(fmt.Sprintf("members:会员 %s 的累计积分 %q 解析不了,按 0 处理", id, row.PointsAccumulated))
			}
			pointsUsed, ok2 := parseDecimalLoose(row.PointsUsed)
			if !ok2 {
				pointsUsed = decimal.Zero
				warns.add(fmt.Sprintf("members:会员 %s 的已用积分 %q 解析不了,按 0 处理", id, row.PointsUsed))
			}
			fee, ok3 := parseDecimalLoose(row.FeeAmountYuan)
			if !ok3 {
				fee = decimal.Zero
			}
			consumedAmount, ok4 := parseDecimalLoose(row.ConsumedAmountYuan)
			if !ok4 {
				consumedAmount = decimal.Zero
			}
			consumedCount, ok5 := parseIntLoose(row.ConsumedCount)
			if !ok5 {
				consumedCount = 0
			}
			limitedUse, ok6 := parseIntLoose(row.LimitedUseCount)
			if !ok6 {
				limitedUse = 0
				warns.add(fmt.Sprintf("members:会员 %s 的限用次数 %q 解析不了,按 0(不限次)处理", id, row.LimitedUseCount))
			}
			if sixunUnlimitedUseSentinels[strings.TrimSpace(row.LimitedUseCount)] {
				// 999998 / 999999 = "不限次" ⇒ 本域的 0。
				limitedUse = 0
			}

			validFrom := parseTimeLoose(row.ValidFrom)
			validUntil := parseTimeLoose(row.ValidUntil)
			birthday := parseTimeLoose(row.Birthday)
			operatedAt := parseTimeLoose(row.OperatedAt)

			// ⑤ 已存在?(**写前先查** —— upsert 的 RowsAffected 分不出新建/覆盖)。
			exists, softDeleted := memberState(tx, branchID, id)
			if softDeleted {
				// ⚠️ 软删的会员**不复活**:本域已经明确"这张卡不要了",
				//   源库还留着它只是因为两边没同步 ⇒ 跳过 + warning。
				res.Skipped++
				warns.add(fmt.Sprintf("members:会员 %s 在本域已被软删,导入不复活它", id))
				errs.add(archivedto.ScopeMembers, id, "会员已被软删")
				continue
			}

			createdAt := now
			updatedAt := now
			if !exists && operatedAt != nil {
				// 新会员:档案状态截止到源库的最后一次操作时刻。
				updatedAt = *operatedAt
			}
			m := model.Member{
				ID:                 id,
				BranchID:           branchID,
				OpID:               importOpID(branchID, id, now),
				CardTypeID:         mt.ID,
				CardFaceNo:         strings.TrimSpace(row.CardFaceNo),
				Name:               strings.TrimSpace(row.Name),
				Phone:              strings.TrimSpace(row.Phone),
				Mobile:             strings.TrimSpace(row.Mobile),
				Gender:             strings.TrimSpace(row.Gender),
				Birthday:           birthday,
				Status:             status,
				ValidFrom:          validFrom,
				ValidUntil:         validUntil,
				Favorite:           strings.TrimSpace(row.Favorite),
				IDCardNo:           strings.TrimSpace(row.IDCardNo),
				Remark:             strings.TrimSpace(row.Remark),
				FeeAmountYuan:      fee,
				LimitedUseCount:    limitedUse,
				UsedCount:          0,
				PointsAccumulated:  pointsAcc,
				PointsUsed:         pointsUsed,
				BalanceYuan:        balance,
				ConsumedAmountYuan: consumedAmount,
				ConsumedCount:      consumedCount,
				OpenID:             strings.TrimSpace(row.OpenID),
				CreatedBy:          importActor,
				UpdatedBy:          importActor,
				CreatedAt:          createdAt,
				UpdatedAt:          updatedAt,
			}

			updateCols := memberIdentityColumns
			if mode == "overwrite" {
				// ⚠️ overwrite:连财务列与状态列一起覆盖(运维显式选择)。
				updateCols = memberOverwriteColumns
			}
			upsert := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "id"}, {Name: "branch_id"}},
				DoUpdates: clause.AssignmentColumns(append(append([]string{}, updateCols...),
					"updated_by", "updated_at")),
			}).
				// ⚠️ Select("*")：GORM 会省略带 default 标签的零值字段
				//   ⇒ 写 0 会被数据库默认值覆盖(同 UpsertType 的理由)。
				Select("*").
				Create(&m)
			if upsert.Error != nil {
				return fmt.Errorf("写会员失败(%s): %w", id, upsert.Error)
			}
			if exists {
				res.Updated++
			} else {
				res.Inserted++
			}
		}
		return nil
	})
	if err != nil {
		return res, warns.list(), errs.list()
	}
	return res, warns.list(), errs.list()
}

// memberIdentityColumns 是 incremental 模式下**允许更新**的列。
//
// ⚠️ **只有身份字段**:姓名/电话/性别/生日/爱好/身份证/备注/有效期/卡面号/类别/会员费/限用次数。
//
// ⛔ **不含** points_accumulated / points_used / balance_yuan /
// consumed_amount_yuan / consumed_count / status / used_count ——
// 那些是本域 POS 业务的事实源(积分与储值由 sale.paid 订阅与手工操作累积),
// 覆盖它们等于**用源库的历史快照抹掉本域已经发生的业务**。
var memberIdentityColumns = []string{
	"name", "phone", "mobile", "gender", "birthday", "favorite", "id_card_no",
	"remark", "valid_from", "valid_until", "card_face_no", "card_type_id",
	"fee_amount_yuan", "limited_use_count",
}

// memberOverwriteColumns 是 overwrite 模式下允许更新的列(身份 + 财务 + 状态)。
//
// ⚠️ used_count **不在里面**:源库没有对应列(合同里没有),没有值可覆盖 ⇒ 保持原值。
var memberOverwriteColumns = []string{
	"name", "phone", "mobile", "gender", "birthday", "favorite", "id_card_no",
	"remark", "valid_from", "valid_until", "card_face_no", "card_type_id",
	"fee_amount_yuan", "limited_use_count",
	"points_accumulated", "points_used", "balance_yuan",
	"consumed_amount_yuan", "consumed_count", "status",
}

// memberState 判断该会员在本域的状态。
//
// ⚠️ 用 Unscoped 查:软删的行**仍在表里**(只是查不到)⇒
//   不看 deleted_at 就分不出"这张卡被门店删了" ⇒ 导入会把它复活。
func memberState(tx *gorm.DB, branchID, memberID string) (exists bool, softDeleted bool) {
	var row model.Member
	err := tx.Unscoped().
		Select("id", "deleted_at").
		Where("id = ? AND branch_id = ?", memberID, branchID).
		First(&row).Error
	if err != nil {
		return false, false
	}
	return true, row.DeletedAt.Valid
}

// importOpID 造一个导入用的 op_id(会员表的 op_id 有唯一索引)。
//
// ⚠️ 必须**每次导入都不同**:同一张卡重复导入时,老行还在表里,
//   用固定 op_id 会撞唯一索引(而且撞的是"另一个"唯一约束 ⇒
//   ON CONFLICT (id, branch_id) 救不了,直接报错)。
func importOpID(branchID, memberID string, now time.Time) string {
	return fmt.Sprintf("import:%s:%s:%d", branchID, memberID, now.UnixNano())
}

// normalizeMemberID 规整会员号。
//
// ⚠️ 思迅的 card_id 带反引号与前导零("`00123")⇒ 本域统一去掉,
//   否则同一个会员在两端是两个字面量不同的 id ⇒ 查不到、还会重复建卡。
//   (口径见 docs/FIELD_SPEC.md 的 members.id 说明。)
func normalizeMemberID(raw string) string {
	v := strings.TrimSpace(raw)
	v = strings.Trim(v, "`'\"")
	v = strings.TrimSpace(v)
	// 去前导零(保留至少一位)。
	trimmed := strings.TrimLeft(v, "0")
	if trimmed == "" && v != "" {
		return "0"
	}
	return trimmed
}

// memberTypeNameOf 把思迅的 card_type 解析成**类别名**。
//
// ⚠️ 认不出来时返回 ""(调用方跳过该行 + warning)——
//   绝不"猜一个类别"、更不能"自动建一个"。
func memberTypeNameOf(raw string, knownNames map[string]bool) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	if name, ok := sixunCardTypeNames[v]; ok {
		return name
	}
	// 容错:源库若直接给的就是类别名,按名字认。
	if knownNames[v] {
		return v
	}
	return ""
}

// memberStatusOf 把思迅的 card_status 映射成本域状态枚举。
//
// ⚠️ 第二个返回值 false = "认不出,已按 normal 处理"(调用方给 warning)。
func memberStatusOf(raw string) (string, bool) {
	v := strings.TrimSpace(raw)
	if v == "" {
		// 没给状态 ⇒ 正常(源库里绝大多数是 0)。
		return model.MemberStatusNormal, true
	}
	if st, ok := sixunCardStatusMap[v]; ok {
		return st, true
	}
	// 容错:已经是本域枚举就认。
	if model.ValidMemberStatus(v) {
		return v, true
	}
	return model.MemberStatusNormal, false
}

// parseDecimalLoose 解析十进制字符串。
//
// ⚠️ 空串**不算** 0:返回 ok=false ⇒ 调用方能区分"没有值"与"值是 0"。
//   (这是余额那条铁律的基础。)
func parseDecimalLoose(raw string) (decimal.Decimal, bool) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return decimal.Zero, false
	}
	d, err := decimal.NewFromString(v)
	if err != nil {
		return decimal.Zero, false
	}
	return d, true
}

// parseIntLoose 解析整数字符串(空串 ⇒ false)。
func parseIntLoose(raw string) (int, bool) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseBoolLoose 解析"是不是 true"(源库是 0/1 的 char)。
func parseBoolLoose(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "y", "yes", "是":
		return true
	}
	return false
}

// parseTimeLoose 尽力把源值解析成时间。
//
// ⚠️ 源库的日期列有多种写法(带/不带时分秒、带/不带时间戳)⇒ 多试几种布局。
//   解析不出来返回 nil(那一列就不写),**不报错** —— 一个生日解析不了
//   不该让整张卡导不进来。
func parseTimeLoose(raw string) *time.Time {
	v := strings.TrimSpace(raw)
	if v == "" {
		return nil
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.000",
		"2006-01-02",
		"2006/01/02",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, v); err == nil {
			return &t
		}
	}
	return nil
}

// warnCollector 收集 warning(带上限)。
//
// ⚠️ 上限的意义:一次全量导入可能产生上千条同类 warning,
//   全量返回会把响应体撑大,并把"第一条真正的异常"淹没 ⇒ 截断 + 汇总说明。
type warnCollector struct {
	items []string
	extra int
}

func (w *warnCollector) add(msg string) {
	if len(w.items) < maxImportWarnings {
		w.items = append(w.items, msg)
		return
	}
	w.extra++
}

func (w *warnCollector) list() []string {
	if w.extra == 0 {
		return w.items
	}
	return append(w.items, fmt.Sprintf("另有 %d 条同类提示未列出(上限 %d 条)", w.extra, maxImportWarnings))
}

// errCollector 收集逐行失败明细(带上限)。
type errCollector struct {
	items []archivedto.ImportErrorItem
	extra int
}

func (e *errCollector) add(scope, id, msg string) {
	if len(e.items) < maxImportErrors {
		e.items = append(e.items, archivedto.ImportErrorItem{Scope: scope, ID: id, Error: msg})
		return
	}
	e.extra++
}

func (e *errCollector) list() []archivedto.ImportErrorItem {
	if e.extra == 0 {
		return e.items
	}
	return append(e.items, archivedto.ImportErrorItem{
		Scope: "import",
		ID:    "-",
		Error: fmt.Sprintf("另有 %d 条逐行明细未列出(上限 %d 条)", e.extra, maxImportErrors),
	})
}
