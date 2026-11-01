// Package service_test —— **区间销售预取**(2026-11-01 性能优化)的不变量。
//
// # 为什么锁这几条
//
//	成本链是**逐日**推进的:每一天都要问一次 cube「这天卖了什么」
//	⇒ 远程往返次数 = 链的天数 ⇒ 耗时随日期**线性增长**
//	(生产实测 店 00:10-29 ≈780ms、10-31 ≈1.5s、11-01 ≈2.0s、11-05 ≈3.3s,
//	 每次远程查询约 330ms)。
//
//	改成「一次查回区间内所有天」之后,最怕的不是变慢,而是**数字变了**
//	⇒ 本文件锁的是**数值一个字节都不变**这件事:
//
//	① 批量查询的**形状**(oper_date 的 gte/lte 过滤 + item_no 的 in 过滤
//	   + 两个维度 + 按天数放大的 limit);
//	② 预取回来的行**按天分发**(不能串到别的日子);
//	③ 没有预取 ⇒ 仍走逐日查询(行为不变);
//	④ 数据被 limit 截断 ⇒ **丢弃预取**(宁可慢,不可算错);
//	⑤ **等价性**:有预取 / 无预取 算出来的逐部位与全店数字**完全相等**;
//	   且 cube 调用次数从 N 次降到 1 次(⇒ 预取真的被用上了)。
//
// # 判别力(⚠️ 这条最重要)
//
//	⑤ 的断言是"cube 调用次数 == 1"。
//	⇒ 把预取逻辑短路掉(例如不挂 ctx、或区间查询拼错)时,该断言**立刻红**。
//	⇒ 而"数字相等"这条断言单独看是**抓不到**短路掉的 —— 数字照样对,只是慢。
//	⇒ 两个断言合起来才既锁住"对"又锁住"快"。
package service_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead/cubereadfake"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// 本文件用的营业日(2026-10-28 ~ 11-01,均 = 当地 14:00 +08)。
//
// ⚠️ rgBaseDay 是**链的起点**(期初生效日)⇒ 链从那天起算 ⇒ 链有 5 天。
var (
	rgBaseDay = time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)
	rgDay1    = time.Date(2026, 10, 29, 6, 0, 0, 0, time.UTC)
	rgDay2    = time.Date(2026, 10, 30, 6, 0, 0, 0, time.UTC)
	rgDay3    = time.Date(2026, 10, 31, 6, 0, 0, 0, time.UTC)
	rgDay4    = time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)
)

// rgDateKey 是营业日的日期键('YYYY-MM-DD')。
func rgDateKey(t time.Time) string {
	return t.In(fmLoc).Format("2006-01-02")
}

// rgRangeRow 造一行"区间查询"形态的 cube 返回(带 oper_date 维度)。
//
// ⚠️ 逐日查询也用它 —— 多一个 oper_date 键对逐日路径无害(那条路径只读 item_no)。
func rgRangeRow(date, sku, qty, amount string) map[string]any {
	return fmCubeSalesRowOn(date, sku, qty, amount)
}

// rgIsRangeQuery 报这次查询是不是**区间查询**(而不是逐日查询)。
func rgIsRangeQuery(q cubeclient.CubeQuery) bool {
	from, to := fmCubeDateRange(q)
	return from != "" && to != "" && from != to
}

// rgHandler 造一个"既能答逐日、也能答区间"的 cube 替身处理器。
//
// ⚠️ **必须按日期过滤**:假身若对任何日期都返回同一批销售
//
//	⇒ 销售被重复计入每一天 ⇒ 期初被算成"卖了两遍"的残值 ⇒ 账面应有/成本全错。
func rgHandler(rowsByDate map[string][]map[string]any) func(string, cubeclient.CubeQuery) ([]map[string]any, int, error) {
	return func(m string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		if m != "sales_report" {
			return nil, 0, nil
		}
		rows, n := fmCubeRowsForRange(rowsByDate, q)
		return rows, n, nil
	}
}

