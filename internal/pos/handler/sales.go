package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/pos/model"
	"github.com/YunBright/supertrade/internal/pos/service"
)

// ---- 请求体 ----

// createSaleReq 是开单(收款)的请求体。
//
// ⚠️ **金额字段一律字符串**("22.20"),不是 JSON number。
type createSaleReq struct {
	// OpID 是幂等键(uuid,必填)。同 op_id 重复提交 ⇒ 返回首次那张单。
	OpID string `json:"op_id"`
	// BranchID 只做**一致性校验**(真源是 X-Branch-ID header)。
	BranchID string `json:"branch_id"`
	// MemberID 是会员号(可空;空 ⇒ 不计积分)。
	MemberID string `json:"member_id"`
	// ShiftID 是班次(可空)。
	ShiftID string `json:"shift_id"`
	// OperatorID 是收银员。
	OperatorID string `json:"operator_id"`
	// AuthorizedBy 是越权授权人(可空)。
	AuthorizedBy string `json:"authorized_by"`
	// Lines 是明细(至少一行)。
	Lines []saleLineReq `json:"lines"`
	// Payments 是收付(至少一条)。
	Payments []paymentReq `json:"payments"`
	// TotalAmountYuan 是应收合计(必须 = Σ 明细 amount_yuan)。
	TotalAmountYuan string `json:"total_amount_yuan"`
	// DiscountAmountYuan 是优惠合计(展示 / 审计)。
	DiscountAmountYuan string `json:"discount_amount_yuan"`
	// PaidAmountYuan 是实收。
	PaidAmountYuan string `json:"paid_amount_yuan"`
	// ChangeAmountYuan 是找零(必须 = 实收 - 合计)。
	ChangeAmountYuan string `json:"change_amount_yuan"`
}

// saleLineReq 是一行明细。
type saleLineReq struct {
	ProductID          string `json:"product_id"`
	ProductName        string `json:"product_name"`
	ProductNo          string `json:"product_no"`
	Barcode            string `json:"barcode"`
	Unit               string `json:"unit"`
	Qty                string `json:"qty"`
	OriginalPriceYuan  string `json:"original_price_yuan"`
	UnitPriceYuan      string `json:"unit_price_yuan"`
	DiscountAmountYuan string `json:"discount_amount_yuan"`
	AmountYuan         string `json:"amount_yuan"`
	IsExchange         bool   `json:"is_exchange"`
	IsVoid             bool   `json:"is_void"`
}

// paymentReq 是一条收付。
type paymentReq struct {
	PayWay              string `json:"pay_way"`
	AmountYuan          string `json:"amount_yuan"`
	ExchangeRate        string `json:"exchange_rate"`
	ConvertedAmountYuan string `json:"converted_amount_yuan"`
	RefFlowNo           string `json:"ref_flow_no"`
}

// refundReq 是退货的请求体。
type refundReq struct {
	// OpID 是幂等键(可空 ⇒ 服务端生成 uuid)。
	OpID string `json:"op_id"`
	// OperatorID 是操作人(可空 ⇒ 用单据上的收银员)。
	OperatorID string `json:"operator_id"`
	// AuthorizedBy 是越权授权人(可空)。
	AuthorizedBy string `json:"authorized_by"`
	// Reason 是退货原因(可空)。
	Reason string `json:"reason"`
	// Lines 是**要退的行子集**(2026-10-22 新增)。
	//
	// ⚠️ **空 ⇒ 整单退**(旧行为);非空 ⇒ 行级部分退货。
	//
	// ⚠️ 数量一律**字符串**(与开单同款)—— JSON number 到 Dart 侧会变 double。
	Lines []refundLineReq `json:"lines"`
}

// refundLineReq 是"要退哪一行、退多少"。
type refundLineReq struct {
	// LineID 是明细行 id(= <sale_id>-L<n>)。
	LineID string `json:"line_id"`
	// Qty 是退多少(可空 ⇒ 该行全部未退数量)。
	Qty string `json:"qty"`
}

// priceOverrideReq 是改价的请求体。
type priceOverrideReq struct {
	// UnitPriceYuan 是新的成交单价(必填,>0)。
	UnitPriceYuan string `json:"unit_price_yuan"`
	// AmountYuan 是新的行金额(可空 ⇒ 按 单价 × 数量 重算)。
	AmountYuan string `json:"amount_yuan"`
	// DiscountAmountYuan 是新的行优惠额(可空 ⇒ 保留原值)。
	DiscountAmountYuan string `json:"discount_amount_yuan"`
	// AuthorizedBy 是越权授权人(可空)。
	//
	// ⚠️ **没有 pos:price-override scope 时,必须带它**才能放行,
	//   并且会写进 sales_orders.authorized_by 做审计。
	AuthorizedBy string `json:"authorized_by"`
}

