package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/posgateway/service"
)

// fakeResp 是一个下游桩的返回值。
type fakeResp struct {
	body json.RawMessage
	err  error
}

// fakeCaller 是假下游。
//
// ⚠️ 按 "appID path" 精确匹配;带 query 的调用会**再去掉 query** 匹配一次
// (测试里写路径比拼 query 稳)。
type fakeCaller struct {
	responses map[string]fakeResp
	calls     []string
	lastBody  []byte
}

func newFakeCaller() *fakeCaller {
	return &fakeCaller{responses: map[string]fakeResp{}}
}

// stub 注册一个桩。
func (f *fakeCaller) stub(key string, body string) *fakeCaller {
	f.responses[key] = fakeResp{body: json.RawMessage(body)}
	return f
}

// stubErr 注册一个出错的桩。
func (f *fakeCaller) stubErr(key string, err error) *fakeCaller {
	f.responses[key] = fakeResp{err: err}
	return f
}

func (f *fakeCaller) lookup(appID, path string) (fakeResp, bool) {
	if r, ok := f.responses[appID+" "+path]; ok {
		return r, true
	}
	base := appID + " " + path
	if i := indexByte(path, '?'); i >= 0 {
		base = appID + " " + path[:i]
	}
	r, ok := f.responses[base]
	return r, ok
}

func (f *fakeCaller) Get(_ context.Context, appID, path string) (json.RawMessage, error) {
	f.calls = append(f.calls, appID+" "+path)
	r, ok := f.lookup(appID, path)
	if !ok {
		return nil, &service.DownstreamError{
			AppID: appID, Path: path, Status: http.StatusNotFound,
			Code: "not_found", Message: "fake: 没注册这个桩", Err: errors.New("no stub"),
		}
	}
	return r.body, r.err
}

func (f *fakeCaller) Post(_ context.Context, appID, path string, body []byte) (json.RawMessage, error) {
	f.calls = append(f.calls, appID+" "+path)
	f.lastBody = append([]byte(nil), body...)
	r, ok := f.lookup(appID, path)
	if !ok {
		return nil, &service.DownstreamError{
			AppID: appID, Path: path, Status: http.StatusNotFound,
			Code: "not_found", Message: "fake: 没注册这个桩", Err: errors.New("no stub"),
		}
	}
	return r.body, r.err
}

// indexByte 是 strings.IndexByte 的本地小工具(避免为一个调用 import strings)。
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// authErr 造一个"下游 403"。
func authErr(appID, path string) error {
	return &service.DownstreamError{
		AppID: appID, Path: path, Status: http.StatusForbidden,
		Code: "forbidden", Message: "下游拒绝:该门店下没有所需权限", Err: errors.New("forbidden"),
	}
}

// businessErr 造一个"下游业务错误"(503,非鉴权)。
func businessErr(appID, path string) error {
	return &service.DownstreamError{
		AppID: appID, Path: path, Status: http.StatusServiceUnavailable,
		Code: "upstream_unavailable", Message: "下游暂不可用", Err: errors.New("unavailable"),
	}
}

// happyStores / happyShifts / happyEmployees / happyMemberConfig /
// happyPaymentWays 是 /home 用到的一组正常桩。
func happyStores(caller *fakeCaller) *fakeCaller {
	return caller.stub(service.AppMasterData+" /stores", `{"stores":[
		{"branch_id":"B10","code":"S10","name":"云商超市","pos_enabled":true,
		 "member_enabled":true,"counter_nos":"01,02"}
	],"count":1}`)
}

func happyShifts(caller *fakeCaller) *fakeCaller {
	return caller.stub(service.AppMasterData+" /shifts", `{"shifts":[
		{"id":"SH20261021093000123","branch_id":"B10","employee_id":"u-1",
		 "counter_no":"01","opened_at":"2026-10-21T09:30:00+08:00","status":"open"}
	],"count":1}`)
}

