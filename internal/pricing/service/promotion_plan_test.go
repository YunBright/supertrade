// 本文件锁住「促销方案(图4)」读路径的分流、形状与**审核状态翻译**。
//
// # 为什么要有这组用例
//
// 1. 三态分流:问不到 ⇒ 报错(不许回落成本地)。
// 2. **approve_flag 必须翻译**:源列是 varchar(1),实测 `2` = 已审核。
//    把源码透给前端 ⇒ 前端要自己知道 "2" 是什么意思,而这份知识会散落。
// 3. 两条路径的响应结构必须逐字相同。
//
// ⚠️ 本文件是 `package service`(与同目录既有测试同包)⇒ 复用
// fakeSource / externalSource / localSource,不再定义第二份。
package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	_ "modernc.org/sqlite" // 注册驱动名 "sqlite"(纯 Go 驱动,无需 CGO)

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead/cubereadfake"
	"github.com/YunBright/supertrade/internal/pricing/model"
)

// newPlanDB 构造**文件型** SQLite + 本域表。
func newPlanDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "pricing.db")
	dialector := sqlite.Dialector{DriverName: "sqlite", DSN: dsn}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.PriceList{}, &model.Promotion{}, &model.PromotionPlan{}), "auto migrate")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// newPlanSvc 构造服务(解析器 + 本地库 + cube 读端三件都装)。
func newPlanSvc(t *testing.T, src *fakeSource) (*PromotionPlanService, *gorm.DB, *cubereadfake.Client) {
	t.Helper()
	db := newPlanDB(t)
	reader := cubereadfake.New()
	reader.Handler = func(m string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		if m != "promotion_plan" {
			return nil, 0, nil
		}
		// ⚠️ 按查询里的方案号过滤(模拟 cube 的 filter 语义)——
		// 否则"查一个不存在的方案号"也会返回行,404 用例就测不出来。
		for _, f := range q.Filters {
			if f.Member != "promotion_plan.plan_no" {
				continue
			}
			want := ""
			if len(f.Values) > 0 {
				want, _ = f.Values[0].(string)
			}
			if want != "" && want != "PS002109160001" {
				return nil, 0, nil
			}
		}
		rows := []map[string]any{
			{
				"promotion_plan.plan_no":      "PS002109160001",
				"promotion_plan.plan_name":    "开业促销",
				"promotion_plan.plan_memo":    "开业促销",
				"promotion_plan.begin_date":   "2021-09-17",
				"promotion_plan.end_date":     "2021-09-18",
				"promotion_plan.created_at":   "09 17 2021  9:55AM",
				"promotion_plan.created_by":   "2001",
				"promotion_plan.approved_at":  "09 17 2021  9:56AM",
				"promotion_plan.approved_by":  "2001",
				"promotion_plan.stopped_at":   "09 24 2021 11:13AM",
				"promotion_plan.stopped_by":   "5001",
				"promotion_plan.approve_flag": "2",
				"promotion_plan.member_level": "无",
			},
			{
				"promotion_plan.plan_no":      "PS002109160002",
				"promotion_plan.plan_name":    "草稿方案",
				"promotion_plan.plan_memo":    "",
				"promotion_plan.begin_date":   "2021-09-17",
				"promotion_plan.end_date":     "2021-09-18",
				"promotion_plan.created_at":   "09 17 2021  9:55AM",
				"promotion_plan.created_by":   "2001",
				"promotion_plan.approved_at":  "",
				"promotion_plan.approved_by":  "",
				"promotion_plan.stopped_at":   "",
				"promotion_plan.stopped_by":   "",
				"promotion_plan.approve_flag": "1",
				"promotion_plan.member_level": "无",
			},
		}
		return rows, len(rows), nil
	}
	svc := NewPromotionPlanService(db, nil)
	svc.SetCubeSource(src, reader)
	return svc, db, reader
}

