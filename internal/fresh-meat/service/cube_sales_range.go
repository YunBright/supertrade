// Package service / cube_sales_range.go —— **一次查回整条链的销售**(2026-11-01 性能优化)。
//
// # 为什么有这个文件(生产实测)
//
//	毛肉看板的成本链是**逐日**推进的(cut_chain.go 的 computeCutChainDays):
//	从链起点一天一天往前接,每一天都要问一次 cube「这天卖了什么」
//	⇒ 远程往返次数 = 链的天数 ⇒ 耗时随日期**线性增长**。
//
//	生产实测(店 00):10-29 ≈780ms、10-31 ≈1.5s、11-01 ≈2.0s、11-05 ≈3.3s,
//	每次远程查询约 330ms ⇒ 天数就是耗时的主因。
//
// ⇒ 改法:**一次查回区间内所有天的销售**,再按天分发 ⇒ N 次远程往返变 1 次。
//
// # ⚠️ 硬要求:数值**一个字节都不变**
//
//	这不是"重写口径",只是把 N 次远程查询合成 1 次:
//
//	逐日查询:filters = [oper_date equals D] + [item_no in 登记过的货号],
//	          dimensions = [item_no]            ⇒ 行 = (营业日, 货号) 的净销售
//	区间查询:filters = [oper_date gte from] + [oper_date lte to] + [item_no in 登记过的货号],
//	          dimensions = [oper_date, item_no]  ⇒ 行 = (营业日, 货号) 的净销售
//
//	⇒ 同一个 measure、同一批货号、同一个 (营业日, 货号) 分组
//	⇒ **每个 (营业日, 货号) 的净销量/净销售额与逐日查询逐字相同**。
//
// # 口径归属(不变)
//
//	销售数据来自思迅**日汇总表** t_rm_daysum,**不是**明细流水聚合 ——
//	见 cube_sales.go 的文件头(那里的取舍说明一字不改)。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead"
)

// cube 侧 sales_report 的成员名与算子名(集中成常量,别散着写裸字符串)。
//
// ⚠️ 算子名是**跨仓契约**(cube 编译成 SQL 语义),拼错了要到运行时才知道
// (400 unsupported operator)⇒ 用常量。
const (
	// cubeSalesModel 是思迅日汇总表对应的 cube model。
	cubeSalesModel = "sales_report"
	// cubeSalesOperDateMember 是**营业日**维度(思迅源库里是 char(10) 字符串)。
	cubeSalesOperDateMember = "sales_report.oper_date"
	// cubeSalesItemNoMember 是**货号**维度(= 本域 branch_cut_mappings.cube_product_id)。
	cubeSalesItemNoMember = "sales_report.item_no"
	// cubeSalesTotalNetQtyMember / cubeSalesTotalNetAmountMember 是两个净销售度量。
	cubeSalesTotalNetQtyMember    = "sales_report.total_net_qty"
	cubeSalesTotalNetAmountMember = "sales_report.total_net_amount"

	// cubeSalesOpGTE / cubeSalesOpLTE 是**闭区间**过滤的算子。
	//
	// ⚠️ **必须用 gte/lte 的字面值比较,不能 CAST / inDateRange**:
	//	oper_date 在源库是 char(10) 字符串,CAST 会让百万行全表扫描
	//	(实测 7 天区间 225ms)。同款先例见 internal/salesagg/cubeq 的 DateFilters。
	cubeSalesOpGTE = "gte"
	cubeSalesOpLTE = "lte"
)

// errCubeSalesRangeUnusable 表示**这份区间数据不能用**(调用方必须丢弃预取、回落逐日查询)。
//
// ⚠️ 这不是"查询失败",而是"这份数据**不可信**" ⇒ 调用方必须**丢弃预取**、回落逐日查询。
//
//	宁可慢,不可算错(少算几天销售 = 成本链断链 = 毛利虚高)。
//
// ⚠️ **"不可信"有三种**(都在 loadCubeSalesRange 里判):
//
//	① 数据被 limit 截断(total > len(rows));
//	② 某行**归不到区间内的某一天**(取不到日期,或日期不在区间里)
//	   —— 说明 cube 没用我们要求的形状/过滤 ⇒ 不能信任它与逐日查询等价;
//	③ 某一天的行数超过逐日查询的上限 ⇒ 两边行集可能不同。
var errCubeSalesRangeUnusable = errors.New("fresh-meat: 区间销售数据不可信,必须回落逐日查询")

