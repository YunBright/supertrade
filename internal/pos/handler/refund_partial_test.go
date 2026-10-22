package handler_test

import (
		"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/gin-gonic/gin"

	"github.com/YunBright/supertrade/internal/pos/handler"
	"github.com/YunBright/supertrade/internal/pos/service"
)

// saleBodyTwoLines 是**两行**的开单请求体(金额不同,便于断言"只退了一行")。
const saleBodyTwoLines = `{
  "op_id":"op-two-lines",
  "branch_id":"10",
  "operator_id":"u-1",
  "lines":[
    {"product_id":"P1","product_name":"可乐","product_no":"P1","barcode":"6900000000001","unit":"瓶",
     "qty":"2","original_price_yuan":"12.00","unit_price_yuan":"11.10","discount_amount_yuan":"1.80","amount_yuan":"22.20"},
    {"product_id":"P2","product_name":"酸奶","product_no":"P2","barcode":"6900000000002","unit":"杯",
     "qty":"1","original_price_yuan":"10.00","unit_price_yuan":"8.80","discount_amount_yuan":"0.00","amount_yuan":"8.80"}
  ],
  "payments":[{"pay_way":"RMB","amount_yuan":"31.00","exchange_rate":"1","converted_amount_yuan":"31.00"}],
  "total_amount_yuan":"31.00",
  "discount_amount_yuan":"1.80",
  "paid_amount_yuan":"31.00",
  "change_amount_yuan":"0.00"
}`

// newTwoLineRouter 装一个有两行单的 router。
func newTwoLineRouter(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{
		handler.ScopeSale:   true,
		handler.ScopeRefund: true,
	}))
	created := decodeSale(t, do(t, r, http.MethodPost, "/sales", "10", saleBodyTwoLines))
	return r, created.SaleID
}

// TestRefundSale_PartialLines_OK 锁"**行级部分退货**的端点契约"。
//
// ⚠️ 请求体新增 `lines` —— 不带 = 整单退(旧行为),带 = 只退这些行。
func TestRefundSale_PartialLines_OK(t *testing.T) {
	r, saleID := newTwoLineRouter(t)

	w := do(t, r, http.MethodPost, "/sales/"+saleID+"/refund", "10",
		`{"op_id":"op-refund-1","operator_id":"u-2","lines":[{"line_id":"`+saleID+`-L2"}]}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	got := decodeSale(t, w)
	require.NotNil(t, got.Order)
	assert.Equal(t, service.StatusPartiallyReturned, got.Order.Status,
		"只退了一行 ⇒ 单子还没退完 ⇒ 不能是 returned")
}

// TestRefundSale_PartialLines_ExceedsSold_409 锁"**超退 ⇒ 409**"。
//
// ⚠️ 错误码是**契约**:客户端要靠它区分"再退一次"与"退错了行"。
func TestRefundSale_PartialLines_ExceedsSold_409(t *testing.T) {
	r, saleID := newTwoLineRouter(t)

	w := do(t, r, http.MethodPost, "/sales/"+saleID+"/refund", "10",
		`{"op_id":"op-refund-1","lines":[{"line_id":"`+saleID+`-L1","qty":"3"}]}`)
	require.Equal(t, http.StatusConflict, w.Code, "body=%s", w.Body.String())
	e := decodeErr(t, w)
	assert.Equal(t, "refund_exceeds_sold", e.Code)
}

// TestRefundSale_PartialLines_UnknownLine_404 锁"要退的行不属于该单 ⇒ 404"。
func TestRefundSale_PartialLines_UnknownLine_404(t *testing.T) {
	r, saleID := newTwoLineRouter(t)

	w := do(t, r, http.MethodPost, "/sales/"+saleID+"/refund", "10",
		`{"op_id":"op-refund-1","lines":[{"line_id":"SO-other-L1"}]}`)
	require.Equal(t, http.StatusNotFound, w.Code, "body=%s", w.Body.String())
	e := decodeErr(t, w)
	assert.Equal(t, "sale_line_not_found", e.Code)
}

// TestRefundSale_PartialLines_EmptyQty_IsWholeLine 锁"qty 缺省 = 退该行全部"。
func TestRefundSale_PartialLines_EmptyQty_IsWholeLine(t *testing.T) {
	r, saleID := newTwoLineRouter(t)

	w := do(t, r, http.MethodPost, "/sales/"+saleID+"/refund", "10",
		`{"op_id":"op-refund-1","lines":[{"line_id":"`+saleID+`-L1"}]}`)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	got := decodeSale(t, w)
	require.NotNil(t, got.Order)
	assert.Equal(t, service.StatusPartiallyReturned, got.Order.Status)
}

// TestCreateSale_CreditPayment_ShortPaid_OK 锁"**挂账**(实收不足)的开单契约"。
//
// ⚠️ 以前这条被拒(409 amount_mismatch)⇒ 现实收银里的挂账单**无法表达**。
func TestCreateSale_CreditPayment_ShortPaid_OK(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{handler.ScopeSale: true}))

	body := `{
      "op_id":"op-credit","branch_id":"10","operator_id":"u-1",
      "lines":[{"product_id":"P1","qty":"1","unit_price_yuan":"22.20","amount_yuan":"22.20"}],
      "payments":[
        {"pay_way":"RMB","amount_yuan":"20.00","exchange_rate":"1","converted_amount_yuan":"20.00"},
        {"pay_way":"CRED","amount_yuan":"2.20","exchange_rate":"1","converted_amount_yuan":"2.20"}
      ],
      "total_amount_yuan":"22.20",
      "paid_amount_yuan":"20.00",
      "change_amount_yuan":"-2.20"
    }`
	w := do(t, r, http.MethodPost, "/sales", "10", body)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	got := decodeSale(t, w)
	require.NotNil(t, got.Order)
	assert.Equal(t, "20.00", got.Order.PaidAmountYuan.StringFixed(2), "实收就是实收(不含挂账)")
	assert.Equal(t, "-2.20", got.Order.ChangeAmountYuan.StringFixed(2), "负找零 = 欠款")
}

// TestCreateSale_ShortPaidWithoutCredit_409 锁"实收不足但**没给挂账收付** ⇒ 409"。
func TestCreateSale_ShortPaidWithoutCredit_409(t *testing.T) {
	db := newTestDB(t)
	r := newTestRouter(t, db, fakeResolver(map[string]bool{handler.ScopeSale: true}))

	body := `{
      "op_id":"op-short","branch_id":"10","operator_id":"u-1",
      "lines":[{"product_id":"P1","qty":"1","unit_price_yuan":"22.20","amount_yuan":"22.20"}],
      "payments":[{"pay_way":"RMB","amount_yuan":"20.00","exchange_rate":"1","converted_amount_yuan":"20.00"}],
      "total_amount_yuan":"22.20",
      "paid_amount_yuan":"20.00",
      "change_amount_yuan":"-2.20"
    }`
	w := do(t, r, http.MethodPost, "/sales", "10", body)
	require.Equal(t, http.StatusConflict, w.Code, "body=%s", w.Body.String())
	e := decodeErr(t, w)
	assert.Equal(t, "amount_mismatch", e.Code)
}
