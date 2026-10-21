// 本文件锁住**离线价格包**(GET /prices/export)的 HTTP 层行为。
//
// # 与 GET /prices 的差别
//
//	/prices   必须带 product_ids(上限 500,超限**静默截断**)
//	/prices/export  不带 product_ids,给**全店**默认价目表的生效价 + 生效促销
//
// # 守门链说明
//
// 生产上挂 `rbac.RequireScopeWithBranch("pricing:view", ...)`(需要 userd),
// 单测里只挂 handler —— 业务行为与权限层是两件事。
package handler

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/pricing/model"
)

// exportPriceRow 是导出响应里的一行。
type exportPriceRow struct {
	ProductID      string  `json:"product_id"`
	PriceYuan      string  `json:"price_yuan"`
	PromoPriceYuan *string `json:"promo_price_yuan"`
	PromoPlanNo    *string `json:"promo_plan_no"`
	PromoPlanName  *string `json:"promo_plan_name"`
	PromoBeginDay  *string `json:"promo_begin_day"`
	PromoEndDay    *string `json:"promo_end_day"`
}

// exportPriceBody 是导出响应。
type exportPriceBody struct {
	BranchID string           `json:"branch_id"`
	AsOf     string           `json:"as_of"`
	Prices   []exportPriceRow `json:"prices"`
	Count    int              `json:"count"`
	Total    int              `json:"total"`
}

// day 造一个"当天零点"的时间(促销与售价都是日粒度)。
func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