// 区间查询的行数上限。
const (
	// cubeSalesDailyRowLimit 是**逐日**查询的行数上限(loadCubeSales 用)。
	//
	// ⚠️ 区间预取必须与它**保持一致**:逐日查询会被这个上限截断,而区间查询不会
	//	⇒ 两者可能给出不同的行集 ⇒ 数值会不一样。
	//	⇒ 区间预取里**任何一天**的行数超过它,就整份丢掉(见 loadCubeSalesRange)。
	cubeSalesDailyRowLimit = 200
	// cubeSalesRangeRowLimitPerDay 是区间查询**每一天**预留的行数。
	//
	//	鲜肉域关心的货号只有几十个(实测店 00 是 34 个)⇒ 每天每号一行 ⇒ 40 够用。
	cubeSalesRangeRowLimitPerDay = 40
	// cubeSalesRangeRowLimitExtra 是区间查询的固定余量。
	cubeSalesRangeRowLimitExtra = 50
	// cubeSalesRangeRowLimitMax 是区间查询行数上限的**天花板**。
	//
	//	⚠️ 区间太长时(例如按年查)行数会爆 ⇒ 封顶 ⇒ 触发截断保护 ⇒ 回落逐日查询
	//	(慢,但数字对)。
	cubeSalesRangeRowLimitMax = 20000
)

// cubeSalesRangeCtxKey 是"预取的区间销售"在 ctx 里的键。
//
// ⚠️ 用**私有类型**做键(不是 string)⇒ 别的包不可能撞上这个键。
type cubeSalesRangeCtxKey struct{}

// withCubeSalesRange 把预取的销售行挂到 ctx 上(返回**新的** ctx,原 ctx 不变)。
//
// # 为什么用 ctx 传(而不是给一串函数签名加参数)
//
//	预取结果要跨过 computeCutChainDays → runner.day → computeCutDailyCostsChained
//	→ loadSkuFlows → loadCubeSalesInto 这 5 层调用,而每层的签名都还有别的用途。
//	⇒ 给每层加一个参数 = 改 5 个签名 + 5 个调用点 ⇒ 以后每加一层都得记得传。
//
//	⇒ ctx 是 Go 里**唯一**天然贯穿调用栈的载体。
//
// # 为什么**安全**(不会跨请求串数据)
//
//	ctx 是**请求级**的:每个 HTTP 请求自己建一个 ctx,请求结束即丢弃
//	⇒ 挂在上面的预取结果最多活一个请求 ⇒ 绝不会把 A 店的预取喂给 B 店。
//
//	⚠️ 反过来说:**不要**把它做成 Service 字段或包级变量
//	⇒ 那才是跨请求串数据(店 00 的销售会被喂给店 01)。
func withCubeSalesRange(ctx context.Context, rows map[string][]cubeSaleRow) context.Context {
	return context.WithValue(ctx, cubeSalesRangeCtxKey{}, rows)
}

// cubeSalesRangeFrom 从 ctx 里取预取的销售行。
//
// 第二个返回值 = 有没有预取 ⇒ **没有预取不是错误**(调用方走原来的逐日查询)。
//
// ⚠️ 返回的 map 里**每一天都有键**(哪怕那天没卖 ⇒ 空切片)
//
//	⇒ 调用方能用"键在不在"区分「这天在预取区间里、但没卖」与「这天不在预取区间里」
//	⇒ 后者必须回落逐日查询,绝不能当成"这天没卖"(那会让成本链凭空断一天)。
func cubeSalesRangeFrom(ctx context.Context) (map[string][]cubeSaleRow, bool) {
	if ctx == nil {
		return nil, false
	}
	v := ctx.Value(cubeSalesRangeCtxKey{})
	if v == nil {
		return nil, false
	}
	rows, ok := v.(map[string][]cubeSaleRow)
	if !ok {
		return nil, false
	}
	return rows, true
}

