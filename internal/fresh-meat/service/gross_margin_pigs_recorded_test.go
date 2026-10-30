package service_test

// 毛利看板必须告诉前端「**今天到底录没录猪**」(2026-10-30 owner 需求)。
//
// # 为什么要这个字段
//
// 门店在**还没录猪**的时候看毛利看板,看到的是一组**临时数**:
//   - 入库(进货)= 0 —— 没猪 ⇒ 没有分割 ⇒ 没有入库;
//   - 销售成本按**昨天**的移动加权单价估(今天这批货的成本还没进账);
//   - 期末是**负数**(账面短卖:今天卖的比昨天的结余多)。
//
// ⇒ 这不是"算错了",是"还没录完"。不说明的话,门店会把这个临时数当结论。
//
// # 为什么判据不能是「入库量」
//
// 录了猪但**还没补录分割**(pig_cuts)时,入库量同样是 0
// (applyProducedFallback 在"没有袋子"时直接返回 ⇒ 不兜底)
// ⇒ 用入库量当判据 ⇒ 那种情况下**会误报"没录猪"**。
// ⇒ 只能由后端直接告诉前端"今天到底有没有进猪"。
//
// # 口径:按**营业日**归属
//
// 猪按**到店时间**归属营业日,窗口 = [当天 00:00 bizTZ, +24h bizTZ)
// (见 businessDayBounds)⇒ 不用 UTC 日期硬比。
//
// 本文件锁四件事:
//  ① 没录猪 ⇒ 0(且 JSON 里**键必须在**——缺键 = 前端永远提示"没录猪");
//  ② 录了猪 ⇒ 数得对;
//  ③ 营业日边界(23:59:59 / 次日 00:00:01)归属正确;
//  ④ 判据与「入库量」**无关**(录了猪但没补录分割 ⇒ 入库 0、pigs_recorded > 0)。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// fmBizLoc 是鲜肉域的营业日界时区(+08:00)—— 与 service 的默认 bizTZ 一致。
var fmBizLoc = time.FixedZone("Asia/Shanghai", 8*3600)

// insertPigRow 直接往库里插一行 whole_pig。
//
// ⚠️ 为什么绕过 RecordWholePig:录猪有**营业日前置校验**
//
//	("前一个营业日必须已盘点")⇒ 造"同一天两头猪""跨零点两头猪"这种
//	边界数据时会被业务规则挡住。
//	⇒ 本文件只测**计数口径**,不测录猪流程 ⇒ 直接落库。
func insertPigRow(t *testing.T, branchID, id string, arrivedAt time.Time) {
	t.Helper()
	pig := &model.WholePig{
		ID:                    id,
		EarTag:                "EAR-" + id,
		GrossWeightKg:         d("100"),
		PurchaseUnitPriceYuan: d("20"),
		PurchaseCostYuan:      d("2000"),
		SupplierID:            "SUP-1",
		BranchID:              branchID,
		ArrivedAt:             arrivedAt,
		RecordedBy:            "u-1",
		DataSource:            model.DataSourceStub,
		AdviceStatus:          model.AdviceStatusGenerating,
	}
	mustNoErr(t, testDBSingleton.Create(pig).Error, "insert whole_pig "+id)
}

// TestGrossMargin_PigsRecorded_NoPig 锁「没录猪 ⇒ 0」且 **JSON 键必须在**。
//
// ⚠️ 为什么要锁"键必须在"这个看似显然的点:
//
//	前端拿 `pigs_recorded` 判"今天还没录猪"⇒ 后端一旦把它改成 omitempty
//	(或者用指针),0 的那天**整个键消失** ⇒ 前端拿不到值 ⇒ 只能当"没录"
//	⇒ 录了猪之后提示**永远不消失**(假提示,比没提示更坏)。
func TestGrossMargin_PigsRecorded_NoPig(t *testing.T) {
	svc, _ := setupTestService(t)
	day := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC) // = 2026-10-02 14:00 +08

	report, err := svc.ComputeGrossMargin(context.Background(), "S001", day)
	mustNoErr(t, err, "ComputeGrossMargin")

	if report.PigsRecorded != 0 {
		t.Errorf("★ 没录猪时 pigs_recorded 应为 0, got %d", report.PigsRecorded)
	}

	// JSON 层:键必须存在(哪怕值是 0)。
	b, err := json.Marshal(report)
	mustNoErr(t, err, "marshal report")
	var m map[string]any
	mustNoErr(t, json.Unmarshal(b, &m), "unmarshal report")
	v, ok := m["pigs_recorded"]
	if !ok {
		t.Fatalf("★ 响应里必须有 pigs_recorded 键(前端靠它判'没录猪'): %v", m)
	}
	if n, ok := v.(float64); !ok || n != 0 {
		t.Errorf("★ pigs_recorded 应是数字 0, got %#v", v)
	}
}

