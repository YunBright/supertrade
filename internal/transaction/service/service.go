// Package service —— 流水查询域(图2 / 图3 / 图6)的读服务。
//
// # 这一域回答什么
//
//	商品销售流水 → cube `sale_detail` model(源表 t_rm_saleflow)
//	收银流水     → cube `payflow`     model(源表 t_rm_payflow)
//
// # 数据来源按「门店配没配数据源」分流(判据唯一:internal/cubesource)
//
//	配了源 ⇒ 走 cube(cube-router → 该门店专属 cube 实例)
//	没配源 ⇒ 走本域 PG(sale_flows / pay_flows 表)
//	问不到 / 被停用 ⇒ **503**,不回落本地。
//
// ⚠️ **回落本地 = 把一次链路故障显示成"这家店没有流水"** ——
// 这是 2026-10-21 生产事故的形状,禁止回落。
//
// # ⚠️ 两条路径**共用同一批响应结构体**
//
// (model.SaleFlowItem / model.PayFlowItem)⇒ 形状不会漂移。
// 派生列(原价金额 / 折扣度 / 折人民币金额)也由**同一组函数**算,
// 两边口径一致。
//
// # ⚠️ cube 侧维度契约(**假设**,cube 仓并行实现中)
//
// sale_detail(按 PLAN §1.1 重写后的 canonical 名):
//
//	id / order_no / random_order_no / branch_no / product_id /
//	source_price / sale_price / qty / sale_amount / sale_way /
//	cashier_id / salesman_id / counter_no / sold_at / online_flag /
//	discount_amount
//
// payflow(按 PLAN §1.2 的 canonical 名):
//
//	id / order_no / random_order_no / sale_amount / branch_no / pay_way /
//	sale_way / card_no / vip_no / currency / exchange_rate / pay_amount /
//	paid_at / cashier_id / salesman_id / counter_no / online_flag
//
// 商品属性(名称 / 自编码 / 规格 / 单位 / 品牌 / 分类)走 **product join**
// (sale_detail 的 schema 里已有 product join;cube 侧按 PLAN §2.1 给
// product model 补 brand_name)。
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/transaction/model"
)

// 分页口径。
const (
	// DefaultPageSize 是流水列表的默认每页条数。
	DefaultPageSize = 50
	// MaxPageSize 是每页条数上限。
	//
	// ⚠️ 上限不是随便定的:page_size 直接落到 cube 的 LIMIT,
	// 一个越界值就能让 cube 把整天的流水拉进内存。
	MaxPageSize = 200
)

// bizLoc 是**业务时区**(源库存的是本地墙钟,不是 UTC)。
//
// ⚠️ 源库(思迅 MSSQL)的 `oper_date` 是 datetime,值形如 "2026-10-22 21:54:52",
// **不带时区** ⇒ 它记的是**门店当地的墙上时间**。
// ⇒ 前端发 UTC(Flutter 的 DateX.toIsoUtc)时必须换算成这个墙钟再比,
// 否则差 8 小时,流水被切掉一整天。
//
// 默认 Asia/Shanghai(UTC+8),cmd 层可用 TRANSACTION_BIZ_TZ 覆盖
// (与 fresh-meat / sales-insight 的 *_BIZ_TZ 同一套约定)。
var bizLoc = mustLoadBizLoc("Asia/Shanghai")

func mustLoadBizLoc(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		// 容器里没有 tzdata 时退回固定 UTC+8 —— 门店就在东八区。
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}

// SetBizLoc 覆盖业务时区(cmd 层装配用)。
func SetBizLoc(loc *time.Location) {
	if loc != nil {
		bizLoc = loc
	}
}

// Service 是流水查询域的服务。
type Service struct {
	// db 是本域 PG(没配数据源的门店走本地时用)。
	db *gorm.DB
	// sources 解析"这个门店配了哪个数据源"。
	sources cubesource.Resolver
	// reader 是 cube 的通用读端(model + 维度 + 过滤 + 分页)。
	reader cuberead.Reader
	logger *slog.Logger
}

// New 构造 Service。
func New(db *gorm.DB, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{db: db, logger: logger}
}

// SetCubeSource 注入数据源解析器与 cube 读端。
//
// ⚠️ **两个都注入**才走 cube 路径;只注入一个 ⇒ 有数据源的门店仍走本地,
// 症状与"没接数据源"完全一样,而且**没有任何报错**。
func (s *Service) SetCubeSource(resolver cubesource.Resolver, reader cuberead.Reader) {
	if s == nil {
		return
	}
	s.sources = resolver
	s.reader = reader
}