func happyEmployees(caller *fakeCaller) *fakeCaller {
	return caller.stub(service.AppMasterData+" /employees", `{"employees":[
		{"id":"u-1","employee_code":"1002","name":"临时","role":"cashier",
		 "status":"active","counter_no":"01","discount_rate":"0.9500"}
	],"count":1}`)
}

func happyMemberConfig(caller *fakeCaller) *fakeCaller {
	return caller.stub(service.AppMember+" /config",
		`{"branch_id":"B10","membership_enabled":true,"points_rate":"1.0000",
		  "points_deduct_rate":"0.0000","points_rounding":"ceiling"}`)
}

func happyPaymentWays(caller *fakeCaller) *fakeCaller {
	return caller.stub(service.AppMember+" /payment-ways", `{"payment_ways":[
		{"code":"RMB","name":"现金","rate":"1.0000","is_integral":false,"sort_order":1},
		{"code":"JFP","name":"积分","rate":"1.0000","is_integral":true,"sort_order":2}
	],"count":2}`)
}

// newHappyCaller 造一组全部正常的桩。
func newHappyCaller() *fakeCaller {
	c := newFakeCaller()
	happyStores(c)
	happyShifts(c)
	happyEmployees(c)
	happyMemberConfig(c)
	happyPaymentWays(c)
	return c
}

func TestService_Home_AggregatesAllSlots(t *testing.T) {
	caller := newHappyCaller()
	svc := service.New(caller, nil)

	view, err := svc.Home(context.Background(), service.HomeInput{
		BranchID:   "B10",
		OperatorID: "u-1",
	})
	require.NoError(t, err)

	require.NotNil(t, view.Store)
	assert.Equal(t, "B10", view.Store.ID)
	assert.Equal(t, "云商超市", view.Store.DisplayName)

	require.NotNil(t, view.Shift)
	assert.Equal(t, "SH20261021093000123", view.Shift.ID)
	assert.Equal(t, "u-1", view.Shift.OperatorID)

	require.NotNil(t, view.Operator)
	assert.Equal(t, "1002", view.Operator.EmployeeCode)
	assert.Equal(t, "临时", view.Operator.Name)
	assert.Equal(t, "0.9500", view.Operator.DiscountRate.String())

	require.NotNil(t, view.MemberEnabled)
	assert.True(t, *view.MemberEnabled)

	require.Len(t, view.PaymentWays, 2)
	assert.Equal(t, "RMB", view.PaymentWays[0].Code)
	assert.Equal(t, "现金", view.PaymentWays[0].Name)

	assert.Empty(t, view.Warnings, "全部下游正常时不该有 warning")
}

func TestService_Home_MemberSettingsDown_DegradesWithWarning(t *testing.T) {
	caller := newHappyCaller()
	caller.stubErr(service.AppMember+" /config", businessErr(service.AppMember, "/config"))
	svc := service.New(caller, nil)

	view, err := svc.Home(context.Background(), service.HomeInput{BranchID: "B10", OperatorID: "u-1"})
	require.NoError(t, err, "会员开关取不到不该让整个聚合失败")

	assert.Nil(t, view.MemberEnabled, "取不到就是 null,不是 false")
	assert.Contains(t, view.Warnings, service.WarnMemberSettingsDown)

	// 其余各项照常返回。
	require.NotNil(t, view.Store)
	assert.Equal(t, "B10", view.Store.ID)
	require.NotNil(t, view.Shift)
	require.NotNil(t, view.Operator)
	assert.Len(t, view.PaymentWays, 2)
}

func TestService_Home_DownstreamAuthError_FailsWholeRequest(t *testing.T) {
	caller := newHappyCaller()
	caller.stubErr(service.AppMember+" /config", authErr(service.AppMember, "/config"))
	svc := service.New(caller, nil)

	view, err := svc.Home(context.Background(), service.HomeInput{BranchID: "B10", OperatorID: "u-1"})
	require.Error(t, err, "下游 403 必须让整个请求失败")
	assert.Nil(t, view)
	assert.True(t, service.IsAuthError(err), "必须是鉴权错误(供 handler 映射成 403)")
}

