// 本文件锁价格域导出的两件事:
//
//  1. 增量水位(since)真的变成 cube 查询的 gt 过滤;
//  2. MSSQL live 源的 base64 金额被解码(否则整批价格被当成"没维护价"跳过)。
//
// # 背景(2026-10-23 生产事故)
//
// product 改成 live model 之后,思迅直查返回的 decimal 经 go-mssqldb
// 以 []byte 返回,encoding/json 序列化成 **base64**:
//
//	product.avg_price_yuan = "MTIuMDAwMDAw"   (= "12.000000")
//
// 导出端若只按"字面数字"解析,就把 base64 串原样带出
// ⇒ 导入端 parseMoney 失败 ⇒ 那一行被当成"没维护价"跳过
// ⇒ **27569 行价格一行没落库**,报告只写 skipped=27569,**零报错**。
package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/cubeclient"
)

// TestExportArchive_SinceBecomesChangedMemberFilter 锁"since ⇒ cube 查询的 gt 过滤"。
//
// ⚠️ 少了这条过滤,每次同步都是全量,而编排方照样推进水位 ⇒ 增量形同虚设。
func TestExportArchive_SinceBecomesChangedMemberFilter(t *testing.T) {
	s, _, _ := newLocalService(t, &countingCube{})
	reader := &fakeCubeReader{
		rows:  []map[string]any{{"product.id": "P-0001"}},
		total: 1,
	}
	s.SetCubeSource(&fakeSource{src: externalSource()}, reader)

	_, err := s.ExportArchive(context.Background(), "00", archivedto.ScopePrices,
		0, 100, "2026-10-21", "2026-10-23 01:43:56")
	require.NoError(t, err)
	require.Len(t, reader.queries, 1)

	var gt *cubeclient.CubeFilter
	for i := range reader.queries[0].Filters {
		f := reader.queries[0].Filters[i]
		if f.Operator == cubeOperatorGt {
			gt = &reader.queries[0].Filters[i]
			break
		}
	}
	require.NotNil(t, gt, "since 必须变成 cube 查询的 gt 过滤,实际 filters=%+v",
		reader.queries[0].Filters)
	assert.Equal(t, archivedto.ChangedMembers[archivedto.ScopePrices], gt.Member,
		"gt 过滤的成员必须是能力表登记的那个(两边各写一份字面量 ⇒ 静默漏数据)")
	require.Len(t, gt.Values, 1)
	assert.Equal(t, "2026-10-23 01:43:56", gt.Values[0])
}

// TestExportArchive_NoSince_NoChangedMemberFilter 锁"没给 since ⇒ 不加 gt 过滤"。
//
// ⚠️ 反过来也要锁:全量轮次若带上 gt ⇒ 永远只导"变过的" ⇒ 基础数据缺失。
func TestExportArchive_NoSince_NoChangedMemberFilter(t *testing.T) {
	s, _, _ := newLocalService(t, &countingCube{})
	reader := &fakeCubeReader{
		rows:  []map[string]any{{"product.id": "P-0001"}},
		total: 1,
	}
	s.SetCubeSource(&fakeSource{src: externalSource()}, reader)

	_, err := s.ExportArchive(context.Background(), "00", archivedto.ScopePrices,
		0, 100, "2026-10-21", "")
	require.NoError(t, err)
	require.Len(t, reader.queries, 1)
	for _, f := range reader.queries[0].Filters {
		assert.NotEqual(t, cubeOperatorGt, f.Operator,
			"没给 since(= 全量轮次)就不该加 gt 过滤")
	}
}

// TestExportArchive_Base64AmountIsDecoded 锁"MSSQL live 源的 base64 金额被解码"。
func TestExportArchive_Base64AmountIsDecoded(t *testing.T) {
	s, _, _ := newLocalService(t, &countingCube{})
	reader := &fakeCubeReader{
		rows: []map[string]any{{
			"product.id": "P-0001",
			// base64("12.000000") —— go-mssqldb 把 decimal 当 []byte 返回
			"product.avg_price_yuan":  "MTIuMDAwMDAw",
			"product.total_cost_yuan": "MC4wMDAw",
		}},
		total: 1,
	}
	s.SetCubeSource(&fakeSource{src: externalSource()}, reader)

	page, err := s.ExportArchive(context.Background(), "00", archivedto.ScopePrices,
		0, 100, "2026-10-21", "")
	require.NoError(t, err)
	require.Len(t, page.Rows, 1)

	row := page.Rows[0]
	// ⚠️ 值是 "12" 而不是 "12.000000":shopspring 的 Decimal.String() 会裁掉尾零。
	// 与 cube-router 的 decodeNumeric 同款语义(生产实测同样是 "12")。
	assert.Equal(t, "12", row["price_yuan"],
		"base64 金额必须解码:原样带出 ⇒ 导入端解析失败 ⇒ 整批被当成'没维护价'跳过")
	assert.Equal(t, "0", row["cost_yuan"])
}

// TestExportArchive_DirtyAmountIsNotSilentlyZero 锁"读不懂的金额原样带出(不当 0)"。
//
// ⚠️ 静默当 0 ⇒ 脏数据变成"售价 0 元",POS 会照印出去。
func TestExportArchive_DirtyAmountIsNotSilentlyZero(t *testing.T) {
	s, _, _ := newLocalService(t, &countingCube{})
	reader := &fakeCubeReader{
		rows: []map[string]any{{
			"product.id":             "P-0001",
			"product.avg_price_yuan": "@><:6642675",
		}},
		total: 1,
	}
	s.SetCubeSource(&fakeSource{src: externalSource()}, reader)

	page, err := s.ExportArchive(context.Background(), "00", archivedto.ScopePrices,
		0, 100, "2026-10-21", "")
	require.NoError(t, err)
	require.Len(t, page.Rows, 1)

	assert.Equal(t, "@><:6642675", page.Rows[0]["price_yuan"],
		"读不懂的金额要原样带出,让导入端报告它(而不是静默当 0)")
}