// loadCubeSalesRange 一次查回 [fromKey, toKey] 区间里**每一天**的销售(按 SKU 聚合)。
//
// # 返回
//
//	map[营业日键][]cubeSaleRow —— 键是 'YYYY-MM-DD',
//	与 loadCubeSalesInto 算 operDate 的表达式**逐字相同**
//	(都是 day.In(bizTZ).Format("2006-01-02"))。
//
//	⚠️ 区间内**每一天都有键**(没卖的那天是空切片)⇒ 调用方能区分
//	「区间内、这天没卖」与「区间外」。
//
// # 不可信 ⇒ 整份作废(⚠️ 宁可慢,不可算错)
//
//	cube 返的 total 是"去掉 limit 之后的总行数"(COUNT(*) OVER(),在 GROUP BY 之后求值)
//	⇒ total > len(rows) ⇒ 行被 limit 截掉了 ⇒ 这份数据**不能用**
//	⇒ 返 errCubeSalesRangeUnusable,由调用方丢弃预取、回落逐日查询。
//
//	⚠️ 还有两种"不可信":
//
//	① **某一行归不到区间内的某一天**(取不到日期,或日期不在 [from,to] 里)
//	   ⇒ cube 没用我们要求的形状/过滤 ⇒ 不能信任它与逐日查询等价。
//	② **某一天**的行数超过逐日查询的上限(cubeSalesDailyRowLimit)
//	   ⇒ 逐日查询那天会被截断、而区间查询不会 ⇒ 两边行集不同 ⇒ 数值会不一样。
//
//	⇒ 三种都是**整份丢掉**,不是"丢掉那一行"。
//	⚠️ **静默丢行 = 把「这天没卖」当成事实** ⇒ 营收被当成 0 ⇒ 成本链凭空断链
//	⇒ 成本塌成残值 ⇒ **毛利虚高**,而且零报错。
//	⇒ 宁可退回慢路径(逐日查),也不能让数字错。
func (s *Service) loadCubeSalesRange(ctx context.Context, branchID, fromKey, toKey string,
	skus []string,
) (map[string][]cubeSaleRow, error) {
	if s.cubeReader == nil {
		return nil, fmt.Errorf("%w: fresh-meat 未装配 cube 读端", ErrCubeUnavailable)
	}
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	fromKey = strings.TrimSpace(fromKey)
	toKey = strings.TrimSpace(toKey)
	if fromKey == "" || toKey == "" {
		return nil, fmt.Errorf("%w: 区间起止日期必填(YYYY-MM-DD)", ErrInvalidInput)
	}
	if toKey < fromKey {
		return nil, fmt.Errorf("%w: 区间起止日期颠倒(%s > %s)", ErrInvalidInput, fromKey, toKey)
	}

	filters := []cubeclient.CubeFilter{
		cuberead.Filter(cubeSalesOperDateMember, cubeSalesOpGTE, fromKey),
		cuberead.Filter(cubeSalesOperDateMember, cubeSalesOpLTE, toKey),
	}
	if len(skus) > 0 {
		filters = append(filters, cuberead.InFilter(cubeSalesItemNoMember, skus))
	}

	days := cubeSalesRangeDays(fromKey, toKey)
	limit := cubeSalesRangeLimit(days)
	rows, total, err := s.cubeReader.LoadCubeQueryPage(ctx, cubeSalesModel, cubeclient.CubeQuery{
		Measures:   cubeSalesMeasures,
		Dimensions: []string{cubeSalesOperDateMember, cubeSalesItemNoMember},
		Filters:    filters,
		// ⚠️ **必须限量**:没有 limit 的查询会拉回整张日汇总表。
		//	区间查询的行数 ≈ 天数 × 货号数 ⇒ limit 按天数放大(见 cubeSalesRangeLimit)。
		Limit: limit,
		// ⚠️ **必须 Total: true** —— 没有它就不知道数据有没有被 limit 截断
		//	⇒ 截断了也看不出来 ⇒ 少算几天销售 ⇒ 成本链断链 ⇒ 毛利虚高。
		Total: true,
	})
	if err != nil {
		return nil, fmt.Errorf("query cube %s(range): %w", cubeSalesModel, err)
	}
	if total > len(rows) {
		return nil, fmt.Errorf("%w: cube 返 %d 行、limit %d ⇒ 数据被截断",
			errCubeSalesRangeUnusable, total, limit)
	}

	// 区间内**每一天**都先占一个位(没卖 ⇒ 空切片)⇒ 调用方能判"这天在不在区间里"。
	out := make(map[string][]cubeSaleRow, days)
	for i := 0; i < days; i++ {
		out[cubeSalesRangeDayKey(fromKey, i)] = []cubeSaleRow{}
	}
	for _, row := range rows {
		day := cubeSalesRowDayKey(row[cubeSalesOperDateMember])
		// ⚠️⚠️ **归不到某一天 ⇒ 整份预取作废,不是只丢这一行**。
		//
		//	**为什么不能只丢这一行**(这是本文件最重要的一条):
		//
		//	丢掉它 ⇒ 那一天的预取结果变成"**空**" ⇒ 收方以为"这天没卖"
		//	⇒ 营收被当成 0 ⇒ 成本链凭空断链 ⇒ 成本塌成残值 ⇒ **毛利虚高**
		//	⇒ 而这一切**零报错**(门店只会看到"今天没卖东西")。
		//
		//	⇒ **静默丢行 = 把「这天没卖」当成事实** ⇒ 这正是 2026-10-27 踩过的坑
		//	(成本塌成期初残值、毛利率 97%)的同一种形状。
		//
		//	⇒ 宁可退回慢路径(逐日查),也不能让数字错。
		if day == "" {
			return nil, fmt.Errorf("%w: 有行取不到营业日 ⇒ cube 没按我们要求的形状返回",
				errCubeSalesRangeUnusable)
		}
		// ⚠️ 日期落在区间外 ⇒ 说明 cube 没用我们要求的过滤(它自己改了口径)
		//	⇒ 同样不能信任它与逐日查询等价 ⇒ 整份作废。
		if day < fromKey || day > toKey {
			return nil, fmt.Errorf("%w: 有行的营业日 %s 不在区间 [%s, %s] 内 ⇒ cube 没按我们要求的过滤返回",
				errCubeSalesRangeUnusable, day, fromKey, toKey)
		}
		sku := cuberead.Str(row, cubeSalesItemNoMember)
		if sku == "" {
			// ⚠️ 这里**可以**只丢这一行:逐日路径对空货号的行也是直接跳过
			//	⇒ 两边行集相同 ⇒ 数值相同。
			continue
		}
		out[day] = append(out[day], cubeSaleRow{
			SKU:        sku,
			QtyKg:      cubeclient.ParseDecimal(row[cubeSalesTotalNetQtyMember]),
			AmountYuan: cubeclient.ParseDecimal(row[cubeSalesTotalNetAmountMember]),
		})
	}
	// ⚠️ **逐日截断检测**:某天的行数超过逐日查询的上限 ⇒ 逐日查询那天会被截断、
	//	而区间查询不会 ⇒ 两边行集不同 ⇒ 数值会不一样 ⇒ 整份预取丢掉。
	for day, dayRows := range out {
		if len(dayRows) > cubeSalesDailyRowLimit {
			return nil, fmt.Errorf("%w: %s 有 %d 行,超过逐日上限 %d ⇒ 两边行集会不同",
				errCubeSalesRangeUnusable, day, len(dayRows), cubeSalesDailyRowLimit)
		}
	}
	return out, nil
}