// rgHandlerNoDate 让**区间查询**返回**不带 oper_date 键**的行。
//
// ⚠️ 模拟「cube 没按我们要求的形状返回」(维度里少了 oper_date / 驱动没给日期)
//
//	⇒ 收方**无法把行归到某一天**。
//
// ⚠️ 逐日查询照常返回带日期的行 —— 因为逐日路径**根本不需要**日期
//
//	(dimensions = [item_no])⇒ 回落逐日时数字必须完全正常。
func rgHandlerNoDate(rowsByDate map[string][]map[string]any) func(string, cubeclient.CubeQuery) ([]map[string]any, int, error) {
	inner := rgHandler(rowsByDate)
	return func(m string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		if m == "sales_report" && rgIsRangeQuery(q) {
			from, to := fmCubeDateRange(q)
			var out []map[string]any
			for date, rows := range rowsByDate {
				if date < from || date > to {
					continue
				}
				for _, r := range rows {
					cp := make(map[string]any, len(r))
					for k, v := range r {
						if k == "sales_report.oper_date" {
							continue
						}
						cp[k] = v
					}
					out = append(out, cp)
				}
			}
			return out, len(out), nil
		}
		return inner(m, q)
	}
}

// rgHandlerFailRange 让**区间查询**失败(逐日查询照常成功)。
//
// ⚠️ 用来造「预取失败 ⇒ 回落逐日」这条路径:
//
//	非权限类错误 ⇒ 预取被丢弃、不挂 ctx ⇒ 日循环照旧逐日查 ⇒ 数字不变。
func rgHandlerFailRange(rowsByDate map[string][]map[string]any, err error) func(string, cubeclient.CubeQuery) ([]map[string]any, int, error) {
	inner := rgHandler(rowsByDate)
	return func(m string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		if m == "sales_report" && rgIsRangeQuery(q) {
			return nil, 0, err
		}
		return inner(m, q)
	}
}

// rgHandlerForbidden 让**区间查询**返回权限错(403)。
//
// ⚠️ 权限错**不许回落**:回落 = 把一次权限故障显示成「这天没卖东西」。
func rgHandlerForbidden(rowsByDate map[string][]map[string]any) func(string, cubeclient.CubeQuery) ([]map[string]any, int, error) {
	inner := rgHandler(rowsByDate)
	return func(m string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		if m == "sales_report" && rgIsRangeQuery(q) {
			return nil, 0, fmt.Errorf("%w: 测试造的 403", cubeclient.ErrCubeForbidden)
		}
		return inner(m, q)
	}
}

// rgHandlerTruncated 让**区间查询**返回"被截断"的形态(total > len(rows))。
//
// ⚠️ cube 的 total 是"去掉 limit 之后的总行数"⇒ total 比实际行数多
//
//	⇒ 说明数据被 limit 截掉了 ⇒ 这份预取**不能用**。
func rgHandlerTruncated(rowsByDate map[string][]map[string]any) func(string, cubeclient.CubeQuery) ([]map[string]any, int, error) {
	inner := rgHandler(rowsByDate)
	return func(m string, q cubeclient.CubeQuery) ([]map[string]any, int, error) {
		if m == "sales_report" && rgIsRangeQuery(q) {
			rows, _, err := inner(m, q)
			if err != nil {
				return nil, 0, err
			}
			return rows, len(rows) + 7, nil
		}
		return inner(m, q)
	}
}

