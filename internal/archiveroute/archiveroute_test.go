package archiveroute_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/YunBright/supertrade/internal/archiveroute"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cubesource"
)

// fakeResolver 是 cubesource.Resolver 的 fake。
type fakeResolver struct {
	src cubesource.Source
	err error
}

func (f fakeResolver) Resolve(_ context.Context, _ string) (cubesource.Source, error) {
	if f.err != nil {
		return cubesource.Source{}, f.err
	}
	return f.src, nil
}

// TestDecideExport_三态 锁"导出判据的三个状态必须严格区分"。
//
// ⚠️ "没配"(本地) / "配了"(空) / "问不到"(503) 三者混在一起,
//
//	就会把一次链路故障显示成「这个门店没有数据」—— 那是**假结论**。
func TestDecideExport_三态(t *testing.T) {
	cases := []struct {
		name    string
		res     archiveroute.Resolver
		want    archiveroute.Mode
		wantErr error
	}{
		{
			name: "没配数据源 ⇒ 走本地",
			res:  fakeResolver{src: cubesource.Source{BranchID: "10", Configured: false}},
			want: archiveroute.ModeLocal,
		},
		{
			name: "配了且启用 ⇒ 数据在外部 ⇒ 导出空",
			res: fakeResolver{src: cubesource.Source{
				BranchID: "10", Configured: true, Enabled: true, CubeSourceName: "cube-x"}},
			want: archiveroute.ModeExternal,
		},
		{
			name:    "配了但被停用 ⇒ 503(不是没配)",
			res:     fakeResolver{src: cubesource.Source{BranchID: "10", Configured: true, Enabled: false}},
			wantErr: archiveroute.ErrDisabled,
		},
		{
			name:    "问不到路由 ⇒ 503",
			res:     fakeResolver{err: errors.New("dapr 断了")},
			wantErr: archiveroute.ErrUnavailable,
		},
		{
			name:    "缺 cube:read ⇒ 403(不是 503)",
			res:     fakeResolver{err: cubeclient.ErrCubeForbidden},
			wantErr: archiveroute.ErrForbidden,
		},
		{
			name:    "登录态失效 ⇒ 401",
			res:     fakeResolver{err: cubeclient.ErrCubeAuthRejected},
			wantErr: archiveroute.ErrAuthRejected,
		},
		{
			name: "未装配解析器 ⇒ 走本地(自营店场景)",
			res:  nil,
			want: archiveroute.ModeLocal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, err := archiveroute.DecideExport(context.Background(), tc.res, "10")
			if tc.wantErr != nil {
				require.Error(t, err)
				assert.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, mode)
		})
	}
}

// TestDecideImport_配了源就拒 锁"导入判据:配了源 ⇒ 只读(409)。
//
// ⚠️ **配了但被停用也是 409**:源已经配了 ⇒ 这家店的数据在外部系统里
//
//	⇒ 往本域 PG 写一份"没人看的记录"就是制造假数据。
func TestDecideImport_配了源就拒(t *testing.T) {
	mode, err := archiveroute.DecideImport(context.Background(),
		fakeResolver{src: cubesource.Source{BranchID: "10", Configured: true, Enabled: true}}, "10")
	require.NoError(t, err)
	assert.Equal(t, archiveroute.ModeExternal, mode)

	mode, err = archiveroute.DecideImport(context.Background(),
		fakeResolver{src: cubesource.Source{BranchID: "10", Configured: true, Enabled: false}}, "10")
	require.NoError(t, err)
	assert.Equal(t, archiveroute.ModeExternal, mode, "配了但被停用 ⇒ 仍然只读")
}

// TestDecideImport_问不到就放行 锁"导入路径解析失败 ⇒ 放行(自己兜底)"。
//
// ⚠️ 与导出**相反**:导出回落本地 = 把故障显示成空列表;
//
//	导入放行 = 写路径自己会再查一次目标数据,写不进去会**诚实地**失败。
func TestDecideImport_问不到就放行(t *testing.T) {
	_, err := archiveroute.DecideImport(context.Background(),
		fakeResolver{err: errors.New("dapr 断了")}, "10")
	require.Error(t, err, "调用方据此决定放行(并记一行日志)")
	assert.ErrorIs(t, err, archiveroute.ErrUnavailable)

	// 没配 ⇒ 写本地。
	mode, err := archiveroute.DecideImport(context.Background(),
		fakeResolver{src: cubesource.Source{BranchID: "10"}}, "10")
	require.NoError(t, err)
	assert.Equal(t, archiveroute.ModeLocal, mode)
}
