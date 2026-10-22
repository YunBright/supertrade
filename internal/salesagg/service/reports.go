// Package service / reports.go —— 图7 的 11 个销售报表。
//
// # 为什么是**另一个** service(而不是给现有 Service 加方法)
//
// 现有 `Service` 回答的是"这张报表(品类 / 商品口径)长什么样",
// 它有自己的汇总口径(独立的无分组查询)、自己的毛利覆盖率阈值
// (MinCostCoverage)、自己的 daily 事实库回落链。
//
// 图7 要的是**一个壳 + 11 种列**的通用报表:
// 同一套响应结构(code / columns / rows / summary / count / page /
// page_size / total),列随 code 变。
//
// ⇒ 单独一个 service,两条链互不影响 —— 改报表口径不会碰坏
// 现有 `/reports/sales`(wechat-bot 的群卡片在用它)。
//
// # ⚠️ 数据来源按「门店配没配数据源」分流(判据唯一:internal/cubesource)
//
//	配了源 ⇒ 走 cube
//	没配源 ⇒ 本域 PG
//	问不到 / 被停用 ⇒ **503**,不回落本地。
//
// # ⚠️ 成本是**估算值**,必须带覆盖率
//
// 现有 Service 的包注释里记着实测:
// hbposv10 的 base_price 全表为 0、base_price1 仅 308/27312 ≈ 1.1%
// ⇒ 当日按类别聚合总覆盖率仅 25.3%。
//
// ⇒ 汇总里**必须**同时给出 costed_rows / total_rows / cost_coverage,
// 让调用方自己决定要不要展示毛利。直接用 cost=0 的组算毛利率会显示 100%,
// 那比"不显示"更坏。
//
// # ⚠️ 11 个 code 的 cube 维度(契约假设,cube 仓并行实现中)
//
// sales_report(t_rm_daysum + item → item_cls → item_cls_parent):
//
//	维度 item_no / oper_date / supcust_no
//	joins item.name / item.unit / item.brand_name /
//	      item.clsno / item_cls.id / item_cls.name /
//	      item_cls_parent.id / item_cls_parent.name
//	measure count / total_net_amount / total_net_qty / total_ref_cost / costed_rows
//
// sale_day(t_rm_daysum):
//
//	维度 product_id / supplier_id / branch_id / business_date
//	measure count / net_sale_quantity / net_sale_amount_yuan /
//	        total_cost_yuan / avg_cost_yuan
//
// ⚠️ **两个 model 各缺对方的一半**:
// sales_report 有类别链但**没有** branch_no 维度;
// sale_day 有 branch_no 但**没有**类别链。
// ⇒ 「商品-分部」「类别-分部」「分部」「库位」四个 code 走 sale_day,
//   其余走 sales_report。
//
// ⚠️ **库位(code=location)在源库 / cube 侧没有对应列**
// (全仓 grep 不到 location / 库位)⇒ 本轮以源库分店号
// (sale_day.branch_id)近似,并在行标签里写明「库位(分店)」。
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead"
	"github.com/YunBright/supertrade/internal/cubesource"
)

// 报表 code(前端**不硬编码**菜单文案,由 /reports/sales/menu-items 下发)。
const (
	ReportProductBarcodeGroup = "product_barcode_group"
	ReportProductPriceGroup   = "product_price_group"
	ReportProductBranch       = "product_branch"
	ReportCategoryBranch      = "category_branch"
	ReportBranch              = "branch"
	ReportLocation            = "location"
	ReportCategoryLarge       = "category_large"
	ReportCategory            = "category"
	ReportDailyAverage        = "daily_average"
	ReportProductDetail       = "product_detail"
	ReportSupplier            = "supplier"
)

// ReportMenuItem 是菜单项(code + 中文标签)。
type ReportMenuItem struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

// ReportMenu 是 `/reports/sales/menu-items` 的响应。
type ReportMenu struct {
	Items []ReportMenuItem `json:"items"`
}

// ReportColumn 是列描述(前端按它渲染表头)。
type ReportColumn struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	// Type 是 string / decimal / int。
	Type string `json:"type"`
	// Align 是 right / left(可空)。
	Align string `json:"align,omitempty"`
}

// ReportSummary 是顶部汇总。
//
// ⚠️ **costed_rows / total_rows / cost_coverage 必须在**:
// 没有它们,调用方无法区分「毛利率 100%」是真实的还是「成本全为 0」的假象
// (实测 hbposv10 当日 64 个类别组里多组覆盖率 0%)。
type ReportSummary struct {
	// NetAmountYuan 是净销售额(元)。
	NetAmountYuan string `json:"net_amount_yuan"`
	// NetQty 是净销量(称重重量,不是件数)。
	NetQty string `json:"net_qty"`
	// RefCostYuan 是参考成本(元)。**估算值**。
	RefCostYuan string `json:"ref_cost_yuan"`
	// CostedRows / TotalRows 是成本覆盖率的两端(可为空 = 该 model 没有成本口径)。
	CostedRows *int64 `json:"costed_rows"`
	TotalRows  *int64 `json:"total_rows"`
	// CostCoverage 是覆盖率(如 "25.3%")。可为空。
	CostCoverage string `json:"cost_coverage"`
	// MarginAvailable 告诉调用方能不能展示毛利。
	MarginAvailable bool `json:"margin_available"`
	// RefMarginYuan / RefMarginPct 是参考毛利。覆盖率不足时为空。
	RefMarginYuan string `json:"ref_margin_yuan"`
	RefMarginPct string `json:"ref_margin_pct"`
	// RowCount 是这一页的行数(不是总行数)。
	RowCount int `json:"row_count"`
	// DayCount 是区间内的营业天数(日均口径用)。
	DayCount int `json:"day_count"`
	// DailyAvgYuan 是日均销售额(元)。
	DailyAvgYuan string `json:"daily_avg_yuan"`
}