// ---- 响应体 ----

// saleResp 是单据的响应体(开单 / 查单 / 退货共用)。
type saleResp struct {
	// SaleID 是销售单号。
	SaleID string `json:"sale_id"`
	// Replayed 表示这次是**重放**(同 op_id 重复提交)—— 返回的是首次那张单。
	Replayed bool `json:"replayed"`
	// Order 是单头。
	Order *model.SalesOrder `json:"order"`
	// Lines 是明细。
	Lines []model.SaleLine `json:"lines"`
	// Payments 是收付。
	Payments []model.Payment `json:"payments"`
}

// listResp 是列表的响应体。
type listResp struct {
	Sales    []model.SalesOrder `json:"sales"`
	Count    int                `json:"count"`
	Total    int64              `json:"total"`
	Page     int                `json:"page"`
	PageSize int                `json:"page_size"`
}

// ---- handlers ----

// createSale POST /sales
//
// ⚠️ **开单 + 收款 + 发事件在同一个事务里**(见 service.Create)。
func (h *Handler) createSale(c *gin.Context) {
	branchID, ok := branchFromHeader(c)
	if !ok {
		writeError(c, http.StatusBadRequest, "branch_required", "请求未指定目标 branch_id")
		return
	}
	var req createSaleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// body 的 branch_id 只做一致性校验 —— 真源永远是 header。
	if b := strings.TrimSpace(req.BranchID); b != "" && b != branchID {
		writeError(c, http.StatusBadRequest, "branch_mismatch",
			"body 的 branch_id 与 X-Branch-ID 不一致")
		return
	}
	in, err := req.toInput()
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	in.BranchID = branchID
	in.CreatedBy = callerSub(c)

	order, replayed, err := h.sales.Create(c.Request.Context(), in)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	lines, pays, err := h.sales.Children(c.Request.Context(), order.ID)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, saleResp{
		SaleID:   order.ID,
		Replayed: replayed,
		Order:    order,
		Lines:    lines,
		Payments: pays,
	})
}

// toInput 把请求体转成 service 入参(顺带做金额字符串的严格解析)。
func (r createSaleReq) toInput() (service.CreateSaleInput, error) {
	in := service.CreateSaleInput{
		OpID:         strings.TrimSpace(r.OpID),
		MemberID:     strings.TrimSpace(r.MemberID),
		ShiftID:      strings.TrimSpace(r.ShiftID),
		OperatorID:   strings.TrimSpace(r.OperatorID),
		AuthorizedBy: strings.TrimSpace(r.AuthorizedBy),
	}
	var err error
	if in.TotalAmountYuan, err = parseAmount("total_amount_yuan", r.TotalAmountYuan); err != nil {
		return in, err
	}
	if in.DiscountAmountYuan, err = parseAmount("discount_amount_yuan", r.DiscountAmountYuan); err != nil {
		return in, err
	}
	if in.PaidAmountYuan, err = parseAmount("paid_amount_yuan", r.PaidAmountYuan); err != nil {
		return in, err
	}
	if in.ChangeAmountYuan, err = parseAmount("change_amount_yuan", r.ChangeAmountYuan); err != nil {
		return in, err
	}
	in.Lines = make([]service.SaleLineInput, 0, len(r.Lines))
	for i, l := range r.Lines {
		li := service.SaleLineInput{
			ProductID:   strings.TrimSpace(l.ProductID),
			ProductName: l.ProductName,
			ProductNo:   l.ProductNo,
			Barcode:     l.Barcode,
			Unit:        l.Unit,
			IsExchange:  l.IsExchange,
			IsVoid:      l.IsVoid,
		}
		if li.Qty, err = parseAmount("lines["+strconv.Itoa(i)+"].qty", l.Qty); err != nil {
			return in, err
		}
		if li.OriginalPriceYuan, err = parseAmount("lines["+strconv.Itoa(i)+"].original_price_yuan", l.OriginalPriceYuan); err != nil {
			return in, err
		}
		if li.UnitPriceYuan, err = parseAmount("lines["+strconv.Itoa(i)+"].unit_price_yuan", l.UnitPriceYuan); err != nil {
			return in, err
		}
		if li.DiscountAmountYuan, err = parseAmount("lines["+strconv.Itoa(i)+"].discount_amount_yuan", l.DiscountAmountYuan); err != nil {
			return in, err
		}
		if li.AmountYuan, err = parseAmount("lines["+strconv.Itoa(i)+"].amount_yuan", l.AmountYuan); err != nil {
			return in, err
		}
		in.Lines = append(in.Lines, li)
	}
	in.Payments = make([]service.PaymentInput, 0, len(r.Payments))
	for i, p := range r.Payments {
		pi := service.PaymentInput{
			PayWay:    strings.TrimSpace(p.PayWay),
			RefFlowNo: p.RefFlowNo,
		}
		if pi.AmountYuan, err = parseAmount("payments["+strconv.Itoa(i)+"].amount_yuan", p.AmountYuan); err != nil {
			return in, err
		}
		if pi.ExchangeRate, err = parseAmount("payments["+strconv.Itoa(i)+"].exchange_rate", p.ExchangeRate); err != nil {
			return in, err
		}
		if pi.ExchangeRate.IsZero() {
			pi.ExchangeRate = decimalOne
		}
		if pi.ConvertedAmountYuan, err = parseAmount("payments["+strconv.Itoa(i)+"].converted_amount_yuan", p.ConvertedAmountYuan); err != nil {
			return in, err
		}
		in.Payments = append(in.Payments, pi)
	}
	return in, nil
}

