package service_test

// 2026-10-30:**跨门店选用基线**(owner 第 2 条)。
//
// # 锁的是什么(口径,不是实现细节)
//
//	① 优先级:**精确 > 按体重段 > 整店**;
//	② 同优先级命中多个 ⇒ 取被借店**样本数最多**的那个(确定性);
//	③ 生效:本店该分层**自己不可用** ⇒ 采用被借店同一 (band, cut) 那一行的数字,
//	   并把本店的 source_branch_id 记成**被借方**;
//	④ 被借店那一行自己也不可用 ⇒ **不借用**(回退 LLM);
//	⑤ 管理员手动生成的行(manual_generated=true)**不被借用覆盖**。
//
// ⚠ 这些用例都走**公开 API** + 真实 SQLite ⇒ 引擎接线错了也会红。

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// fmBorrowCfg 写一条借用配置(公开 API)。
func fmBorrowCfg(t *testing.T, svc *service.Service, ctx context.Context,
	borrower, source, band string, cut model.CutType, enabled bool) {
	t.Helper()
	_, err := svc.UpsertForecastBaselineBorrow(ctx, borrower, service.ForecastBaselineBorrowInput{
		SourceBranchID: source, WeightBand: band, CutType: cut, Enabled: enabled,
	})
	mustNoErr(t, err, "UpsertForecastBaselineBorrow "+borrower+"/"+source)
}

// ---- ① 优先级:精确 > 按体重段 > 整店 ----

// TestForecastBorrow_PriorityExactOverBandOverWhole 锁「优先级」。
//
// 三条配置同时开着:
//
//	整店借用(都空)→ S002(p50 12);体重段借用(200-249)→ S003(p50 20);精确(200-249×belly)→ S004(p50 30)。
//
// ⇒ 精确赢;把精确关掉 ⇒ 体重段赢;再把体重段关掉 ⇒ 整店赢。
func TestForecastBorrow_PriorityExactOverBandOverWhole(t *testing.T) {
	svc, ctx, _ := splitSetup(t)
	fmBorrowSeed(t, "S002", "200-249", model.CutBelly, 5, "12")
	fmBorrowSeed(t, "S003", "200-249", model.CutBelly, 5, "20")
	fmBorrowSeed(t, "S004", "200-249", model.CutBelly, 5, "30")

	fmBorrowCfg(t, svc, ctx, "S001", "S002", "", "", true)                    // 整店
	fmBorrowCfg(t, svc, ctx, "S001", "S003", "200-249", "", true)             // 体重段
	fmBorrowCfg(t, svc, ctx, "S001", "S004", "200-249", model.CutBelly, true) // 精确

	// ---- ① 精确优先 ----
	_, err := svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines")
	row, ok := estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("★ 借用后本店应有一行基线")
	}
	if !row.P50Kg.Equal(d("30")) {
		t.Errorf("★ 精确分层优先级最高,应借 S004 的 30,got %s", row.P50Kg)
	}
	if row.SourceBranchID != "S004" {
		t.Errorf("★ source_branch_id 应记被借方 S004,got %q", row.SourceBranchID)
	}

	// ---- ② 关掉精确 ⇒ 体重段赢 ----
	fmBorrowCfg(t, svc, ctx, "S001", "S004", "200-249", model.CutBelly, false)
	_, err = svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines(关掉精确)")
	row, ok = estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("★ 本店应仍有一行基线")
	}
	if !row.P50Kg.Equal(d("20")) {
		t.Errorf("★ 精确关掉后应借体重段的 S003 的 20,got %s", row.P50Kg)
	}
	if row.SourceBranchID != "S003" {
		t.Errorf("★ source_branch_id 应记被借方 S003,got %q", row.SourceBranchID)
	}

	// ---- ③ 再关掉体重段 ⇒ 整店赢 ----
	fmBorrowCfg(t, svc, ctx, "S001", "S003", "200-249", "", false)
	_, err = svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines(关掉体重段)")
	row, ok = estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("★ 本店应仍有一行基线")
	}
	if !row.P50Kg.Equal(d("12")) {
		t.Errorf("★ 只剩整店借用时应借 S002 的 12,got %s", row.P50Kg)
	}
	if row.SourceBranchID != "S002" {
		t.Errorf("★ source_branch_id 应记被借方 S002,got %q", row.SourceBranchID)
	}
}

