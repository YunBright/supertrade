// 本文件锁住**离线会员包**(GET /members/export)的 HTTP 层行为。
//
// # 为什么是 package handler(而不是 handler_test)
//
// 会员域的既有测试都是 `package handler_test`(外部测试包),
// 而本端点的 handler 方法是**未导出**的(exportMembers),
// 外部测试包拿不到它 ⇒ 只能挂 `RegisterRoutes`。
//
// ⚠️ 但 `RegisterRoutes` 上的读端点挂 `rbac.RequireScopeWithBranch`,
// 单测里没有 userd ⇒ 一律 403 ⇒ 测不到业务行为。
//
// ⇒ 这里用**内部测试包**直接挂 handler 方法(与 catalog / pricing 的
// 本地端点测试同款思路):业务行为与权限层是**两件事**。
//
// # 守门链说明
//
// 生产上这个端点挂 `rbac.RequireScopeWithBranch("member:view", ...)`。
package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite" // 纯 Go 驱动(无需 CGO)

	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/internal/member/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// newExportTestEngine 装配一个内存版 member handler(不接 auth,只测业务与错误信封)。
func newExportTestEngine(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dsn := filepath.Join(t.TempDir(), "member.db")
	db, err := gorm.Open(sqlite.Dialector{DriverName: "sqlite", DSN: dsn}, &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&model.Member{}, &model.MemberType{}), "auto migrate")

	svc := service.New(db)
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) })

	h := New(svc, nil, nil)
	r := gin.New()
	r.Use(middleware.XBranchID())
	// ⚠️ 注册顺序照抄生产:静态段 /members/export 在参数段 /members/:id **之前**
	//   (见 handler.go 的 RegisterRoutes)⇒ 顺带验证 gin 不会 panic。
	r.GET("/members/export", h.exportMembers)
	r.GET("/members/:id", h.getMember)
	return r, db
}

// exportMemberRow 是导出响应里的一行。
type exportMemberRow struct {
	ID                string `json:"id"`
	BranchID          string `json:"branch_id"`
	Name              string `json:"name"`
	CardFaceNo        string `json:"card_face_no"`
	CardTypeID        string `json:"card_type_id"`
	Phone             string `json:"phone"`
	Mobile            string `json:"mobile"`
	Status            string `json:"status"`
	PointsAccumulated string `json:"points_accumulated"`
	PointsUsed        string `json:"points_used"`
	RemainingPoints   string `json:"remaining_points"`
	BalanceYuan       string `json:"balance_yuan"`
	UpdatedAt         string `json:"updated_at"`
}

// exportMemberBody 是导出响应。
type exportMemberBody struct {
	BranchID string            `json:"branch_id"`
	Members  []exportMemberRow `json:"members"`
	Count    int               `json:"count"`
	Total    int               `json:"total"`
}

