// 本文件锁住「会员查询的关键词参数名」这条契约。
//
// # 守的是什么
//
//	服务端读 `?q=`,而 **Flutter POS 客户端发的是 `?keyword=`**
//	(supertrade_pos/lib/infra/http/member_api.dart:60
//	 `query: {'field': field, 'keyword': keyword}`)。
//
//	⇒ 只认 `q` 时,`keyword` 被**静默忽略** ⇒ 过滤条件为空
//	⇒ 端点**返 200、字段齐全** ⇒ 看起来完全正常
//	⇒ 症状:「收银员输卡号查会员,弹出几千张卡」。
//
// # 为什么这类缺陷容易漏
//
//	**端点存在、HTTP 200、响应结构完整、会员对象有全部字段** ——
//	任何只断言"端点在 / 字段对"的契约测试都是绿的。
//	必须**用客户端实际发的那份查询串**打,并断言**命中数**。
//
// # 为什么用**内部测试包**直接挂 handler 方法
//
//	生产上 /members 挂 `rbac.RequireScopeWithBranch("member:view", ...)`,
//	单测里没有 userd ⇒ 一律 403 ⇒ 测不到业务行为。
//	⇒ 与 catalog / pricing 的本地端点测试、member 自己的 export_test.go 同款思路:
//	**业务行为与权限层是两件事**。
//
// # 为什么服务端兼容而不是改前端
//
//	改前端 ⇒ 要重新打包发布 POS 客户端(现场设备),周期长、风险大;
//	改服务端 ⇒ 一次部署,**现场已装的客户端立刻恢复**。
package handler

import (
	"encoding/json"
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
	_ "modernc.org/sqlite"

	"github.com/YunBright/supertrade/internal/member/model"
	"github.com/YunBright/supertrade/internal/member/service"
	"github.com/YunBright/supertrade/pkg/middleware"
)

// newSearchTestEngine 装配一个内存版 member handler,只挂 /members 的 handler 方法。
func newSearchTestEngine(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dsn := filepath.Join(t.TempDir(), "member-search.db")
	db, err := gorm.Open(sqlite.Dialector{DriverName: "sqlite", DSN: dsn}, &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&model.Member{}, &model.MemberType{}), "auto migrate")

	svc := service.New(db)
	svc.SetClock(func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) })

	h := New(svc, nil, nil)
	r := gin.New()
	r.Use(middleware.XBranchID())
	r.GET("/members", h.listMembers)
	return r, db
}