// rgScenarioRows 造一条 5 天链的销售(基线日 + 4 天)。
//
// ⚠️ 两个部位(belly / 后腿)都有销售 ⇒ 部位级数字才有内容。
func rgScenarioRows() map[string][]map[string]any {
	return map[string][]map[string]any{
		"2026-10-28": {rgRangeRow("2026-10-28", "P-BELLY", "5", "90")},
		"2026-10-29": {
			rgRangeRow("2026-10-29", "P-BELLY", "10", "180"),
			rgRangeRow("2026-10-29", "P-LEG", "3", "60"),
		},
		"2026-10-30": {
			rgRangeRow("2026-10-30", "P-BELLY", "12", "216"),
			rgRangeRow("2026-10-30", "P-LEG", "4", "80"),
		},
		"2026-10-31": {
			rgRangeRow("2026-10-31", "P-BELLY", "8", "144"),
			rgRangeRow("2026-10-31", "P-LEG", "2", "40"),
		},
		"2026-11-01": {
			rgRangeRow("2026-11-01", "P-BELLY", "6", "108"),
			rgRangeRow("2026-11-01", "P-LEG", "1", "20"),
		},
	}
}

// rgSetup 起一个"有源门店"的毛利环境,cube 替身由调用方给。
//
// ⚠️ 复用 ssSetup:它已经登记了 belly(P-BELLY)/ 后腿(P-LEG) 两个部位的货号映射。
func rgSetup(t *testing.T, handler func(string, cubeclient.CubeQuery) ([]map[string]any, int, error)) (
	*service.Service, *cubereadfake.Client, context.Context,
) {
	t.Helper()
	svc, ctx := ssSetup(t)
	cube := &cubereadfake.Client{}
	cube.Handler = handler
	svc.SetCubeReader(cube)
	svc.SetSourceResolver(fmFakeResolver{src: cubesource.Source{
		BranchID: "S001", Configured: true, Enabled: true, CubeSourceName: "cube-sixun-ysx-fb",
	}})
	return svc, cube, ctx
}

// rgSeedBaseline 录基线日的期初结转(**链的起点**)。
//
// ⚠️ **一次调用写两个部位** —— 分两次写会触发"改期初 = 换链起点 ⇒ 清链外旧账"。
func rgSeedBaseline(t *testing.T, svc *service.Service) {
	t.Helper()
	svc.SetClock(fmClock(rgDay1))
	_, err := svc.SaveStockOpenings(context.Background(), "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: rgDateKey(rgBaseDay),
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
		{CutType: model.CutLegRear, BizDate: rgDateKey(rgBaseDay),
			ColdStoreKg: d("50"), UnitPriceYuan: d("20")},
	})
	mustNoErr(t, err, "SaveStockOpenings")
}

// rgCubeCalls 数一下这个替身被 cube sales_report 问了几次。
func rgCubeCalls(cube *cubereadfake.Client) int {
	return len(cube.QueriesFor("sales_report"))
}