func TestService_Home_StoreMissing_Fails(t *testing.T) {
	caller := newHappyCaller()
	caller.stub(service.AppMasterData+" /stores", `{"stores":[],"count":0}`)
	svc := service.New(caller, nil)

	_, err := svc.Home(context.Background(), service.HomeInput{BranchID: "B10"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, service.ErrStoreNotFound))
}

func TestService_Home_ShiftDown_Degrades(t *testing.T) {
	caller := newHappyCaller()
	caller.stubErr(service.AppMasterData+" /shifts", businessErr(service.AppMasterData, "/shifts"))
	svc := service.New(caller, nil)

	view, err := svc.Home(context.Background(), service.HomeInput{BranchID: "B10", OperatorID: "u-1"})
	require.NoError(t, err)
	assert.Nil(t, view.Shift)
	assert.Contains(t, view.Warnings, service.WarnShiftUnavailable)
	require.NotNil(t, view.Operator, "班次取不到时收银员退回按登录人找")
}

func TestService_Config_Aggregates(t *testing.T) {
	caller := newHappyCaller()
	caller.stub(service.AppMember+" /types", `{"member_types":[
		{"id":"VIP","name":"储值卡","discount_rate":"0.9500","fee_amount_yuan":"10.0000",
		 "allow_stored_value":true,"allow_points_pay":true,"priority":1,"status":"active"}
	],"count":1}`)
	svc := service.New(caller, nil)

	view, err := svc.Config(context.Background(), "B10")
	require.NoError(t, err)
	require.NotNil(t, view.Store)
	require.NotNil(t, view.MemberEnabled)
	assert.True(t, *view.MemberEnabled)
	require.Len(t, view.MemberTypes, 1)
	assert.Equal(t, "0.9500", view.MemberTypes[0].DiscountRate.String())
	assert.Len(t, view.PaymentWays, 2)
	assert.NotEmpty(t, view.KeypadLayout.Rows, "数字键盘布局必须给")
	assert.Empty(t, view.Warnings)
}

func TestService_Config_MemberTypesDown_Degrades(t *testing.T) {
	caller := newHappyCaller()
	caller.stubErr(service.AppMember+" /types", businessErr(service.AppMember, "/types"))
	svc := service.New(caller, nil)

	view, err := svc.Config(context.Background(), "B10")
	require.NoError(t, err)
	assert.Empty(t, view.MemberTypes)
	assert.Contains(t, view.Warnings, service.WarnMemberTypesDown)
}

func TestService_LastOrder_AggregatesMemberLinesPayments(t *testing.T) {
	caller := newFakeCaller()
	caller.stub(service.AppMember+" /members/M1",
		`{"id":"M1","branch_id":"B10","name":"张三","card_type_id":"VIP","balance_yuan":"88.5000"}`)
	caller.stub(service.AppMember+" /members/M1/last-order",
		`{"member_id":"M1","branch_id":"B10","source":"member_local",
		  "last_order":{"sale_id":"S1","paid_at":"2026-10-20T18:00:00+08:00",
		                "points_earned":"12.0000","operator_id":"u-1"},
		  "sale_lines":[],"payments":[]}`)
	caller.stub(service.AppPOS+" /sales/S1", `{"sale_id":"S1","replayed":false,
		"lines":[{"id":"S1-L1","sale_id":"S1","product_id":"P1","product_name":"可乐",
		          "qty":"2","unit_price_yuan":"3.5000","amount_yuan":"7.0000"}],
		"payments":[{"id":"S1-P1","sale_id":"S1","pay_way":"RMB",
		             "amount_yuan":"7.0000","exchange_rate":"1.0000"}]}`)
	svc := service.New(caller, nil)

	view, err := svc.LastOrder(context.Background(), "B10", "M1")
	require.NoError(t, err)

	var member map[string]any
	require.NoError(t, json.Unmarshal(view.Member, &member))
	assert.Equal(t, "M1", member["id"])
	assert.Equal(t, "张三", member["name"])

	require.NotNil(t, view.LastOrder)
	assert.Equal(t, "S1", view.LastOrder.SaleID)
	assert.Equal(t, "12.0000", view.LastOrder.PointsEarned.String())

	var lines []map[string]any
	require.NoError(t, json.Unmarshal(view.SaleLines, &lines))
	require.Len(t, lines, 1)
	assert.Equal(t, "可乐", lines[0]["product_name"])
	// 金额必须是字符串(下游 pos 的 decimal 默认序列化成字符串)。
	assert.Equal(t, "7.0000", lines[0]["amount_yuan"])

	var pays []map[string]any
	require.NoError(t, json.Unmarshal(view.Payments, &pays))
	require.Len(t, pays, 1)
	assert.Equal(t, "RMB", pays[0]["pay_way"])

	assert.Empty(t, view.Warnings)
}