// ---- ② 同优先级 ⇒ 取样本数最多的那个 ----

// TestForecastBorrow_SameScopeTakesMostSamples 锁「同一优先级取样本最多的被借店」。
//
// 两个整店借用:S002(5 个样本)与 S005(9 个样本)⇒ 应借 S005。
func TestForecastBorrow_SameScopeTakesMostSamples(t *testing.T) {
	svc, ctx, _ := splitSetup(t)
	fmBorrowSeed(t, "S002", "200-249", model.CutBelly, 5, "12")
	fmBorrowSeed(t, "S005", "200-249", model.CutBelly, 9, "40")

	fmBorrowCfg(t, svc, ctx, "S001", "S002", "", "", true)
	fmBorrowCfg(t, svc, ctx, "S001", "S005", "", "", true)

	_, err := svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines")
	row, ok := estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("★ 借用后本店应有一行基线")
	}
	if !row.P50Kg.Equal(d("40")) {
		t.Errorf("★ 同优先级应借样本最多的 S005(9 个样本)的 40,got %s", row.P50Kg)
	}
	if row.SourceBranchID != "S005" {
		t.Errorf("★ source_branch_id 应记 S005,got %q", row.SourceBranchID)
	}
	if row.SampleCount != 9 {
		t.Errorf("★ 样本数应是被借方的 9,got %d", row.SampleCount)
	}
}

// ---- ③ 被借店自己也不可用 ⇒ 不借用 ----

// TestForecastBorrow_SourceNotUsable_NoBorrow 锁「被借店也不可用 ⇒ 不借用」。
//
// ⚠️ 不能拿别家**没有样本**的数字当基线 —— 那是把噪声喂进建议分割。
//
// ⚠️ 2026-10-30:门槛改成 **n ≥ 1** ⇒ "不可用"只剩一种：**一个样本都没有**
//
//	(sample_count = 0)⇒ 那正是"被借店那个分层没有样本"的样子。
func TestForecastBorrow_SourceNotUsable_NoBorrow(t *testing.T) {
	svc, ctx, _ := splitSetup(t)
	// S002 那一层**一个样本都没有** ⇒ 不可用。
	fmBorrowSeed(t, "S002", "200-249", model.CutBelly, 0, "12")
	fmBorrowCfg(t, svc, ctx, "S001", "S002", "", "", true)

	_, err := svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines")
	if row, ok := estBaseline(t, "S001", "200-249", model.CutBelly); ok {
		t.Errorf("★ 被借店不可用 ⇒ 不该借用,got %+v", row)
	}
}

// ---- ④ 本店样本够 ⇒ 不借用 ----

// TestForecastBorrow_OwnSamplesEnough_NoBorrow 锁「本店自己够 ⇒ 不用别家的」。
func TestForecastBorrow_OwnSamplesEnough_NoBorrow(t *testing.T) {
	svc, ctx, sku := splitSetup(t)
	fmBorrowSeed(t, "S002", "200-249", model.CutBelly, 5, "12")
	fmBorrowCfg(t, svc, ctx, "S001", "S002", "", "", true)

	// 本店 5 头猪(各补录 belly 10kg)⇒ 实录池 5 个样本 ⇒ 够用。
	for i := 0; i < 5; i++ {
		tag := string(rune('A' + i))
		pigID := estPig(t, svc, ctx, "S001", "EB-"+tag, "200", splitDay)
		estPigCut(t, svc, ctx, "S001", pigID, sku, "B-"+tag, "10")
	}
	_, err := svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines")
	row, ok := estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("★ 本店应有自己的基线行")
	}
	if !row.P50Kg.Equal(d("10")) {
		t.Errorf("★ 本店样本够 ⇒ 应用自己的 10,不该借 S002 的 12,got %s", row.P50Kg)
	}
	if row.SourceBranchID != "S001" {
		t.Errorf("★ source_branch_id 应是本店 S001,got %q", row.SourceBranchID)
	}
}