// ReportDataRow 是一行。**键随 code 变**(由 columns 描述)。
//
// ⚠️ 叫 ReportDataRow 而不是 ReportRow:本包里已有一个 ReportRow
// (现有 `/reports/sales` 的固定形状行:code/name/extra/net_amount/…)。
// 两个同名结构体放在同一个包里 ⇒ 后来者一定会用错 ⇒ 改名。
type ReportDataRow map[string]any

// Report 是 `/reports/sales/by-code` 的响应(所有报表共用一个壳)。
type Report struct {
	// Code 是报表 code(回显请求值)。
	Code string `json:"code"`
	// Columns 是列描述。
	Columns []ReportColumn `json:"columns"`
	// Rows 是数据行。
	Rows []ReportDataRow `json:"rows"`
	// Summary 是顶部汇总。
	Summary ReportSummary `json:"summary"`
	// Count 是**本页**行数。
	Count int `json:"count"`
	// Page / PageSize 回显请求值。
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
	// Total 是过滤后的总行数。
	Total int `json:"total"`
	// DataSource 是 "cube" / "local"。
	//
	// ⚠️ PLAN §3.5 之外的附加字段:没有它,调用方无法区分
	// "这家店这段时间没生意"与"这条路查的是别家店"。
	DataSource string `json:"data_source,omitempty"`
}

// ReportQuery 是报表查询条件。
type ReportQuery struct {
	// Code 是报表 code(必填)。
	Code string
	// From / To 是营业日期闭区间(YYYY-MM-DD)。
	From string
	To   string
	// BranchID 是源库分店号(思迅 branch_no)。空 = 不过滤。
	BranchID string
	// ProductID / CategoryID / SupplierID / SaleWay / BrandName 是维度过滤。
	ProductID  string
	CategoryID string
	SupplierID string
	SaleWay    string
	BrandName  string
	// Page / PageSize 是分页。
	Page     int
	PageSize int
}

func (q ReportQuery) normalized() ReportQuery {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = 100
	}
	if q.PageSize > 500 {
		q.PageSize = 500
	}
	return q
}

// ---- cube 维度清单 ----

// salesReportMeasures 是 sales_report 的聚合 measure。
//
// ⚠️ costed_rows / count 一起取不是为了好看:没有它们,
// 调用方无法区分「毛利率 100%」是真实的还是「成本全为 0」的假象。
var reportSalesMeasures = []string{
	"sales_report.total_net_amount",
	"sales_report.total_net_qty",
	"sales_report.total_ref_cost",
	"sales_report.costed_rows",
	"sales_report.count",
}

// saleDayMeasures 是 sale_day 的聚合 measure。
//
// ⚠️ sale_day **没有** costed_rows ⇒ 走它的四个 code 没有成本覆盖率口径
// ⇒ 汇总里 costed_rows / total_rows / cost_coverage 留空,
// margin_available = false(前端据此不展示毛利)。
var reportSaleDayMeasures = []string{
	"sale_day.net_sale_amount_yuan",
	"sale_day.net_sale_quantity",
	"sale_day.total_cost_yuan",
	"sale_day.count",
}

// reportSpec 是一个报表 code 的定义。
type reportSpec struct {
	code  string
	label string
	// model 是 cube model 名。
	model string
	// dims 是分组维度(全部进 GROUP BY)。
	dims []string
	// labelDim 是行标签的成员(空 = 用 dims 的第一个成员当标签)。
	labelDim string
	// extraDims 是**带进行里**但不参与分组的成员。
	extraDims []string
	// measures 是聚合 measure。
	measures []string
	// columns 是列描述。
	columns []ReportColumn
	// post 是行后处理(可选)。
	post string
}