// listSales GET /sales?from=&to=&page=&page_size=
func (h *Handler) listSales(c *gin.Context) {
	branchID, ok := branchFromHeader(c)
	if !ok {
		writeError(c, http.StatusBadRequest, "branch_required", "请求未指定目标 branch_id")
		return
	}
	f := service.ListFilter{
		BranchID: branchID,
		Page:     atoiDefault(c.Query("page"), 1),
		PageSize: atoiDefault(c.Query("page_size"), service.DefaultPageSize),
	}
	if v := strings.TrimSpace(c.Query("from")); v != "" {
		t, err := parseTimeParam("from", v)
		if err != nil {
			writeError(c, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		f.From = &t
	}
	if v := strings.TrimSpace(c.Query("to")); v != "" {
		t, err := parseTimeParam("to", v)
		if err != nil {
			writeError(c, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		f.To = &t
	}
	rows, total, err := h.sales.List(c.Request.Context(), f)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	page, size := service.NormalizePage(f.Page, f.PageSize)
	c.JSON(http.StatusOK, listResp{
		Sales:    rows,
		Count:    len(rows),
		Total:    total,
		Page:     page,
		PageSize: size,
	})
}

// getSale GET /sales/:id
func (h *Handler) getSale(c *gin.Context) {
	branchID, ok := branchFromHeader(c)
	if !ok {
		writeError(c, http.StatusBadRequest, "branch_required", "请求未指定目标 branch_id")
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		writeError(c, http.StatusBadRequest, "bad_request", "id 不能为空")
		return
	}
	order, lines, pays, err := h.sales.Get(c.Request.Context(), branchID, id)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, saleResp{
		SaleID:   order.ID,
		Order:    order,
		Lines:    lines,
		Payments: pays,
	})
}

// refundSale POST /sales/:id/refund
func (h *Handler) refundSale(c *gin.Context) {
	branchID, ok := branchFromHeader(c)
	if !ok {
		writeError(c, http.StatusBadRequest, "branch_required", "请求未指定目标 branch_id")
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		writeError(c, http.StatusBadRequest, "bad_request", "id 不能为空")
		return
	}
	var req refundReq
	// body 可空(全部字段都是可选的)。
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	// 行级部分退货(2026-10-22):把 lines 解析成 service 入参。
	refundLines := make([]service.RefundLineInput, 0, len(req.Lines))
	for i, l := range req.Lines {
		li := service.RefundLineInput{LineID: strings.TrimSpace(l.LineID)}
		if li.LineID == "" {
			writeError(c, http.StatusBadRequest, "bad_request",
				"lines["+strconv.Itoa(i)+"].line_id 不能为空")
			return
		}
		if strings.TrimSpace(l.Qty) != "" {
			qty, err := parseAmount("lines["+strconv.Itoa(i)+"].qty", l.Qty)
			if err != nil {
				writeError(c, http.StatusBadRequest, "bad_request", err.Error())
				return
			}
			li.Qty = qty
		}
		refundLines = append(refundLines, li)
	}
	order, replayed, err := h.sales.Refund(c.Request.Context(), service.RefundInput{
		OpID:         strings.TrimSpace(req.OpID),
		SaleID:       id,
		BranchID:     branchID,
		OperatorID:   strings.TrimSpace(req.OperatorID),
		AuthorizedBy: strings.TrimSpace(req.AuthorizedBy),
		Reason:       req.Reason,
		Lines:        refundLines,
		CreatedBy:    callerSub(c),
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	lines, pays, err := h.sales.Children(c.Request.Context(), order.ID)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, saleResp{
		SaleID:   order.ID,
		Replayed: replayed,
		Order:    order,
		Lines:    lines,
		Payments: pays,
	})
}

// priceOverride POST /sales/:id/lines/:line_id/price-override
//
// ⚠️ **scope 检查在这里做**,不在路由中间件里 ——
//
//	无 pos:price-override 时返回可引导的 403:
//
//	{code:"pos_permission_required",
//	 message:"当前收银员没有改价权限，请输入授权",
//	 required_scope:"pos:price-override"}
//
//	带 authorized_by 时放行,并把授权人写进 sales_orders.authorized_by 做审计。
func (h *Handler) priceOverride(c *gin.Context) {
	branchID, ok := branchFromHeader(c)
	if !ok {
		writeError(c, http.StatusBadRequest, "branch_required", "请求未指定目标 branch_id")
		return
	}
	saleID := strings.TrimSpace(c.Param("id"))
	lineID := strings.TrimSpace(c.Param("line_id"))
	if saleID == "" || lineID == "" {
		writeError(c, http.StatusBadRequest, "bad_request", "id / line_id 不能为空")
		return
	}
	var req priceOverrideReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	unitPrice, err := parseAmount("unit_price_yuan", req.UnitPriceYuan)
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	amount, err := parseAmount("amount_yuan", req.AmountYuan)
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	discount, err := parseAmount("discount_amount_yuan", req.DiscountAmountYuan)
	if err != nil {
		writeError(c, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	authorizedBy := strings.TrimSpace(req.AuthorizedBy)

	has, err := h.hasScope(c, branchID, ScopePriceOverride)
	if err != nil {
		if errors.Is(err, errUnauthenticated) {
			writeError(c, http.StatusUnauthorized, "unauthenticated", "缺少已签 token")
			return
		}
		// 权限服务不可用 ⇒ 拒绝(fail-closed),但**允许带 authorized_by 放行**:
		// 授权人已经由 POS 端校验过,不能因为 userd 抖动就挡住店长改价。
		h.logger.Warn("pos: 改价权限查询失败", "err", err)
		if authorizedBy == "" {
			writeError(c, http.StatusServiceUnavailable, "userd_unavailable", err.Error())
			return
		}
	} else if !has && authorizedBy == "" {
		c.AbortWithStatusJSON(http.StatusForbidden, permRequiredBody{
			Code:          "pos_permission_required",
			Message:       "当前收银员没有改价权限，请输入授权",
			RequiredScope: ScopePriceOverride,
		})
		return
	}

	order, err := h.sales.PriceOverride(c.Request.Context(), service.PriceOverrideInput{
		SaleID:             saleID,
		LineID:             lineID,
		BranchID:           branchID,
		UnitPriceYuan:      unitPrice,
		AmountYuan:         amount,
		DiscountAmountYuan: discount,
		HasDiscountAmount:  strings.TrimSpace(req.DiscountAmountYuan) != "",
		AuthorizedBy:       authorizedBy,
		UpdatedBy:          callerSub(c),
	})
	if err != nil {
		h.mapErr(c, err)
		return
	}
	lines, pays, err := h.sales.Children(c.Request.Context(), order.ID)
	if err != nil {
		h.mapErr(c, err)
		return
	}
	c.JSON(http.StatusOK, saleResp{
		SaleID:   order.ID,
		Order:    order,
		Lines:    lines,
		Payments: pays,
	})
}

// ---- 小工具 ----

// decimalOne 是默认汇率 1。
var decimalOne = decimal.NewFromInt(1)

// atoiDefault 解析 query 里的整数,失败/缺省用 def。
func atoiDefault(s string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return v
}

// parseTimeParam 解析时间参数(支持 RFC3339 与 "2006-01-02 15:04:05")。
func parseTimeParam(name, v string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, nil
		}
	}
	return time.Time{}, &amountParseError{field: name, value: v, err: errBadTimeFormat}
}

// errBadTimeFormat 表示时间参数格式不认识。
var errBadTimeFormat = errors.New("pos: 时间格式不认识")
