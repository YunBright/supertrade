package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/internal/member/service"
)

// strPtr 取字符串指针(局部更新的"传了"标记)。
//
// ⚠️ 与 config_write_test.go 的同名 helper 同款 —— 放在本包共享的 helper 文件里
// 会撞名,这里用本文件私有的名字。
func profileStrPtr(s string) *string { return &s }

// seedMemberWithOp 造一个会员(带**唯一** op_id)。
//
// ⚠️ 为什么不用共享的 seedMember:它不写 op_id ⇒ 空串撞上 members.op_id 的唯一索引
//   ⇒ 同一个测试里插第二个会员就报 UNIQUE constraint failed。
//   (op_id 在生产里总是有值 —— 开卡时写进去的。)
func seedMemberWithOp(t *testing.T, db *gorm.DB, id, branchID string) {
	t.Helper()
	require.NoError(t, db.Create(&model.Member{
		ID: id, BranchID: branchID, CardTypeID: "T1", Name: "张三",
		OpID:              "seed-" + branchID + "-" + id,
		Status:            model.MemberStatusNormal,
		PointsAccumulated: decimal.Zero, PointsUsed: decimal.Zero,
		BalanceYuan:        decimal.Zero, ConsumedAmountYuan: decimal.Zero,
		CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)
}

// TestUpdateProfile_ChangesName 锁"改名字成功"。
func TestUpdateProfile_ChangesName(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")

	row, err := svc.UpdateProfile(context.Background(), service.UpdateProfileInput{
		OpID: "op-prof-1", BranchID: "10", MemberID: "M0001",
		Name: profileStrPtr("李四"), OperatorID: "u-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "李四", row.Name)

	var m model.Member
	require.NoError(t, db.Where("id = ? AND branch_id = ?", "M0001", "10").First(&m).Error)
	assert.Equal(t, "李四", m.Name, "落库的姓名也要更新")
}

// TestUpdateProfile_UnsetFieldsUnchanged 锁"未传的字段保持原值"。
//
// ⚠️ 这是局部更新语义的核心:管理端只改一个字段时,
//   全量替换会把没改的清掉(电话/性别/爱好…全没了)。
func TestUpdateProfile_UnsetFieldsUnchanged(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")
	require.NoError(t, db.Model(&model.Member{}).
		Where("id = ? AND branch_id = ?", "M0001", "10").
		Updates(map[string]any{
			"phone": "13800000000", "gender": "男", "favorite": "钓鱼", "remark": "老顾客",
		}).Error)

	_, err := svc.UpdateProfile(context.Background(), service.UpdateProfileInput{
		OpID: "op-prof-1", BranchID: "10", MemberID: "M0001",
		Name: profileStrPtr("李四"), OperatorID: "u-1",
	})
	require.NoError(t, err)

	var m model.Member
	require.NoError(t, db.Where("id = ? AND branch_id = ?", "M0001", "10").First(&m).Error)
	assert.Equal(t, "李四", m.Name, "传了的字段要更新")
	assert.Equal(t, "13800000000", m.Phone, "没传的字段必须保持原值")
	assert.Equal(t, "男", m.Gender)
	assert.Equal(t, "钓鱼", m.Favorite)
	assert.Equal(t, "老顾客", m.Remark)
}

// TestUpdateProfile_Idempotent 锁"同 op_id 重复提交 ⇒ 只生效一次"。
func TestUpdateProfile_Idempotent(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")

	_, err := svc.UpdateProfile(context.Background(), service.UpdateProfileInput{
		OpID: "op-prof-1", BranchID: "10", MemberID: "M0001",
		Name: profileStrPtr("李四"), OperatorID: "u-1",
	})
	require.NoError(t, err)

	// 重放:同一个 op_id、不同内容 ⇒ 不得再改一次。
	_, err = svc.UpdateProfile(context.Background(), service.UpdateProfileInput{
		OpID: "op-prof-1", BranchID: "10", MemberID: "M0001",
		Name: profileStrPtr("王五"), OperatorID: "u-1",
	})
	require.NoError(t, err, "重放必须返回幂等的成功响应,不能报错")

	var m model.Member
	require.NoError(t, db.Where("id = ? AND branch_id = ?", "M0001", "10").First(&m).Error)
	assert.Equal(t, "李四", m.Name, "同 op_id 只能生效一次")
}

// TestUpdateProfile_UnknownCardType_NotFound 锁"类别不存在 ⇒ member_type_not_found"。
func TestUpdateProfile_UnknownCardType_NotFound(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")

	_, err := svc.UpdateProfile(context.Background(), service.UpdateProfileInput{
		OpID: "op-prof-1", BranchID: "10", MemberID: "M0001",
		CardTypeID: profileStrPtr("NOPE"), OperatorID: "u-1",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrMemberTypeNotFound), "实际 %v", err)
}

// TestUpdateProfile_CrossBranchCategory_NotFound 锁"跨店类别 ⇒ 拒绝"。
//
// ⚠️ 挂到别的店的类别上 ⇒ POS 的类别下拉里根本看不到这张卡(会员"消失"了)。
func TestUpdateProfile_CrossBranchCategory_NotFound(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedType(t, db, "T9", "99", "别的店的会员卡")
	seedMember(t, db, "M0001", "10")

	_, err := svc.UpdateProfile(context.Background(), service.UpdateProfileInput{
		OpID: "op-prof-1", BranchID: "10", MemberID: "M0001",
		CardTypeID: profileStrPtr("T9"), OperatorID: "u-1",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrMemberTypeNotFound), "实际 %v", err)
}

// TestUpdateProfile_MemberNotFound 锁"会员不存在 ⇒ ErrMemberNotFound"。
func TestUpdateProfile_MemberNotFound(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")

	_, err := svc.UpdateProfile(context.Background(), service.UpdateProfileInput{
		OpID: "op-prof-1", BranchID: "10", MemberID: "NOPE",
		Name: profileStrPtr("李四"),
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrMemberNotFound), "实际 %v", err)
}

// ---- 列表翻页 / 总数 ----

// TestListMembers_Pagination 锁"offset 翻页 ⇒ 不重叠 + total 是命中总数"。
//
// ⚠️ 门店10 导入后有 4128 个会员,而 limit 上限 200
// ⇒ 没有 offset/total 的话前端只能看到前 200 条(静默截断)。
func TestListMembers_Pagination(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	for _, id := range []string{"M1", "M2", "M3", "M4", "M5"} {
		seedMemberWithOp(t, db, id, "10")
	}

	seen := map[string]bool{}
	for offset := 0; offset < 5; offset += 2 {
		rows, total, err := svc.ListMembersPage(context.Background(), service.ListMembersInput{
			BranchID: "10", Limit: 2, Offset: offset,
		})
		require.NoError(t, err)
		assert.Equal(t, int64(5), total, "total 必须是命中总数")
		require.LessOrEqual(t, len(rows), 2)
		for _, r := range rows {
			assert.False(t, seen[r.ID], "翻页拿到的行不能重叠: %s", r.ID)
			seen[r.ID] = true
		}
	}
	assert.Len(t, seen, 5, "翻完应当拿到全部 5 行")
}

// TestListMembers_TotalCountsFiltered 锁"total 与列表**用同一组条件**"。
func TestListMembers_TotalCountsFiltered(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMemberWithOp(t, db, "M1", "10")
	seedMemberWithOp(t, db, "M2", "10")
	require.NoError(t, db.Model(&model.Member{}).
		Where("id = ? AND branch_id = ?", "M1", "10").
		Update("name", "张三").Error)
	require.NoError(t, db.Model(&model.Member{}).
		Where("id = ? AND branch_id = ?", "M2", "10").
		Update("name", "李四").Error)

	rows, total, err := svc.ListMembersPage(context.Background(), service.ListMembersInput{
		BranchID: "10", Field: "name", Q: "张", Limit: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total, "total 必须与过滤条件一致")
	assert.Len(t, rows, 1)
	assert.Equal(t, "M1", rows[0].ID)
}

// TestListMembers_InvalidField 锁"field 不在白名单 ⇒ 400(计数也不能绕过白名单)"。
func TestListMembers_InvalidField(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")

	_, _, err := svc.ListMembersPage(context.Background(), service.ListMembersInput{
		BranchID: "10", Field: "id_card_no; drop table members", Limit: 10,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, model.ErrInvalidQueryField), "实际 %v", err)
}

// TestListMembers_OutOfRange 锁"越界的 limit/offset 不 panic"。
func TestListMembers_OutOfRange(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M0001", "10")

	rows, total, err := svc.ListMembersPage(context.Background(), service.ListMembersInput{
		BranchID: "10", Limit: 999999, Offset: 999999,
	})
	require.NoError(t, err)
	assert.Empty(t, rows)
	assert.Equal(t, int64(1), total)

	// 负数 offset ⇒ 当 0 处理。
	rows, _, err = svc.ListMembersPage(context.Background(), service.ListMembersInput{
		BranchID: "10", Limit: 10, Offset: -5,
	})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

// TestListMembers_ExcludesOtherBranch 锁"只返回本门店的会员"。
func TestListMembers_ExcludesOtherBranch(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMemberWithOp(t, db, "M1", "10")
	seedMemberWithOp(t, db, "M2", "99")

	rows, total, err := svc.ListMembersPage(context.Background(), service.ListMembersInput{
		BranchID: "10", Limit: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	require.Len(t, rows, 1)
	assert.Equal(t, "M1", rows[0].ID)
}
