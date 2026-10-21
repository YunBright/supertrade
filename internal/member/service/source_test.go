package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/internal/member/service"
)

// ---- 测试替身 ----

// fakeSource 替身 SourceResolver。
type fakeSource struct {
	src cubeclient.BranchSource
	err error
	// calls 记录解析次数(用来断言"每次读都问一次"或"没问")。
	calls int
}

func (f *fakeSource) ResolveBranchSource(_ context.Context, _ string) (cubeclient.BranchSource, error) {
	f.calls++
	if f.err != nil {
		return cubeclient.BranchSource{}, f.err
	}
	return f.src, nil
}

// fakeCubeReader 替身 MemberReader。
type fakeCubeReader struct {
	rows  []cubeclient.MemberRow
	total int
	err   error
	// queries 记录每次查询条件(用来断言过滤 / 排序 / 分页确实传下去了)。
	queries []cubeclient.MemberQuery
}

func (f *fakeCubeReader) ListMembers(_ context.Context, q cubeclient.MemberQuery) ([]cubeclient.MemberRow, int, error) {
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.rows, f.total, nil
}

func (f *fakeCubeReader) GetMember(_ context.Context, memberID string) (*cubeclient.MemberRow, error) {
	// ⚠️ 模拟 cube 的**包含**语义(源库卡号带前导空格 ⇒ equals 匹配不到)
	// + 归一化后精确挑一条。
	for i := range f.rows {
		if strings.Contains(f.rows[i].ID, memberID) {
			return &f.rows[i], nil
		}
	}
	return nil, cubeclient.ErrCubeNotFound
}

func localSource() cubeclient.BranchSource {
	return cubeclient.BranchSource{BranchID: "10"}
}

func externalSource() cubeclient.BranchSource {
	return cubeclient.BranchSource{
		BranchID:       "00",
		Configured:     true,
		Enabled:        true,
		CubeSourceName: "cube-sixun-ysx-fb",
	}
}

// ---- 用例 ----

// TestListMembers_BranchWithCubeSourceReadsFromCube 锁"配了数据源的门店走 cube"。
//
// ⚠️ 这条是本次要修的**症状本体**:不路由 ⇒ 列表恒空
// ⇒ 前端显示「该门店还没有会员档案」(而会员其实在外部系统里)。
func TestListMembers_BranchWithCubeSourceReadsFromCube(t *testing.T) {
	svc, db := newService(t)
	// 本域 PG 里**没有**这个门店的会员(外部 ERP 的事实源不在本地)。
	seedMember(t, db, "LOCAL-1", "10")

	src := &fakeSource{src: externalSource()}
	reader := &fakeCubeReader{
		rows: []cubeclient.MemberRow{{
			ID: "801598", Name: "邹美玉", CardType: "0", Status: "0",
			PointsAccumulated: decimal.NewFromInt(1660),
		}},
		total: 4128,
	}
	svc.SetCubeSource(src, reader)

	rows, total, err := svc.ListMembersPage(context.Background(), service.ListMembersInput{
		BranchID: "00", Limit: 20,
	})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "801598", rows[0].ID)
	assert.Equal(t, "邹美玉", rows[0].Name)
	assert.Equal(t, int64(4128), total, "total 必须来自 cube(不受 limit 影响)")

	// ⚠️ 查询条件要**原样**传给 cube(过滤 / 排序 / 分页都在那边做)。
	require.Len(t, reader.queries, 1)
	q := reader.queries[0]
	assert.Equal(t, 20, q.Limit)
	assert.Equal(t, 0, q.Offset)
}

// TestListMembers_BranchWithoutCubeSourceReadsLocal 锁"没配数据源的门店读本域 PG"。
func TestListMembers_BranchWithoutCubeSourceReadsLocal(t *testing.T) {
	svc, db := newService(t)
	seedMember(t, db, "LOCAL-1", "10")

	src := &fakeSource{src: localSource()}
	reader := &fakeCubeReader{}
	svc.SetCubeSource(src, reader)

	rows, _, err := svc.ListMembersPage(context.Background(), service.ListMembersInput{
		BranchID: "10", Limit: 20,
	})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "LOCAL-1", rows[0].ID)
	assert.Empty(t, reader.queries, "没配数据源的门店**不该**打 cube")
}