// reportSpecs 是 11 个 code 的定义表。
//
// ⚠️ **表驱动**而不是 11 个 if/else:11 个 if/else 会长到没人敢改,
// 而这张表一眼能看出"哪个 code 走哪个 model、哪些列"。
var reportSpecs = []reportSpec{
	{
		code:  ReportProductBarcodeGroup,
		label: "商品销售条码分组统计",
		model: "sales_report",
		// ⚠️ 思迅 **item_no 就是条码**(领域事实:6957583900828 → item_no 同值)
		// ⇒ "按条码分组"就是按 item_no 分组。
		dims:      []string{"sales_report.item_no"},
		labelDim:  "item.name",
		extraDims: []string{"item.unit"},
		measures:  reportSalesMeasures,
		columns: []ReportColumn{
			{Key: "label", Title: "商品条码", Type: "string"},
			{Key: "product_name", Title: "品名", Type: "string"},
			{Key: "unit", Title: "单位", Type: "string"},
			{Key: "net_qty", Title: "销售数量", Type: "decimal", Align: "right"},
			{Key: "net_amount_yuan", Title: "销售金额", Type: "decimal", Align: "right"},
			{Key: "amount_ratio", Title: "占比", Type: "string", Align: "right"},
		},
	},
	{
		code:      ReportProductPriceGroup,
		label:     "商品销售价格分组统计",
		model:     "sales_report",
		dims:      []string{"sales_report.item_no"},
		labelDim:  "item.name",
		extraDims: []string{"item.unit"},
		measures:  reportSalesMeasures,
		post:      "price_band",
		columns: []ReportColumn{
			{Key: "label", Title: "价格分组", Type: "string"},
			{Key: "product_id", Title: "商品编码", Type: "string"},
			{Key: "product_name", Title: "品名", Type: "string"},
			{Key: "unit", Title: "单位", Type: "string"},
			{Key: "avg_price_yuan", Title: "均价", Type: "decimal", Align: "right"},
			{Key: "net_qty", Title: "销售数量", Type: "decimal", Align: "right"},
			{Key: "net_amount_yuan", Title: "销售金额", Type: "decimal", Align: "right"},
			{Key: "amount_ratio", Title: "占比", Type: "string", Align: "right"},
		},
	},
	{
		code:      ReportProductBranch,
		label:     "商品-分部汇总",
		model:     "sale_day",
		dims:      []string{"sale_day.branch_id", "sale_day.product_id"},
		labelDim:  "",
		extraDims: []string{},
		measures:  reportSaleDayMeasures,
		post:      "product_dict",
		columns: []ReportColumn{
			{Key: "label", Title: "商品", Type: "string"},
			{Key: "product_id", Title: "商品编码", Type: "string"},
			{Key: "branch_id", Title: "分部", Type: "string"},
			{Key: "net_qty", Title: "销售数量", Type: "decimal", Align: "right"},
			{Key: "net_amount_yuan", Title: "销售金额", Type: "decimal", Align: "right"},
			{Key: "amount_ratio", Title: "占比", Type: "string", Align: "right"},
		},
	},
	{
		code:      ReportCategoryBranch,
		label:     "类别-分部汇总",
		model:     "sale_day",
		dims:      []string{"sale_day.branch_id", "sale_day.product_id"},
		labelDim:  "",
		extraDims: []string{},
		measures:  reportSaleDayMeasures,
		post:      "category_dict",
		columns: []ReportColumn{
			{Key: "label", Title: "类别", Type: "string"},
			{Key: "branch_id", Title: "分部", Type: "string"},
			{Key: "net_qty", Title: "销售数量", Type: "decimal", Align: "right"},
			{Key: "net_amount_yuan", Title: "销售金额", Type: "decimal", Align: "right"},
			{Key: "amount_ratio", Title: "占比", Type: "string", Align: "right"},
		},
	},
	{
		code:      ReportBranch,
		label:     "分部汇总",
		model:     "sale_day",
		dims:      []string{"sale_day.branch_id"},
		labelDim:  "",
		extraDims: []string{},
		measures:  reportSaleDayMeasures,
		post:      "branch",
		columns: []ReportColumn{
			{Key: "label", Title: "分部", Type: "string"},
			{Key: "branch_id", Title: "分部编码", Type: "string"},
			{Key: "net_qty", Title: "销售数量", Type: "decimal", Align: "right"},
			{Key: "net_amount_yuan", Title: "销售金额", Type: "decimal", Align: "right"},
			{Key: "amount_ratio", Title: "占比", Type: "string", Align: "right"},
		},
	},
	{
		code:      ReportLocation,
		label:     "库位销售汇总",
		model:     "sale_day",
		dims:      []string{"sale_day.branch_id"},
		labelDim:  "",
		extraDims: []string{},
		measures:  reportSaleDayMeasures,
		post:      "location",
		columns: []ReportColumn{
			{Key: "label", Title: "库位(分店)", Type: "string"},
			{Key: "branch_id", Title: "库位编码", Type: "string"},
			{Key: "net_qty", Title: "销售数量", Type: "decimal", Align: "right"},
			{Key: "net_amount_yuan", Title: "销售金额", Type: "decimal", Align: "right"},
			{Key: "amount_ratio", Title: "占比", Type: "string", Align: "right"},
		},
	},
	{
		code:      ReportCategoryLarge,
		label:     "大类汇总",
		model:     "sales_report",
		dims:      []string{"item_cls_parent.id"},
		labelDim:  "item_cls_parent.name",
		extraDims: []string{},
		measures:  reportSalesMeasures,
		columns: []ReportColumn{
			{Key: "label", Title: "大类", Type: "string"},
			{Key: "net_qty", Title: "销售数量", Type: "decimal", Align: "right"},
			{Key: "net_amount_yuan", Title: "销售金额", Type: "decimal", Align: "right"},
			{Key: "amount_ratio", Title: "占比", Type: "string", Align: "right"},
		},
	},
	{
		code:      ReportCategory,
		label:     "类别汇总",
		model:     "sales_report",
		dims:      []string{"item_cls.id"},
		labelDim:  "item_cls.name",
		extraDims: []string{},
		measures:  reportSalesMeasures,
		columns: []ReportColumn{
			{Key: "label", Title: "类别", Type: "string"},
			{Key: "category_id", Title: "类别编码", Type: "string"},
			{Key: "net_qty", Title: "销售数量", Type: "decimal", Align: "right"},
			{Key: "net_amount_yuan", Title: "销售金额", Type: "decimal", Align: "right"},
			{Key: "amount_ratio", Title: "占比", Type: "string", Align: "right"},
		},
	},
	{
		code:      ReportDailyAverage,
		label:     "日均销售",
		model:     "sales_report",
		dims:      []string{"sales_report.oper_date"},
		labelDim:  "",
		extraDims: []string{},
		measures:  reportSalesMeasures,
		post:      "daily",
		columns: []ReportColumn{
			{Key: "label", Title: "营业日期", Type: "string"},
			{Key: "net_qty", Title: "销售数量", Type: "decimal", Align: "right"},
			{Key: "net_amount_yuan", Title: "销售金额", Type: "decimal", Align: "right"},
			{Key: "amount_ratio", Title: "占比", Type: "string", Align: "right"},
		},
	},
	{
		code:      ReportProductDetail,
		label:     "商品明细",
		model:     "sales_report",
		dims:      []string{"sales_report.item_no"},
		labelDim:  "item.name",
		extraDims: []string{"item.unit", "item.brand_name"},
		measures:  reportSalesMeasures,
		columns: []ReportColumn{
			{Key: "label", Title: "商品编码", Type: "string"},
			{Key: "product_name", Title: "品名", Type: "string"},
			{Key: "brand_name", Title: "品牌", Type: "string"},
			{Key: "unit", Title: "单位", Type: "string"},
			{Key: "net_qty", Title: "销售数量", Type: "decimal", Align: "right"},
			{Key: "net_amount_yuan", Title: "销售金额", Type: "decimal", Align: "right"},
			{Key: "amount_ratio", Title: "占比", Type: "string", Align: "right"},
		},
	},
	{
		code:      ReportSupplier,
		label:     "供应商销售汇总",
		model:     "sales_report",
		dims:      []string{"sales_report.supcust_no"},
		labelDim:  "",
		extraDims: []string{},
		measures:  reportSalesMeasures,
		post:      "supplier_dict",
		columns: []ReportColumn{
			{Key: "label", Title: "供应商", Type: "string"},
			{Key: "supplier_id", Title: "供应商编码", Type: "string"},
			{Key: "net_qty", Title: "销售数量", Type: "decimal", Align: "right"},
			{Key: "net_amount_yuan", Title: "销售金额", Type: "decimal", Align: "right"},
			{Key: "amount_ratio", Title: "占比", Type: "string", Align: "right"},
		},
	},
}