// cubeSalesRowsForDay 取某一天的 cube 销售行:**优先用 ctx 里的预取**,没有就逐日查。
//
// ⚠️ **没预取不是错误** —— 那就是"这次没人预取"⇒ 走原来的逐日查询,行为一模一样。
//
// ⚠️ **必须判"这一天在不在预取区间里"**:区间外的日子(不该出现,但防御性地判一次)
//
//	⇒ 回落逐日查 ⇒ 绝不能把"区间外"当成"这天没卖"(那会让成本链凭空断一天)。
func (s *Service) cubeSalesRowsForDay(ctx context.Context, branchID, operDate string,
	skus []string,
) ([]cubeSaleRow, error) {
	if prefetched, ok := cubeSalesRangeFrom(ctx); ok {
		if rows, covered := prefetched[operDate]; covered {
			return rows, nil
		}
	}
	return s.loadCubeSales(ctx, branchID, operDate, skus)
}

// prefetchCubeSalesRange 预取 [start..to] 区间的销售,并挂到返回的 ctx 上。
//
// # 返回的 ctx
//
//	挂了预取 ⇒ 逐日路径不再远程查询;
//	没挂(无源门店 / 预取失败 / 被截断)⇒ 返回**原 ctx** ⇒ 逐日路径照旧。
//
// # 错误语义(⚠️ 与现在**逐字一致**)
//
//	① salesReadPath 报错 ⇒ **原样上抛**(403/401 原样抛,其它 503)
//	② 预取时 cube 报权限错(403/401)⇒ **原样上抛,不回落**
//	   (回落 = 把一次权限故障显示成「这天没卖东西」)
//	③ 其它预取错误 ⇒ **Warn 一条日志,不挂预取**
//	   ⇒ 让逐日路径自己去跑,产生**原本**的错误/语义
//	   (⚠️ 绝不能把一次故障显示成"这天没卖东西")
func (s *Service) prefetchCubeSalesRange(ctx context.Context, branchID string,
	start, to time.Time,
) (context.Context, error) {
	// 该店走不走 cube?问不到 / 被停用 ⇒ 原样上抛(语义与现在一致)。
	useCube, err := s.salesReadPath(ctx, branchID)
	if err != nil {
		return ctx, err
	}
	if !useCube {
		return ctx, nil
	}
	loc := s.bizTZOrUTC()
	fromKey := start.In(loc).Format("2006-01-02")
	toKey := to.In(loc).Format("2006-01-02")
	if toKey < fromKey {
		return ctx, nil
	}
	skus, _, err := s.mappedSkus(ctx, branchID)
	if err != nil {
		// 本地映射表读不出来 ⇒ 逐日路径也会读它 ⇒ 不预取(让它按原样报错)。
		return ctx, nil
	}
	if len(skus) == 0 {
		// 该店一个货号都没登记 ⇒ 没有猪肉销售可言(逐日路径也是这么处理的)。
		return ctx, nil
	}
	rows, err := s.loadCubeSalesRange(ctx, branchID, fromKey, toKey, skus)
	if err != nil {
		if errors.Is(err, cubeclient.ErrCubeForbidden) || errors.Is(err, cubeclient.ErrCubeAuthRejected) {
			// ⚠️ 权限类错误**原样上抛,不回落**(见上面的错误语义②)。
			return ctx, err
		}
		s.pubLogger.Warn("预取区间销售失败(数据不可信),回落逐日查询(数值不受影响,只是慢)",
			"branch_id", branchID, "from", fromKey, "to", toKey, "err", err)
		return ctx, nil
	}
	return withCubeSalesRange(ctx, rows), nil
}