// TestGrossMargin_PigsRecorded_CountsPigs 锁「录了几头就是几头」。
func TestGrossMargin_PigsRecorded_CountsPigs(t *testing.T) {
	svc, _ := setupTestService(t)
	day := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC) // = 2026-10-02 14:00 +08

	insertPigRow(t, "S001", "WP-A", day)
	insertPigRow(t, "S001", "WP-B", day.Add(time.Hour))

	report, err := svc.ComputeGrossMargin(context.Background(), "S001", day)
	mustNoErr(t, err, "ComputeGrossMargin")
	if report.PigsRecorded != 2 {
		t.Errorf("★ 同一天录了 2 头猪,pigs_recorded 应为 2, got %d", report.PigsRecorded)
	}
}

// TestGrossMargin_PigsRecorded_BizDayBoundary 锁**营业日边界**。
//
// ⚠️ 生产现场踩过这个坑(店 00 / 2026-10-29):
//
//	猪 arrived_at=2026-10-28T16:00:00Z ⇒ 换算成 +08:00 是 **10-29 00:00**
//	⇒ 按 UTC 日期硬比会算成 10-28(错一天)
//	⇒ 门店看到的"今天录没录猪"跟实际差一天。
//
// # ⚠️ 判别力关键:测试数据必须落在「**UTC 日历日 ≠ 营业日**」的那几个小时
//
// 营业日窗口 = [当天 00:00 +08, 次日 00:00 +08) = [前一天 16:00Z, 当天 16:00Z)
// ⇒ **凌晨 00:00~08:00 (+08)** 这段的猪,其 UTC 日历日是**前一天**。
//
//	⇒ 只测"23:59"是**测不出**这个错的(23:59 +08 = 当天 15:59Z,UTC 还是同一天)。
//
// 本用例故意在**两端各放一头**:
//   - 00:00:01 +08 ⇒ UTC 落在**前一天**;
//   - 12:00 +08 / 23:59:59 +08 ⇒ UTC 落在**当天**;
//   - 次日 00:00:01 +08 ⇒ 归次日。
//
// ⇒ 三个断言各自都能把「按 UTC 日期归属」的实现夹出来(见下面的期望值)。
func TestGrossMargin_PigsRecorded_BizDayBoundary(t *testing.T) {
	svc, _ := setupTestService(t)

	early := time.Date(2026, 10, 2, 0, 0, 1, 0, fmBizLoc)   // UTC 10-01 16:00:01
	noon := time.Date(2026, 10, 2, 12, 0, 0, 0, fmBizLoc)   // UTC 10-02 04:00:00
	late := time.Date(2026, 10, 2, 23, 59, 59, 0, fmBizLoc) // UTC 10-02 15:59:59
	next := time.Date(2026, 10, 3, 0, 0, 1, 0, fmBizLoc)    // UTC 10-02 16:00:01

	insertPigRow(t, "S001", "WP-EARLY", early)
	insertPigRow(t, "S001", "WP-NOON", noon)
	insertPigRow(t, "S001", "WP-LATE", late)
	insertPigRow(t, "S001", "WP-NEXT", next)

	// ---- 营业日 10-02 ⇒ 3 头 ----
	//
	// ⚠️ 若按 UTC 日历日归属:UTC 10-02 只含 noon + late ⇒ 2 ⇒ 本断言会红。
	rep02, err := svc.ComputeGrossMargin(context.Background(), "S001", early)
	mustNoErr(t, err, "ComputeGrossMargin(10-02)")
	if rep02.PigsRecorded != 3 {
		t.Errorf("★ 营业日 10-02 应有 3 头猪(00:00:01 / 12:00 / 23:59:59 +08),got %d"+
			"(若为 2 ⇒ 按 UTC 日历日归属了,错一天)", rep02.PigsRecorded)
	}

	// ---- 营业日 10-01 ⇒ 0 头 ----
	//
	// ⚠️ 若按 UTC 日期归属:UTC 10-01 含 early(10-01 16:00:01Z)⇒ 1 ⇒ 本断言会红。
	rep01, err := svc.ComputeGrossMargin(context.Background(), "S001",
		time.Date(2026, 10, 1, 12, 0, 0, 0, fmBizLoc))
	mustNoErr(t, err, "ComputeGrossMargin(10-01)")
	if rep01.PigsRecorded != 0 {
		t.Errorf("★ 营业日 10-01 没进猪,pigs_recorded 应为 0,got %d"+
			"(非 0 ⇒ 凌晨进场的猪被算到前一天了)", rep01.PigsRecorded)
	}

	// ---- 营业日 10-03 ⇒ 1 头 ----
	//
	// ⚠️ 若按 UTC 日期归属:next 是 10-02 16:00:01Z ⇒ 落在 UTC 10-02 ⇒ 本断言会红。
	rep03, err := svc.ComputeGrossMargin(context.Background(), "S001", next)
	mustNoErr(t, err, "ComputeGrossMargin(10-03)")
	if rep03.PigsRecorded != 1 {
		t.Errorf("★ 营业日 10-03 应有 1 头猪(00:00:01 +08),got %d"+
			"(若为 0 ⇒ 凌晨进场的猪被算到前一天了)", rep03.PigsRecorded)
	}
}

