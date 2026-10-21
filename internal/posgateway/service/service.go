package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// BFF 自己的(非下游)错误。
var (
	// ErrInvalidInput 是入参不合法。
	ErrInvalidInput = errors.New("posgateway: 入参不合法")
	// ErrStoreNotFound 是"这家店在 master_data.stores 里没有档案"。
	ErrStoreNotFound = errors.New("posgateway: 门店档案不存在")
	// ErrMemberNotFound 是"这个会员号在本店不存在"。
	ErrMemberNotFound = errors.New("posgateway: 会员不存在")
)

// warning 取不到时写进响应的 warning 码。
//
// ⚠️ 这些码是**契约**:前端按它们决定"这一项没有"还是"这一项不知道"。
const (
	WarnShiftUnavailable    = "shift_unavailable"
	WarnOperatorUnavailable = "operator_unavailable"
	WarnOperatorNotFound    = "operator_not_found"
	WarnMemberSettingsDown  = "member_settings_unavailable"
	WarnPaymentWaysDown     = "payment_ways_unavailable"
	WarnMemberTypesDown     = "member_types_unavailable"
	WarnLastOrderDown       = "last_order_unavailable"
	WarnSaleLinesDown       = "sale_lines_unavailable"
	WarnPromotionsDown      = "promotions_unavailable"
)

// ---- 视图(给前端的那一面) ----

// StoreView 是门店(来自 master-data 的 GET /stores)。
type StoreView struct {
	// ID 是门店 id(= branch_id)。
	ID string `json:"id"`
	// Code 是门店业务码。
	Code string `json:"code"`
	// DisplayName 是门店名(POS 顶部显示的那个名字)。
	DisplayName string `json:"display_name"`
	// PosEnabled 是"是否启用云商POS"(镜像自 auth.branches.pos_enabled)。
	PosEnabled bool `json:"pos_enabled"`
	// MemberEnabled 是**门店档案**里的会员开关。
	//
	// ⚠️ 它与 member.member_settings.membership_enabled 是**两处**开关(正交):
	//   前者是"这家店做不做会员",后者是"这家店的会员参数"。
	//   `/home` 的 member_enabled 取的是**后者**(见 HomeView.MemberEnabled)。
	MemberEnabled bool `json:"member_enabled"`
	// CounterNos 是收银台编号列表(逗号分隔)。
	CounterNos string `json:"counter_nos"`
}

// ShiftView 是当前班次(来自 master-data 的 GET /shifts)。
type ShiftView struct {
	// ID 是班次 id(SH<yyyymmdd><seq>)。
	ID string `json:"id"`
	// OpenedAt 是开班时刻(RFC3339)。
	OpenedAt string `json:"opened_at"`
	// OperatorID 是开班收银员(auth user id)。
	OperatorID string `json:"operator_id"`
	// CounterNo 是收银台号。
	CounterNo string `json:"counter_no"`
}

// OperatorView 是当前收银员(来自 master-data 的 GET /employees)。
type OperatorView struct {
	// EmployeeCode 是工号。
	EmployeeCode string `json:"employee_code"`
	// Name 是姓名。
	Name string `json:"name"`
	// Role 是角色(cashier / supervisor / manager)。
	Role string `json:"role"`
	// CounterNo 是默认收银台号。
	CounterNo string `json:"counter_no"`
	// DiscountRate 是折扣权限(字符串,例 "0.9500")。
	//
	// ⚠️ 是字符串不是 number:0.95 到 Dart 侧会变 0.9499999999999996。
	DiscountRate AmountString `json:"discount_rate"`
}

// PaymentWayView 是付款方式字典项(来自 member 的 GET /payment-ways)。
type PaymentWayView struct {
	// Code 是付款方式码(RMB / SWX / SZF / JFP / EKT …)。
	Code string `json:"code"`
	// Name 是付款方式名。
	Name string `json:"name"`
	// Rate 是汇率(字符串,1 = 1:1)。
	Rate AmountString `json:"rate"`
	// IsIntegral 是"是否积分支付"。
	IsIntegral bool `json:"is_integral"`
	// SortOrder 是排序权重(小的在前)。
	SortOrder int `json:"sort_order"`
}

// MemberTypeView 是会员类别(来自 member 的 GET /types)。
type MemberTypeView struct {
	// ID 是类别 id。
	ID string `json:"id"`
	// Name 是类别名。
	Name string `json:"name"`
	// DiscountRate 是会员折扣(字符串,0.95 = 95 折)。
	DiscountRate AmountString `json:"discount_rate"`
	// FeeAmountYuan 是会员费(字符串)。
	FeeAmountYuan AmountString `json:"fee_amount_yuan"`
	// AllowStoredValue 是"是否储值卡"。
	AllowStoredValue bool `json:"allow_stored_value"`
	// AllowPointsPay 是"是否可用积分抵扣"。
	AllowPointsPay bool `json:"allow_points_pay"`
	// Priority 是排序权重。
	Priority int `json:"priority"`
	// Status 是 active / inactive。
	Status string `json:"status"`
}