// readPath 判定该门店的流水该走哪条路。
//
// ⚠️ 问不到路由 / 数据源被停用 ⇒ **503**,不回落本地。
func (s *Service) readPath(ctx context.Context, branchID string) (bool, error) {
	useCube, err := cubesource.Decide(ctx, s.sources, branchID)
	if err == nil {
		if useCube && s.reader == nil {
			// 配了源却没装读端 ⇒ 装配错误,大声失败(不能回落本地)。
			return false, fmt.Errorf("%w: transaction 缺少 cube 读端", model.ErrCubeSourceUnavailable)
		}
		return useCube, nil
	}
	switch {
	case errors.Is(err, cubesource.ErrSourceDisabled):
		return false, fmt.Errorf("%w: 门店 %s 的数据源已停用",
			model.ErrCubeSourceDisabled, strings.TrimSpace(branchID))
	case errors.Is(err, cubesource.ErrSourceUnavailable):
		return false, fmt.Errorf("%w: %v", model.ErrCubeSourceUnavailable, err)
	default:
		// 权限类错误原样上抛(403 / 401)。
		return false, err
	}
}

// SaleQuery 是商品销售流水的查询条件。
type SaleQuery struct {
	// Q 命中**单号 / 随机单号**(大小写无关)。空 = 不过滤。
	//
	// ⚠️ 只搜单号:图2 / 图3 里另有商品 / 类别 / 收银员等**独立筛选**,
	// 关键字框的位置就是"单号"那一列。商品维度另有 product_id 参数。
	Q string
	// From / To 是销售时间区间(闭区间)。空 = 不过滤。
	From string
	To   string
	// BranchID 是**源库**的分店号(思迅 branch_no,如 "0001")。
	//
	// ⚠️ 不是本系统的 X-Branch-ID(那是路由键)。
	BranchID string
	// ProductID / CategoryID 是商品 / 分类过滤。
	ProductID  string
	CategoryID string
	// SaleWay / CashierID / SalesmanID / CounterNo 是销售方式 / 收银员 / 营业员 / 柜台。
	SaleWay    string
	CashierID  string
	SalesmanID string
	CounterNo  string
	// Page 从 1 开始;PageSize 是每页条数。
	Page     int
	PageSize int
}

func (q SaleQuery) normalized() SaleQuery {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = DefaultPageSize
	}
	if q.PageSize > MaxPageSize {
		q.PageSize = MaxPageSize
	}
	q.From, q.To = defaultDayRange(q.From, q.To)
	return q
}

// defaultDayRange 在**完全没给时间区间**时补上"今天"。
//
// ⚠️ 为什么必须补(2026-10-23 生产实测):
//
//	流水表是**逐笔**的(t_rm_saleflow 330 万行 / t_rm_payflow 114 万行),
//	不带区间就是**全表扫 + 排序** ⇒ 必然撞 cube 的 10s 超时 ⇒
//	端点返 503「数据源暂不可用」。
//
//	⇒ 而 503 的文案完全指不到真因("查询太大"被说成"数据源挂了"),
//	  排查会去查 cube 的健康度 —— 真因只是"没给时间区间"。
//
// ⇒ 不给区间 = 给今天(与思迅老系统的默认口径一致:图2 的默认就是
// 开始 00:00 / 结束 23:59)。
// ⇒ 给了任意一端就**不动**(半开区间是有意义的用法)。
//
// ⚠️ **两条路径都补**(本域 PG 与 cube):
//	只给 cube 补 ⇒ 同一个查询在两个门店给出不同答案(本地是"全部"、cube 是"今天"),
//	而"换个门店就换个答案"正是本仓最忌讳的那类问题。
func defaultDayRange(from, to string) (string, string) {
	if strings.TrimSpace(from) != "" || strings.TrimSpace(to) != "" {
		return from, to
	}
	today := time.Now().In(bizLoc).Format("2006-01-02")
	return today, today
}

// PayQuery 是收银流水的查询条件。
type PayQuery struct {
	// Q 命中单号 / 随机单号。空 = 不过滤。
	Q string
	// From / To 是收款时间区间(闭区间)。空 = 不过滤。
	From string
	To   string
	// BranchID 是源库分店号。
	BranchID string
	// PayWay / CashierID / CardNo / VipNo 是付款方式 / 收银员 / 卡号 / 会员号。
	PayWay    string
	CashierID string
	CardNo    string
	VipNo     string
	// Page 从 1 开始;PageSize 是每页条数。
	Page     int
	PageSize int
}

func (q PayQuery) normalized() PayQuery {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = DefaultPageSize
	}
	if q.PageSize > MaxPageSize {
		q.PageSize = MaxPageSize
	}
	q.From, q.To = defaultDayRange(q.From, q.To)
	return q
}

