// 本文件锁住「金额 ≥ X」筛选的 **HTTP 层**语义(2026-10-23 新增)。
//
// # 为什么非法值必须 400
//
// "点了筛选没反应"与"参数被忽略"长得一模一样 —— 用户会以为功能坏了,
// 而真相是后端把非法值悄悄丢了。
//
// 而且它是**金额**:把 "abc" 当成"不过滤"会返回**全量单据**,
// 用户以为"金额 ≥ 的单据就这么几条",实际看到的是全部 ⇒ 这不是"体验差",
// 是**给了错答案**。
//
// ⇒ 诚实报错(400),让调用方知道这个参数没被接受。
package handler

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ⚠️ 非法 amount_gte ⇒ 400(不是 200 + 静默忽略)。
func TestListSheets_InvalidAmountGteIs400(t *testing.T) {
	r, db := newRouter(t, &fakeSource{src: localSource()})
	seedLocal(t, db)

	for _, bad := range []string{"abc", "12abc", "1,000", "--5", "1e5x"} {
		w := do(r, "/purchase-sheets?amount_gte="+bad, "10")
		require.Equal(t, http.StatusBadRequest, w.Code,
			"amount_gte=%q 必须是 400,实际 %d: %s", bad, w.Code, w.Body.String())
		m := decode(t, w)
		assert.Equal(t, "bad_request", m["code"])
	}
}

// ⚠️ 合法 amount_gte ⇒ 200,且**真的收窄命中数**(不是被忽略)。
func TestListSheets_AmountGteNarrowsResult(t *testing.T) {
	r, db := newRouter(t, &fakeSource{src: localSource()})
	seedLocal(t, db)
	// seedLocal 的两行:PI-1 金额 241 / RO-1 金额 100。

	w := do(r, "/purchase-sheets?amount_gte=200&page=1&page_size=50", "10")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	m := decode(t, w)
	assert.Equal(t, float64(1), m["total"], "金额≥200 必须真的收窄命中数")
	items := m["items"].([]any)
	require.Len(t, items, 1)
	assert.Equal(t, "PI-1", items[0].(map[string]any)["sheet_no"])
}

// ⚠️ 没传 amount_gte ⇒ 不过滤(返回全部)。
func TestListSheets_NoAmountGteMeansNoFilter(t *testing.T) {
	r, db := newRouter(t, &fakeSource{src: localSource()})
	seedLocal(t, db)

	w := do(r, "/purchase-sheets?page=1&page_size=50", "10")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	m := decode(t, w)
	assert.Equal(t, float64(2), m["total"], "没传 amount_gte 时不能过滤")
}

// ⚠️ amount_gte=0 是**合法下限**(不是"没传")⇒ 必须真的过滤。
func TestListSheets_ZeroAmountGteIsNotTreatedAsAbsent(t *testing.T) {
	r, db := newRouter(t, &fakeSource{src: localSource()})
	seedLocal(t, db)

	w := do(r, "/purchase-sheets?amount_gte=0&page=1&page_size=50", "10")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	m := decode(t, w)
	assert.Equal(t, float64(2), m["total"], "金额≥0 命中全部两行(且必须真的走过滤分支)")
}