// cubeSalesRangeDays 算区间天数(含头含尾);日期解析不了就当 1 天。
func cubeSalesRangeDays(fromKey, toKey string) int {
	from, err := time.Parse("2006-01-02", fromKey)
	if err != nil {
		return 1
	}
	to, err := time.Parse("2006-01-02", toKey)
	if err != nil {
		return 1
	}
	days := int(to.Sub(from).Hours()/24) + 1
	if days < 1 {
		return 1
	}
	return days
}

// cubeSalesRangeLimit 算区间查询的行数上限。
//
//	天数 × 每天预留(40 行)+ 余量(50 行),并封顶。
//
// ⚠️ **下限是逐日查询的上限**(200)⇒ 区间查询对任何一天都不会比逐日查询更严
//
//	⇒ 不会因为预取把某一天的数据查少。
func cubeSalesRangeLimit(days int) int {
	limit := days*cubeSalesRangeRowLimitPerDay + cubeSalesRangeRowLimitExtra
	if limit < cubeSalesDailyRowLimit {
		limit = cubeSalesDailyRowLimit
	}
	if limit > cubeSalesRangeRowLimitMax {
		limit = cubeSalesRangeRowLimitMax
	}
	return limit
}

// cubeSalesRangeDayKey 取区间里第 i 天的日期键(从 fromKey 起,逐日 +1)。
func cubeSalesRangeDayKey(fromKey string, i int) string {
	from, err := time.Parse("2006-01-02", fromKey)
	if err != nil {
		return fromKey
	}
	return from.AddDate(0, 0, i).Format("2006-01-02")
}

// cubeSalesRowDayKey 从一行 cube 返回里取**营业日键**('YYYY-MM-DD')。
//
// ⚠️ **必须稳健**:oper_date 的值可能是
//
//	字符串 'YYYY-MM-DD'(源列是 char(10),cube 侧 RTRIM 后原样返回),
//	time.Time(驱动把日期列解析成了时间),
//	[]byte(MSSQL 驱动的字符串形态)。
//
//	⇒ 统一取**前 10 位**(日期部分),与 day.In(bizTZ).Format("2006-01-02") 同形。
//
// ⚠️ 取不到日期的行**直接丢掉**:它没法归到任何一天 ⇒ 留在哪都是错的。
func cubeSalesRowDayKey(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case time.Time:
		return t.Format("2006-01-02")
	case string:
		return cubeSalesDayKeyPrefix(t)
	case []byte:
		return cubeSalesDayKeyPrefix(string(t))
	default:
		return cubeSalesDayKeyPrefix(fmt.Sprint(v))
	}
}

// cubeSalesDayKeyPrefix 取日期串的前 10 位(不够就原样返回)。
func cubeSalesDayKeyPrefix(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 10 {
		return s[:10]
	}
	return s
}