// reportSpecOf 按 code 取报表定义。
func reportSpecOf(code string) (reportSpec, bool) {
	for _, s := range reportSpecs {
		if s.code == code {
			return s, true
		}
	}
	return reportSpec{}, false
}

// Menu 返回 11 个菜单项(前端不硬编码文案)。
func Menu() ReportMenu {
	items := make([]ReportMenuItem, 0, len(reportSpecs))
	for _, s := range reportSpecs {
		items = append(items, ReportMenuItem{Code: s.code, Label: s.label})
	}
	return ReportMenu{Items: items}
}

// ErrUnknownReportCode 请求里的 code 不在 11 个之内。
var ErrUnknownReportCode = errors.New("salesagg: 不支持的报表 code")

// ErrReportSourceUnavailable 数据源不可用(问不到路由 / 已停用)⇒ 503。
var ErrReportSourceUnavailable = errors.New("salesagg: 数据源路由暂不可用")

// ErrReportSourceDisabled 该门店的数据源被停用 ⇒ 503。
var ErrReportSourceDisabled = errors.New("salesagg: 该门店的数据源已停用")

// ReportsService 是图7 的通用报表服务。
type ReportsService struct {
	reader  cuberead.Reader
	sources cubesource.Resolver
	logger  *slog.Logger
}

// NewReportsService 构造 ReportsService。
func NewReportsService(reader cuberead.Reader, logger *slog.Logger) *ReportsService {
	if logger == nil {
		logger = slog.Default()
	}
	return &ReportsService{reader: reader, logger: logger}
}

// SetSourceResolver 注入数据源解析器。
//
// ⚠️ 与已有 Service 的 daily 事实库回落链**无关**:
// 本服务只读 cube(或本域 PG),不查 sales_daily_*。
func (s *ReportsService) SetSourceResolver(r cubesource.Resolver) {
	if s == nil {
		return
	}
	s.sources = r
}

// readPath 判定该门店的报表该走哪条路。
//
// ⚠️ 问不到 / 停用 ⇒ **503**,不回落本地。
func (s *ReportsService) readPath(ctx context.Context, branchID string) (bool, error) {
	useCube, err := cubesource.Decide(ctx, s.sources, branchID)
	if err == nil {
		if useCube && s.reader == nil {
			return false, fmt.Errorf("%w: sales-agg 缺少 cube 读端", ErrReportSourceUnavailable)
		}
		return useCube, nil
	}
	switch {
	case errors.Is(err, cubesource.ErrSourceDisabled):
		return false, fmt.Errorf("%w: 门店 %s 的数据源已停用",
			ErrReportSourceDisabled, strings.TrimSpace(branchID))
	case errors.Is(err, cubesource.ErrSourceUnavailable):
		return false, fmt.Errorf("%w: %v", ErrReportSourceUnavailable, err)
	default:
		return false, err
	}
}

// Run 跑一张报表。
func (s *ReportsService) Run(ctx context.Context, branchID string, q ReportQuery) (*Report, error) {
	spec, ok := reportSpecOf(q.Code)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownReportCode, q.Code)
	}
	useCube, err := s.readPath(ctx, branchID)
	if err != nil {
		return nil, err
	}
	q = q.normalized()
	if !useCube {
		// ⚠️ 本域 PG 里**没有**与 cube 同口径的销售日结事实
		// (自营店的销售在 POS 单据里,口径与 cube 侧不同)。
		// ⇒ 本地路径**明确**返回"没有数据源",而不是给出与 cube 口径不同的数字
		// (同一份报表两种算法 ⇒ 数字对不上,而且没人说得清哪份对)。
		return &Report{
			Code:      q.Code,
			Columns:   spec.columns,
			Rows:      []ReportDataRow{},
			Summary:   ReportSummary{NetAmountYuan: "0.00", NetQty: "0.00", RefCostYuan: "0.00", CostCoverage: "", RowCount: 0},
			Count:     0,
			Page:      q.Page,
			PageSize:  q.PageSize,
			Total:     0,
			DataSource: "local_unavailable",
		}, nil
	}
	if s.reader == nil {
		return nil, fmt.Errorf("%w: sales-agg 缺少 cube 读端", ErrReportSourceUnavailable)
	}
	return s.runCube(ctx, spec, q)
}

// containsDim 判断成员是否已在维度列表里(cube 的 order/filter 成员必须先被 select)。
func containsDim(dims []string, member string) bool {
	for _, d := range dims {
		if d == member {
			return true
		}
	}
	return false
}

// isBaseModelMember 判断成员是不是**本 model 自己的**成员(而不是 join 进来的)。
//
// ⚠️ cube 的 order 成员校验只认 base model 的成员
// (实测 2026-10-23:order 引用 joined model 的成员 ⇒ 400
// `QUERY_INVALID: order member not found`)。
// ⇒ 排序成员不是本 model 的 ⇒ 就不发 order。
func isBaseModelMember(model, member string) bool {
	prefix := model + "."
	return strings.HasPrefix(member, prefix)
}