// defaultDayRange 在**完全没给时间区间**时补上"今天"。
//
// ⚠️ 为什么必须补(2026-10-23 生产实测):
//
//	流水表是**逐笔**的(t_rm_saleflow 330 万行 / t_rm_payflow 114 万行),
//	不带区间就是**全表扫 + 排序** ⇒ 必然撞 cube 的 10s 超时 ⇒
//	端点返 503「数据源暂不可用」。
//
//	⇒ 而 503 的文案完全指不到真因("查询太大"被说成"数据源挂了"),
//	  排查会去查 cube 的健康度 —— 真因只是"没给时间区间"。
//
// ---- cube 维度清单(契约假设集中在这里) ----

// saleDetailDims 是 cube `sale_detail` model 的维度。
func saleDetailDims() []string {
	return []string{
		"sale_detail.id", "sale_detail.order_no", "sale_detail.random_order_no",
		"sale_detail.branch_no", "sale_detail.product_id",
		"sale_detail.source_price", "sale_detail.sale_price", "sale_detail.qty",
		"sale_detail.sale_amount", "sale_detail.sale_way",
		"sale_detail.cashier_id", "sale_detail.salesman_id", "sale_detail.counter_no",
		"sale_detail.sold_at", "sale_detail.online_flag", "sale_detail.discount_amount",
		// product join:商品属性。
		"product.sub_code", "product.name", "product.spec", "product.unit",
		"product.brand_name",
	}
}

// payFlowDims 是 cube `payflow` model 的维度。
func payFlowDims() []string {
	return []string{
		"payflow.id", "payflow.order_no", "payflow.random_order_no",
		"payflow.branch_no", "payflow.sale_way", "payflow.pay_way",
		"payflow.card_no", "payflow.vip_no", "payflow.exchange_rate",
		"payflow.pay_amount", "payflow.sale_amount",
		"payflow.paid_at", "payflow.cashier_id", "payflow.salesman_id",
		"payflow.counter_no",
	}
}

// ---- 商品销售流水 ----

// ListSales 分页查商品销售流水。
func (s *Service) ListSales(ctx context.Context, branchID string, q SaleQuery) (*model.SaleFlowPage, error) {
	useCube, err := s.readPath(ctx, branchID)
	if err != nil {
		return nil, err
	}
	q = q.normalized()
	if useCube {
		return s.cubeListSales(ctx, q)
	}
	return s.localListSales(ctx, branchID, q)
}

// saleFilters 构造销售流水的过滤条件(两条路径**同一套口径**)。
func saleFilters(q SaleQuery) []cubeclient.CubeFilter {
	var out []cubeclient.CubeFilter
	if q.BranchID != "" {
		out = append(out, cuberead.Filter("sale_detail.branch_no",
			cubeclient.OperatorEquals, q.BranchID))
	}
	if q.ProductID != "" {
		out = append(out, cuberead.Filter("sale_detail.product_id",
			cubeclient.OperatorEquals, q.ProductID))
	}
	if q.CategoryID != "" {
		out = append(out, cuberead.Filter("product.category_id",
			cubeclient.OperatorEquals, q.CategoryID))
	}
	if q.SaleWay != "" {
		out = append(out, cuberead.Filter("sale_detail.sale_way",
			cubeclient.OperatorEquals, q.SaleWay))
	}
	if q.CashierID != "" {
		out = append(out, cuberead.Filter("sale_detail.cashier_id",
			cubeclient.OperatorEquals, q.CashierID))
	}
	if q.SalesmanID != "" {
		out = append(out, cuberead.Filter("sale_detail.salesman_id",
			cubeclient.OperatorEquals, q.SalesmanID))
	}
	if q.CounterNo != "" {
		out = append(out, cuberead.Filter("sale_detail.counter_no",
			cubeclient.OperatorEquals, q.CounterNo))
	}
	out = append(out, rangeFilters("sale_detail.sold_at", q.From, q.To)...)
	return out
}

// rangeFilters 构造时间区间过滤(闭区间)。
//
// ⚠️ 只给"日"的时候补 00:00:00 / 23:59:59 ——
// 否则 lte "2026-10-22" 会把当天 21:54 的流水**漏掉**,
// 而症状是"今天的流水少了一半",没有任何报错。
func rangeFilters(member, from, to string) []cubeclient.CubeFilter {
	var out []cubeclient.CubeFilter
	if from != "" {
		out = append(out, cuberead.Filter(member, "gte", dayStart(from)))
	}
	if to != "" {
		out = append(out, cuberead.Filter(member, "lte", dayEnd(to)))
	}
	return out
}

