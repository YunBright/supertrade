package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/member/model"
)

// fakeArchiveExporter 替身上游导出器(记录调用参数)。
type fakeArchiveExporter struct {
	page   *archivedto.ExportPage
	err    error
	called int
	scope  string
}

func (f *fakeArchiveExporter) Export(
	_ context.Context, _, _ string, scope string, _, _ int,
) (*archivedto.ExportPage, error) {
	f.called++
	f.scope = scope
	if f.err != nil {
		return nil, f.err
	}
	return f.page, nil
}

// TestExportArchive_NoSourceReadsLocal 锁"没配数据源 ⇒ 导本域 PG"。
func TestExportArchive_NoSourceReadsLocal(t *testing.T) {
	svc, db := newService(t)
	seedType(t, db, "T1", "10", "会员卡")
	seedMember(t, db, "M-1", "10")

	exporter := &fakeArchiveExporter{}
	svc.SetCubeSource(&fakeSource{src: cubesource.Source{BranchID: "10"}}, &fakeCubeReader{})
	svc.SetArchiveExporter(exporter)

	page, err := svc.ExportArchive(context.Background(), "10", "", archivedto.ScopeMembers, 0, 10)
	require.NoError(t, err)
	assert.Equal(t, 0, exporter.called, "没配数据源不该打上游")
	require.Len(t, page.Rows, 1)
	assert.Equal(t, "M-1", page.Rows[0]["id"])
}

// TestExportArchive_ConfiguredSourceForwardsUpstream 锁"配了数据源 ⇒ 原样转发上游"。
//
// ⚠️ 上游的字段名是**导入契约**那一份 ⇒ 原样转发才不会出现"导出/导入字段对不上"。
func TestExportArchive_ConfiguredSourceForwardsUpstream(t *testing.T) {
	svc, _ := newService(t)
	exporter := &fakeArchiveExporter{
		page: &archivedto.ExportPage{
			Scope: archivedto.ScopeMembers, BranchID: "00", Count: 1, Total: 4128,
			Rows: []map[string]any{{"id": "801598", "name": "邹美玉", "balance_yuan": "0"}},
		},
	}
	svc.SetCubeSource(&fakeSource{src: externalSource()}, &fakeCubeReader{})
	svc.SetArchiveExporter(exporter)

	page, err := svc.ExportArchive(context.Background(), "00", "Bearer x", archivedto.ScopeMembers, 0, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, exporter.called, "配了数据源必须走上游")
	assert.Equal(t, archivedto.ScopeMembers, exporter.scope)
	assert.Equal(t, 4128, page.Total, "上游的 total 必须原样带出")
	require.Len(t, page.Rows, 1)
	assert.Equal(t, "邹美玉", page.Rows[0]["name"])
}

// TestExportArchive_SourceResolveFailureIsNotEmptyList 锁"问不到路由 ⇒ 报错,不是空列表"。
func TestExportArchive_SourceResolveFailureIsNotEmptyList(t *testing.T) {
	svc, _ := newService(t)
	svc.SetCubeSource(&fakeSource{err: assertErr("cube-router 不可达")}, &fakeCubeReader{})
	svc.SetArchiveExporter(&fakeArchiveExporter{})

	_, err := svc.ExportArchive(context.Background(), "00", "", archivedto.ScopeMembers, 0, 10)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrCubeSourceUnavailable)
}

// TestExportArchive_UnsupportedScope 锁"非法 scope ⇒ 400"。
func TestExportArchive_UnsupportedScope(t *testing.T) {
	svc, _ := newService(t)
	svc.SetCubeSource(&fakeSource{src: cubesource.Source{BranchID: "10"}}, &fakeCubeReader{})

	_, err := svc.ExportArchive(context.Background(), "10", "", "payments", 0, 10)
	require.Error(t, err)
	assert.ErrorIs(t, err, model.ErrInvalidInput)
}

// assertErr 造一个普通 error。
func assertErr(msg string) error { return errString(msg) }

// errString 是最小 error 实现（避免为一个测试引 errors 包）。
type errString string

func (e errString) Error() string { return string(e) }