// runCube 走 cube 的报表查询。
func (s *ReportsService) runCube(ctx context.Context, spec reportSpec, q ReportQuery) (*Report, error) {
	filters, err := s.buildFilters(ctx, spec, q)
	if err != nil {
		return nil, err
	}
	dims := append([]string{}, spec.dims...)
	dims = append(dims, spec.extraDims...)
	// ⚠️ **labelDim 必须进查询**(2026-10-22 生产实测踩到,两个 bug 同源)。
	//
	//	① **标签是空的**:行标签取自 `cuberead.Str(r, spec.labelDim)`,
	//	   但 labelDim **从来没被 SELECT 出来** ⇒ 行标签恒为 ""。
	//	   受影响:product_barcode_group / product_price_group / category /
	//	   category_large 四个 code(它们的 labelDim 都不是 dims[0])。
	//
	//	⇒ 把 labelDim 加进查询,标签才有值。
	if spec.labelDim != "" && !containsDim(dims, spec.labelDim) {
		dims = append(dims, spec.labelDim)
	}
	//	② **cube 直接 400**:`order` 引用了 **joined model** 的成员。
	//	   实测(2026-10-23 生产直打 cube app :8083/query):
	//	     dims=[item_cls.id, item_cls.name] + order=[item_cls.id] ⇒ 400
	//	       `QUERY_INVALID: order member not found: item_cls.id`
	//	     同样的 dims、**去掉 order** ⇒ 200
	//	     order 换成 base model 的成员(sales_report.oper_date)⇒ 400
	//	       (它没被 select)
	//	   ⇒ cube 的 order 成员校验只认 **base model 的成员**,
	//	     而 cube 自己的契约写的是"order 成员必须**被 select**"
	//	     (docs/dapr-app-contract.md) —— 两者不一致,这是 cube 侧的行为。
	//	   ⇒ 本服务**不去赌** cube 会不会接受:排序成员属于 joined model 时
	//	     **不发 order**(行序由 cube 决定;前端另有客户端排序)。
	//	     等 cube 把校验放宽后,这里可以再把 order 加回来。
	order := []cubeclient.CubeOrder{}
	if isBaseModelMember(spec.model, spec.dims[0]) {
		order = []cubeclient.CubeOrder{{ID: spec.dims[0], Order: "desc"}}
	}
	cq := cubeclient.CubeQuery{
		Measures:   spec.measures,
		Dimensions: dims,
		Filters:    filters,
		Order:      order,
		Limit:      q.PageSize,
		Offset:     (q.Page - 1) * q.PageSize,
		Total:      true,
	}
	rows, total, err := s.reader.LoadCubeQueryPage(ctx, spec.model, cq)
	if err != nil {
		return nil, err
	}
	summary, empty, err := s.summary(ctx, spec, q)
	if err != nil {
		return nil, err
	}
	if empty {
		// ⚠️ **空结果**要分两种:
		//
		//	空 + 没有明细行 ⇒ "这段时间/这个筛选条件下没有数据" ⇒
		//	  返一份**全零**的报表(不是 500)。
		//	  理由:用户按一个没卖过的商品查报表,"没有数据"就是答案。
		//	空 + **有**明细行 ⇒ 自相矛盾 ⇒ 报错(别拿 0 元糊弄)。
		if len(rows) > 0 {
			return nil, fmt.Errorf("salesagg: %s 汇总查询返回空结果,但明细有 %d 行",
				spec.code, len(rows))
		}
	}
	out := make([]ReportDataRow, 0, len(rows))
	for _, r := range rows {
		row, err := s.buildRow(ctx, spec, r, summary)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	// 金额倒序 —— 报表打开时用户最先看的顺序。
	sort.SliceStable(out, func(i, j int) bool {
		return rowAmount(out[i]).GreaterThan(rowAmount(out[j]))
	})
	summary.RowCount = len(out)
	return &Report{
		Code:       spec.code,
		Columns:    spec.columns,
		Rows:       out,
		Summary:    summary,
		Count:      len(out),
		Page:       q.Page,
		PageSize:   q.PageSize,
		Total:      total,
		DataSource: "cube",
	}, nil
}

// buildFilters 构造报表的过滤条件。
func (s *ReportsService) buildFilters(ctx context.Context, spec reportSpec, q ReportQuery) ([]cubeclient.CubeFilter, error) {
	var out []cubeclient.CubeFilter
	dateMember := "sales_report.oper_date"
	if spec.model == "sale_day" {
		dateMember = "sale_day.business_date"
	}
	if q.From != "" {
		out = append(out, cuberead.Filter(dateMember, "gte", q.From))
	}
	if q.To != "" {
		out = append(out, cuberead.Filter(dateMember, "lte", q.To))
	}
	if q.BranchID != "" {
		// ⚠️ **只有 sale_day 有 branch 维度**;sales_report 刻意不暴露它
		// (cube 的 sales_report schema 顶部写明"门店区域刻意不作为维度")。
		if spec.model == "sale_day" {
			out = append(out, cuberead.Filter("sale_day.branch_id",
				cubeclient.OperatorEquals, q.BranchID))
		}
	}
	if q.ProductID != "" {
		member := "sales_report.item_no"
		if spec.model == "sale_day" {
			member = "sale_day.product_id"
		}
		out = append(out, cuberead.Filter(member, cubeclient.OperatorEquals, q.ProductID))
	}
	if q.SupplierID != "" {
		member := "sales_report.supcust_no"
		if spec.model == "sale_day" {
			member = "sale_day.supplier_id"
		}
		out = append(out, cuberead.Filter(member, cubeclient.OperatorEquals, q.SupplierID))
	}
	if q.CategoryID != "" {
		// ⚠️ 分类过滤用**子树**(该分类 + 它的所有子孙),与 catalog 的
		// 商品档案筛选同口径(否则侧栏 count 与列表 total 对不上)。
		ids, err := s.categorySubtree(ctx, q.CategoryID)
		if err != nil {
			return nil, err
		}
		member := "item.clsno"
		if spec.model == "sale_day" {
			// sale_day 没有类别链 ⇒ 用 product 字典的 category_id 过滤
			// (先在 Go 侧算出这批商品,再用 in 过滤)。
			ids, err = s.productIDsByCategory(ctx, ids)
			if err != nil {
				return nil, err
			}
			member = "sale_day.product_id"
		}
		out = append(out, cuberead.InFilter(member, ids))
	}
	if q.BrandName != "" {
		out = append(out, cuberead.Filter("item.brand_name",
			cubeclient.OperatorEquals, q.BrandName))
	}
	if q.SaleWay != "" {
		// ⚠️ **sale_way 在 sales_report / sale_day 上都没有** ⇒
		// 用 sale_detail(逐笔流水)先算出"这段时间按这个销售方式卖过的商品",
		// 再用 in 过滤报表。
		//
		// ⚠️ 空集合 ⇒ 命中 0 行(不是"不过滤")。
		ids, err := s.productIDsBySaleWay(ctx, q)
		if err != nil {
			return nil, err
		}
		member := "sales_report.item_no"
		if spec.model == "sale_day" {
			member = "sale_day.product_id"
		}
		out = append(out, cuberead.InFilter(member, ids))
	}
	return out, nil
}

// categorySubtree 返回某分类**及其所有子孙**的 id 集合。
//
// ⚠️ 与 catalog 的 localCategorySubtree / cube categorySubtree 同口径。
func (s *ReportsService) categorySubtree(ctx context.Context, rootID string) ([]string, error) {
	rows, _, err := s.reader.LoadCubeQueryPage(ctx, "category", cubeclient.CubeQuery{
		Measures:   []string{"category.count"},
		Dimensions: []string{"category.id", "category.name", "category.parent_id"},
		Limit:      5000,
	})
	if err != nil {
		return nil, err
	}
	type node struct {
		id   string
		par  string
	}
	nodes := make([]node, 0, len(rows))
	for _, r := range rows {
		nodes = append(nodes, node{
			id:  cuberead.Str(r, "category.id"),
			par: cuberead.Str(r, "category.parent_id"),
		})
	}
	children := map[string][]string{}
	for _, n := range nodes {
		children[n.par] = append(children[n.par], n.id)
	}
	out := []string{rootID}
	queue := []string{rootID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range children[cur] {
			out = append(out, c)
			queue = append(queue, c)
		}
	}
	return out, nil
}

// productIDsByCategory 返回某个分类子树下的商品 id 集合。
func (s *ReportsService) productIDsByCategory(ctx context.Context, categoryIDs []string) ([]string, error) {
	if len(categoryIDs) == 0 {
		return nil, nil
	}
	rows, _, err := s.reader.LoadCubeQueryPage(ctx, "product", cubeclient.CubeQuery{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.id"},
		Filters:    []cubeclient.CubeFilter{cuberead.InFilter("product.category_id", categoryIDs)},
		Limit:      100000,
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, cuberead.Str(r, "product.id"))
	}
	return out, nil
}

// productIDsBySaleWay 返回"这段时间按该销售方式卖过的商品"id 集合。
//
// ⚠️ sale_way 只在**逐笔流水**(sale_detail)上有 ⇒ 先查流水再反查报表。
func (s *ReportsService) productIDsBySaleWay(ctx context.Context, q ReportQuery) ([]string, error) {
	filters := []cubeclient.CubeFilter{
		cuberead.Filter("sale_detail.sale_way", cubeclient.OperatorEquals, q.SaleWay),
	}
	if q.From != "" {
		filters = append(filters, cuberead.Filter("sale_detail.sold_at", "gte", q.From+" 00:00:00"))
	}
	if q.To != "" {
		filters = append(filters, cuberead.Filter("sale_detail.sold_at", "lte", q.To+" 23:59:59"))
	}
	rows, _, err := s.reader.LoadCubeQueryPage(ctx, "sale_detail", cubeclient.CubeQuery{
		Measures:   []string{"sale_detail.count"},
		Dimensions: []string{"sale_detail.product_id"},
		Filters:    filters,
		Limit:      100000,
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, cuberead.Str(r, "sale_detail.product_id"))
	}
	return out, nil
}

// summary 取汇总(**独立的无分组查询**)。
//
// ⚠️ 不能由"把明细行加起来"代替:明细是分组后的子集,
// 求和会偏低(且 cube 侧带 limit 截断)—— 现有 Service 的注释里
// 记着线上实测「顶部只统计到 15.6%,零报错」。
//
// 返回的第二个值是"空结果"标记(见 runCube 的处理)。
func (s *ReportsService) summary(ctx context.Context, spec reportSpec, q ReportQuery) (ReportSummary, bool, error) {
	filters, err := s.buildFilters(ctx, spec, q)
	if err != nil {
		return ReportSummary{}, false, err
	}
	rows, _, err := s.reader.LoadCubeQueryPage(ctx, spec.model, cubeclient.CubeQuery{
		Measures: spec.measures,
		Filters:  filters,
		Limit:    1,
	})
	if err != nil {
		return ReportSummary{}, false, err
	}
	if len(rows) == 0 {
		return ReportSummary{}, true, nil
	}
	r := rows[0]
	sum := ReportSummary{}
	var amount, qty, refCost decimal.Decimal
	var costed, total int64
	if spec.model == "sale_day" {
		amount = cuberead.Dec(r, "sale_day.net_sale_amount_yuan")
		qty = cuberead.Dec(r, "sale_day.net_sale_quantity")
		refCost = cuberead.Dec(r, "sale_day.total_cost_yuan")
		total = int64(cuberead.Dec(r, "sale_day.count").InexactFloat64())
	} else {
		amount = cuberead.Dec(r, "sales_report.total_net_amount")
		qty = cuberead.Dec(r, "sales_report.total_net_qty")
		refCost = cuberead.Dec(r, "sales_report.total_ref_cost")
		costed = int64(cuberead.Dec(r, "sales_report.costed_rows").InexactFloat64())
		total = int64(cuberead.Dec(r, "sales_report.count").InexactFloat64())
	}
	sum.NetAmountYuan = cuberead.Money2(amount)
	sum.NetQty = cuberead.Money2(qty)
	sum.RefCostYuan = cuberead.Money2(refCost)
	if spec.model == "sales_report" {
		sum.CostedRows = &costed
		sum.TotalRows = &total
		cov := 0.0
		if total > 0 {
			cov = float64(costed) / float64(total)
		}
		sum.CostCoverage = fmt.Sprintf("%.1f%%", cov*100)
		sum.MarginAvailable = cov >= MinCostCoverage
		if sum.MarginAvailable {
			m := amount.Sub(refCost)
			sum.RefMarginYuan = cuberead.Money2(m)
			if !amount.IsZero() {
				sum.RefMarginPct = fmt.Sprintf("%.1f%%",
					m.Div(amount).Mul(decimal.NewFromInt(100)).InexactFloat64())
			}
		}
	}
	// 日均(区间内的营业天数按 from/to 推;没给区间就按 1 天)。
	days := 1
	if q.From != "" && q.To != "" {
		if d, err := daysBetween(q.From, q.To); err == nil && d > 0 {
			days = d
		}
	}
	sum.DayCount = days
	sum.DailyAvgYuan = cuberead.Money2(amount.Div(decimal.NewFromInt(int64(days))))
	return sum, false, nil
}

// buildRow 把一行 cube 结果组装成响应行。
func (s *ReportsService) buildRow(ctx context.Context, spec reportSpec, r map[string]any, sum ReportSummary) (ReportDataRow, error) {
	var amount, qty decimal.Decimal
	if spec.model == "sale_day" {
		amount = cuberead.Dec(r, "sale_day.net_sale_amount_yuan")
		qty = cuberead.Dec(r, "sale_day.net_sale_quantity")
	} else {
		amount = cuberead.Dec(r, "sales_report.total_net_amount")
		qty = cuberead.Dec(r, "sales_report.total_net_qty")
	}
	row := ReportDataRow{
		"net_qty":        cuberead.Money2(qty),
		"net_amount_yuan": cuberead.Money2(amount),
	}
	totalAmount, _ := decimal.NewFromString(sum.NetAmountYuan)
	if !totalAmount.IsZero() {
		row["amount_ratio"] = fmt.Sprintf("%.2f%%",
			amount.Div(totalAmount).Mul(decimal.NewFromInt(100)).InexactFloat64())
	} else {
		row["amount_ratio"] = "0.00%"
	}
	switch spec.post {
	case "price_band":
		// 价格分组:均价 = 净额 / 净量(用**当期**成交均价,
		// 不用当前挂牌价 —— 挂牌价会变,用它分组会让历史报表变形)。
		id := cuberead.Str(r, "sales_report.item_no")
		avg := decimal.Zero
		if !qty.IsZero() {
			avg = amount.Div(qty)
		}
		band, lo, hi := priceBand(avg)
		row["label"] = band
		row["product_id"] = id
		row["product_name"] = cuberead.Str(r, "item.name")
		row["unit"] = cuberead.Str(r, "item.unit")
		row["avg_price_yuan"] = cuberead.Money2(avg)
		row["price_band"] = fmt.Sprintf("%s~%s", lo, hi)
	case "product_dict":
		pid := cuberead.Str(r, "sale_day.product_id")
		branch := cuberead.Str(r, "sale_day.branch_id")
		names, units, err := s.productDict(ctx, []string{pid})
		if err != nil {
			return nil, err
		}
		row["label"] = names[pid]
		row["product_id"] = pid
		row["branch_id"] = branch
		if u, ok := units[pid]; ok {
			row["unit"] = u
		}
	case "category_dict":
		pid := cuberead.Str(r, "sale_day.product_id")
		branch := cuberead.Str(r, "sale_day.branch_id")
		names, err := s.categoryNames(ctx, []string{pid})
		if err != nil {
			return nil, err
		}
		row["label"] = names[pid]
		row["branch_id"] = branch
	case "branch":
		branch := cuberead.Str(r, "sale_day.branch_id")
		row["label"] = branch
		row["branch_id"] = branch
	case "location":
		branch := cuberead.Str(r, "sale_day.branch_id")
		// ⚠️ 库位在源库 / cube 侧**没有**对应列 ⇒ 以分店号近似,
		// 标签里写明"(分店)"避免被当成真正的库位编码。
		row["label"] = branch + "(分店)"
		row["branch_id"] = branch
	case "daily":
		date := cuberead.Str(r, "sales_report.oper_date")
		row["label"] = date
		row["business_date"] = date
	case "supplier_dict":
		sid := cuberead.Str(r, "sales_report.supcust_no")
		names, err := s.supplierDict(ctx, []string{sid})
		if err != nil {
			return nil, err
		}
		row["label"] = names[sid]
		row["supplier_id"] = sid
	default:
		// 无后处理的 code:标签 = labelDim(或第一个维度),其余维度直接带出。
		label := ""
		if spec.labelDim != "" {
			label = cuberead.Str(r, spec.labelDim)
		} else {
			label = cuberead.Str(r, spec.dims[0])
		}
		row["label"] = label
		for _, d := range spec.dims {
			row[shortKey(d)] = cuberead.Str(r, d)
		}
		for _, d := range spec.extraDims {
			row[shortKey(d)] = cuberead.Str(r, d)
		}
		switch spec.code {
		case ReportProductBarcodeGroup:
			row["label"] = cuberead.Str(r, "sales_report.item_no")
			row["product_id"] = cuberead.Str(r, "sales_report.item_no")
			row["product_name"] = cuberead.Str(r, "item.name")
			row["unit"] = cuberead.Str(r, "item.unit")
		case ReportProductDetail:
			row["label"] = cuberead.Str(r, "sales_report.item_no")
			row["product_id"] = cuberead.Str(r, "sales_report.item_no")
			row["product_name"] = cuberead.Str(r, "item.name")
			row["unit"] = cuberead.Str(r, "item.unit")
			row["brand_name"] = cuberead.Str(r, "item.brand_name")
		case ReportCategoryLarge:
			row["category_id"] = cuberead.Str(r, "item_cls_parent.id")
		case ReportCategory:
			row["category_id"] = cuberead.Str(r, "item_cls.id")
		}
	}
	return row, nil
}

// productDict 取商品名 / 单位字典。
func (s *ReportsService) productDict(ctx context.Context, ids []string) (map[string]string, map[string]string, error) {
	names := map[string]string{}
	units := map[string]string{}
	if len(ids) == 0 {
		return names, units, nil
	}
	rows, _, err := s.reader.LoadCubeQueryPage(ctx, "product", cubeclient.CubeQuery{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.id", "product.name", "product.unit"},
		Filters:    []cubeclient.CubeFilter{cuberead.InFilter("product.id", ids)},
		Limit:      len(ids) * 2,
	})
	if err != nil {
		return nil, nil, err
	}
	for _, r := range rows {
		id := cuberead.Str(r, "product.id")
		if id == "" {
			continue
		}
		names[id] = cuberead.Str(r, "product.name")
		if u := cuberead.Str(r, "product.unit"); u != "" {
			units[id] = u
		}
	}
	return names, units, nil
}

// categoryNames 取商品的**父类别名**(product → category → 父类别)。
//
// ⚠️ cube 的 live↔duck 不许 join ⇒ 分两步:
// 先取商品的 category_id,再取 category 的 parent,再取父类别名。
func (s *ReportsService) categoryNames(ctx context.Context, productIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(productIDs) == 0 {
		return out, nil
	}
	prodRows, _, err := s.reader.LoadCubeQueryPage(ctx, "product", cubeclient.CubeQuery{
		Measures:   []string{"product.count"},
		Dimensions: []string{"product.id", "product.category_id"},
		Filters:    []cubeclient.CubeFilter{cuberead.InFilter("product.id", productIDs)},
		Limit:      len(productIDs) * 2,
	})
	if err != nil {
		return nil, err
	}
	catOf := map[string]string{}
	catIDs := make([]string, 0, len(prodRows))
	for _, r := range prodRows {
		id := cuberead.Str(r, "product.id")
		cid := cuberead.Str(r, "product.category_id")
		catOf[id] = cid
		if cid != "" {
			catIDs = append(catIDs, cid)
		}
	}
	if len(catIDs) == 0 {
		return out, nil
	}
	catRows, _, err := s.reader.LoadCubeQueryPage(ctx, "category", cubeclient.CubeQuery{
		Measures:   []string{"category.count"},
		Dimensions: []string{"category.id", "category.name", "category.parent_id"},
		Filters:    []cubeclient.CubeFilter{cuberead.InFilter("category.id", catIDs)},
		Limit:      len(catIDs) * 2,
	})
	if err != nil {
		return nil, err
	}
	nameOf := map[string]string{}
	parentOf := map[string]string{}
	for _, r := range catRows {
		id := cuberead.Str(r, "category.id")
		nameOf[id] = cuberead.Str(r, "category.name")
		parentOf[id] = cuberead.Str(r, "category.parent_id")
	}
	parentIDs := make([]string, 0, len(catIDs))
	for _, cid := range catIDs {
		if p := parentOf[cid]; p != "" {
			parentIDs = append(parentIDs, p)
		}
	}
	parentName := map[string]string{}
	if len(parentIDs) > 0 {
		pRows, _, err := s.reader.LoadCubeQueryPage(ctx, "category", cubeclient.CubeQuery{
			Measures:   []string{"category.count"},
			Dimensions: []string{"category.id", "category.name"},
			Filters:    []cubeclient.CubeFilter{cuberead.InFilter("category.id", parentIDs)},
			Limit:      len(parentIDs) * 2,
		})
		if err != nil {
			return nil, err
		}
		for _, r := range pRows {
			parentName[cuberead.Str(r, "category.id")] = cuberead.Str(r, "category.name")
		}
	}
	for pid, cid := range catOf {
		if cid == "" {
			continue
		}
		if p := parentOf[cid]; p != "" {
			if n, ok := parentName[p]; ok {
				out[pid] = n
				continue
			}
		}
		out[pid] = nameOf[cid]
	}
	return out, nil
}

// supplierDict 取供应商名。
func (s *ReportsService) supplierDict(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, _, err := s.reader.LoadCubeQueryPage(ctx, "supplier", cubeclient.CubeQuery{
		Measures:   []string{"supplier.count"},
		Dimensions: []string{"supplier.id", "supplier.name"},
		Filters:    []cubeclient.CubeFilter{cuberead.InFilter("supplier.id", ids)},
		Limit:      len(ids) * 2,
	})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		id := cuberead.Str(r, "supplier.id")
		if id == "" {
			continue
		}
		out[id] = cuberead.Str(r, "supplier.name")
	}
	return out, nil
}