// dayStart / dayEnd 把时间边界归一成**源库墙钟**格式。
//
// # ⚠️ 为什么必须归一(2026-10-23 生产实测)
//
// 前端(Flutter)发的是 **RFC3339 UTC**(DateX.toIsoUtc ⇒ "2026-10-21T16:00:00Z"),
// 而源库(思迅 MSSQL)存的是**本地墙钟** —— `oper_date` 是 datetime,值形如
// "2026-10-22 21:54:52",**不带时区**。
//
// ⇒ 直接把 "2026-10-21T16:00:00Z" 丢给 MSSQL 当 datetime 比较:
//   ① 'T' 与 'Z' 让它**转换失败**(SQL Server 认 ISO8601 的 'T' 分隔符要靠
//      特定 SET LANGUAGE / dateformat,默认会报错或截断);
//   ② 就算勉强转过去,UTC 与墙钟**差 8 小时** ⇒ 流水被切掉一整天。
//
// ⇒ 三种输入都收,统一归一成 "YYYY-MM-DD HH:MM:SS"(**业务时区墙钟**):
//
//	"2026-10-22"             → "2026-10-22 00:00:00" / "2026-10-22 23:59:59"
//	"2026-10-22 08:30:00"   → 原样
//	"2026-10-21T16:00:00Z"  → "2026-10-22 00:00:00"(转成业务时区)
//
// # ⚠️ 只给"日"的时候必须补 23:59:59
//
// 否则 lte "2026-10-22" 会把当天 21:54 的流水**漏掉**,
// 而症状是"今天的流水少了一半",没有任何报错。
func dayStart(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 10 {
		return s + " 00:00:00"
	}
	return normalizeToWallClock(s)
}

func dayEnd(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 10 {
		return s + " 23:59:59"
	}
	return normalizeToWallClock(s)
}

// normalizeToWallClock 把 RFC3339 之类带时区的时间串转成业务时区墙钟。
//
// 已经是 "YYYY-MM-DD HH:MM:SS" 的原样返回(源库就是这个格式)。
// 解析不出来也原样返回 ⇒ 让下游(cube / MSSQL)去报错,
// **不要**在这里悄悄改写成一个猜出来的值。
func normalizeToWallClock(s string) string {
	if s == "" {
		return s
	}
	// 已经像源库格式,不用转。
	if len(s) == 19 && s[10] == ' ' {
		return s
	}
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.In(bizLoc).Format("2006-01-02 15:04:05")
		}
	}
	return s
}

// cubeListSales 走 cube 的销售流水。
//
// ⚠️ 关键字要查**两次**(单号 / 随机单号)再在 Go 里合并:
// cube **没有 OR 算子** ⇒ 一次查询表达不了"命中 A 或 B"。
// 两次都取"够翻到第 N 页"的行数,合并后按 id 去重再切页。
//
// ⚠️ total 是两者之和(**上界**):两次的结果集可能有重叠。
// 前端只用 total 判断"还有没有下一页" ⇒ 上界可接受。
func (s *Service) cubeListSales(ctx context.Context, q SaleQuery) (*model.SaleFlowPage, error) {
	fetch := q.PageSize
	if q.Page > 1 {
		fetch = q.Page * q.PageSize
	}
	type hit struct {
		rows  []map[string]any
		total int
	}
	var hits []hit
	if kw := strings.TrimSpace(q.Q); kw != "" {
		for _, member := range []string{"sale_detail.order_no", "sale_detail.random_order_no"} {
			cq := cubeclient.CubeQuery{
				Measures:   []string{"sale_detail.count"},
				Dimensions: saleDetailDims(),
				Filters: append(saleFilters(q),
					cuberead.Filter(member, cubeclient.OperatorContainsCI, kw)),
				Order:  []cubeclient.CubeOrder{{ID: "sale_detail.order_no", Order: "desc"}},
				Limit:  fetch,
				Total:  true,
			}
			rows, total, err := s.reader.LoadCubeQueryPage(ctx, "sale_detail", cq)
			if err != nil {
				return nil, err
			}
			hits = append(hits, hit{rows: rows, total: total})
		}
	} else {
		cq := cubeclient.CubeQuery{
			Measures:   []string{"sale_detail.count"},
			Dimensions: saleDetailDims(),
			Filters:    saleFilters(q),
			Order:      []cubeclient.CubeOrder{{ID: "sale_detail.order_no", Order: "desc"}},
			Limit:      fetch,
			Offset:     (q.Page - 1) * q.PageSize,
			Total:      true,
		}
		rows, total, err := s.reader.LoadCubeQueryPage(ctx, "sale_detail", cq)
		if err != nil {
			return nil, err
		}
		hits = append(hits, hit{rows: rows, total: total})
	}

	seen := make(map[string]bool)
	merged := make([]map[string]any, 0, len(hits)*fetch)
	sum := 0
	for _, h := range hits {
		sum += h.total
		for _, r := range h.rows {
			id := cuberead.Str(r, "sale_detail.id")
			if seen[id] {
				continue
			}
			seen[id] = true
			merged = append(merged, r)
		}
	}
	items := make([]model.SaleFlowItem, 0, len(merged))
	for _, r := range merged {
		items = append(items, saleFlowItemOf(r))
	}
	if len(items) > 0 {
		// 合并之后才知道真正的第 N 页在哪 ⇒ 在 Go 里切页。
		start := (q.Page - 1) * q.PageSize
		if start >= len(items) {
			items = nil
		} else {
			end := start + q.PageSize
			if end > len(items) {
				end = len(items)
			}
			items = items[start:end]
		}
	}
	return &model.SaleFlowPage{
		Items:      items,
		Count:      len(items),
		Page:       q.Page,
		PageSize:   q.PageSize,
		Total:      sum,
		DataSource: "cube",
	}, nil
}