// TestCubeSalesRange_QueryShape 锁 ①:批量查询的**形状**。
//
// # 为什么这条重要
//
//	区间查询的形状**错了**,数字就会悄悄不同:
//
//	- 少了 gte/lte ⇒ 查了全表(慢)或跨了区间(串数);
//	- 用了 CAST / inDateRange ⇒ oper_date 是 char(10),CAST 让百万行全表扫描;
//	- 少了 oper_date 维度 ⇒ 拿不回"这行是哪天的" ⇒ 行没法分发到日子上;
//	- limit 没放大 ⇒ 数据被截断(截断保护会兜住,但白跑一趟)。
func TestCubeSalesRange_QueryShape(t *testing.T) {
	svc, cube, ctx := rgSetup(t, rgHandler(rgScenarioRows()))
	rgSeedBaseline(t, svc)

	svc.SetClock(fmClock(rgDay4))
	_, err := svc.ComputeGrossMargin(ctx, "S001", rgDay4)
	mustNoErr(t, err, "ComputeGrossMargin")

	qs := cube.QueriesFor("sales_report")
	if len(qs) != 1 {
		t.Fatalf("★ 有预取时 cube 只该查 1 次,got %d 次(每次远程往返约 330ms —— 这是性能优化的全部意义)", len(qs))
	}
	q := qs[0].Query

	// ① oper_date 的 gte / lte 闭区间过滤(不能用 CAST / inDateRange)。
	var gte, lte []string
	for _, f := range q.Filters {
		if f.Member != "sales_report.oper_date" || len(f.Values) == 0 {
			continue
		}
		s, _ := f.Values[0].(string)
		switch f.Operator {
		case "gte":
			gte = append(gte, s)
		case "lte":
			lte = append(lte, s)
		}
	}
	if len(gte) != 1 || gte[0] != "2026-10-28" {
		t.Errorf("★ oper_date 应有一条 gte 过滤(= 链起点),got %v", gte)
	}
	if len(lte) != 1 || lte[0] != "2026-11-01" {
		t.Errorf("★ oper_date 应有一条 lte 过滤(= 目标日),got %v", lte)
	}

	// ② item_no 的 in 过滤(只查映射表登记过的货号)。
	items := qs[0].FilterValues("sales_report.item_no")
	if len(items) != 2 {
		t.Errorf("★ 应按 2 个登记货号过滤,got %v", items)
	}

	// ③ 两个维度:oper_date + item_no(少 oper_date 就没法按天分发)。
	if len(q.Dimensions) != 2 ||
		q.Dimensions[0] != "sales_report.oper_date" ||
		q.Dimensions[1] != "sales_report.item_no" {
		t.Errorf("★ 维度应是 [oper_date, item_no],got %v", q.Dimensions)
	}

	// ④ limit 按天数放大(5 天 ⇒ 5×40+50 = 250;不是逐日的 200)。
	wantLimit := 5*40 + 50
	if q.Limit != wantLimit {
		t.Errorf("★ limit 应按天数放大成 %d,got %d", wantLimit, q.Limit)
	}
	if q.Limit <= 200 {
		t.Errorf("★ 区间查询的 limit 必须大于逐日查询的 200,got %d", q.Limit)
	}

	// ⑤ 必须 Total: true(没有它就不知道数据有没有被截断)。
	if !q.Total {
		t.Errorf("★ 区间查询必须 Total: true(否则发现不了截断)")
	}
}

// TestCubeSalesRange_DispatchesRowsByDay 锁 ②:预取回来的行**按天分发**。
//
// # 场景
//
//	10-29 卖 belly 10kg/180;10-30 卖 belly 20kg/360。
//
// # 期望
//
//	10-29 的营收 = 180(**不是** 180+360 = 540)
//	10-30 的营收 = 360。
//
// ⚠️ 如果行没按天分发(比如全塞到某一天),成本链的期初就会算错
//
//	⇒ 成本错 ⇒ 毛利错 ⇒ 而"营收"这一列最先露馅。
func TestCubeSalesRange_DispatchesRowsByDay(t *testing.T) {
	svc, cube, ctx := rgSetup(t, rgHandler(map[string][]map[string]any{
		"2026-10-29": {rgRangeRow("2026-10-29", "P-BELLY", "10", "180")},
		"2026-10-30": {rgRangeRow("2026-10-30", "P-BELLY", "20", "360")},
	}))
	rgSeedBaseline(t, svc)

	svc.SetClock(fmClock(rgDay1))
	rep29, err := svc.ComputeGrossMargin(ctx, "S001", rgDay1)
	mustNoErr(t, err, "ComputeGrossMargin 10-29")
	if !rep29.Total.RevenueYuan.Equal(d("180")) {
		t.Errorf("★ 10-29 的营收应是 180(只有 10-29 的销售),got %s —— 预取的行没有按天分发",
			rep29.Total.RevenueYuan)
	}

	svc.SetClock(fmClock(rgDay2))
	rep30, err := svc.ComputeGrossMargin(ctx, "S001", rgDay2)
	mustNoErr(t, err, "ComputeGrossMargin 10-30")
	if !rep30.Total.RevenueYuan.Equal(d("360")) {
		t.Errorf("★ 10-30 的营收应是 360,got %s", rep30.Total.RevenueYuan)
	}

	// 每次请求各自预取一次(预取是**请求级**的,不跨请求复用)。
	if got := rgCubeCalls(cube); got != 2 {
		t.Errorf("★ 两次请求应各自预取一次(共 2 次),got %d", got)
	}
}