// ---- ⑤ 手动生成过的行**照样**被重算覆盖(但留痕) ----

// TestForecastBorrow_ManualRowIsOverwrittenButKeptAsTrace 锁⑤。
//
// # ⚠️ 这是 2026-10-30 **故意**改掉的行为（owner 重新定义模型）
//
//	旧口径：「管理员手动生成的行整层跳过」——manual_generated 保护它不被重算/借用覆盖。
//	新口径：**基线是派生结果**（样本库才是底座）
//	         ⇒ 重算就该把它算回去 ⇒ `manual_generated` 退化成
//	           「**曾手工触发过**」的**留痕**，不再保护任何东西。
//
// # 期望
//
//	① 数字被重算/借用覆盖（不再钉死 99）；
//	② `manual_generated` 标记**保留**（可追溯："这一层曾经被人工设过"）。
func TestForecastBorrow_ManualRowIsOverwrittenButKeptAsTrace(t *testing.T) {
	svc, ctx, _ := splitSetup(t)
	fmBorrowSeed(t, "S002", "200-249", model.CutBelly, 5, "12")
	// 本店有一行**手动生成**的基线(p50 99)。
	mustNoErr(t, testDBSingleton.Create(&model.PigForecastBaseline{
		ID: "PFB-manual-S001", BranchID: "S001", WeightBand: "200-249", CutType: model.CutBelly,
		SampleCount: 1, SampleSource: service.SampleSourceRecorded, SourceBranchID: "S001",
		ManualGenerated: true, GeneratedFromDate: "2026-10-29",
		MeanKg: d("99"), P10Kg: d("99"), P50Kg: d("99"), P90Kg: d("99"),
		ComputedAt: time.Now(),
	}).Error, "插手动基线")
	fmBorrowCfg(t, svc, ctx, "S001", "S002", "", "", true)

	_, err := svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines")
	row, ok := estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("★ 本店应有一行基线")
	}
	if row.P50Kg.Equal(d("99")) {
		t.Errorf("★ 基线是派生物 ⇒ 重算/借用应覆盖它(不该钉死在 99),got %s", row.P50Kg)
	}
	if !row.P50Kg.Equal(d("12")) {
		t.Errorf("★ 本店自己没样本 ⇒ 该借 S002 的 12,got %s", row.P50Kg)
	}
	if !row.ManualGenerated {
		t.Errorf("★ manual_generated 应保留为「曾手工触发」的留痕")
	}
	if row.GeneratedFromDate != "2026-10-29" {
		t.Errorf("★ generated_from_date 应保留,got %q", row.GeneratedFromDate)
	}
}

// ---- ⑥ 借用配置关掉 ⇒ 不借用(且旧行要消失) ----

// TestForecastBorrow_Disabled_NoBorrow 锁「enabled=false ⇒ 不用别家的」。
func TestForecastBorrow_Disabled_NoBorrow(t *testing.T) {
	svc, ctx, _ := splitSetup(t)
	fmBorrowSeed(t, "S002", "200-249", model.CutBelly, 5, "12")
	fmBorrowCfg(t, svc, ctx, "S001", "S002", "", "", true)

	_, err := svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines")
	if _, ok := estBaseline(t, "S001", "200-249", model.CutBelly); !ok {
		t.Fatalf("★ 借用后本店应有一行基线")
	}

	// 关掉借用 ⇒ 重算 ⇒ 借来的那一行必须消失(否则本店会一直用别家的旧数字)。
	fmBorrowCfg(t, svc, ctx, "S001", "S002", "", "", false)
	_, err = svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines(关掉借用)")
	if row, ok := estBaseline(t, "S001", "200-249", model.CutBelly); ok {
		t.Errorf("★ 关掉借用后借来的行应消失,got %+v", row)
	}
}