// GetSale 按 id 查单条销售流水。
func (s *Service) GetSale(ctx context.Context, branchID, id string) (*model.SaleFlowItem, error) {
	useCube, err := s.readPath(ctx, branchID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, model.ErrSaleFlowNotFound
	}
	if useCube {
		rows, _, err := s.reader.LoadCubeQueryPage(ctx, "sale_detail", cubeclient.CubeQuery{
			Measures:   []string{"sale_detail.count"},
			Dimensions: saleDetailDims(),
			Filters: []cubeclient.CubeFilter{
				cuberead.Filter("sale_detail.order_no", cubeclient.OperatorEquals, id),
			},
			Limit: 2,
		})
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, model.ErrSaleFlowNotFound
		}
		item := saleFlowItemOf(rows[0])
		return &item, nil
	}
	if s.db == nil {
		return nil, model.ErrLocalStoreNotReady
	}
	var row model.SaleFlow
	err = s.db.WithContext(ctx).
		Where("order_no = ? AND branch_id = ?", id, branchID).
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return nil, model.ErrSaleFlowNotFound
	}
	if err != nil {
		return nil, err
	}
	item := saleFlowItemOfLocal(row)
	return &item, nil
}

// saleFlowItemOf 把 cube 行组装成响应项。
func saleFlowItemOf(r map[string]any) model.SaleFlowItem {
	salePrice := cuberead.Dec(r, "sale_detail.sale_price")
	sourcePrice := cuberead.Dec(r, "sale_detail.source_price")
	qty := cuberead.Dec(r, "sale_detail.qty")
	item := model.SaleFlowItem{
		ID:              cuberead.Str(r, "sale_detail.order_no"),
		OrderNo:         cuberead.Str(r, "sale_detail.order_no"),
		RandomOrderNo:   cuberead.Str(r, "sale_detail.random_order_no"),
		BranchID:        cuberead.Str(r, "sale_detail.branch_no"),
		SoldAt:          cuberead.Time(cuberead.Str(r, "sale_detail.sold_at")),
		ProductID:       cuberead.Str(r, "sale_detail.product_id"),
		SubCode:         cuberead.Str(r, "product.sub_code"),
		ProductName:     cuberead.Str(r, "product.name"),
		Spec:            cuberead.Str(r, "product.spec"),
		BrandName:       cuberead.Str(r, "product.brand_name"),
		Unit:            cuberead.Str(r, "product.unit"),
		SaleWay:         cuberead.Str(r, "sale_detail.sale_way"),
		CounterNo:       cuberead.Str(r, "sale_detail.counter_no"),
		CashierID:       cuberead.Str(r, "sale_detail.cashier_id"),
		SalesmanID:      cuberead.Str(r, "sale_detail.salesman_id"),
		Qty:             cuberead.MoneyPtr(&qty),
		SaleAmount:      cuberead.MoneyPtr(cuberead.DecPtr(r, "sale_detail.sale_amount")),
		DiscountAmount:  cuberead.MoneyPtr(cuberead.DecPtr(r, "sale_detail.discount_amount")),
		SalePrice:       cuberead.MoneyPtr(&salePrice),
		SourcePrice:     cuberead.MoneyPtr(&sourcePrice),
		OnlineFlag:      cuberead.Str(r, "sale_detail.online_flag"),
	}
	applyDerived(&item, salePrice, sourcePrice, qty)
	return item
}