// TestCubeSalesRange_NoPrefetch_FallsBackToPerDay 锁 ③:没有预取 ⇒ 仍走逐日查询。
//
// # 场景
//
//	区间查询失败(非权限类)⇒ 预取被丢弃、不挂 ctx。
//
// # 期望
//
//	① 逐日查询照常发生(4 天链 ⇒ 4 次 + 1 次失败的区间查询);
//	② **数字不变**;
//	③ 请求**不报错** —— 预取失败不该把一次可恢复的故障变成 500。
func TestCubeSalesRange_NoPrefetch_FallsBackToPerDay(t *testing.T) {
	svc, cube, ctx := rgSetup(t, rgHandlerFailRange(rgScenarioRows(), errors.New("cube 超时")))
	rgSeedBaseline(t, svc)

	svc.SetClock(fmClock(rgDay4))
	rep, err := svc.ComputeGrossMargin(ctx, "S001", rgDay4)
	mustNoErr(t, err, "ComputeGrossMargin(区间查询失败 ⇒ 应回落逐日,不该报错)")

	// 链有 5 天(10-28 ~ 11-01)⇒ 逐日 5 次 + 失败的区间查询 1 次。
	if got := rgCubeCalls(cube); got != 6 {
		t.Errorf("★ 没有预取时应逐日查 5 次(+1 次失败的区间查询),got %d", got)
	}
	// 数字不变:11-01 的营收 = 108 + 20 = 128。
	if !rep.Total.RevenueYuan.Equal(d("128")) {
		t.Errorf("★ 回落逐日时营收应是 128,got %s", rep.Total.RevenueYuan)
	}
}

// TestCubeSalesRange_Truncated_DiscardsPrefetch 锁 ④:截断 ⇒ 丢弃预取。
//
// # 场景
//
//	区间查询返 total > len(rows) ⇒ 数据被 limit 截断 ⇒ 这份预取不能用。
//
// # 期望
//
//	预取被丢弃 ⇒ 逐日查询兜底 ⇒ 数字不变。
//
// ⚠️ **宁可慢,不可算错**:截断的数据会少掉几天销售
//
//	⇒ 成本链凭空断链 ⇒ 成本塌成残值 ⇒ 毛利虚高(这正是 2026-10-27 踩过的坑)。
func TestCubeSalesRange_Truncated_DiscardsPrefetch(t *testing.T) {
	svc, cube, ctx := rgSetup(t, rgHandlerTruncated(rgScenarioRows()))
	rgSeedBaseline(t, svc)

	svc.SetClock(fmClock(rgDay4))
	rep, err := svc.ComputeGrossMargin(ctx, "S001", rgDay4)
	mustNoErr(t, err, "ComputeGrossMargin(截断 ⇒ 应回落逐日)")

	if got := rgCubeCalls(cube); got != 6 {
		t.Errorf("★ 截断时不该用预取:应逐日查 5 次(+1 次区间查询),got %d", got)
	}
	if !rep.Total.RevenueYuan.Equal(d("128")) {
		t.Errorf("★ 截断回落后营收应是 128,got %s", rep.Total.RevenueYuan)
	}
}