// doExport 打一个 GET 请求(可带 X-Branch-ID)。
func doExport(t *testing.T, r *gin.Engine, path, branch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if branch != "" {
		req.Header.Set("X-Branch-ID", branch)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// seedExportMember 直接插一行会员(绕开开卡的类别校验)。
func seedExportMember(t *testing.T, db *gorm.DB, id, branchID string, points, used, balance decimal.Decimal) {
	t.Helper()
	require.NoError(t, db.Create(&model.Member{
		ID: id, BranchID: branchID, CardTypeID: "T1", CardFaceNo: "F-" + id,
		// ⚠️ op_id 上有**唯一索引** ⇒ 每行必须给不同的 op_id
		//   (留空会撞 UNIQUE,症状是"第二个会员插不进去")。
		OpID: "op-" + id,
		Name: "会员" + id, Phone: "0771-000", Mobile: "13900000000",
		Status:            model.MemberStatusNormal,
		PointsAccumulated: points, PointsUsed: used,
		BalanceYuan: balance, ConsumedAmountYuan: decimal.Zero,
		CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)
}

// TestMembersExport_ReturnsAllMembers 锁"一次给全本门店会员"。
func TestMembersExport_ReturnsAllMembers(t *testing.T) {
	r, db := newExportTestEngine(t)

	seedExportMember(t, db, "M0001", "10", decimal.NewFromInt(100), decimal.NewFromInt(30), decimal.RequireFromString("12.50"))
	seedExportMember(t, db, "M0002", "10", decimal.Zero, decimal.Zero, decimal.Zero)
	// 别的门店 ⇒ 不能混进来。
	seedExportMember(t, db, "M0003", "01", decimal.NewFromInt(999), decimal.Zero, decimal.Zero)

	w := doExport(t, r, "/members/export", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body exportMemberBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Members, 2, "只能导出本门店的会员,body=%s", w.Body.String())
	assert.Equal(t, "10", body.BranchID)
	assert.Equal(t, 2, body.Count)
	assert.Equal(t, 2, body.Total)
}

// TestMembersExport_RemainingPointsIsFilled 锁"**remaining_points 有值**"。
//
// ⚠️ remaining_points 是**派生**字段(gorm:"-" 不落库),
// 忘了 fillDerived ⇒ 客户端拿到的剩余积分全是 0
// ⇒ 算积分、抵扣积分全错,而且**零报错**。
func TestMembersExport_RemainingPointsIsFilled(t *testing.T) {
	r, db := newExportTestEngine(t)

	seedExportMember(t, db, "M0001", "10",
		decimal.NewFromInt(100), decimal.NewFromInt(30), decimal.RequireFromString("12.50"))

	w := doExport(t, r, "/members/export", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body exportMemberBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Members, 1)
	row := body.Members[0]
	assert.Equal(t, "100", row.PointsAccumulated)
	assert.Equal(t, "30", row.PointsUsed)
	assert.Equal(t, "70", row.RemainingPoints, "剩余积分必须被填上(累计获得 − 累计扣减)")
	assert.Equal(t, "12.5", row.BalanceYuan)
}

// TestMembersExport_AmountsAreStrings 锁"金额/积分是**字符串**"。
//
// ⚠️ JSON number 到了 Dart 侧会被 jsonDecode 变成 double,精度当场丢掉。
func TestMembersExport_AmountsAreStrings(t *testing.T) {
	r, db := newExportTestEngine(t)

	seedExportMember(t, db, "M0001", "10",
		decimal.NewFromInt(100), decimal.Zero, decimal.RequireFromString("12.50"))

	w := doExport(t, r, "/members/export", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var raw struct {
		Members []map[string]any `json:"members"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	require.Len(t, raw.Members, 1)
	for _, key := range []string{
		"points_accumulated", "points_used", "remaining_points", "balance_yuan",
	} {
		assert.IsType(t, "", raw.Members[0][key], "%s 必须是字符串", key)
	}
}

// TestMembersExport_FieldNamesAreExact 锁"**字段名与 model.Member 的 json tag 逐字对齐**"。
//
// ⚠️ 客户端按这些名字建本地表 —— 差一个字符就是"整列读不到",而且**零报错**。
func TestMembersExport_FieldNamesAreExact(t *testing.T) {
	r, db := newExportTestEngine(t)

	seedExportMember(t, db, "M0001", "10", decimal.Zero, decimal.Zero, decimal.Zero)

	w := doExport(t, r, "/members/export", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var raw struct {
		Members []map[string]any `json:"members"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	require.Len(t, raw.Members, 1, "body=%s", w.Body.String())

	want := []string{
		"id", "branch_id", "name", "card_face_no", "card_type_id", "phone", "mobile",
		"status", "points_accumulated", "points_used", "remaining_points",
		"balance_yuan", "updated_at",
	}
	got := make([]string, 0, len(raw.Members[0]))
	for k := range raw.Members[0] {
		got = append(got, k)
	}
	assert.ElementsMatch(t, want, got, "导出行的字段名必须逐字对齐")
}

// TestMembersExport_NotLimitedByMaxListLimit 锁"**绕开 MaxListLimit**"。
//
// ⚠️ 门店 10 导入后有 4128 个会员,而 listMembers 的 limit 上限是 200
// ⇒ 分页拉全量是 21 次往返。
//
// ⇒ 导出必须**一次给全**,不受那个上限约束。
func TestMembersExport_NotLimitedByMaxListLimit(t *testing.T) {
	r, db := newExportTestEngine(t)

	const n = service.MaxListLimit + 50
	for i := 0; i < n; i++ {
		seedExportMember(t, db, fmt.Sprintf("M%04d", i), "10",
			decimal.Zero, decimal.Zero, decimal.Zero)
	}

	w := doExport(t, r, "/members/export", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body exportMemberBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Len(t, body.Members, n, "导出不受 MaxListLimit 限制")
	assert.Equal(t, n, body.Total)
}

// TestMembersExport_MissingBranch_400 锁"缺 X-Branch-ID ⇒ 400 branch_required"。
func TestMembersExport_MissingBranch_400(t *testing.T) {
	r, _ := newExportTestEngine(t)

	w := doExport(t, r, "/members/export", "")
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	var body struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "branch_required", body.Code)
}

// TestMembersExportRouteWinsOverParam 锁"**/members/export 不被 /members/:id 吃掉**"。
//
// gin 的路由树里静态段优先于参数段 —— 但这条依赖 gin 版本,必须实测
// (catalog 的 TestProductsLocalRouteWinsOverParam 是同一条纪律)。
//
// ⚠️ 如果这条挂了,症状是"GET /members/export 返回 404 member_not_found",
// 看起来像"会员没导入",实际是路由被吃掉了 —— 排查方向会完全跑偏。
func TestMembersExportRouteWinsOverParam(t *testing.T) {
	r, db := newExportTestEngine(t)

	seedExportMember(t, db, "M0001", "10", decimal.Zero, decimal.Zero, decimal.Zero)

	w := doExport(t, r, "/members/export", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body exportMemberBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Members, 1, "命中了错误的 handler:%s", w.Body.String())

	// 真正带 :id 的路径仍然按 id 处理。
	w = doExport(t, r, "/members/M0001", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
}