// ---- ⑧ 被借方是"手动生成"的 ⇒ 借来的行仍要能刷新、也要能清掉 ----

// TestForecastBorrow_SourceIsManual_BorrowedRowStillRefreshedAndRemovable 锁 ⑧。
//
// # 这一条是 2026-10-30 复盘时挖出来的坑
//
//	被借方 S002 的基线是**管理员手动生成**的(manual_generated=true、样本数 1)
//	⇒ 借来的行会**继承** manual_generated
//	⇒ 若「重算时按 manual_generated 跳过整层」⇒ 借来的数字**永远不刷新**
//	⇒ 若「清理时按 manual_generated 跳过」⇒ 关掉借用后那一行**永远删不掉**
//	⇒ 「关掉借用就回到只用本店样本」这条语义就废了
//	⇒ 而这恰恰是**最可能发生的组合**(管理员手动生成一条基线,别的店来借)。
//
// # 期望
//
//	① 借来的行被标成 borrowed=true(与"管理员手动指定"区分开);
//	② 被借方重新生成后,重算 ⇒ 本店借来的数字**跟着变**;
//	③ 关掉借用 ⇒ 重算 ⇒ 借来的行**消失**。
func TestForecastBorrow_SourceIsManual_BorrowedRowStillRefreshedAndRemovable(t *testing.T) {
	svc, ctx, _ := splitSetup(t)
	// S002:管理员手动生成的基线(样本数 1 ⇒ 只因 manual_generated 才可用)。
	mustNoErr(t, testDBSingleton.Create(&model.PigForecastBaseline{
		ID: "PFB-S002-manual", BranchID: "S002", WeightBand: "200-249", CutType: model.CutBelly,
		SampleCount: 1, SampleSource: service.SampleSourceRecorded, SourceBranchID: "S002",
		ManualGenerated: true, GeneratedFromDate: "2026-10-29",
		MeanKg: d("12"), P10Kg: d("12"), P50Kg: d("12"), P90Kg: d("12"),
		ComputedAt: time.Now(),
	}).Error, "插手动基线(S002)")
	fmBorrowCfg(t, svc, ctx, "S001", "S002", "", "", true)

	// ① 借来的行要标 borrowed(否则会被当成管理员手动指定的 ⇒ 被钉死)。
	_, err := svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines")
	row, ok := estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("★ 借用后本店应有一行基线")
	}
	if !row.Borrowed {
		t.Errorf("★ 借来的行必须标 borrowed=true(否则会被当成管理员手动指定的): %+v", row)
	}
	if !row.P50Kg.Equal(d("12")) {
		t.Errorf("★ 应借到 S002 的 12,got %s", row.P50Kg)
	}

	// ② 被借方重新生成(数字变了)⇒ 本店借来的数字要跟着变(不能被钉死)。
	mustNoErr(t, testDBSingleton.Model(&model.PigForecastBaseline{}).
		Where("id = ?", "PFB-S002-manual").
		Updates(map[string]any{
			"p50_kg": d("40"), "mean_kg": d("40"), "p10_kg": d("40"), "p90_kg": d("40"),
		}).Error, "改 S002 的手动基线")
	_, err = svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines(第二次)")
	row, ok = estBaseline(t, "S001", "200-249", model.CutBelly)
	if !ok {
		t.Fatalf("★ 本店应仍有一行基线")
	}
	if !row.P50Kg.Equal(d("40")) {
		t.Errorf("★ 被借方重新生成后,借来的数字应跟着刷新成 40,got %s"+
			"(被钉死 ⇒ 本店会一直用别家的旧数字)", row.P50Kg)
	}

	// ③ 关掉借用 ⇒ 借来的行必须消失。
	fmBorrowCfg(t, svc, ctx, "S001", "S002", "", "", false)
	_, err = svc.RecomputeForecastBaselines(ctx, "S001")
	mustNoErr(t, err, "RecomputeForecastBaselines(关掉借用)")
	if row, ok := estBaseline(t, "S001", "200-249", model.CutBelly); ok {
		t.Errorf("★ 关掉借用后借来的行应消失"+
			"(它继承了 manual_generated ⇒ 不被清就会一直用别家的数字): %+v", row)
	}
}

