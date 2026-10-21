// 本文件锁住「档案导出」端口的两条路径。
//
// # 两条路径必须是**同一份契约**
//
//	有源 ⇒ 转发上游导出端点(cube-router 的 GET /v1/export)
//	没配源 ⇒ 从本域 PG 读,组装成同一份 archivedto 契约
//
// ⚠️ 契约的字段名只有一处定义(internal/archivedto.ExportColumns):
// 两边各写一份 struct 的代价是**静默丢数据** ——
// "商品名字段名拼错"的后果是 27565 个商品的 name 全是空,而**没有一行报错**。
package service_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/catalog/model"
	"github.com/YunBright/supertrade/internal/catalog/service"
)

// fakeDapr 记录 dapr 服务调用的参数,并返回预置响应。
type fakeDapr struct {
	appID  string
	method string
	verb   string
	// outMeta 记录调用时 ctx 上的 outgoing metadata(用来断言 JWT / 门店透传)。
	outMeta metadata.MD
	body    []byte
}

func (f *fakeDapr) InvokeMethod(ctx context.Context, appID, methodName, verb string) ([]byte, error) {
	f.appID = appID
	f.method = methodName
	f.verb = verb
	f.outMeta, _ = metadata.FromOutgoingContext(ctx)
	return f.body, nil
}

// TestArchiveExport_ConfiguredSourceForwardsToUpstream 锁"有源 ⇒ 转发上游"。
//
// ⚠️ **不回落本地**:转发失败/为空都**不**改成本地数据 ——
// 回落 = 把"外部系统里没有这份档案"说成"本地也没有"。
func TestArchiveExport_ConfiguredSourceForwardsToUpstream(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"scope":       "products",
		"branch_id":   "00",
		"cube_source": "cube-sixun-ysx-fb",
		"rows":        []map[string]any{{"id": "P-1", "name": "外部商品"}},
		"count":       1,
		"total":       1,
		"offset":      0,
		"limit":       5000,
	})
	require.NoError(t, err)
	dapr := &fakeDapr{body: body}

	arc := service.NewArchiveService(newTestDB(t))
	arc.SetSourceResolver(&fakeSource{src: configuredSource()})
	arc.SetExporter(service.NewDaprArchiveExporter(dapr, "supertrade-cube-router"))

	page, err := arc.Export(context.Background(), service.ExportInput{
		BranchID: "00",
		Bearer:   "Bearer test-token",
		Scope:    archivedto.ScopeProducts,
		Offset:   0,
		Limit:    5000,
	})
	require.NoError(t, err)

	// 上游被正确调用了(app-id / 路径 / 动词)。
	assert.Equal(t, "supertrade-cube-router", dapr.appID)
	assert.Equal(t, "GET", dapr.verb)
	assert.Contains(t, dapr.method, "v1/export?")
	assert.Contains(t, dapr.method, "scope=products")
	assert.Contains(t, dapr.method, "offset=0")
	assert.Contains(t, dapr.method, "limit=5000")

	// ⚠️ JWT 与门店必须透传:上游有 per-branch scope 守门(cube:read),
	// 少了 Authorization 就是 403 —— 而症状看起来像"这个门店没权限导档案"。
	assert.Equal(t, []string{"Bearer test-token"}, dapr.outMeta.Get("authorization"))
	assert.Equal(t, []string{"00"}, dapr.outMeta.Get("x-branch-id"))

	// 上游的 body 原样透出(archivedto 契约)。
	require.Len(t, page.Rows, 1)
	assert.Equal(t, "P-1", page.Rows[0]["id"])
	assert.Equal(t, "外部商品", page.Rows[0]["name"])
	assert.Equal(t, "products", page.Scope)
	assert.Equal(t, "00", page.BranchID)
	assert.Equal(t, 1, page.Total)
}

// TestArchiveExport_NoSourceReadsLocalContract 锁"没配源 ⇒ 本域 PG 的契约行"。
//
// ⚠️ **列名逐字取自 archivedto.ExportColumns** —— 那是导入端读列名的唯一真相源。
func TestArchiveExport_NoSourceReadsLocalContract(t *testing.T) {
	db := newTestDB(t)
	arc := service.NewArchiveService(db)
	arc.SetSourceResolver(&fakeSource{src: localSource()})

	page, err := arc.Export(context.Background(), service.ExportInput{
		BranchID: "10",
		Scope:    archivedto.ScopeProducts,
		Offset:   0,
		Limit:    100,
	})
	require.NoError(t, err)
	assert.Equal(t, archivedto.ScopeProducts, page.Scope)
	assert.Equal(t, "10", page.BranchID)
	assert.Empty(t, page.Rows, "本地没商品 ⇒ 空行(不是报错)")

	// 灌一条商品再导。
	require.NoError(t, db.Create(&model.Product{
		ID: "LOCAL-1", BranchID: "10", Name: "本地商品", Status: "1", CreatedBy: "test",
	}).Error)
	page, err = arc.Export(context.Background(), service.ExportInput{
		BranchID: "10",
		Scope:    archivedto.ScopeProducts,
		Offset:   0,
		Limit:    100,
	})
	require.NoError(t, err)
	require.Len(t, page.Rows, 1)
	row := page.Rows[0]
	for _, col := range archivedto.ExportColumns[archivedto.ScopeProducts] {
		assert.Contains(t, row, col, "契约列 %q 必须出现", col)
	}
	assert.Equal(t, "LOCAL-1", row["id"])
	// ⚠️ 价格归 pricing 服务 ⇒ 本域导出给空字符串(不是 0 元)。
	assert.Equal(t, "", row["price_yuan"])
	assert.Equal(t, "", row["cost_yuan"])
}

// TestArchiveExport_UnsupportedScope 锁"本域不导的资料域 ⇒ 明确报错"。
func TestArchiveExport_UnsupportedScope(t *testing.T) {
	arc := service.NewArchiveService(newTestDB(t))
	arc.SetSourceResolver(&fakeSource{src: localSource()})

	_, err := arc.Export(context.Background(), service.ExportInput{
		BranchID: "10",
		Scope:    archivedto.ScopePrices,
		Offset:   0,
		Limit:    100,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, service.ErrUnsupportedExportScope)
}