// KeypadLayout 是数字键盘布局。
type KeypadLayout struct {
	// Version 是布局版本(前端据此决定要不要刷新缓存)。
	Version int `json:"version"`
	// Rows 是按键行,每行从左到右。
	Rows [][]string `json:"rows"`
}

// DefaultKeypadLayout 是数字键盘布局的**后端默认值**。
//
// ⚠️ 真源按设计文档 §8 在 **POS 本地**("界面布局、菜单顺序、是否显示某功能"归 POS 本地)。
// 这里给一份后端默认值,只为让客户端首屏不用**再硬编码一份** ——
// 两份枚举必然漂移,而漂移的表现是"某个按键打出来是空白",极难发现。
func DefaultKeypadLayout() KeypadLayout {
	return KeypadLayout{
		Version: 1,
		Rows: [][]string{
			{"7", "8", "9", "del"},
			{"4", "5", "6", "clear"},
			{"1", "2", "3", "enter"},
			{"0", "00", "."},
		},
	}
}

// HomeView 是 GET /home 的响应体。
type HomeView struct {
	// Store 是门店信息。
	Store *StoreView `json:"store"`
	// Shift 是当前班次;nil = 没有未交班的班次(或取不到)。
	Shift *ShiftView `json:"shift"`
	// Operator 是当前收银员;nil = 没找到(或取不到)。
	Operator *OperatorView `json:"operator"`
	// MemberEnabled 是会员开关(member.member_settings.membership_enabled)。
	//
	// ⚠️ 指针:nil = **取不到**(见 warnings),与"false = 这家店不开会员"是两件事。
	MemberEnabled *bool `json:"member_enabled"`
	// PaymentWays 是付款方式字典(取不到时是空数组)。
	PaymentWays []PaymentWayView `json:"payment_ways"`
	// Warnings 是"哪几项没取到"的点名清单。
	Warnings []string `json:"warnings"`
}

// ConfigView 是 GET /config 的响应体。
type ConfigView struct {
	// Store 是门店信息。
	Store *StoreView `json:"store"`
	// MemberEnabled 是会员开关(nil = 取不到)。
	MemberEnabled *bool `json:"member_enabled"`
	// MemberTypes 是会员类别(取不到时是空数组)。
	MemberTypes []MemberTypeView `json:"member_types"`
	// PaymentWays 是付款方式字典(取不到时是空数组)。
	PaymentWays []PaymentWayView `json:"payment_ways"`
	// KeypadLayout 是数字键盘布局。
	KeypadLayout KeypadLayout `json:"keypad_layout"`
	// Warnings 是"哪几项没取到"的点名清单。
	Warnings []string `json:"warnings"`
}

// LastOrderSummary 是上一单摘要(来自 member 的 GET /members/{id}/last-order)。
type LastOrderSummary struct {
	// SaleID 是销售单号。
	SaleID string `json:"sale_id"`
	// PaidAt 是收款时刻(RFC3339)。
	PaidAt string `json:"paid_at"`
	// PointsEarned 是这一单累计的积分(字符串)。
	PointsEarned AmountString `json:"points_earned"`
	// OperatorID 是收银员。
	OperatorID string `json:"operator_id"`
}

// LastOrderView 是 GET /members/{id}/last-order 的响应体(图7「上一单」一次返回)。
//
// ⚠️ member / sale_lines / payments 三块都是**原样透传**下游的 JSON,
//
//	不在本服务重新建模 —— 单一真源,零漂移。
//	字段名 = 各下游服务的 JSON tag(member 的 model.Member /
//	pos 的 model.SaleLine / model.Payment)。
type LastOrderView struct {
	// Member 是会员档案(member 服务的 GET /members/{id} 原始 JSON)。
	Member json.RawMessage `json:"member"`
	// LastOrder 是会员域掌握的上一单凭证(nil = 该会员还没有会员消费)。
	LastOrder *LastOrderSummary `json:"last_order"`
	// SaleLines 是上一单的商品明细(pos 服务的 GET /sales/{id} 原始 JSON 数组)。
	SaleLines json.RawMessage `json:"sale_lines"`
	// Payments 是上一单的收付流水(pos 服务的 GET /sales/{id} 原始 JSON 数组)。
	Payments json.RawMessage `json:"payments"`
	// Warnings 是"哪几块没取到"的点名清单。
	Warnings []string `json:"warnings"`
}