func TestService_LastOrder_PosDown_Degrades(t *testing.T) {
	caller := newFakeCaller()
	caller.stub(service.AppMember+" /members/M1", `{"id":"M1","name":"张三"}`)
	caller.stub(service.AppMember+" /members/M1/last-order",
		`{"member_id":"M1","last_order":{"sale_id":"S1","paid_at":"2026-10-20T18:00:00+08:00",
		 "points_earned":"12.0000","operator_id":"u-1"}}`)
	caller.stubErr(service.AppPOS+" /sales/S1", businessErr(service.AppPOS, "/sales/S1"))
	svc := service.New(caller, nil)

	view, err := svc.LastOrder(context.Background(), "B10", "M1")
	require.NoError(t, err)
	assert.Equal(t, "[]", string(view.SaleLines), "pos 取不到时 sale_lines 是空数组")
	assert.Equal(t, "[]", string(view.Payments))
	assert.Contains(t, view.Warnings, service.WarnSaleLinesDown)
}

func TestService_LastOrder_MemberNotFound_Is404(t *testing.T) {
	caller := newFakeCaller()
	caller.stubErr(service.AppMember+" /members/M1", &service.DownstreamError{
		AppID: service.AppMember, Path: "/members/M1", Status: http.StatusNotFound,
		Code: "not_found", Message: "会员不存在", Err: errors.New("not found"),
	})
	svc := service.New(caller, nil)

	_, err := svc.LastOrder(context.Background(), "B10", "M1")
	require.Error(t, err)
	assert.True(t, errors.Is(err, service.ErrMemberNotFound))
}

func TestService_LastOrder_DownstreamAuthError_Fails(t *testing.T) {
	caller := newFakeCaller()
	caller.stub(service.AppMember+" /members/M1", `{"id":"M1"}`)
	caller.stubErr(service.AppMember+" /members/M1/last-order", authErr(service.AppMember, "/members/M1/last-order"))
	svc := service.New(caller, nil)

	_, err := svc.LastOrder(context.Background(), "B10", "M1")
	require.Error(t, err)
	assert.True(t, service.IsAuthError(err))
}

func TestService_SearchProducts_ReturnsCandidates(t *testing.T) {
	caller := newFakeCaller()
	caller.stub(service.AppCatalog+" /products/local", `{"products":[
		{"id":"02000110","name":"远航九江双蒸酒","barcode":"6904406383122","sub_code":"JZJ",
		 "unit":"瓶","category_id":"01","category_name":"酒","supplier_id":"S1",
		 "supplier_name":null,"status":"0"},
		{"id":"02000111","name":"远航九江双蒸酒(换购)","barcode":"6904406383122","sub_code":"JZJ",
		 "unit":"瓶","category_id":"01","category_name":"酒","supplier_id":"S1",
		 "supplier_name":null,"status":"0"}
	],"count":2,"page":1,"page_size":20,"total":2}`)
	svc := service.New(caller, nil)

	view, err := svc.SearchProducts(context.Background(), service.ProductSearchInput{
		BranchID: "B10", Keyword: "JZJ", Page: 1, PageSize: 20,
	})
	require.NoError(t, err)
	assert.Equal(t, "JZJ", view.Keyword)
	require.Len(t, view.Candidates, 2, "一个自编码命中多个商品 ⇒ 返回候选列表")
	assert.Equal(t, "02000110", view.Candidates[0].ID)
	assert.Equal(t, "02000111", view.Candidates[1].ID)
	assert.Equal(t, 2, view.Total)
}