// saleFlowItemOfLocal 把本地行组装成响应项(**同款结构**)。
func saleFlowItemOfLocal(r model.SaleFlow) model.SaleFlowItem {
	item := model.SaleFlowItem{
		ID:             r.ID,
		OrderNo:        r.OrderNo,
		RandomOrderNo:  r.RandomOrderNo,
		BranchID:       r.SourceBranch,
		SoldAt:         r.SoldAt.Format(time.RFC3339),
		ProductID:      r.ProductID,
		SubCode:        r.SubCode,
		ProductName:    r.ProductName,
		Spec:           r.Spec,
		BrandName:      r.BrandName,
		Unit:           r.Unit,
		SaleWay:        r.SaleWay,
		CounterNo:      r.CounterNo,
		CounterName:    r.CounterName,
		CashierID:      r.CashierID,
		CashierName:    r.CashierName,
		SalesmanID:     r.SalesmanID,
		SalesmanName:   r.SalesmanName,
		Qty:            cuberead.MoneyPtr(&r.Qty),
		SaleAmount:     cuberead.MoneyPtr(&r.SaleAmount),
		DiscountAmount: cuberead.MoneyPtr(&r.DiscountAmount),
		SalePrice:      cuberead.MoneyPtr(&r.SalePrice),
		SourcePrice:    cuberead.MoneyPtr(&r.SourcePrice),
		OnlineFlag:     r.OnlineFlag,
		PayCardNo:      r.PayCardNo,
		LetGoodsType:   r.LetGoodsType,
	}
	applyDerived(&item, r.SalePrice, r.SourcePrice, r.Qty)
	return item
}

// applyDerived 填两个**派生列**(两条路径共用同一份实现)。
//
//	source_amount = source_price × qty
//	discount_rate = sale_price ÷ source_price × 100
//
// ⚠️ **source_price = 0 ⇒ discount_rate 为 null**,绝不编 100%。
//
//	"没维护原价"被显示成"零折" ⇒ 用户会以为商品没打折,
//	而真相是"原价没维护",这个价没法算折扣。
func applyDerived(item *model.SaleFlowItem, salePrice, sourcePrice, qty decimal.Decimal) {
	sourceAmount := sourcePrice.Mul(qty)
	item.SourceAmount = cuberead.MoneyPtr(&sourceAmount)
	if sourcePrice.IsZero() {
		item.DiscountRate = nil
		return
	}
	rate := salePrice.Div(sourcePrice).Mul(decimal.NewFromInt(100)).StringFixed(2)
	item.DiscountRate = &rate
}

// localListSales 走本域 PG 的销售流水。
func (s *Service) localListSales(ctx context.Context, branchID string, q SaleQuery) (*model.SaleFlowPage, error) {
	if s.db == nil {
		return nil, model.ErrLocalStoreNotReady
	}
	tx := s.db.WithContext(ctx).Model(&model.SaleFlow{}).Where("branch_id = ?", branchID)
	if kw := strings.TrimSpace(q.Q); kw != "" {
		like := "%" + strings.ToLower(kw) + "%"
		// ⚠️ **必须自己加括号**:SQL 里 AND 的优先级高于 OR
		// ⇒ 不加括号会把**别的门店**的行也带出来。
		tx = tx.Where("(LOWER(order_no) LIKE ? OR LOWER(random_order_no) LIKE ?)", like, like)
	}
	if q.From != "" {
		tx = tx.Where("sold_at >= ?", dayStart(q.From))
	}
	if q.To != "" {
		tx = tx.Where("sold_at <= ?", dayEnd(q.To))
	}
	if q.BranchID != "" {
		tx = tx.Where("source_branch_no = ?", q.BranchID)
	}
	if q.ProductID != "" {
		tx = tx.Where("product_id = ?", q.ProductID)
	}
	if q.CategoryID != "" {
		tx = tx.Where("category_id = ?", q.CategoryID)
	}
	if q.SaleWay != "" {
		tx = tx.Where("sale_way = ?", q.SaleWay)
	}
	if q.CashierID != "" {
		tx = tx.Where("cashier_id = ?", q.CashierID)
	}
	if q.SalesmanID != "" {
		tx = tx.Where("salesman_id = ?", q.SalesmanID)
	}
	if q.CounterNo != "" {
		tx = tx.Where("counter_no = ?", q.CounterNo)
	}
	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, err
	}
	var rows []model.SaleFlow
	if err := tx.Order("sold_at DESC, id").
		Offset((q.Page - 1) * q.PageSize).
		Limit(q.PageSize).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]model.SaleFlowItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, saleFlowItemOfLocal(r))
	}
	return &model.SaleFlowPage{
		Items:      items,
		Count:      len(items),
		Page:       q.Page,
		PageSize:   q.PageSize,
		Total:      int(total),
		DataSource: "local",
	}, nil
}