// ProductCandidate 是一个候选商品。
//
// ⚠️ **不含 price_yuan**:catalog 的商品价是 *float64(catalog service 的历史契约),
//
//	而本仓的硬约束是"金额绝不用 double"。⇒ 商品价走 `/prices`,这里只给身份字段。
type ProductCandidate struct {
	// ID 是货号(唯一键)。
	ID string `json:"id"`
	// Name 是商品名。
	Name string `json:"name"`
	// Barcode 是条码。
	Barcode string `json:"barcode"`
	// SubCode 是自编码 / 助记码。
	//
	// ⚠️ **不唯一**:少数自编码对应 2 个货号(正常品 + 换购品)
	//   ⇒ 按自编码搜会命中**多个候选**,前端要列候选而不是只取第一条。
	SubCode string `json:"sub_code"`
	// Unit 是单位(nil = 没维护)。
	Unit *string `json:"unit"`
	// CategoryID / CategoryName 是分类(nil 名 = 没查到名,不编造占位名)。
	CategoryID   string  `json:"category_id"`
	CategoryName *string `json:"category_name"`
	// SupplierID / SupplierName 是供应商(nil 名 = 没查到名)。
	SupplierID   string  `json:"supplier_id"`
	SupplierName *string `json:"supplier_name"`
	// Status 是商品状态码(原样透传,判据交给前端)。
	Status string `json:"status"`
}

// ProductSearchView 是 GET /products/search 的响应体。
type ProductSearchView struct {
	// Keyword 是回显的搜索词。
	Keyword string `json:"keyword"`
	// Candidates 是候选商品(**一个自编码可能命中多个**)。
	Candidates []ProductCandidate `json:"candidates"`
	// Count 是本页条数。
	Count int `json:"count"`
	// Total 是过滤后的总条数。
	Total int `json:"total"`
	// Warnings 是"哪几块没取到"的点名清单。
	Warnings []string `json:"warnings"`
}

// LocalPrice 是一行售价(来自 pricing 的 GET /prices)。
type LocalPrice struct {
	// ProductID 是商品 id。
	ProductID string `json:"product_id"`
	// PriceYuan 是零售价(字符串)。
	PriceYuan AmountString `json:"price_yuan"`
	// MemberPriceYuan 是会员价;nil = **没有会员价**(不是"会员价 0 元")。
	MemberPriceYuan *AmountString `json:"member_price_yuan"`
	// CostYuan 是成本价;nil = 不知道成本。
	CostYuan *AmountString `json:"cost_yuan"`
}

// LocalPromotion 是一行当前生效促销(来自 pricing 的 GET /promotions)。
type LocalPromotion struct {
	// ProductID 是商品 id。
	ProductID string `json:"product_id"`
	// PlanNo 是促销方案号。
	PlanNo string `json:"plan_no"`
	// PlanName 是促销方案名。
	PlanName string `json:"plan_name"`
	// PriceYuan 是促销价(字符串)。
	PriceYuan AmountString `json:"price_yuan"`
	// OrigPriceYuan 是原价;nil = 没有原价。
	OrigPriceYuan *AmountString `json:"orig_price_yuan"`
	// BeginDay / EndDay 是促销起止日(yyyy-mm-dd)。
	BeginDay string `json:"begin_day"`
	EndDay   string `json:"end_day"`
}

// PricesView 是 GET /prices 的响应体。
type PricesView struct {
	// BranchID 是门店。
	BranchID string `json:"branch_id"`
	// AsOf 是价格的基准日(yyyy-mm-dd)。
	AsOf string `json:"as_of"`
	// Prices 是本地售价(取不到时是空数组 + warning)。
	Prices []LocalPrice `json:"prices"`
	// Promotions 是当前生效促销(取不到时是空数组 + warning)。
	Promotions []LocalPromotion `json:"promotions"`
	// Warnings 是"哪几块没取到"的点名清单。
	Warnings []string `json:"warnings"`
}

// ---- 下游响应体(私有) ----

type masterDataStoresResp struct {
	Stores []struct {
		BranchID      string `json:"branch_id"`
		Code          string `json:"code"`
		Name          string `json:"name"`
		PosEnabled    bool   `json:"pos_enabled"`
		MemberEnabled bool   `json:"member_enabled"`
		CounterNos    string `json:"counter_nos"`
	} `json:"stores"`
}

type masterDataShiftsResp struct {
	Shifts []struct {
		ID         string `json:"id"`
		BranchID   string `json:"branch_id"`
		EmployeeID string `json:"employee_id"`
		CounterNo  string `json:"counter_no"`
		OpenedAt   string `json:"opened_at"`
		Status     string `json:"status"`
	} `json:"shifts"`
}

