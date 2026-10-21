// Package cubesync —— 会员档案同步链路的单元测试。
//
// # 测什么(对着症状写)
//
//	① 会员 scope 归**member** 导入方(不是 catalog / pricing / inventory)
//	② member_types 在 members **之前**(会员引用类别)
//	③ 会员行的**原值**原样带出(不在编排层"顺手翻译")
//	④ member 的导入端点路径是 **/import**(不是 /archive/import)
package cubesync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/archivedto"
)

// TestDefaultMemberScopes_Order 锁"member_types 在 members 之前"。
//
// ⚠️ 会员引用类别(members.card_type_id → member_types.id)⇒
//
//	反过来就是"引用了还不存在的对象"。
func TestDefaultMemberScopes_Order(t *testing.T) {
	scopes := DefaultMemberScopes()
	require.Len(t, scopes, 2)
	assert.Equal(t, archivedto.ScopeMemberTypes, scopes[0])
	assert.Equal(t, archivedto.ScopeMembers, scopes[1])
}

// TestAllScopes_MemberTypesBeforeMembers 锁"全局 scope 顺序里也是类别在前"。
func TestAllScopes_MemberTypesBeforeMembers(t *testing.T) {
	idxType, idxMembers := -1, -1
	for i, s := range archivedto.AllScopes {
		switch s {
		case archivedto.ScopeMemberTypes:
			idxType = i
		case archivedto.ScopeMembers:
			idxMembers = i
		}
	}
	require.GreaterOrEqual(t, idxType, 0, "AllScopes 缺 member_types")
	require.GreaterOrEqual(t, idxMembers, 0, "AllScopes 缺 members")
	assert.Less(t, idxType, idxMembers, "member_types 必须在 members 之前")
}

// TestSync_MemberScopes_GoToMemberImporter 锁"会员 scope 走 member 导入方"。
//
// ⚠️ 一个 scope 一个归属方:会员归 member(catalog / pricing / inventory 都不接)。
func TestSync_MemberScopes_GoToMemberImporter(t *testing.T) {
	ex := &fakeExporter{pages: map[string][]map[string]any{
		archivedto.ScopeMemberTypes: {{
			"id": "0", "name": "会员卡", "discount_rate": "1",
			"allow_stored_value": "0", "allow_points_pay": "1",
		}},
		archivedto.ScopeMembers: {{
			"id": "M1", "card_type": "0", "name": "张三", "status": "0",
			"balance_yuan": "10", "points_accumulated": "5", "points_used": "1",
		}},
	}}
	memberIm := &fakeImporter{}
	otherIm := &fakeImporter{}
	s := NewSyncer(Config{
		SourceBranchID: "00", TargetBranchID: "10", Mode: "incremental",
		Scopes: DefaultMemberScopes(), PageSize: 10,
	}, fakeToken{token: "T"}, ex, []Owner{
		{Importer: otherIm, Scopes: DefaultCatalogScopes()},
		{Importer: memberIm, Scopes: DefaultMemberScopes()},
	}, nil)

	rep, err := s.Sync(context.Background())
	require.NoError(t, err)
	assert.Len(t, memberIm.snapshot(), 2, "会员的两个 scope 都该发给 member")
	assert.Len(t, otherIm.snapshot(), 0, "别的导入方不该收到会员数据")

	// 顺序:类别先于会员。
	batches := memberIm.snapshot()
	require.Len(t, batches, 2)
	assert.Equal(t, []string{archivedto.ScopeMemberTypes}, batches[0].Scopes)
	assert.Equal(t, []string{archivedto.ScopeMembers}, batches[1].Scopes)
	assert.Len(t, rep.Scopes, 2)
}

// TestBuildImportData_Members_KeepsRawValues 锁"会员行的**原值**原样带出"。
//
// ⚠️ 语义归一化(状态映射 / 999998 哨兵 / 脏余额)在**导入端**做 ——
//
//	在编排层"顺手翻译"会把"读不懂"变成"余额 0",而余额 0 没人会去查。
func TestBuildImportData_Members_KeepsRawValues(t *testing.T) {
	row := map[string]any{
		"id": "M1", "card_type": "0", "name": "张三", "status": "3",
		"limited_use_count": "999998", "balance_yuan": "@><:6642675",
		"points_accumulated": "10", "points_used": "2", "branch_no": "00",
	}
	data := buildImportData(archivedto.ScopeMembers, []map[string]any{row})
	require.Len(t, data.Members, 1)
	m := data.Members[0]
	assert.Equal(t, "M1", m.ID)
	assert.Equal(t, "3", m.Status, "card_status 原值必须原样带出")
	assert.Equal(t, "999998", m.LimitedUseCount, "哨兵必须原样带出")
	assert.Equal(t, "@><:6642675", m.BalanceYuan, "脏余额必须原样带出")
	assert.Equal(t, "00", m.BranchNo, "源库门店号只作审计记录")
}

// TestSidecarImporter_MemberImportPath 锁"member 的导入端点是 /import"。
//
// ⚠️ catalog / pricing / inventory 都是 /archive/import,
//
//	而 member 是 /import(前端路径 /api/v1/member/import)⇒
//	写死一个路径的话 member 那边 404,而症状是"会员一个都没导"。
func TestSidecarImporter_MemberImportPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mode":"incremental"}`))
	}))
	defer srv.Close()

	im := NewSidecarImporterAt(srv.URL, "supertrade-member", "10", "import")
	_, err := im.Import(context.Background(), "TOKEN", archivedto.ImportRequest{
		SourceBranchID: "00", Mode: "incremental", Scopes: []string{"members"},
		Data: archivedto.ImportData{Members: []archivedto.ImportMemberRow{{ID: "M1"}}},
	})
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(gotPath, "/v1.0/invoke/supertrade-member/method/import"),
		"调用路径不对: %s", gotPath)
}

// TestSidecarImporter_DefaultImportPath 锁"其它下游仍是 /archive/import"。
func TestSidecarImporter_DefaultImportPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mode":"incremental"}`))
	}))
	defer srv.Close()

	im := NewSidecarImporter(srv.URL, "supertrade-catalog", "10")
	_, err := im.Import(context.Background(), "TOKEN", archivedto.ImportRequest{
		Mode: "incremental", Scopes: []string{"products"},
	})
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(gotPath, "/method/archive/import"), "调用路径不对: %s", gotPath)
}