// ---- ⑦ 借用配置的 CRUD ----

// TestForecastBaselineBorrow_CRUD 锁「新增/更新/删除 + 确定性主键」。
//
// ⚠️ 主键必须由业务键**确定性**算出 ⇒ purge 才删得到行(见 surrogate_id.go 的说明)。
func TestForecastBaselineBorrow_CRUD(t *testing.T) {
	svc, ctx, _ := splitSetup(t)

	row, err := svc.UpsertForecastBaselineBorrow(ctx, "S001", service.ForecastBaselineBorrowInput{
		SourceBranchID: "S002", WeightBand: "", CutType: "", Enabled: true,
	})
	mustNoErr(t, err, "UpsertForecastBaselineBorrow")
	if row.ID != service.ForecastBaselineBorrowID("S001", "", "") {
		t.Errorf("★ 主键应由业务键确定性算出,got %q", row.ID)
	}
	if row.SourceBranchID != "S002" || !row.Enabled {
		t.Errorf("★ 配置内容不对: %+v", row)
	}

	// 同粒度再提交 ⇒ 覆盖(不堆重复行)。
	_, err = svc.UpsertForecastBaselineBorrow(ctx, "S001", service.ForecastBaselineBorrowInput{
		SourceBranchID: "S003", WeightBand: "", CutType: "", Enabled: false,
	})
	mustNoErr(t, err, "UpsertForecastBaselineBorrow(覆盖)")
	list, err := svc.ListForecastBaselineBorrows(ctx, "S001")
	mustNoErr(t, err, "ListForecastBaselineBorrows")
	if len(list) != 1 {
		t.Fatalf("★ 同一粒度只该有一行,got %d: %+v", len(list), list)
	}
	if list[0].SourceBranchID != "S003" || list[0].Enabled {
		t.Errorf("★ 覆盖后内容应是 S003/disabled: %+v", list[0])
	}

	// 删掉。
	mustNoErr(t, svc.DeleteForecastBaselineBorrow(ctx, "S001", row.ID), "DeleteForecastBaselineBorrow")
	list, err = svc.ListForecastBaselineBorrows(ctx, "S001")
	mustNoErr(t, err, "ListForecastBaselineBorrows")
	if len(list) != 0 {
		t.Errorf("★ 删掉后应为空,got %+v", list)
	}
}

// TestForecastBaselineBorrow_RejectsBadInput 锁「非法配置 ⇒ 400」。
func TestForecastBaselineBorrow_RejectsBadInput(t *testing.T) {
	svc, ctx, _ := splitSetup(t)
	cases := []struct {
		name string
		in   service.ForecastBaselineBorrowInput
	}{
		{"没填被借方", service.ForecastBaselineBorrowInput{SourceBranchID: ""}},
		{"借自己", service.ForecastBaselineBorrowInput{SourceBranchID: "S001"}},
		{"只给部位", service.ForecastBaselineBorrowInput{SourceBranchID: "S002", CutType: model.CutBelly}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.UpsertForecastBaselineBorrow(ctx, "S001", tc.in); err == nil {
				t.Errorf("★ 该配置应被拒:%+v", tc.in)
			}
		})
	}
}