type masterDataEmployeesResp struct {
	Employees []struct {
		ID           string       `json:"id"`
		EmployeeCode string       `json:"employee_code"`
		Name         string       `json:"name"`
		Role         string       `json:"role"`
		Status       string       `json:"status"`
		CounterNo    string       `json:"counter_no"`
		DiscountRate AmountString `json:"discount_rate"`
	} `json:"employees"`
}

type memberSettingsResp struct {
	BranchID          string       `json:"branch_id"`
	MembershipEnabled bool         `json:"membership_enabled"`
	PointsRate        AmountString `json:"points_rate"`
	PointsDeductRate  AmountString `json:"points_deduct_rate"`
	PointsRounding    string       `json:"points_rounding"`
	PasswordRequired  bool         `json:"password_required"`
	FaceRequired      bool         `json:"face_required"`
	MaxFacePerMember  int          `json:"max_face_per_member"`
}

type memberTypesResp struct {
	MemberTypes []struct {
		ID               string       `json:"id"`
		Name             string       `json:"name"`
		DiscountRate     AmountString `json:"discount_rate"`
		FeeAmountYuan    AmountString `json:"fee_amount_yuan"`
		AllowStoredValue bool         `json:"allow_stored_value"`
		AllowPointsPay   bool         `json:"allow_points_pay"`
		Priority         int          `json:"priority"`
		Status           string       `json:"status"`
	} `json:"member_types"`
}

type memberPaymentWaysResp struct {
	PaymentWays []struct {
		Code       string       `json:"code"`
		Name       string       `json:"name"`
		Rate       AmountString `json:"rate"`
		IsIntegral bool         `json:"is_integral"`
		SortOrder  int          `json:"sort_order"`
	} `json:"payment_ways"`
}

type memberLastOrderResp struct {
	LastOrder *struct {
		SaleID       string       `json:"sale_id"`
		PaidAt       string       `json:"paid_at"`
		PointsEarned AmountString `json:"points_earned"`
		OperatorID   string       `json:"operator_id"`
	} `json:"last_order"`
}

type posSaleResp struct {
	SaleID   string          `json:"sale_id"`
	Lines    json.RawMessage `json:"lines"`
	Payments json.RawMessage `json:"payments"`
}

type catalogProductPage struct {
	Products []ProductCandidate `json:"products"`
	Count    int                `json:"count"`
	Total    int                `json:"total"`
}

type pricingPricesResp struct {
	BranchID string       `json:"branch_id"`
	AsOf     string       `json:"as_of"`
	Prices   []LocalPrice `json:"prices"`
}

type pricingPromotionsResp struct {
	BranchID   string           `json:"branch_id"`
	AsOf       string           `json:"as_of"`
	Promotions []LocalPromotion `json:"promotions"`
}

// ---- Service ----

// Service 是 BFF 的聚合服务。
type Service struct {
	caller Caller
	logger *slog.Logger
}

// New 构造 Service。
func New(caller Caller, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{caller: caller, logger: logger}
}

// HomeInput 是 GET /home 的入参。
type HomeInput struct {
	// BranchID 是当前门店(X-Branch-ID)。
	BranchID string
	// OperatorID 是当前登录人的 auth user id(JWT sub)。
	OperatorID string
	// CounterNo 是收银台号(可选;用于在多个未交班里挑一个)。
	CounterNo string
}

// Home 聚合收银主页。
//
// # 降级语义
//
//   - **主数据**(门店):取不到 ⇒ 整个请求失败 —— 一个没有门店的首页没有意义。
//   - 其余各项:取不到 ⇒ 该项返 null + 记一条 warning,其余照常返回。
//   - **任何一项返回 401 / 403** ⇒ 整个请求失败(不可降级)。
func (s *Service) Home(ctx context.Context, in HomeInput) (*HomeView, error) {
	view := &HomeView{
		PaymentWays: []PaymentWayView{},
		Warnings:    []string{},
	}

	// ① 门店 —— 主数据。
	store, err := s.fetchStore(ctx, in.BranchID)
	if err != nil {
		return nil, err
	}
	view.Store = store

	// ② 班次 —— 可降级。
	shift, shiftRow, err := s.fetchShift(ctx, in.BranchID, in.CounterNo)
	if err != nil {
		if IsAuthError(err) {
			return nil, err
		}
		view.Warnings = append(view.Warnings, WarnShiftUnavailable)
		s.logger.Warn("posgateway: 班次取不到,降级", "err", err, "branch_id", in.BranchID)
	}
	view.Shift = shift

	// ③ 收银员 —— 可降级。
	operator, err := s.fetchOperator(ctx, in.BranchID, shiftRow, in.OperatorID)
	if err != nil {
		if IsAuthError(err) {
			return nil, err
		}
		view.Warnings = append(view.Warnings, WarnOperatorUnavailable)
		s.logger.Warn("posgateway: 收银员取不到,降级", "err", err, "branch_id", in.BranchID)
	} else if operator == nil {
		view.Warnings = append(view.Warnings, WarnOperatorNotFound)
	}
	view.Operator = operator

	// ④ 会员开关 —— 可降级(这一项取不到就返 null)。
	enabled, err := s.fetchMemberEnabled(ctx)
	if err != nil {
		if IsAuthError(err) {
			return nil, err
		}
		view.Warnings = append(view.Warnings, WarnMemberSettingsDown)
		s.logger.Warn("posgateway: 会员开关取不到,降级", "err", err, "branch_id", in.BranchID)
	}
	view.MemberEnabled = enabled

	// ⑤ 付款方式 —— 可降级。
	ways, err := s.fetchPaymentWays(ctx)
	if err != nil {
		if IsAuthError(err) {
			return nil, err
		}
		view.Warnings = append(view.Warnings, WarnPaymentWaysDown)
		s.logger.Warn("posgateway: 付款方式取不到,降级", "err", err, "branch_id", in.BranchID)
	} else {
		view.PaymentWays = ways
	}
	return view, nil
}