// ---- 收银流水 ----

// ListPayments 分页查收银流水。
func (s *Service) ListPayments(ctx context.Context, branchID string, q PayQuery) (*model.PayFlowPage, error) {
	useCube, err := s.readPath(ctx, branchID)
	if err != nil {
		return nil, err
	}
	q = q.normalized()
	if useCube {
		return s.cubeListPayments(ctx, q)
	}
	return s.localListPayments(ctx, branchID, q)
}

// payFilters 构造收款流水的过滤条件(两条路径同一套口径)。
func payFilters(q PayQuery) []cubeclient.CubeFilter {
	var out []cubeclient.CubeFilter
	if q.BranchID != "" {
		out = append(out, cuberead.Filter("payflow.branch_no",
			cubeclient.OperatorEquals, q.BranchID))
	}
	if q.PayWay != "" {
		out = append(out, cuberead.Filter("payflow.pay_way",
			cubeclient.OperatorEquals, q.PayWay))
	}
	if q.CashierID != "" {
		out = append(out, cuberead.Filter("payflow.cashier_id",
			cubeclient.OperatorEquals, q.CashierID))
	}
	if q.CardNo != "" {
		out = append(out, cuberead.Filter("payflow.card_no",
			cubeclient.OperatorEquals, q.CardNo))
	}
	if q.VipNo != "" {
		out = append(out, cuberead.Filter("payflow.vip_no",
			cubeclient.OperatorEquals, q.VipNo))
	}
	out = append(out, rangeFilters("payflow.paid_at", q.From, q.To)...)
	return out
}

// cubeListPayments 走 cube 的收款流水。
func (s *Service) cubeListPayments(ctx context.Context, q PayQuery) (*model.PayFlowPage, error) {
	fetch := q.PageSize
	if q.Page > 1 {
		fetch = q.Page * q.PageSize
	}
	type hit struct {
		rows  []map[string]any
		total int
	}
	var hits []hit
	if kw := strings.TrimSpace(q.Q); kw != "" {
		for _, member := range []string{"payflow.order_no", "payflow.random_order_no"} {
			cq := cubeclient.CubeQuery{
				Measures:   []string{"payflow.count"},
				Dimensions: payFlowDims(),
				Filters: append(payFilters(q),
					cuberead.Filter(member, cubeclient.OperatorContainsCI, kw)),
				Order:  []cubeclient.CubeOrder{{ID: "payflow.order_no", Order: "desc"}},
				Limit:  fetch,
				Total:  true,
			}
			rows, total, err := s.reader.LoadCubeQueryPage(ctx, "payflow", cq)
			if err != nil {
				return nil, err
			}
			hits = append(hits, hit{rows: rows, total: total})
		}
	} else {
		cq := cubeclient.CubeQuery{
			Measures:   []string{"payflow.count"},
			Dimensions: payFlowDims(),
			Filters:    payFilters(q),
			Order:      []cubeclient.CubeOrder{{ID: "payflow.order_no", Order: "desc"}},
			Limit:      fetch,
			Offset:     (q.Page - 1) * q.PageSize,
			Total:      true,
		}
		rows, total, err := s.reader.LoadCubeQueryPage(ctx, "payflow", cq)
		if err != nil {
			return nil, err
		}
		hits = append(hits, hit{rows: rows, total: total})
	}
	seen := make(map[string]bool)
	merged := make([]map[string]any, 0, len(hits)*fetch)
	sum := 0
	for _, h := range hits {
		sum += h.total
		for _, r := range h.rows {
			id := cuberead.Str(r, "payflow.id")
			if seen[id] {
				continue
			}
			seen[id] = true
			merged = append(merged, r)
		}
	}
	items := make([]model.PayFlowItem, 0, len(merged))
	for _, r := range merged {
		items = append(items, payFlowItemOf(r))
	}
	if len(items) > 0 {
		start := (q.Page - 1) * q.PageSize
		if start >= len(items) {
			items = nil
		} else {
			end := start + q.PageSize
			if end > len(items) {
				end = len(items)
			}
			items = items[start:end]
		}
	}
	return &model.PayFlowPage{
		Items:      items,
		Count:      len(items),
		Page:       q.Page,
		PageSize:   q.PageSize,
		Total:      sum,
		DataSource: "cube",
	}, nil
}

