package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/cashrecon/model"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead/cubereadfake"
	"github.com/YunBright/supertrade/internal/cubesource"
)

// cubeRows 是对一个业务日的 cube 返回行（两个源分店 + 两个 pay_way）。
func cubeRows() []map[string]any {
	return []map[string]any{
		{"payflow.pay_way": "SWX", "payflow.branch_no": "01",
			"payflow.count": "8", "payflow.total_pay_amount_yuan": "100.00"},
		{"payflow.pay_way": "SWX", "payflow.branch_no": "02",
			"payflow.count": "2", "payflow.total_pay_amount_yuan": "50.00"},
		{"payflow.pay_way": "LEP", "payflow.branch_no": "01",
			"payflow.count": "1", "payflow.total_pay_amount_yuan": "30.00"},
	}
}

// TestCashFlows_CubePerDayQueries 锁住"逐日查询 + 只按 paid_at 过滤 + Limit ≤ 200"。
//
// 一次查 31 天等于让思迅源库扫全部收款流水；按日聚合后单日 ≤ 20 行。
func TestCashFlows_CubePerDayQueries(t *testing.T) {
	svc, _ := newTestService(t)
	fake := cubereadfake.New()
	fake.Handler = func(string, cubeclient.CubeQuery) ([]map[string]any, int, error) {
		return cubeRows(), 3, nil
	}
	svc.SetCashSource(cubeSource(), fake)

	rows, src, err := svc.CashDayRows(context.Background(), "00", day(2026, 10, 28), day(2026, 10, 30))
	require.NoError(t, err)
	require.Equal(t, CashSourceCube, src)
	// 3 天 × 2 个 pay_way = 6 行。
	require.Len(t, rows, 6)

	qs := fake.QueriesFor("payflow")
	require.Len(t, qs, 3, "三天必须发三次单日查询，不允许一次拉 3 天")
	for i, q := range qs {
		require.LessOrEqual(t, q.Query.Limit, 200, "防御性上限")
		require.True(t, q.Query.Total)
		require.Contains(t, q.Query.Dimensions, "payflow.pay_way")
		require.Contains(t, q.Query.Dimensions, "payflow.branch_no")
		require.Contains(t, q.Query.Measures, "payflow.total_pay_amount_yuan")

		// 过滤条件只能落在 paid_at 上（带 pay_way 过滤会漏掉未映射枚举）。
		for _, f := range q.Query.Filters {
			require.Equal(t, "payflow.paid_at", f.Member, "只允许按 paid_at 过滤")
		}
		wantDay := day(2026, 10, 28).AddDate(0, 0, i).Format("2006-01-02")
		// gte / lte 是两条独立过滤（cube 的算子不共用一条），这里汇总该 member 的值。
		var vals []any
		for _, f := range q.Query.Filters {
			if f.Member == "payflow.paid_at" {
				vals = append(vals, f.Values...)
			}
		}
		require.Equal(t, []any{wantDay + " 00:00:00", wantDay + " 23:59:59"}, vals)
	}

	// 默认映射：SWX→wechat（两源分店求和），LEP→unmapped（不猜）。
	var swx, lep *CashDay
	for i := range rows {
		if rows[i].BizDate.Format("2006-01-02") != "2026-10-28" {
			continue
		}
		switch rows[i].PayWay {
		case "SWX":
			swx = &rows[i]
		case "LEP":
			lep = &rows[i]
		}
	}
	require.NotNil(t, swx)
	require.NotNil(t, lep)
	require.Equal(t, model.MethodWechat, swx.Method)
	require.Equal(t, "150", swx.AmountNet.String())
	require.Equal(t, 10, swx.TxnCount)
	require.Equal(t, model.MethodUnmapped, lep.Method)
}