// Config 聚合 POS 端配置。
//
// 降级语义与 [Service.Home] 相同:门店是主数据,其余可降级,鉴权失败整体失败。
func (s *Service) Config(ctx context.Context, branchID string) (*ConfigView, error) {
	view := &ConfigView{
		MemberTypes: []MemberTypeView{},
		PaymentWays: []PaymentWayView{},
		Warnings:    []string{},
	}

	store, err := s.fetchStore(ctx, branchID)
	if err != nil {
		return nil, err
	}
	view.Store = store

	enabled, err := s.fetchMemberEnabled(ctx)
	if err != nil {
		if IsAuthError(err) {
			return nil, err
		}
		view.Warnings = append(view.Warnings, WarnMemberSettingsDown)
		s.logger.Warn("posgateway: 会员开关取不到,降级", "err", err, "branch_id", branchID)
	}
	view.MemberEnabled = enabled

	types, err := s.fetchMemberTypes(ctx)
	if err != nil {
		if IsAuthError(err) {
			return nil, err
		}
		view.Warnings = append(view.Warnings, WarnMemberTypesDown)
		s.logger.Warn("posgateway: 会员类别取不到,降级", "err", err, "branch_id", branchID)
	} else {
		view.MemberTypes = types
	}

	ways, err := s.fetchPaymentWays(ctx)
	if err != nil {
		if IsAuthError(err) {
			return nil, err
		}
		view.Warnings = append(view.Warnings, WarnPaymentWaysDown)
		s.logger.Warn("posgateway: 付款方式取不到,降级", "err", err, "branch_id", branchID)
	} else {
		view.PaymentWays = ways
	}

	view.KeypadLayout = DefaultKeypadLayout()
	return view, nil
}

// LastOrder 聚合图7「上一单」:会员档案 + 上一单的行与收付。
//
// # 降级语义
//
//   - 会员档案:查不到 ⇒ 404(会员不存在);其它错误 ⇒ 整个请求失败。
//   - 会员域的上一单凭证:取不到 ⇒ last_order = null + warning。
//   - pos 的行与收付:取不到 ⇒ sale_lines / payments 为空数组 + warning。
//   - **任何一项返回 401 / 403** ⇒ 整个请求失败。
func (s *Service) LastOrder(ctx context.Context, branchID, memberID string) (*LastOrderView, error) {
	memberID = strings.TrimSpace(memberID)
	if memberID == "" {
		return nil, fmt.Errorf("%w: member_id 必填", ErrInvalidInput)
	}
	view := &LastOrderView{
		SaleLines: json.RawMessage("[]"),
		Payments:  json.RawMessage("[]"),
		Warnings:  []string{},
	}

	// ① 会员档案 —— 主数据。
	memberRaw, err := s.caller.Get(ctx, AppMember, "/members/"+url.PathEscape(memberID))
	if err != nil {
		if de := AsDownstreamError(err); de != nil && de.Status == http.StatusNotFound {
			return nil, fmt.Errorf("%w: member_id=%s", ErrMemberNotFound, memberID)
		}
		return nil, err
	}
	view.Member = memberRaw

	// ② 会员域的上一单凭证 —— 可降级。
	lastRaw, err := s.caller.Get(ctx, AppMember, "/members/"+url.PathEscape(memberID)+"/last-order")
	if err != nil {
		if IsAuthError(err) {
			return nil, err
		}
		view.Warnings = append(view.Warnings, WarnLastOrderDown)
		s.logger.Warn("posgateway: 上一单凭证取不到,降级", "err", err, "member_id", memberID)
	} else {
		var lo memberLastOrderResp
		if err := json.Unmarshal(lastRaw, &lo); err != nil {
			view.Warnings = append(view.Warnings, WarnLastOrderDown)
			s.logger.Warn("posgateway: 上一单凭证解析失败,降级", "err", err, "member_id", memberID)
		} else if lo.LastOrder != nil {
			view.LastOrder = &LastOrderSummary{
				SaleID:       lo.LastOrder.SaleID,
				PaidAt:       lo.LastOrder.PaidAt,
				PointsEarned: lo.LastOrder.PointsEarned,
				OperatorID:   lo.LastOrder.OperatorID,
			}
		}
	}

	// ③ 上一单的行与收付(pos)—— 可降级。
	saleID := ""
	if view.LastOrder != nil {
		saleID = view.LastOrder.SaleID
	}
	if saleID == "" {
		return view, nil
	}
	saleRaw, err := s.caller.Get(ctx, AppPOS, "/sales/"+url.PathEscape(saleID))
	if err != nil {
		if IsAuthError(err) {
			return nil, err
		}
		view.Warnings = append(view.Warnings, WarnSaleLinesDown)
		s.logger.Warn("posgateway: 上一单明细取不到,降级", "err", err, "sale_id", saleID)
		return view, nil
	}
	var sale posSaleResp
	if err := json.Unmarshal(saleRaw, &sale); err != nil {
		view.Warnings = append(view.Warnings, WarnSaleLinesDown)
		s.logger.Warn("posgateway: 上一单明细解析失败,降级", "err", err, "sale_id", saleID)
		return view, nil
	}
	if len(sale.Lines) > 0 {
		view.SaleLines = sale.Lines
	}
	if len(sale.Payments) > 0 {
		view.Payments = sale.Payments
	}
	return view, nil
}