// TestListMembers_SourceResolverFailureIsNotEmptyList 锁"问不到数据源 ⇒ 报错,不是空列表"。
//
// ⚠️ 这是本次修复里最要紧的一条:回落成空列表 = 把一次链路故障
// 显示成「该门店还没有会员档案」—— 根因与结论完全无关。
func TestListMembers_SourceResolverFailureIsNotEmptyList(t *testing.T) {
	svc, db := newService(t)
	seedMember(t, db, "LOCAL-1", "10")

	src := &fakeSource{err: errors.New("cube-router 不可达")}
	reader := &fakeCubeReader{}
	svc.SetCubeSource(src, reader)

	_, _, err := svc.ListMembersPage(context.Background(), service.ListMembersInput{
		BranchID: "00", Limit: 20,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceUnavailable)
}

// TestListMembers_SourceDisabledIsNotLocalFallback 锁"数据源被停用 ⇒ 报错"。
func TestListMembers_SourceDisabledIsNotLocalFallback(t *testing.T) {
	svc, _ := newService(t)
	disabled := externalSource()
	disabled.Enabled = false
	src := &fakeSource{src: disabled}
	reader := &fakeCubeReader{}
	svc.SetCubeSource(src, reader)

	_, _, err := svc.ListMembersPage(context.Background(), service.ListMembersInput{
		BranchID: "00", Limit: 20,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceDisabled)
}

// TestEnsureLocalSource 锁"写端点的只读闸门"。
func TestEnsureLocalSource(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	// 没配数据源 ⇒ 放行。
	svc.SetCubeSource(&fakeSource{src: localSource()}, &fakeCubeReader{})
	require.NoError(t, svc.EnsureLocalSource(ctx, "10"))

	// 配了数据源 ⇒ 拒(该门店的账在外部系统里)。
	svc.SetCubeSource(&fakeSource{src: externalSource()}, &fakeCubeReader{})
	err := svc.EnsureLocalSource(ctx, "00")
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrExternalSourceReadOnly)
}

// TestMemberFromCubeRow_Semantics 锁"cube 原始值 → 本域语义"的归一化。
//
// ⚠️ 这些映射与导入端(sixunCardTypeNames / sixunCardStatusMap)必须**同款**,
// 否则同一个会员在"导入到 10"与"直接查 00"两条路上显示成两个样子。
func TestMemberFromCubeRow_Semantics(t *testing.T) {
	svc, _ := newService(t)
	row := cubeclient.MemberRow{
		ID:                 " 801598", // 源库有前导空格
		CardType:           "1",       // 储值卡
		Status:             "3",       // 冻结(保守按 frozen)
		LimitedUseCount:    999999,    // "不限次"哨兵
		PointsAccumulated:  decimal.NewFromInt(100),
		PointsUsed:         decimal.NewFromInt(40),
		BalanceYuan:        decimal.RequireFromString("31.6"),
		ConsumedAmountYuan: decimal.RequireFromString("27862.68"),
		ConsumedCount:      1315,
	}
	src := &fakeSource{src: externalSource()}
	reader := &fakeCubeReader{rows: []cubeclient.MemberRow{row}, total: 1}
	svc.SetCubeSource(src, reader)

	got, err := svc.Get(context.Background(), "00", "801598")
	require.NoError(t, err)
	assert.Equal(t, "801598", got.ID, "卡号要去掉前导空格")
	assert.Equal(t, "储值卡", got.CardTypeID, "类别码要翻成类别名")
	assert.Equal(t, model.MemberStatusFrozen, got.Status)
	assert.Equal(t, 0, got.LimitedUseCount, "不限次哨兵要转 0")
	assert.Equal(t, "60", got.RemainingPoints.String(), "剩余积分 = 累计 − 已用")
	assert.Equal(t, "31.6", got.BalanceYuan.String())
	assert.Equal(t, "27862.68", got.ConsumedAmountYuan.String())
	assert.Equal(t, 1315, got.ConsumedCount)
}