// TestCashFlows_CubeResultCached 锁住 5 分钟 TTL：同一业务日第二次不再打 cube。
func TestCashFlows_CubeResultCached(t *testing.T) {
	svc, _ := newTestService(t)
	fake := cubereadfake.New()
	fake.Handler = func(string, cubeclient.CubeQuery) ([]map[string]any, int, error) {
		return cubeRows(), 3, nil
	}
	svc.SetCashSource(cubeSource(), fake)

	d := day(2026, 10, 30)
	_, _, err := svc.CashDayRows(context.Background(), "00", d, d)
	require.NoError(t, err)
	_, _, err = svc.CashDayRows(context.Background(), "00", d, d)
	require.NoError(t, err)
	require.Len(t, fake.QueriesFor("payflow"), 1, "第二次应命中缓存")

	svc.InvalidateCashCache("00")
	_, _, err = svc.CashDayRows(context.Background(), "00", d, d)
	require.NoError(t, err)
	require.Len(t, fake.QueriesFor("payflow"), 2, "Invalidate 后必须重新查")
}

// TestCashFlows_FiltersSourceBranchNos 锁住 cash_source_branch_nos 规则。
func TestCashFlows_FiltersSourceBranchNos(t *testing.T) {
	svc, _ := newTestService(t)
	require.NoError(t, svc.SaveRule(context.Background(), "00", "u1",
		RuleCashSourceBranchNos, `["01"]`))

	fake := cubereadfake.New()
	fake.Handler = func(string, cubeclient.CubeQuery) ([]map[string]any, int, error) {
		return cubeRows(), 3, nil
	}
	svc.SetCashSource(cubeSource(), fake)

	d := day(2026, 10, 30)
	rows, _, err := svc.CashDayRows(context.Background(), "00", d, d)
	require.NoError(t, err)
	require.Len(t, rows, 2, "只保留源分店 01（SWX + LEP）")
	var swx *CashDay
	for i := range rows {
		if rows[i].PayWay == "SWX" {
			swx = &rows[i]
		}
	}
	require.NotNil(t, swx)
	require.Equal(t, "100", swx.AmountNet.String(), "分店 02 的 50 元必须被过滤掉")
}

// TestCashFlows_DisabledSourceIs503 锁住"配了但停用 ⇒ 503，绝不回落本地"。
func TestCashFlows_DisabledSourceIs503(t *testing.T) {
	svc, _ := newTestService(t)
	fake := cubereadfake.New()
	svc.SetCashSource(fakeResolver{src: cubesource.Source{Configured: true, Enabled: false}}, fake)

	_, _, err := svc.CashFlows(context.Background(), "00", day(2026, 10, 30), day(2026, 10, 30), Rules{})
	require.Error(t, err)
	require.True(t, errors.Is(err, model.ErrCubeSourceDisabled))
	require.Empty(t, fake.Queries(), "停用的门店不允许被查询")
}

// TestCashFlows_ResolverErrorIs503 锁住"问不到路由 ⇒ 503，不回落本地"。
func TestCashFlows_ResolverErrorIs503(t *testing.T) {
	svc, _ := newTestService(t)
	fake := cubereadfake.New()
	svc.SetCashSource(fakeResolver{err: errors.New("dapr invoke failed")}, fake)

	_, _, err := svc.CashFlows(context.Background(), "00", day(2026, 10, 30), day(2026, 10, 30), Rules{})
	require.Error(t, err)
	require.True(t, errors.Is(err, model.ErrCubeSourceUnavailable))
	require.Empty(t, fake.Queries())
}

// TestCashFlows_CubeFailureIs503 锁住"查询失败 ≠ 当天没有流水"。
func TestCashFlows_CubeFailureIs503(t *testing.T) {
	svc, _ := newTestService(t)
	fake := cubereadfake.New()
	fake.Handler = func(string, cubeclient.CubeQuery) ([]map[string]any, int, error) {
		return nil, 0, errors.New("cube 500")
	}
	svc.SetCashSource(cubeSource(), fake)

	_, _, err := svc.CashFlows(context.Background(), "00", day(2026, 10, 30), day(2026, 10, 30), Rules{})
	require.Error(t, err)
	require.True(t, errors.Is(err, model.ErrCubeQueryFailed))
}

