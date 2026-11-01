// Package service / cube_sales.go —— **有源门店**的销售读取(cube 侧)。
//
// # 为什么需要这个文件(2026-10-26 门店反馈)
//
// 门店反馈:「10-01 设了期初、10-02 录了猪,但毛利看板看不到门店 00 的任何销售数据」。
//
// 生产实测的根因是**读错了地方**:
//
//	moving_average.go 的 loadSkuFlows 只读本地 PG 的 line_sales_by_pigs;
//	而这张表**只由 Dapr 的 sale.paid 事件写入**
//	⇒ 那是**无源门店**(店 10,本地 POS)的事件。
//
//	有源门店(店 00 = cube-sixun-ysx-fb,店 01 = cube-sixun-hbposv7-jiale)
//	的销售在**它自己的 cube 源里** ⇒ 本地 line_sales_by_pigs 对它永远是空的
//	⇒ 毛利看板永远是 revenue=0 / sold_kg=0 / 毛利 0。
//
// ⇒ **有源查 cube 源,无源查本地 PG**(与 cubesource.Decide 的三态判据同源)。
//   店与店之间**没有映射关系**:不因为"都在一个系统里"就把店 10 的事件算到店 00 头上。
//
// # 读哪个 cube model
//
//	`sales_report` —— 思迅**日汇总表** t_rm_daysum:
//
//		oper_date(营业日) × item_no(货号)
//		total_net_amount = SUM(sale_amt - ret_amt)  ← **已扣退货**
//		total_net_qty    = SUM(sale_qty - ret_qty)    ← **已扣退货**
//
// 为什么不用 `sale_detail`(逐笔流水 t_rm_saleflow):
//
//   - 它只有 count 一个 measure(其它 measure 全部 502),拿不到**按 SKU 聚合**的
//     kg/金额 ⇒ 要 1800 行逐笔拉回来在 Go 侧求和(实测店 00 单日 1803 行);
//   - 它按 sold_at(成交时刻)过滤,而鲜肉域的营业日是 bizTZ 的[当天 00:00, +24h)
//     ⇒ 跨零点的那几笔会落到不同营业日(口径漂移);
//   - **t_rm_daysum 的 oper_date 就是思迅的营业日** ⇒ 与鲜肉域的营业日语义天然对齐。
//
// ⚠️ 日汇总表 vs 逐笔流水的**已知差异**(实测店 00 / 2026-10-02):
//
//	日汇总(本文件):鲜肉 27 个 SKU / 172.0399 kg / 3480.30 元
//	逐笔流水:       鲜肉 247 行 / 164.5264 kg / 3364.20 元
//
// 差 7.5kg / 116 元 —— 源系统里"日汇总"与"流水"是两套落库,补录/回退会略有出入。
// ⇒ 选日汇总是**口径取舍**:营业日对齐 + 一次聚合查询;代价是与逐笔流水有微差。
//   这条取舍必须写在这里,免得以后有人以为两边本该完全相等。
//
// # 退货
//
// 日汇总的 net 口径**已经扣掉退货**(sale_amt - ret_amt)⇒
// 退货行天然不进 revenue/soldKg,不需要像本地路径那样再判 OrderStatus。
//
// # pig_id / 整猪归摊
//
// cube 侧**没有 pig_id** ⇒ 本文件**只产出 SKU 级 kg/金额**,
// 不做"归到哪头猪"的 FIFO 归摊。
//
// ⇒ 影响面要说清:毛利看板是**按部位**看的(门店口径:「这个模块只管部位数据」)
//   ⇒ SKU 级 kg/金额已经够算部位毛利;
//   而"整猪毛利"(归到具体猪)需要 pig_id ⇒ 有源门店**拿不到**。
//   这不是本文件造成的,是 cube 源本身没有这个维度。
//
// # oper_date 与 bizTZ
//
// cube 的 oper_date 是**源系统的营业日**(思迅按自己的结算日切),
// 本域的 bizTZ(Asia/Shanghai)只用来切本域自己产生的数据(猪进场/盘点/搬台)。
//
// ⇒ 两者**不强制相等**:本文件用 cube 的 oper_date 直接等值匹配一个日期,
//   查的就是"那个营业日"的销售,不做二次时区换算。
package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead"
	"github.com/shopspring/decimal"
)

// cubeSalesMeasures 是从 cube `sales_report` 取的度量集合。
//
// ⚠️ 必须带 count:没有它,调用方无法区分
//
//	「这家店这天一个货号都没卖」(真的 0 行)
//	「cube 侧这个 measure 没生效」(假 0 行)
//
// 而这两种在毛利报表上长得一模一样(都是 revenue=0)。
var cubeSalesMeasures = []string{
	"sales_report.total_net_amount",
	"sales_report.total_net_qty",
	"sales_report.count",
}