// seedSearchMember 造一个会员(**必须带唯一 op_id**)。
//
// ⚠️ members.op_id 上有**唯一索引** ⇒ 每行都要不同的 op_id,
//   留空会撞 UNIQUE(症状是"第二个会员插不进去")。
func seedSearchMember(t *testing.T, db *gorm.DB, id, branchID string) {
	t.Helper()
	require.NoError(t, db.Create(&model.Member{
		ID: id, BranchID: branchID, CardTypeID: "T1", Name: "会员" + id,
		OpID:               "op-" + branchID + "-" + id,
		Status:             model.MemberStatusNormal,
		PointsAccumulated:  decimal.Zero,
		PointsUsed:         decimal.Zero,
		BalanceYuan:        decimal.Zero,
		ConsumedAmountYuan: decimal.Zero,
		CreatedBy:          "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)
}

// doSearch 打一个 GET /members 请求。
func doSearch(t *testing.T, r *gin.Engine, path, branchID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if branchID != "" {
		req.Header.Set("X-Branch-ID", branchID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// listMembersResp 是 /members 的响应形状。
type listMembersResp struct {
	Members []map[string]any `json:"members"`
	Count   int              `json:"count"`
	Total   int              `json:"total"`
}

// decodeSearchMembers 解 /members 的响应。
func decodeSearchMembers(t *testing.T, w *httptest.ResponseRecorder) listMembersResp {
	t.Helper()
	var out listMembersResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), "body=%s", w.Body.String())
	return out
}

// TestListMembers_KeywordParamIsHonoured 锁"**?keyword= 也要过滤**"。
//
// ⚠️ 断言的是**命中数**,不是"返回了 200" ——
//   过滤条件为空时端点照样返 200 + 完整结构。
func TestListMembers_KeywordParamIsHonoured(t *testing.T) {
	r, db := newSearchTestEngine(t)
	seedSearchMember(t, db, "100109", "10")
	seedSearchMember(t, db, "200208", "10")
	seedSearchMember(t, db, "300307", "10")

	// ① 规范名 ?q=。
	q := doSearch(t, r, "/members?field=card_id&q=100109", "10")
	require.Equal(t, http.StatusOK, q.Code, "q 形态应 200: %s", q.Body.String())
	byQ := decodeSearchMembers(t, q)
	assert.Equal(t, 1, byQ.Total, "?q=100109 应只命中 1 张卡(实际 %d)", byQ.Total)

	// ② **POS 客户端形态** ?keyword= —— 修复前这里 total=3(全部门店)。
	kw := doSearch(t, r, "/members?field=card_id&keyword=100109", "10")
	require.Equal(t, http.StatusOK, kw.Code, "keyword 形态应 200: %s", kw.Body.String())
	byKeyword := decodeSearchMembers(t, kw)
	assert.Equal(t, 1, byKeyword.Total,
		"?keyword=100109 必须命中 1 张卡(实际 %d)—— 过滤被忽略时退化成\"列出全部门店会员\"",
		byKeyword.Total)
	require.Len(t, byKeyword.Members, 1)
	assert.Equal(t, "100109", byKeyword.Members[0]["id"])
}

// TestListMembers_KeywordParamMatchesQ 锁"两个参数名的结果**逐字相同**"。
//
// ⚠️ 两条路径必须等价 ——
//   否则"哪个参数名生效"就变成"看客户端发哪个",而客户端会改版。
func TestListMembers_KeywordParamMatchesQ(t *testing.T) {
	r, db := newSearchTestEngine(t)
	seedSearchMember(t, db, "100109", "10")
	seedSearchMember(t, db, "200208", "10")

	q := doSearch(t, r, "/members?field=card_id&q=200208", "10")
	kw := doSearch(t, r, "/members?field=card_id&keyword=200208", "10")
	require.Equal(t, http.StatusOK, q.Code)
	require.Equal(t, http.StatusOK, kw.Code)
	assert.JSONEq(t, q.Body.String(), kw.Body.String(),
		"?q= 与 ?keyword= 的响应必须逐字相同")
}

// TestListMembers_EmptyKeywordListsAll 锁"关键词为空 ⇒ 列出全部门店"。
//
// ⚠️ 这是**有意的**:POS 上"不输入就浏览全店"是主场景。
//   但它与"传了关键词却被忽略"是**两件事** ——
//   前者是设计,后者是缺陷(过滤条件非空却没生效)。
func TestListMembers_EmptyKeywordListsAll(t *testing.T) {
	r, db := newSearchTestEngine(t)
	seedSearchMember(t, db, "100109", "10")
	seedSearchMember(t, db, "200208", "10")

	resp := doSearch(t, r, "/members?field=card_id", "10")
	require.Equal(t, http.StatusOK, resp.Code)
	out := decodeSearchMembers(t, resp)
	assert.Equal(t, 2, out.Total, "空关键词 ⇒ 列出全部门店会员")
}

// TestListMembers_KeywordNotInThisBranch 锁"过滤不跨店"。
func TestListMembers_KeywordNotInThisBranch(t *testing.T) {
	r, db := newSearchTestEngine(t)
	seedSearchMember(t, db, "100109", "10")
	seedSearchMember(t, db, "100109", "11")

	resp := doSearch(t, r, "/members?field=card_id&keyword=100109", "11")
	require.Equal(t, http.StatusOK, resp.Code)
	out := decodeSearchMembers(t, resp)
	assert.Equal(t, 1, out.Total)
	require.Len(t, out.Members, 1)
	assert.Equal(t, "11", out.Members[0]["branch_id"], "只能看到本店的会员")
}