// priceBand 返回均价所在的**价格档**。
//
// ⚠️ 档位边界是**本仓的选择**,不是源库的既有口径 ——
// 业务没给过档位表 ⇒ 这里写死一份并在注释里标明。
// 业务要改档位,改这一个函数即可(所有 code 共用)。
func priceBand(avg decimal.Decimal) (label string, lo string, hi string) {
	bands := []struct {
		lo, hi string
		max    decimal.Decimal
	}{
		{"0", "1", decimal.NewFromInt(1)},
		{"1", "3", decimal.NewFromInt(3)},
		{"3", "5", decimal.NewFromInt(5)},
		{"5", "10", decimal.NewFromInt(10)},
		{"10", "20", decimal.NewFromInt(20)},
		{"20", "50", decimal.NewFromInt(50)},
	}
	for _, b := range bands {
		if avg.LessThanOrEqual(b.max) {
			return b.lo + "~" + b.hi + "元", b.lo, b.hi
		}
	}
	return "50元以上", "50", "-"
}

// shortKey 把 cube 成员名压成行里的短键。
//
// "sales_report.item_no" → "item_no"; "item.brand_name" → "brand_name"。
func shortKey(member string) string {
	if i := strings.LastIndex(member, "."); i >= 0 {
		return member[i+1:]
	}
	return member
}

// rowAmount 取行里的金额(用于排序)。
func rowAmount(row ReportDataRow) decimal.Decimal {
	v, _ := row["net_amount_yuan"].(string)
	d, _ := decimal.NewFromString(v)
	return d
}

// daysBetween 计算闭区间的天数。
func daysBetween(from, to string) (int, error) {
	f, err := parseDay(from)
	if err != nil {
		return 0, err
	}
	t, err := parseDay(to)
	if err != nil {
		return 0, err
	}
	if t.Before(f) {
		return 0, fmt.Errorf("salesagg: 区间结束早于开始(%s~%s)", from, to)
	}
	return int(t.Sub(f).Hours()/24) + 1, nil
}

// parseDay 解析 "YYYY-MM-DD"。
func parseDay(s string) (time.Time, error) {
	return time.Parse("2006-01-02", strings.TrimSpace(s))
}