// ProductSearchInput 是 GET /products/search 的入参。
type ProductSearchInput struct {
	// BranchID 是当前门店。
	BranchID string
	// Keyword 是搜索词(命中 货号 / 自编码 / 名称 / 条码)。
	Keyword string
	// Page / PageSize 是分页(<=0 用下游默认值)。
	Page     int
	PageSize int
}

// SearchProducts 代理 catalog 的商品搜索。
//
// # ⚠️ 调的是 catalog 的 **`/products/search`**(商品选择控件那条,已按数据源分流)
//
// catalog 的读端点按「门店配没配数据源」分流(判据只有这一套):
//
//	配了源 ⇒ 走那个 cube 实例(商品档案在外部系统里)
//	没配源 ⇒ 走本域 PG(自营门店 10)
//
// ⚠️ 2026-10-22:这里原本调的是 catalog 的 `/products/local`——
// 那条端点**只读本域 PG**,名字承诺"本地"、数据却是另一套
// ⇒ 同一个门店在 BFF 与后台管理页会看到**两个不同的商品列表**
// (门店 00 的本地副本已被清空 ⇒ BFF 搜出来是空的,而后台管理页是全的)。
// ⇒ 那条端点已删除,统一走 `/products/search`。
//
// ⚠️ catalog 的搜索本身命中 **货号 / 自编码 / 名称 / 条码** 四列
// (思迅的搜索框是「输入编码、名称或助记码(Q)」,而"编码"是**两个**东西:
// 货号唯一 / 自编码不唯一)⇒ 四者命中在下游已经成立,本服务只做代理 + 收敛字段名。
//
// ⚠️ **一个自编码可能命中多个商品**(思迅实测 9 组「正常品 + 换购品」共用自编码)
// ⇒ 返回候选列表,前端要能选,不能只取第一条。
func (s *Service) SearchProducts(ctx context.Context, in ProductSearchInput) (*ProductSearchView, error) {
	q := url.Values{}
	q.Set("q", in.Keyword)
	if in.Page > 0 {
		q.Set("page", strconv.Itoa(in.Page))
	}
	if in.PageSize > 0 {
		q.Set("page_size", strconv.Itoa(in.PageSize))
	}
	raw, err := s.caller.Get(ctx, AppCatalog, "/products/search?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var page catalogProductPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("posgateway: 解析 %s/products/search 失败: %w", AppCatalog, err)
	}
	if page.Products == nil {
		page.Products = []ProductCandidate{}
	}
	return &ProductSearchView{
		Keyword:    in.Keyword,
		Candidates: page.Products,
		Count:      page.Count,
		Total:      page.Total,
		Warnings:   []string{},
	}, nil
}

// PricesInput 是 GET /prices 的入参。
type PricesInput struct {
	// BranchID 是当前门店。
	BranchID string
	// ProductIDs 是要查价的商品 id(货号)。
	ProductIDs []string
	// OnDate 是促销的基准日(yyyy-mm-dd);空 = 今天。
	OnDate string
}