// TestCashFlows_LocalPath 锁住"没配数据源 ⇒ 读本地 pay_flows"。
func TestCashFlows_LocalPath(t *testing.T) {
	svc, db := newTestService(t)
	svc.SetCashSource(localSource(), cubereadfake.New())

	insertPayFlows(t, db,
		map[string]any{"branch_id": "00", "pay_way": "RMB", "pay_amount": "100", "exchange_rate": "1", "paid_at": "2026-10-30 09:00:00"},
		map[string]any{"branch_id": "00", "pay_way": "RMB", "pay_amount": "-20", "exchange_rate": "1", "paid_at": "2026-10-30 15:00:00"},
		map[string]any{"branch_id": "00", "pay_way": "SWX", "pay_amount": "50", "exchange_rate": "1", "paid_at": "2026-10-30 12:00:00"},
		map[string]any{"branch_id": "00", "pay_way": "SWX", "pay_amount": "50", "exchange_rate": "1", "paid_at": "2026-10-31 00:00:01"},
	)

	rows, src, err := svc.CashDayRows(context.Background(), "00", day(2026, 10, 30), day(2026, 10, 30))
	require.NoError(t, err)
	require.Equal(t, CashSourceLocal, src)
	require.Len(t, rows, 2)
	require.Equal(t, "2026-10-30", rows[0].BizDate.Format("2006-01-02"))
	require.Equal(t, "RMB", rows[0].PayWay)
	require.Equal(t, "80", rows[0].AmountNet.String(), "负数冲销原样进净额")
	require.Equal(t, 2, rows[0].TxnCount)
	require.Equal(t, model.MethodCash, rows[0].Method)
}

// TestValidateRange_RejectsOver31Days 锁住区间上限（cube 成本的最后一道闸）。
func TestValidateRange_RejectsOver31Days(t *testing.T) {
	svc, _ := newTestService(t)
	from := day(2026, 10, 1)
	to := day(2026, 11, 1) // 32 天
	require.ErrorIs(t, svc.validateRange("00", from, to), model.ErrRangeTooLarge)
	require.NoError(t, svc.validateRange("00", from, to.AddDate(0, 0, -1)))
	require.ErrorIs(t, svc.validateRange("", from, to), model.ErrInvalidInput)
}

// TestMethodMap_UnknownPayWayIsUnmapped 锁住"未命中不猜"。
func TestMethodMap_UnknownPayWayIsUnmapped(t *testing.T) {
	svc, _ := newTestService(t)
	require.NoError(t, svc.SaveRule(context.Background(), "00", "u1",
		RulePaymentMethodMap, `{"XYZ":"wechat"}`))

	rules, err := svc.LoadRules(context.Background(), "00")
	require.NoError(t, err)
	m := methodMap(rules)
	require.Equal(t, model.MethodWechat, methodOf(m, "xyz"), "规则键大小写不敏感")
	require.Equal(t, model.MethodUnmapped, methodOf(m, "ABC"))
}

// TestSaveRule_InvalidValueRejected 锁住 fail-closed：配错的规则不许入库。
func TestSaveRule_InvalidValueRejected(t *testing.T) {
	svc, _ := newTestService(t)
	require.ErrorIs(t, svc.SaveRule(context.Background(), "00", "u1",
		RuleArrivalToleranceDays, `9`), model.ErrInvalidInput)
	require.ErrorIs(t, svc.SaveRule(context.Background(), "00", "u1",
		RulePaymentMethodMap, `{"SWX":"nonsense"}`), model.ErrInvalidInput)
	require.ErrorIs(t, svc.SaveRule(context.Background(), "00", "u1",
		"unknown_key", `1`), model.ErrInvalidInput)
}