func TestService_Prices_AggregatesPriceAndPromotion(t *testing.T) {
	caller := newFakeCaller()
	caller.stub(service.AppPricing+" /prices/local", `{"branch_id":"B10","as_of":"2026-10-21",
		"prices":[{"product_id":"P1","price_yuan":"3.5000","member_price_yuan":"3.0000"}],"count":1}`)
	caller.stub(service.AppPricing+" /promotions/local", `{"branch_id":"B10","as_of":"2026-10-21",
		"promotions":[{"product_id":"P1","plan_no":"PL1","plan_name":"特价","price_yuan":"2.5000",
		 "orig_price_yuan":"3.5000","begin_day":"2026-10-21","end_day":"2026-10-22"}],"count":1}`)
	svc := service.New(caller, nil)

	view, err := svc.Prices(context.Background(), service.PricesInput{
		BranchID: "B10", ProductIDs: []string{"P1"},
	})
	require.NoError(t, err)
	assert.Equal(t, "2026-10-21", view.AsOf)
	require.Len(t, view.Prices, 1)
	assert.Equal(t, "3.5000", view.Prices[0].PriceYuan.String())
	require.NotNil(t, view.Prices[0].MemberPriceYuan)
	assert.Equal(t, "3.0000", view.Prices[0].MemberPriceYuan.String())
	require.Len(t, view.Promotions, 1)
	assert.Equal(t, "2.5000", view.Promotions[0].PriceYuan.String())
	assert.Empty(t, view.Warnings)
}

func TestService_Prices_PromotionsDown_Degrades(t *testing.T) {
	caller := newFakeCaller()
	caller.stub(service.AppPricing+" /prices/local", `{"branch_id":"B10","as_of":"2026-10-21",
		"prices":[{"product_id":"P1","price_yuan":"3.5000"}],"count":1}`)
	caller.stubErr(service.AppPricing+" /promotions/local", businessErr(service.AppPricing, "/promotions/local"))
	svc := service.New(caller, nil)

	view, err := svc.Prices(context.Background(), service.PricesInput{
		BranchID: "B10", ProductIDs: []string{"P1"},
	})
	require.NoError(t, err)
	require.Len(t, view.Prices, 1)
	assert.Empty(t, view.Promotions)
	assert.Contains(t, view.Warnings, service.WarnPromotionsDown)
}

func TestService_Prices_WithoutProductIDs_IsInvalidInput(t *testing.T) {
	svc := service.New(newFakeCaller(), nil)
	_, err := svc.Prices(context.Background(), service.PricesInput{BranchID: "B10"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, service.ErrInvalidInput))
}

func TestService_CreateSale_ForwardsBodyVerbatim(t *testing.T) {
	caller := newFakeCaller()
	caller.stub(service.AppPOS+" /sales", `{"sale_id":"S1","replayed":false}`)
	svc := service.New(caller, nil)

	body := []byte(`{"op_id":"op-uuid-1","branch_id":"B10","lines":[{"product_id":"P1","qty":"2"}],"payments":[{"pay_way":"RMB","amount_yuan":"7.0000"}]}`)
	raw, err := svc.CreateSale(context.Background(), "B10", body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"sale_id":"S1","replayed":false}`, string(raw))

	// op_id 必须**一字不改**地到下游(离线重传靠它幂等)。
	assert.Equal(t, string(body), string(caller.lastBody))
	var forwarded map[string]any
	require.NoError(t, json.Unmarshal(caller.lastBody, &forwarded))
	assert.Equal(t, "op-uuid-1", forwarded["op_id"])
}

func TestService_CreateSale_EmptyBody_IsInvalidInput(t *testing.T) {
	svc := service.New(newFakeCaller(), nil)
	_, err := svc.CreateSale(context.Background(), "B10", nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, service.ErrInvalidInput))
}