// TestPricesExport_RetailAndPromoAreSeparateFields 锁"零售价与促销价**分开给**"。
//
// ⚠️ 成交价 = min(零售价, 促销价),由消费方决定。
//
//	只给一个"价" ⇒ 消费方要么按原价收钱(多收),要么再查一次。
func TestPricesExport_RetailAndPromoAreSeparateFields(t *testing.T) {
	r, db := localRouterWithDB(t)

	require.NoError(t, db.Create(&model.PriceList{
		ID: "default", BranchID: "10", ProductID: "P1",
		PriceYuan:   decimal.RequireFromString("15.00"),
		EffectiveAt: day(2026, 1, 1), Status: "active",
		CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)
	require.NoError(t, db.Create(&model.Promotion{
		ID: "PS001", BranchID: "10", ProductID: "P1",
		PlanName:  "长期店特不用补差2026.1.1-12.31日",
		PriceYuan: decimal.RequireFromString("9.90"),
		BeginAt:   day(2026, 1, 1), EndAt: day(2026, 12, 31), Status: "active",
		CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)

	w := getJSON(t, r, "/prices/export?on_date=2026-10-21", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body exportPriceBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Prices, 1, "body=%s", w.Body.String())
	assert.Equal(t, "10", body.BranchID)
	assert.Equal(t, "2026-10-21", body.AsOf)
	assert.Equal(t, 1, body.Count)
	assert.Equal(t, 1, body.Total)

	row := body.Prices[0]
	assert.Equal(t, "P1", row.ProductID)
	assert.Equal(t, "15", row.PriceYuan, "零售价")
	require.NotNil(t, row.PromoPriceYuan, "必须带促销价(否则消费方按原价收钱)")
	assert.Equal(t, "9.9", *row.PromoPriceYuan, "促销价")
	require.NotNil(t, row.PromoPlanNo)
	assert.Equal(t, "PS001", *row.PromoPlanNo)
	require.NotNil(t, row.PromoPlanName)
	assert.Equal(t, "长期店特不用补差2026.1.1-12.31日", *row.PromoPlanName)
	require.NotNil(t, row.PromoBeginDay)
	assert.Equal(t, "2026-01-01", *row.PromoBeginDay)
	require.NotNil(t, row.PromoEndDay)
	assert.Equal(t, "2026-12-31", *row.PromoEndDay)
}

// TestPricesExport_AmountsAreStrings 锁"金额是**字符串**"。
//
// ⚠️ JSON number 到了 Dart 侧会被 jsonDecode 变成 double,精度当场丢掉
// (9.90 → 9.899999999999999)⇒ 收银对账差一分钱。
func TestPricesExport_AmountsAreStrings(t *testing.T) {
	r, db := localRouterWithDB(t)

	require.NoError(t, db.Create(&model.PriceList{
		ID: "default", BranchID: "10", ProductID: "P1",
		PriceYuan:   decimal.RequireFromString("15.00"),
		EffectiveAt: day(2026, 1, 1), Status: "active",
		CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)

	w := getJSON(t, r, "/prices/export?on_date=2026-10-21", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var raw struct {
		Prices []map[string]any `json:"prices"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	require.Len(t, raw.Prices, 1)
	assert.IsType(t, "", raw.Prices[0]["price_yuan"], "price_yuan 必须是字符串")
	// ⚠️ 金额**绝不能**是 JSON number(Dart 侧会变 double,精度当场丢掉)。
	//   Go 的 json 把 JSON number 解成 float64 ⇒ 用它判"是不是数字"。
	_, priceIsNumber := raw.Prices[0]["price_yuan"].(float64)
	assert.False(t, priceIsNumber, "price_yuan 不能是 JSON number")
	_, promoIsNumber := raw.Prices[0]["promo_price_yuan"].(float64)
	assert.False(t, promoIsNumber, "promo_price_yuan 不能是 JSON number")
}

// TestPricesExport_NoPromoMeansNull 锁"没有促销时 promo_price_yuan 是 **null**"。
//
// ⚠️ null 与 "0" 必须能区分:促销价 0 元的商品 ≠ 没有促销。
//
// ⚠️ 且**键必须在场**(不是缺省)—— 客户端才能区分"没有促销"
// 与"响应里根本没这个字段"。
func TestPricesExport_NoPromoMeansNull(t *testing.T) {
	r, db := localRouterWithDB(t)

	require.NoError(t, db.Create(&model.PriceList{
		ID: "default", BranchID: "10", ProductID: "P1",
		PriceYuan:   decimal.RequireFromString("9.90"),
		EffectiveAt: day(2026, 1, 1), Status: "active",
		CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)

	w := getJSON(t, r, "/prices/export?on_date=2026-10-21", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body exportPriceBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Prices, 1)
	assert.Nil(t, body.Prices[0].PromoPriceYuan, "没有促销时 promo_price_yuan 应为 null")
	assert.Contains(t, w.Body.String(), `"promo_price_yuan":null`,
		"键必须在场(不能靠 omitempty 省掉)")
}

// TestPricesExport_TakesLowestPromoPrice 锁"同商品多促销 ⇒ 取**最低价**"。
//
// ⚠️ 口径与 ActivePromotions 一致(cube promotion model 的 min_promo_price:
// 「用户实际能买到的最低价」)⇒ 导出与查价不能给出两个价。
func TestPricesExport_TakesLowestPromoPrice(t *testing.T) {
	r, db := localRouterWithDB(t)

	require.NoError(t, db.Create(&model.PriceList{
		ID: "default", BranchID: "10", ProductID: "P1",
		PriceYuan:   decimal.RequireFromString("15.00"),
		EffectiveAt: day(2026, 1, 1), Status: "active",
		CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)
	for _, p := range []struct {
		id    string
		price string
	}{
		{"PS-EXPENSIVE", "12.00"},
		{"PS-CHEAP", "8.80"},
	} {
		require.NoError(t, db.Create(&model.Promotion{
			ID: p.id, BranchID: "10", ProductID: "P1",
			PlanName:  "促销单" + p.id,
			PriceYuan: decimal.RequireFromString(p.price),
			BeginAt:   day(2026, 1, 1), EndAt: day(2026, 12, 31), Status: "active",
			CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}).Error)
	}

	w := getJSON(t, r, "/prices/export?on_date=2026-10-21", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body exportPriceBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Prices, 1)
	require.NotNil(t, body.Prices[0].PromoPriceYuan)
	assert.Equal(t, "8.8", *body.Prices[0].PromoPriceYuan, "必须取最低促销价")
	require.NotNil(t, body.Prices[0].PromoPlanNo)
	assert.Equal(t, "PS-CHEAP", *body.Prices[0].PromoPlanNo, "促销单号要与最低价那条一致")
}

// TestPricesExport_OnDatePicksFuturePromo 锁"on_date 传未来日期 ⇒ 取那一天生效的促销"。
//
// ⚠️ 客户端要能**提前**把未来的促销价存下来,到日子自动生效
// ⇒ 现在查不到未来促销 = 离线包缺了未来几天到期的价。
func TestPricesExport_OnDatePicksFuturePromo(t *testing.T) {
	r, db := localRouterWithDB(t)

	require.NoError(t, db.Create(&model.Promotion{
		ID: "PS-FUTURE", BranchID: "10", ProductID: "P1",
		PlanName:  "圣诞促销",
		PriceYuan: decimal.RequireFromString("7.70"),
		BeginAt:   day(2026, 12, 1), EndAt: day(2026, 12, 31), Status: "active",
		CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)

	// 今天:促销还没开始 ⇒ 没有促销价。
	w := getJSON(t, r, "/prices/export?on_date=2026-10-21", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	var body exportPriceBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Prices, 0, "未生效的促销今天不能出现,body=%s", w.Body.String())

	// 未来某天:促销生效 ⇒ 有促销价。
	w = getJSON(t, r, "/prices/export?on_date=2026-12-15", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	body = exportPriceBody{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Prices, 1, "未来生效的促销必须能被取到,body=%s", w.Body.String())
	require.NotNil(t, body.Prices[0].PromoPriceYuan)
	assert.Equal(t, "7.7", *body.Prices[0].PromoPriceYuan)
	assert.Equal(t, "2026-12-15", body.AsOf)
}

// TestPricesExport_DefaultAsOfIsToday 锁"不传 on_date ⇒ 基准日是**今天**"。
func TestPricesExport_DefaultAsOfIsToday(t *testing.T) {
	r, _ := localRouterWithDB(t)

	w := getJSON(t, r, "/prices/export", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body exportPriceBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	now := time.Now()
	assert.Equal(t, now.Format("2006-01-02"), body.AsOf, "默认基准日必须是今天")
}

// TestPricesExport_PromoWithoutRetailPriceStillAppears 锁"只有促销、没有零售价的行也要给"。
//
// ⚠️ 源库存在这种脏数据(促销挂在没维护零售价的商品上)。
//
//	只给"有零售价的行" ⇒ 客户端永远看不到这条促销
//	⇒ 该商品被当成"无促销"处理。
func TestPricesExport_PromoWithoutRetailPriceStillAppears(t *testing.T) {
	r, db := localRouterWithDB(t)

	require.NoError(t, db.Create(&model.Promotion{
		ID: "PS001", BranchID: "10", ProductID: "P-NO-PRICE",
		PlanName:  "只有促销",
		PriceYuan: decimal.RequireFromString("5.00"),
		BeginAt:   day(2026, 1, 1), EndAt: day(2026, 12, 31), Status: "active",
		CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}).Error)

	w := getJSON(t, r, "/prices/export?on_date=2026-10-21", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body exportPriceBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Prices, 1, "只有促销的行也必须给,body=%s", w.Body.String())
	assert.Equal(t, "P-NO-PRICE", body.Prices[0].ProductID)
	assert.Equal(t, "", body.Prices[0].PriceYuan, "没维护零售价 ⇒ 空串(不是 0)")
	require.NotNil(t, body.Prices[0].PromoPriceYuan)
	assert.Equal(t, "5", *body.Prices[0].PromoPriceYuan)
}

// TestPricesExport_OnlyDefaultListAndOwnBranch 锁"只导**默认价目表** + 本门店的数据"。
func TestPricesExport_OnlyDefaultListAndOwnBranch(t *testing.T) {
	r, db := localRouterWithDB(t)

	for _, p := range []struct {
		listID, branchID, productID string
	}{
		{"default", "10", "P-DEFAULT"},
		{"vip", "10", "P-VIP-LIST"},
		{"default", "01", "P-OTHER-BRANCH"},
	} {
		require.NoError(t, db.Create(&model.PriceList{
			ID: p.listID, BranchID: p.branchID, ProductID: p.productID,
			PriceYuan:   decimal.RequireFromString("15.00"),
			EffectiveAt: day(2026, 1, 1), Status: "active",
			CreatedBy: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}).Error)
	}

	w := getJSON(t, r, "/prices/export?on_date=2026-10-21", "10")
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var body exportPriceBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Prices, 1, "只导 default 价目表 + 本门店,body=%s", w.Body.String())
	assert.Equal(t, "P-DEFAULT", body.Prices[0].ProductID)
}

// TestPricesExport_MissingBranch_400 锁"缺 X-Branch-ID ⇒ 400 branch_required"。
func TestPricesExport_MissingBranch_400(t *testing.T) {
	r, _ := localRouterWithDB(t)

	w := getJSON(t, r, "/prices/export", "")
	require.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	var body struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "branch_required", body.Code)
}

// TestPricesExport_BadOnDate_400 锁"非法 on_date ⇒ 400 bad_on_date"。
//
// ⚠️ 绝不"尽力解析"成一个大概的日期 —— 那会让促销在错误的日期生效,
// 印出低于真价的价签。
func TestPricesExport_BadOnDate_400(t *testing.T) {
	r, _ := localRouterWithDB(t)

	for _, bad := range []string{"2026-13-01", "2026/10/21", "20261021", "abc"} {
		w := getJSON(t, r, "/prices/export?on_date="+bad, "10")
		require.Equal(t, http.StatusBadRequest, w.Code,
			"on_date=%s 必须被拒,body=%s", bad, w.Body.String())
		var body struct {
			Code string `json:"code"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		assert.Equal(t, "bad_on_date", body.Code)
	}
}