// TestCubeSalesRange_UnassignableRow_DiscardsPrefetch ⭐ 锁"归不到某一天 ⇒ **整份**作废"。
//
// # 这是本文件第二条最重要的规则(2026-11-01 修)
//
//	区间查询回来的行**归不到某一天**(取不到日期)时,**不能只丢这一行**:
//
//	只丢这一行 ⇒ 那一天的预取结果变成"**空**" ⇒ 收方以为"这天没卖"
//	⇒ 营收被当成 0 ⇒ 成本链凭空断链 ⇒ 成本塌成残值 ⇒ **毛利虚高**
//	⇒ 而且**零报错**(门店只会看到"今天没卖东西")。
//
//	⇒ 正确做法:**整份预取作废** ⇒ 回落逐日查询 ⇒ 数字与逐日路径**完全一致**。
//
// # 判别力
//
//	把"整份作废"改回"只丢这一行"时,本用例**立刻红**:
//	  - cube 调用次数会从 1+N 掉到 1(预取被当成了成功的空结果);
//	  - 营收会从 128 掉到 0。
func TestCubeSalesRange_UnassignableRow_DiscardsPrefetch(t *testing.T) {
	// 区间查询返回**不带 oper_date 键**的行(模拟"cube 没按我们要的形状返回")。
	svc, cube, ctx := rgSetup(t, rgHandlerNoDate(rgScenarioRows()))
	rgSeedBaseline(t, svc)

	svc.SetClock(fmClock(rgDay4))
	rep, err := svc.ComputeGrossMargin(ctx, "S001", rgDay4)
	mustNoErr(t, err, "ComputeGrossMargin(区间行归不到某一天 ⇒ 应整份作废、回落逐日)")

	// ① 预取被丢弃 ⇒ 1 次区间查询 + 5 次逐日查询。
	if got := rgCubeCalls(cube); got != 6 {
		t.Errorf("★ 行归不到某一天时应**整份丢弃预取**(1 次区间 + 5 次逐日),got %d 次 —— "+
			"只丢那一行会让预取变成'成功的空结果' ⇒ 营收被当成 0 ⇒ 毛利虚高", got)
	}
	// ② 数字与逐日路径完全一致(11-01 = 108 + 20 = 128),**不为 0**。
	if !rep.Total.RevenueYuan.Equal(d("128")) {
		t.Errorf("★ 回落逐日后营收应是 128,got %s(为 0 = 预取被当成了'这天没卖')",
			rep.Total.RevenueYuan)
	}
}

// TestCubeSalesRange_PrefetchForbidden_Propagates 锁:权限错**原样上抛,不回落**。
//
// ⚠️ 回落 = 把一次权限故障显示成「这家店这天没卖东西」
//
//	⇒ 门店会以为"今天真没卖",而真相是账号没被授予 cube:read。
func TestCubeSalesRange_PrefetchForbidden_Propagates(t *testing.T) {
	svc, cube, ctx := rgSetup(t, rgHandlerForbidden(rgScenarioRows()))
	rgSeedBaseline(t, svc)

	svc.SetClock(fmClock(rgDay4))
	_, err := svc.ComputeGrossMargin(ctx, "S001", rgDay4)
	if err == nil {
		t.Fatalf("★ 预取报权限错时不该静默出结果(回落 = 把故障显示成「没卖东西」)")
	}
	if !errors.Is(err, cubeclient.ErrCubeForbidden) {
		t.Errorf("★ 应原样上抛 cubeclient.ErrCubeForbidden,got %v", err)
	}
	// 权限错 ⇒ 不回落 ⇒ 逐日查询**不该**发生。
	if got := rgCubeCalls(cube); got != 1 {
		t.Errorf("★ 权限错应原样上抛、不回落逐日,cube 被查了 %d 次(应只有 1 次区间查询)", got)
	}
}

