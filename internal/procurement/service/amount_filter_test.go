// 本文件锁住「采购收货」的「金额 ≥ X」筛选(2026-10-23 新增)。
//
// # 为什么以前做不到
//
// 金额原先只有**度量**(total_amount_yuan = SUM(amount)),
// 而 cube 的 filter 只作用于**维度** ⇒ "金额 ≥ X"在**协议层**就表达不出来。
// ⇒ 当时的选择是"保持不支持"并在前端标注。
//
// 现在 cube 的 purchase_sheet 补了 **amount 维度**(源列 sheet_amt = 单据金额)
// ⇒ 过滤成立(等价于 SQL 的 HAVING)。
//
// # 为什么这组用例必须存在
//
// 一个**被静默忽略**的金额过滤,返回的是"看起来正常"的完整列表 ——
// 与"筛选没命中"长得一模一样。
// ⇒ 必须断言"过滤条件真的进了 cube query",而不只是"返回了 200"。
package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/procurement/service"
)

// decPtr 造一个 decimal 指针(金额下限用指针,0 也是合法下限)。
func decPtr(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

// filterOf 在查询里找某个 member 的过滤。
func filterOf(t *testing.T, q cubeclient.CubeQuery, member string) (cubeclient.CubeFilter, bool) {
	t.Helper()
	for _, f := range q.Filters {
		if f.Member == member {
			return f, true
		}
	}
	return cubeclient.CubeFilter{}, false
}

// ⚠️ amount_gte 必须真的进 cube 过滤,而且落在**金额维度**上。
//
// 若误写成对**度量**的过滤,cube 会 400(不会给错答案);
// 但更糟的情况是后端静默丢掉这个筛选 ⇒ 用户看到全量单据却以为"就这几条"。
func TestListSheets_AmountGteReachesCubeFilter(t *testing.T) {
	src := &fakeSource{src: configuredSource()}
	svc, _, reader := newSvc(t, src)

	_, err := svc.List(branchCtx("00"), "00", service.Query{
		AmountGte: decPtr("200"), Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	q, ok := reader.LastQuery("purchase_sheet")
	require.True(t, ok)

	f, ok := filterOf(t, q.Query, "purchase_sheet.amount")
	require.True(t, ok,
		"amount_gte 必须落到 cube 的**金额维度** purchase_sheet.amount 上"+
			"(度量不能被过滤;静默丢掉过滤 = 用户看到全量单据)")
	assert.Equal(t, "gte", f.Operator, "金额下限必须用 >=(闭下界)")
	require.Len(t, f.Values, 1)
	// ⚠️ 值必须是**字符串**:MSSQL 按类型优先级决定把谁转成谁 ——
	// nvarchar 优先级低于 numeric ⇒ 转成 numeric ⇒ 精确十进制比较;
	// 若下发 JSON 数字(float64),float 优先级**高于** numeric
	// ⇒ 会把 numeric 列转成 float 再比 ⇒ 丢末位精度。
	assert.Equal(t, "200", f.Values[0],
		"金额过滤值必须以字符串下发(保住 numeric 精度)")
}

// ⚠️ 没传 amount_gte 时**不能**有任何金额过滤。
//
// 否则"没筛选"和"筛选了但没命中"会给出同样的空列表 —— 用户分不出来。
func TestListSheets_NoAmountGteMeansNoAmountFilter(t *testing.T) {
	src := &fakeSource{src: configuredSource()}
	svc, _, reader := newSvc(t, src)

	_, err := svc.List(branchCtx("00"), "00", service.Query{Page: 1, PageSize: 50})
	require.NoError(t, err)
	q, ok := reader.LastQuery("purchase_sheet")
	require.True(t, ok)
	assert.False(t, q.HasFilter("purchase_sheet.amount"),
		"没传 amount_gte 时不能挂金额过滤")
}

// ⚠️ 本域 PG 路径:金额下限必须**真的收窄命中数**。
//
// 用 total_amount_yuan(单据金额),不是 total_net_amount_yuan(净额):
// 采购侧 RO 的金额在源库里是**正数**,净额才取负 ——
// 用净额会让两条路径给出不同答案。
func TestListSheets_AmountGteNarrowsLocalResult(t *testing.T) {
	src := &fakeSource{src: localSource()}
	svc, db, _ := newSvc(t, src)
	seedLocal(t, db)
	// seedLocal 的两行:PI-1 金额 241 / RO-1 金额 100。

	all, err := svc.List(branchCtx("10"), "10", service.Query{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Equal(t, 2, all.Total)

	byAmount, err := svc.List(branchCtx("10"), "10", service.Query{
		AmountGte: decPtr("200"), Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, byAmount.Total, "金额≥200 必须真的收窄命中数(241 命中,100 不命中)")
	require.Len(t, byAmount.Items, 1)
	assert.Equal(t, "PI-1", byAmount.Items[0].SheetNo)

	none, err := svc.List(branchCtx("10"), "10", service.Query{
		AmountGte: decPtr("500"), Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, none.Total, "金额≥500 必须命中 0 条")

	// ⚠️ 0 是**合法下限**(不是"没传")⇒ 必须真的过滤(命中两行)。
	zero, err := svc.List(branchCtx("10"), "10", service.Query{
		AmountGte: decPtr("0"), Page: 1, PageSize: 50,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, zero.Total, "金额≥0 必须真的过滤(而不是被当成'没传')")
}