// Prices 代理 pricing 的**当前生效售价 + 当前生效促销**。
//
// ⚠️ 判据在 pricing 侧（配了数据源走 cube、没配走本地）⇒ 本服务不关心数据源。
//
// # 降级语义
//
//   - 售价:取不到 ⇒ 整个请求失败(这个端点就是查价,没有价等于没查到)。
//   - 促销:取不到 ⇒ promotions = [] + warning(**绝大多数商品本来就没有促销**,
//     那是正常状态不是故障)。
//   - **任何一项返回 401 / 403** ⇒ 整个请求失败。
func (s *Service) Prices(ctx context.Context, in PricesInput) (*PricesView, error) {
	ids := joinIDs(in.ProductIDs)
	if ids == "" {
		return nil, fmt.Errorf("%w: product_ids 必填(逗号分隔)", ErrInvalidInput)
	}
	view := &PricesView{
		BranchID:   in.BranchID,
		Prices:     []LocalPrice{},
		Promotions: []LocalPromotion{},
		Warnings:   []string{},
	}

	priceRaw, err := s.caller.Get(ctx, AppPricing, "/prices?product_ids="+url.QueryEscape(ids))
	if err != nil {
		return nil, err
	}
	var priceResp pricingPricesResp
	if err := json.Unmarshal(priceRaw, &priceResp); err != nil {
		return nil, fmt.Errorf("posgateway: 解析 %s/prices 失败: %w", AppPricing, err)
	}
	view.Prices = priceResp.Prices
	if view.Prices == nil {
		view.Prices = []LocalPrice{}
	}
	view.AsOf = priceResp.AsOf

	promoQ := url.Values{}
	promoQ.Set("product_ids", ids)
	if strings.TrimSpace(in.OnDate) != "" {
		promoQ.Set("on_date", strings.TrimSpace(in.OnDate))
	}
	promoRaw, err := s.caller.Get(ctx, AppPricing, "/promotions?"+promoQ.Encode())
	if err != nil {
		if IsAuthError(err) {
			return nil, err
		}
		view.Warnings = append(view.Warnings, WarnPromotionsDown)
		s.logger.Warn("posgateway: 促销取不到,降级", "err", err, "branch_id", in.BranchID)
		return view, nil
	}
	var promoResp pricingPromotionsResp
	if err := json.Unmarshal(promoRaw, &promoResp); err != nil {
		view.Warnings = append(view.Warnings, WarnPromotionsDown)
		s.logger.Warn("posgateway: 促销解析失败,降级", "err", err, "branch_id", in.BranchID)
		return view, nil
	}
	view.Promotions = promoResp.Promotions
	if view.Promotions == nil {
		view.Promotions = []LocalPromotion{}
	}
	return view, nil
}

// CreateSale 把开单请求**原样**转发给 pos。
//
// ⚠️ **op_id 必须原样透传**:离线重传靠它幂等(pos 按 op_id 去重,
// 同 op_id 重复提交返回首次那张单)。body 一个字节都不改 ——
// 任何"解析再重组"都会把 op_id 弄丢,而那正是幂等的唯一依据。
//
// ⚠️ 门店由 X-Branch-ID 决定(已随 caller 转发),body 里的 branch_id
//
//	只在 pos 侧做一致性校验。
func (s *Service) CreateSale(ctx context.Context, branchID string, body []byte) (json.RawMessage, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: 请求体不能为空", ErrInvalidInput)
	}
	return s.caller.Post(ctx, AppPOS, "/sales", body)
}

// ---- 各下游的取数 ----

// fetchStore 取门店档案。
func (s *Service) fetchStore(ctx context.Context, branchID string) (*StoreView, error) {
	raw, err := s.caller.Get(ctx, AppMasterData, "/stores")
	if err != nil {
		return nil, err
	}
	var resp masterDataStoresResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("posgateway: 解析 %s/stores 失败: %w", AppMasterData, err)
	}
	for _, row := range resp.Stores {
		if row.BranchID != branchID {
			continue
		}
		return &StoreView{
			ID:            row.BranchID,
			Code:          row.Code,
			DisplayName:   row.Name,
			PosEnabled:    row.PosEnabled,
			MemberEnabled: row.MemberEnabled,
			CounterNos:    row.CounterNos,
		}, nil
	}
	// 门店档案里没有这家店 —— 不是"服务不可用",是"这家店没档案"。
	return nil, fmt.Errorf("%w: branch_id=%s", ErrStoreNotFound, branchID)
}