// seedLocalPlans 灌本域 PG 的门店 10 促销方案。
func seedLocalPlans(t *testing.T, db *gorm.DB) {
	t.Helper()
	begin := time.Date(2021, 9, 17, 0, 0, 0, 0, time.UTC)
	end := time.Date(2021, 9, 18, 0, 0, 0, 0, time.UTC)
	created := time.Date(2021, 9, 17, 9, 55, 13, 0, time.UTC)
	approved := time.Date(2021, 9, 17, 9, 56, 32, 0, time.UTC)
	rows := []any{
		&model.PromotionPlan{PlanNo: "PS002109160001", BranchID: "10",
			PlanName: "开业促销", PlanMemo: "开业促销",
			BeginDate: &begin, EndDate: &end,
			CreatedAt: &created, CreatedBy: "2001",
			ApprovedAt: &approved, ApprovedBy: "2001",
			ApproveFlag: "2", ApproveStatus: "approved", MemberLevel: "无"},
		&model.PromotionPlan{PlanNo: "PS002109160002", BranchID: "10",
			PlanName: "草稿方案",
			BeginDate: &begin, EndDate: &end,
			CreatedAt: &created, CreatedBy: "2001",
			ApproveFlag: "1", ApproveStatus: "draft", MemberLevel: "无"},
		// ⚠️ 第三行只为"会员等级过滤能真的收窄"而存在:
		// 源库实测会员等级恒为"无",但**过滤器本身必须能区分**,
		// 否则"点了筛选没反应"与"参数被忽略"看起来一模一样。
		&model.PromotionPlan{PlanNo: "PS002109160003", BranchID: "10",
			PlanName: "会员等级方案",
			BeginDate: &begin, EndDate: &end,
			CreatedAt: &created, CreatedBy: "2001",
			ApproveFlag: "2", ApproveStatus: "approved", MemberLevel: "银卡"},
	}
	for _, r := range rows {
		require.NoError(t, db.Create(r).Error, "seed")
	}
}

// ---- 三态 ----