// payFlowItemOf 把 cube 行组装成响应项。
func payFlowItemOf(r map[string]any) model.PayFlowItem {
	rate := cuberead.Dec(r, "payflow.exchange_rate")
	payAmount := cuberead.Dec(r, "payflow.pay_amount")
	item := model.PayFlowItem{
		ID:            cuberead.Str(r, "payflow.order_no"),
		OrderNo:       cuberead.Str(r, "payflow.order_no"),
		RandomOrderNo: cuberead.Str(r, "payflow.random_order_no"),
		BranchID:      cuberead.Str(r, "payflow.branch_no"),
		PaidAt:        cuberead.Time(cuberead.Str(r, "payflow.paid_at")),
		SaleWay:       cuberead.Str(r, "payflow.sale_way"),
		PayWay:        cuberead.Str(r, "payflow.pay_way"),
		SaleAmount:    cuberead.MoneyPtr(cuberead.DecPtr(r, "payflow.sale_amount")),
		ExchangeRate:  cuberead.MoneyPtr(&rate),
		PayAmount:     cuberead.MoneyPtr(&payAmount),
		CardNo:        cuberead.Str(r, "payflow.card_no"),
		VipNo:         cuberead.Str(r, "payflow.vip_no"),
		CashierID:     cuberead.Str(r, "payflow.cashier_id"),
		CounterNo:     cuberead.Str(r, "payflow.counter_no"),
		SalesmanID:    cuberead.Str(r, "payflow.salesman_id"),
	}
	// rmb_amount = pay_amount × exchange_rate(派生,源库没有这一列)。
	rmb := payAmount.Mul(rate)
	item.RmbAmount = cuberead.MoneyPtr(&rmb)
	return item
}

// localListPayments 走本域 PG 的收款流水。
func (s *Service) localListPayments(ctx context.Context, branchID string, q PayQuery) (*model.PayFlowPage, error) {
	if s.db == nil {
		return nil, model.ErrLocalStoreNotReady
	}
	tx := s.db.WithContext(ctx).Model(&model.PayFlow{}).Where("branch_id = ?", branchID)
	if kw := strings.TrimSpace(q.Q); kw != "" {
		like := "%" + strings.ToLower(kw) + "%"
		// ⚠️ **必须自己加括号**:SQL 里 AND 的优先级高于 OR
		// ⇒ 不加括号会把**别的门店**的行也带出来。
		tx = tx.Where("(LOWER(order_no) LIKE ? OR LOWER(random_order_no) LIKE ?)", like, like)
	}
	if q.From != "" {
		tx = tx.Where("paid_at >= ?", dayStart(q.From))
	}
	if q.To != "" {
		tx = tx.Where("paid_at <= ?", dayEnd(q.To))
	}
	if q.BranchID != "" {
		tx = tx.Where("source_branch_no = ?", q.BranchID)
	}
	if q.PayWay != "" {
		tx = tx.Where("pay_way = ?", q.PayWay)
	}
	if q.CashierID != "" {
		tx = tx.Where("cashier_id = ?", q.CashierID)
	}
	if q.CardNo != "" {
		tx = tx.Where("card_no = ?", q.CardNo)
	}
	if q.VipNo != "" {
		tx = tx.Where("vip_no = ?", q.VipNo)
	}
	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, err
	}
	var rows []model.PayFlow
	if err := tx.Order("paid_at DESC, id").
		Offset((q.Page - 1) * q.PageSize).
		Limit(q.PageSize).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]model.PayFlowItem, 0, len(rows))
	for _, r := range rows {
		rate := r.ExchangeRate
		payAmount := r.PayAmount
		item := model.PayFlowItem{
			ID:            r.ID,
			OrderNo:       r.OrderNo,
			RandomOrderNo: r.RandomOrderNo,
			BranchID:      r.SourceBranch,
			PaidAt:        r.PaidAt.Format(time.RFC3339),
			SaleWay:       r.SaleWay,
			PayWay:        r.PayWay,
			PayWayName:    r.PayWayName,
			SaleAmount:    cuberead.MoneyPtr(&r.SaleAmount),
			ExchangeRate:  cuberead.MoneyPtr(&rate),
			PayAmount:     cuberead.MoneyPtr(&payAmount),
			CardNo:        r.CardNo,
			VipNo:         r.VipNo,
			CashierID:     r.CashierID,
			CashierName:   r.CashierName,
			CounterNo:     r.CounterNo,
			SalesmanID:    r.SalesmanID,
		}
		rmb := payAmount.Mul(rate)
		item.RmbAmount = cuberead.MoneyPtr(&rmb)
		items = append(items, item)
	}
	return &model.PayFlowPage{
		Items:      items,
		Count:      len(items),
		Page:       q.Page,
		PageSize:   q.PageSize,
		Total:      int(total),
		DataSource: "local",
	}, nil
}