// fetchShift 取当前班次(顺带把原始行带出去给收银员用)。
//
// 多个未交班时:优先匹配收银台号;否则取**最近开班**的那个
// (master-data 的 ListOpen 按 opened_at desc 排序)。
func (s *Service) fetchShift(ctx context.Context, branchID, counterNo string) (*ShiftView, *shiftRow, error) {
	raw, err := s.caller.Get(ctx, AppMasterData, "/shifts?branch_id="+url.QueryEscape(branchID))
	if err != nil {
		return nil, nil, err
	}
	var resp masterDataShiftsResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, nil, fmt.Errorf("posgateway: 解析 %s/shifts 失败: %w", AppMasterData, err)
	}
	best := -1
	for i := range resp.Shifts {
		row := resp.Shifts[i]
		if row.BranchID != "" && row.BranchID != branchID {
			continue
		}
		if counterNo != "" && row.CounterNo == counterNo {
			best = i
			break
		}
		if best < 0 {
			best = i
		}
	}
	if best < 0 {
		// 没有未交班的班次 —— 这是**正常状态**(还没开班),不是错误。
		return nil, nil, nil
	}
	row := resp.Shifts[best]
	view := &ShiftView{
		ID:         row.ID,
		OpenedAt:   row.OpenedAt,
		OperatorID: row.EmployeeID,
		CounterNo:  row.CounterNo,
	}
	return view, &shiftRow{employeeID: row.EmployeeID}, nil
}

// shiftRow 是 fetchShift 顺带带出的原始行(只需要开班人)。
type shiftRow struct {
	employeeID string
}

// fetchOperator 取当前收银员。
//
// 优先用**班次的开班人**;没有班次时用**当前登录人**。
// 找不到 ⇒ (nil, nil):不是错误,只是这一项没有。
func (s *Service) fetchOperator(ctx context.Context, branchID string, shift *shiftRow, operatorID string) (*OperatorView, error) {
	raw, err := s.caller.Get(ctx, AppMasterData, "/employees?branch_id="+url.QueryEscape(branchID))
	if err != nil {
		return nil, err
	}
	var resp masterDataEmployeesResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("posgateway: 解析 %s/employees 失败: %w", AppMasterData, err)
	}
	want := operatorID
	if shift != nil && shift.employeeID != "" {
		want = shift.employeeID
	}
	if want == "" {
		return nil, nil
	}
	for _, row := range resp.Employees {
		if row.ID != want {
			continue
		}
		return &OperatorView{
			EmployeeCode: row.EmployeeCode,
			Name:         row.Name,
			Role:         row.Role,
			CounterNo:    row.CounterNo,
			DiscountRate: row.DiscountRate,
		}, nil
	}
	return nil, nil
}

// fetchMemberEnabled 取会员开关(member.member_settings.membership_enabled)。
func (s *Service) fetchMemberEnabled(ctx context.Context) (*bool, error) {
	raw, err := s.caller.Get(ctx, AppMember, "/config")
	if err != nil {
		return nil, err
	}
	var resp memberSettingsResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("posgateway: 解析 %s/config 失败: %w", AppMember, err)
	}
	enabled := resp.MembershipEnabled
	return &enabled, nil
}

// fetchMemberTypes 取会员类别。
func (s *Service) fetchMemberTypes(ctx context.Context) ([]MemberTypeView, error) {
	raw, err := s.caller.Get(ctx, AppMember, "/types")
	if err != nil {
		return nil, err
	}
	var resp memberTypesResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("posgateway: 解析 %s/types 失败: %w", AppMember, err)
	}
	out := make([]MemberTypeView, 0, len(resp.MemberTypes))
	for _, row := range resp.MemberTypes {
		out = append(out, MemberTypeView{
			ID:               row.ID,
			Name:             row.Name,
			DiscountRate:     row.DiscountRate,
			FeeAmountYuan:    row.FeeAmountYuan,
			AllowStoredValue: row.AllowStoredValue,
			AllowPointsPay:   row.AllowPointsPay,
			Priority:         row.Priority,
			Status:           row.Status,
		})
	}
	return out, nil
}

// fetchPaymentWays 取付款方式字典。
func (s *Service) fetchPaymentWays(ctx context.Context) ([]PaymentWayView, error) {
	raw, err := s.caller.Get(ctx, AppMember, "/payment-ways")
	if err != nil {
		return nil, err
	}
	var resp memberPaymentWaysResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("posgateway: 解析 %s/payment-ways 失败: %w", AppMember, err)
	}
	out := make([]PaymentWayView, 0, len(resp.PaymentWays))
	for _, row := range resp.PaymentWays {
		out = append(out, PaymentWayView{
			Code:       row.Code,
			Name:       row.Name,
			Rate:       row.Rate,
			IsIntegral: row.IsIntegral,
			SortOrder:  row.SortOrder,
		})
	}
	return out, nil
}

// joinIDs 把 id 列表拼成逗号分隔串(去空、去重、保序)。
func joinIDs(ids []string) string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return strings.Join(out, ",")
}