// cubeSaleRow 是 cube 侧一个货号一天的净销售。
type cubeSaleRow struct {
	// SKU 是思迅货号(= 本域 branch_cut_mappings.cube_product_id)。
	SKU string
	// QtyKg 是净销量(已扣退货)。
	QtyKg decimal.Decimal
	// AmountYuan 是净销售额(已扣退货)。
	AmountYuan decimal.Decimal
}

// salesSourceOf 说清"这次的销售收入是从哪读出来的"。
//
// # 为什么要有这个
//
// 门店反馈(2026-10-26):「毛利看板没看到任何销售数据」。
//
// 而"看不到销售"的成因**不止一种**:真没卖 / 数据源没读到 / 读了但过滤错了。
// ⇒ 门店无从判断 ⇒ 只能理解成"今天真没卖"。
//
// ⇒ 把来源**显式说出来**(cube / local),并给一句人话:
// 门店看到"读的是数据源"就知道要去找人查,而不是以为自己没卖。
//
// ⚠️ 三态与 cubesource.Decide 一致:问不到 / 被停用 ⇒ 上抛(不回落)。
func (s *Service) salesSourceOf(ctx context.Context, branchID string) (source, note string, err error) {
	useCube, err := s.salesReadPath(ctx, branchID)
	if err != nil {
		return "", "", err
	}
	if useCube {
		return "cube", "销售数据读自本门店的数据源（思迅）", nil
	}
	return "local", "销售数据读自本系统的收银记录", nil
}

// loadCubeSales 读**有源门店**某个营业日的净销售(按 SKU 聚合)。
//
// skus 是该门店关心的货号集合(来自 branch_cut_mappings)。
// 传空 ⇒ 不按货号过滤 ⇒ 返回该店当天**全部**货号的销售
// (⚠️ 调用方不要这么用:那会把非猪肉的货也算进毛利)。
//
// ⚠️ cube 的 `equals` **只接受单值**,多值必须用 `in`
// (实测:equals 传 2 个值 ⇒ 502;in 传 34 个值 ⇒ 正常)。
func (s *Service) loadCubeSales(ctx context.Context, branchID, operDate string, skus []string) ([]cubeSaleRow, error) {
	if s.cubeReader == nil {
		return nil, fmt.Errorf("%w: fresh-meat 未装配 cube 读端", ErrCubeUnavailable)
	}
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	operDate = strings.TrimSpace(operDate)
	if operDate == "" {
		return nil, fmt.Errorf("%w: oper_date 必填(YYYY-MM-DD)", ErrInvalidInput)
	}

	filters := []cubeclient.CubeFilter{
		cuberead.Filter("sales_report.oper_date", cubeclient.OperatorEquals, operDate),
	}
	if len(skus) > 0 {
		filters = append(filters, cuberead.InFilter("sales_report.item_no", skus))
	}

	rows, _, err := s.cubeReader.LoadCubeQueryPage(ctx, "sales_report", cubeclient.CubeQuery{
		Measures:   cubeSalesMeasures,
		Dimensions: []string{"sales_report.item_no"},
		Filters:    filters,
		// ⚠️ **必须限量**:没有 limit 的查询会拉回整张日汇总表
		// (实测店 00 单日 840 行 / 全店 1854 天)。
		// 鲜肉域关心的货号只有几十个 ⇒ 200 足够,且能兜住"映射表被改乱"的极端情况。
		//
		// ⚠️ 这个上限**必须与区间预取用的那个是同一个常量**(cubeSalesDailyRowLimit):
		//	逐日查询会被它截断、而区间查询不会 ⇒ 两边行集可能不同 ⇒ 数值会不一样
		//	⇒ 区间预取里任何一天超过这个行数,就整份丢掉(见 cube_sales_range.go)。
		Limit: cubeSalesDailyRowLimit,
		Total: true,
	})
	if err != nil {
		return nil, fmt.Errorf("query cube sales_report: %w", err)
	}

	out := make([]cubeSaleRow, 0, len(rows))
	for _, row := range rows {
		sku := cuberead.Str(row, "sales_report.item_no")
		if sku == "" {
			continue
		}
		out = append(out, cubeSaleRow{
			SKU:        sku,
			QtyKg:      cubeclient.ParseDecimal(row["sales_report.total_net_qty"]),
			AmountYuan: cubeclient.ParseDecimal(row["sales_report.total_net_amount"]),
		})
	}
	return out, nil
}