func TestPromotionPlans_ConfiguredSourceReadsCube(t *testing.T) {
	src := &fakeSource{src: externalSource()}
	svc, db, _ := newPlanSvc(t, src)
	seedLocalPlans(t, db)

	page, err := svc.List(context.Background(), "00", PromotionPlanQuery{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	assert.Equal(t, "cube", page.DataSource)
	// ⚠️ **审核状态是翻译后的**,不是源码 "2"。
	assert.Equal(t, "approved", page.Items[0].ApproveStatus)
	assert.Equal(t, "已审核", page.Items[0].ApproveStatusLabel)
	assert.Equal(t, "draft", page.Items[1].ApproveStatus)
	assert.Equal(t, "未审核", page.Items[1].ApproveStatusLabel)
	// 时间被规范化成 RFC3339(+08:00)。
	// ⚠️ 源串 "09 17 2021  9:55AM" 是**无时区**的 MSSQL 布局 ⇒ 按 UTC+8 记账,
	// 直接标成 +08:00(**不是**从 UTC 换算,那会平移 8 小时)。
	assert.Equal(t, "2021-09-17T09:55:00+08:00", page.Items[0].CreatedAt)
	require.NotNil(t, page.Items[0].ApprovedAt)
	assert.Equal(t, "2021-09-17T09:56:00+08:00", *page.Items[0].ApprovedAt)
	require.NotNil(t, page.Items[0].StoppedAt)
	assert.Equal(t, "2021-09-24T11:13:00+08:00", *page.Items[0].StoppedAt)
}

func TestPromotionPlans_NoSourceReadsLocal(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db, _ := newPlanSvc(t, src)
	seedLocalPlans(t, db)

	page, err := svc.List(context.Background(), "10", PromotionPlanQuery{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Len(t, page.Items, 3, "本地路径返回全部三行(含会员等级那行)")
	assert.Equal(t, "local", page.DataSource)
	assert.Equal(t, "approved", page.Items[0].ApproveStatus)
}

func TestPromotionPlans_ResolveFailureIsNotEmptyList(t *testing.T) {
	src := &fakeSource{err: errors.New("cube-router 不可达")}
	svc, db, _ := newPlanSvc(t, src)
	seedLocalPlans(t, db)

	_, err := svc.List(context.Background(), "10", PromotionPlanQuery{Page: 1, PageSize: 50})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceUnavailable)
}

func TestPromotionPlans_SourceDisabledIsNotLocalFallback(t *testing.T) {
	disabled := externalSource()
	disabled.Enabled = false
	src := &fakeSource{src: disabled}
	svc, db, _ := newPlanSvc(t, src)
	seedLocalPlans(t, db)

	_, err := svc.List(context.Background(), "10", PromotionPlanQuery{Page: 1, PageSize: 50})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceDisabled)
}

// ---- 过滤真的生效 ----

func TestPromotionPlans_LocalFilterNarrowsResult(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db, _ := newPlanSvc(t, src)
	seedLocalPlans(t, db)

	all, err := svc.List(context.Background(), "10", PromotionPlanQuery{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Equal(t, 3, all.Total)

	byNo, err := svc.List(context.Background(), "10", PromotionPlanQuery{
		PlanNo: "PS002109160001", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, byNo.Total, "方案号过滤必须真的收窄命中数")

	byStatus, err := svc.List(context.Background(), "10", PromotionPlanQuery{
		ApproveStatus: "draft", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, byStatus.Total, "审核状态过滤必须真的收窄命中数")

	byName, err := svc.List(context.Background(), "10", PromotionPlanQuery{
		PlanName: "开业", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, byName.Total, "方案名过滤必须真的收窄命中数")

	// ⚠️ 会员等级过滤:前端已经在发 member_level,后端静默忽略的话
	// "点了筛选没反应"与"参数被忽略"长得一模一样 ⇒ 必须能断言它真的生效。
	byLevel, err := svc.List(context.Background(), "10", PromotionPlanQuery{
		MemberLevel: "银卡", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, byLevel.Total, "会员等级过滤必须真的收窄命中数")
}

func TestPromotionPlans_CubeQueryCarriesFilters(t *testing.T) {
	src := &fakeSource{src: externalSource()}
	svc, _, reader := newPlanSvc(t, src)

	_, err := svc.List(context.Background(), "00", PromotionPlanQuery{
		PlanNo: "PS002109160001", PlanName: "开业", ApproveStatus: "approved",
		CreatedBy: "2001", MemberLevel: "无", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	q, ok := reader.LastQuery("promotion_plan")
	require.True(t, ok)
	assert.True(t, q.HasFilter("promotion_plan.plan_no"))
	assert.True(t, q.HasFilter("promotion_plan.plan_name"))
	assert.True(t, q.HasFilter("promotion_plan.created_by"))
	// ⚠️ 会员等级也必须真的进过滤:前端已经在发 member_level,
	// 后端静默忽略 ⇒ 用户点了筛选、列表没变,零提示。
	assert.True(t, q.HasFilter("promotion_plan.member_level"),
		"member_level 必须落到 cube 过滤里(前端已经在发这个参数)")
	// ⚠️ 状态是译文 ⇒ 必须反查源码集合。
	assert.Equal(t, []any{"2"}, q.FilterValues("promotion_plan.approve_flag"))
}

// ---- 详情 ----

func TestPromotionPlan_Get_NotFound(t *testing.T) {
	svc, _, _ := newPlanSvc(t, &fakeSource{src: externalSource()})
	_, err := svc.Get(context.Background(), "00", "NOPE")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPromotionPlanNotFound)
}

// ---- 排序 ----

// TestPromotionPlans_CubeQueryCarriesSortOrder 锁"排序真的进了 cube 查询"。
//
// ⚠️ 只断言"返回了 200"是**抓不到**一个被静默忽略的排序参数:
// 用户点了"按方案名称排",列表还是按日期排,零提示。
func TestPromotionPlans_CubeQueryCarriesSortOrder(t *testing.T) {
	src := &fakeSource{src: externalSource()}
	svc, _, reader := newPlanSvc(t, src)

	_, err := svc.List(context.Background(), "00", PromotionPlanQuery{
		Sort: "plan_name", Order: "asc", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)

	q, ok := reader.LastQuery("promotion_plan")
	require.True(t, ok)
	require.Len(t, q.Query.Order, 2, "排序项 = 排序列 + 兜底键")
	assert.Equal(t, "promotion_plan.plan_name", q.Query.Order[0].ID)
	assert.Equal(t, "asc", q.Query.Order[0].Order)
	// ⚠️ 兜底键:排序列会并列 ⇒ 没有兜底键就不是全序 ⇒ 翻页会重复 / 漏行。
	assert.Equal(t, "promotion_plan.plan_no", q.Query.Order[1].ID)
	assert.Equal(t, "asc", q.Query.Order[1].Order)
}

// TestPromotionPlans_SortByPlanNoHasNoDuplicateTieBreaker 锁"按方案编号排时**不重复**给兜底键"。
//
// ⚠️ MSSQL 的 ORDER BY 里同一列出现两次直接报
// "在排序依据列表中多次指定了某一列"(错误 169)⇒ 整条查询 503 `cube_unavailable`。
//
// 症状:按"方案编号"排序时列表**整个打不开**,按其它列排一切正常
// ⇒ 看起来像"编号列坏了",真实原因是兜底键撞了排序列。
func TestPromotionPlans_SortByPlanNoHasNoDuplicateTieBreaker(t *testing.T) {
	src := &fakeSource{src: externalSource()}
	svc, _, reader := newPlanSvc(t, src)

	_, err := svc.List(context.Background(), "00", PromotionPlanQuery{
		Sort: "plan_no", Order: "asc", Page: 1, PageSize: 50,
	})
	require.NoError(t, err)

	q, ok := reader.LastQuery("promotion_plan")
	require.True(t, ok)
	require.Len(t, q.Query.Order, 1, "排序列本身就是 plan_no ⇒ 兜底键必须跳过(否则 MSSQL 169)")
	assert.Equal(t, "promotion_plan.plan_no", q.Query.Order[0].ID)
	assert.Equal(t, "asc", q.Query.Order[0].Order)
}

// TestPromotionPlanLocalOrderExpr_SkipsDuplicateTieBreaker 锁本域 PG 侧的同一件事。
//
// PG 侧重复列不报错,但纯属多余 ⇒ 两边行为必须一致(否则两条路径的分页顺序会分叉)。
func TestPromotionPlanLocalOrderExpr_SkipsDuplicateTieBreaker(t *testing.T) {
	assert.NotContains(t, promotionPlanLocalOrderExpr("plan_no", "asc"), "plan_no ASC,",
		"排序列是 plan_no 时不能重复给兜底键")
	assert.Contains(t, promotionPlanLocalOrderExpr("created_at", "asc"), "plan_no ASC",
		"其它排序列仍要带兜底键(否则并列 ⇒ 不是全序 ⇒ 翻页重复/漏行)")
	assert.Contains(t, promotionPlanLocalOrderExpr("created_at", "desc"), "plan_no ASC")
}

// TestPromotionPlans_DefaultSortIsCreatedAtDesc 锁"默认按制单日期倒序"。
func TestPromotionPlans_DefaultSortIsCreatedAtDesc(t *testing.T) {
	src := &fakeSource{src: externalSource()}
	svc, _, reader := newPlanSvc(t, src)

	_, err := svc.List(context.Background(), "00", PromotionPlanQuery{Page: 1, PageSize: 50})
	require.NoError(t, err)

	q, ok := reader.LastQuery("promotion_plan")
	require.True(t, ok)
	require.NotEmpty(t, q.Query.Order)
	assert.Equal(t, "promotion_plan.created_at", q.Query.Order[0].ID)
	assert.Equal(t, "desc", q.Query.Order[0].Order)
}

// TestPromotionPlans_InvalidSortIsRejected 锁"非法排序字段 ⇒ 400(不是静默回落)"。
//
// 静默回落的后果:用户点了排序、列表没变、**没有任何提示**。
func TestPromotionPlans_InvalidSortIsRejected(t *testing.T) {
	src := &fakeSource{src: externalSource()}
	svc, _, _ := newPlanSvc(t, src)

	_, err := svc.List(context.Background(), "00", PromotionPlanQuery{
		Sort: "plan_no; drop table promotion_plans", Page: 1, PageSize: 50,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrInvalidInput)
}

// TestPromotionPlans_InvalidOrderIsRejected 锁"非法方向 ⇒ 400"。
func TestPromotionPlans_InvalidOrderIsRejected(t *testing.T) {
	src := &fakeSource{src: externalSource()}
	svc, _, _ := newPlanSvc(t, src)

	_, err := svc.List(context.Background(), "00", PromotionPlanQuery{
		Sort: "created_at", Order: "sideways", Page: 1, PageSize: 50,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrInvalidInput)
}

// TestPromotionPlans_LocalSortOrdersRows 锁"本域 PG 路径真的按排序列排"。
func TestPromotionPlans_LocalSortOrdersRows(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db, _ := newPlanSvc(t, src)

	begin := time.Date(2021, 9, 17, 0, 0, 0, 0, time.UTC)
	end := time.Date(2021, 9, 18, 0, 0, 0, 0, time.UTC)
	early := time.Date(2021, 9, 17, 9, 0, 0, 0, time.UTC)
	late := time.Date(2021, 9, 17, 21, 30, 0, 0, time.UTC)
	rows := []any{
		&model.PromotionPlan{PlanNo: "PS002109160001", BranchID: "10", PlanName: "早",
			BeginDate: &begin, EndDate: &end, CreatedAt: &early, CreatedBy: "2001",
			ApproveFlag: "1", ApproveStatus: "draft", MemberLevel: "无"},
		&model.PromotionPlan{PlanNo: "PS002109160002", BranchID: "10", PlanName: "晚",
			BeginDate: &begin, EndDate: &end, CreatedAt: &late, CreatedBy: "2001",
			ApproveFlag: "1", ApproveStatus: "draft", MemberLevel: "无"},
	}
	for _, r := range rows {
		require.NoError(t, db.Create(r).Error, "seed")
	}

	desc, err := svc.List(context.Background(), "10", PromotionPlanQuery{
		Sort: "created_at", Order: "desc", ExcludeExpired: false, Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	require.Len(t, desc.Items, 2)
	assert.Equal(t, "PS002109160002", desc.Items[0].PlanNo, "倒序 ⇒ 新的在前")

	asc, err := svc.List(context.Background(), "10", PromotionPlanQuery{
		Sort: "created_at", Order: "asc", ExcludeExpired: false, Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	require.Len(t, asc.Items, 2)
	assert.Equal(t, "PS002109160001", asc.Items[0].PlanNo, "升序 ⇒ 旧的前")
}

// ---- 两条路径的响应结构必须逐字相同 ----

func TestPromotionPlans_LocalAndCubeHaveSameShape(t *testing.T) {
	cubeSvc, _, _ := newPlanSvc(t, &fakeSource{src: externalSource()})
	cubePage, err := cubeSvc.List(context.Background(), "00", PromotionPlanQuery{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.NotEmpty(t, cubePage.Items)

	localSvc, db, _ := newPlanSvc(t, &fakeSource{src: localSource()})
	seedLocalPlans(t, db)
	localPage, err := localSvc.List(context.Background(), "10", PromotionPlanQuery{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.NotEmpty(t, localPage.Items)

	assert.Equal(t, planKeys(t, cubePage.Items[0]), planKeys(t, localPage.Items[0]),
		"两条路径的方案行字段集合必须一致")
	assert.Equal(t, planKeys(t, *cubePage), planKeys(t, *localPage),
		"两条路径的信封字段集合必须一致")
}

// planKeys 把任意结构体序列化后取键集合(排序)。
func planKeys(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