// TestGrossMargin_PigsRecorded_IndependentFromPurchasedKg 锁「判据与入库量无关」。
//
// ⚠️ 这是**本字段存在的主要理由**:
//
//	录了猪但**还没补录分割** ⇒ 入库量是 0(applyProducedFallback 在
//	"门店没贴标签 ⇒ 没有袋子"时直接返回,不兜底)
//	⇒ 前端若用"入库量 = 0"当"没录猪"的判据 ⇒ 这里会**误报**。
//
// ⇒ 后端必须能区分"没录猪"和"录了猪但还没补录分割"。
func TestGrossMargin_PigsRecorded_IndependentFromPurchasedKg(t *testing.T) {
	svc, _ := setupTestService(t)
	day := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC) // = 2026-10-02 14:00 +08

	// 只录猪,**不**补录分割 ⇒ 没有袋子 ⇒ 没有兜底入库。
	insertPigRow(t, "S001", "WP-NOCUT", day)

	report, err := svc.ComputeGrossMargin(context.Background(), "S001", day)
	mustNoErr(t, err, "ComputeGrossMargin")

	// 前提:入库量确实是 0(否则这个用例就没意义了)。
	var purchasedKg float64
	for _, row := range report.PerCut {
		purchasedKg += row.PurchasedKg.InexactFloat64()
	}
	if purchasedKg != 0 {
		t.Fatalf("前提不成立:没补录分割时入库量应是 0,got %v", purchasedKg)
	}

	// 但 pigs_recorded 必须为 1 ⇒ 前端据此知道"猪录了,只是还没补录分割"。
	if report.PigsRecorded != 1 {
		t.Errorf("★ 录了猪(哪怕没补录分割)pigs_recorded 也应为 1,got %d", report.PigsRecorded)
	}
}

// TestGrossMargin_PigsRecorded_OtherBranchNotCounted 锁「按门店隔离」。
//
// ⚠️ 别的店的猪不该算到本店的"今天录没录猪"上。
func TestGrossMargin_PigsRecorded_OtherBranchNotCounted(t *testing.T) {
	svc, _ := setupTestService(t)
	day := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)

	insertPigRow(t, "S002", "WP-OTHER", day)

	report, err := svc.ComputeGrossMargin(context.Background(), "S001", day)
	mustNoErr(t, err, "ComputeGrossMargin")
	if report.PigsRecorded != 0 {
		t.Errorf("★ 别的店的猪不该算进来,pigs_recorded=%d(want 0)", report.PigsRecorded)
	}
}

// 编译期护栏:确保字段类型还是 int(前端按整数读)。
var _ = service.GrossMarginReport{PigsRecorded: 0}