// TestCubeSalesRange_Equivalence ⭐ 锁 ⑤:有预取 / 无预取 **数字完全相等**。
//
// # 这是本文件最重要的一条
//
//	它是"数值一个字节都不变"这条硬要求的**回归锁**:
//	同一个场景,跑两遍 —— 一遍用预取,一遍不用预取(区间查询失败 ⇒ 回落逐日)
//	⇒ 逐部位的营收/销售成本/毛利 + 全店的营收/销售成本/毛利必须**逐位相等**。
//
// # 判别力
//
//	① 数字相等 —— 抓"预取把数据算错了";
//	② cube 调用次数 A==1 / B==6 —— 抓"预取根本没被用上"
//	   (把预取逻辑短路掉时 ① 照样绿、② 立刻红)。
func TestCubeSalesRange_Equivalence(t *testing.T) {
	rows := rgScenarioRows()

	// ---- A:预取生效(区间查询成功)----
	svcA, cubeA, ctxA := rgSetup(t, rgHandler(rows))
	rgSeedBaseline(t, svcA)
	svcA.SetClock(fmClock(rgDay4))
	repA, err := svcA.ComputeGrossMargin(ctxA, "S001", rgDay4)
	mustNoErr(t, err, "ComputeGrossMargin(A)")

	// ---- B:没有预取(区间查询失败 ⇒ 回落逐日)----
	svcB, cubeB, ctxB := rgSetup(t, rgHandlerFailRange(rows, errors.New("cube 超时")))
	rgSeedBaseline(t, svcB)
	svcB.SetClock(fmClock(rgDay4))
	repB, err := svcB.ComputeGrossMargin(ctxB, "S001", rgDay4)
	mustNoErr(t, err, "ComputeGrossMargin(B)")

	// ---- ① 判别力:cube 调用次数从 N 次降到 1 次 ----
	if got := rgCubeCalls(cubeA); got != 1 {
		t.Fatalf("★ 有预取时 cube 只该查 1 次,got %d 次 —— 预取没被用上(数字对,但没提速)", got)
	}
	if got := rgCubeCalls(cubeB); got != 6 {
		t.Fatalf("★ 没有预取时应逐日查 5 次(+1 次失败的区间查询),got %d", got)
	}

	// ---- ② 逐部位:营收 / 销售成本 / 毛利必须逐位相等 ----
	for _, ct := range []model.CutType{model.CutBelly, model.CutLegRear} {
		a := findPerCut(t, repA.PerCut, ct)
		b := findPerCut(t, repB.PerCut, ct)
		if !a.RevenueYuan.Equal(b.RevenueYuan) {
			t.Errorf("★ 部位 %s 的营收不等:有预取 %s / 无预取 %s", ct, a.RevenueYuan, b.RevenueYuan)
		}
		if !a.SalesCostYuan.Equal(b.SalesCostYuan) {
			t.Errorf("★ 部位 %s 的销售成本不等:有预取 %s / 无预取 %s", ct, a.SalesCostYuan, b.SalesCostYuan)
		}
		if !a.GrossMarginYuan.Equal(b.GrossMarginYuan) {
			t.Errorf("★ 部位 %s 的毛利不等:有预取 %s / 无预取 %s", ct, a.GrossMarginYuan, b.GrossMarginYuan)
		}
		if !a.SoldKg.Equal(b.SoldKg) {
			t.Errorf("★ 部位 %s 的销量不等:有预取 %s / 无预取 %s", ct, a.SoldKg, b.SoldKg)
		}
	}

	// ---- ③ 全店:营收 / 销售成本 / 毛利必须逐位相等 ----
	if !repA.Total.RevenueYuan.Equal(repB.Total.RevenueYuan) {
		t.Errorf("★ 全店营收不等:有预取 %s / 无预取 %s",
			repA.Total.RevenueYuan, repB.Total.RevenueYuan)
	}
	if !repA.Total.SalesCostYuan.Equal(repB.Total.SalesCostYuan) {
		t.Errorf("★ 全店销售成本不等:有预取 %s / 无预取 %s",
			repA.Total.SalesCostYuan, repB.Total.SalesCostYuan)
	}
	if !repA.Total.GrossMarginYuan.Equal(repB.Total.GrossMarginYuan) {
		t.Errorf("★ 全店毛利不等:有预取 %s / 无预取 %s",
			repA.Total.GrossMarginYuan, repB.Total.GrossMarginYuan)
	}

	// ---- ④ 顺带锁住"数字本身不是 0"(防"两边都错成 0"这种假等价)----
	if !repA.Total.RevenueYuan.Equal(d("128")) {
		t.Errorf("★ 11-01 的全店营收应是 108+20=128,got %s", repA.Total.RevenueYuan)
	}
}
