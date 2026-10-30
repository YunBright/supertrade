// 2026-10-30:锁「改期初结转 ⇒ **同事务作废链外的旧快照**」。
//
// # 背景(owner 建议 + 生产实测)
//
//	owner:「在修改期初结算/审核结转时同时执行快照清除并重新快照的事务」。
//
//	生产实测(店 00):门店 10-29 晚上才补录期初(生效日 10-28),
//	而 10-01~10-27 的**上线前旧账**早就物化成了成本日快照。
//	⇒ 链的起点被拉回 10-01 ⇒ 上线前的旧账(账面短卖的负链值)被算进链里
//	⇒ 门店看到"期初为什么有负数"(猪耳 −1.279 / 猪肺 −17.64 / 猪手 −9.2 kg)。
//
// # 口径
//
//	链的起点 = **期初结转的最早生效日**(见 cutChainBaseline)
//	⇒ 起点之前的快照**不属于这条链** ⇒ 期初一改就必须**作废**
//	⇒ 不许出现"新期初 + 旧快照"并存的状态(先清,后重算)。
package service_test

import (
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// fmSnapCount 数某店在某天的成本日快照行数。
//
// ⚠️ 用**前缀匹配**而不是等值:gorm.io/datatypes 的 Date 落库保留时区表示
//
//	(业务时区 "2026-10-02T00:00:00+08:00" / UTC "2026-10-02T00:00:00Z")
//	⇒ 等值比较会因构造方式不同而落空(见 fmCutCostDaily 的同款注释)。
func fmSnapCount(t *testing.T, branchID, bizDate string) int64 {
	t.Helper()
	var n int64
	mustNoErr(t, testDBSingleton.Model(&model.CutCostDaily{}).
		Where("branch_id = ? AND biz_date LIKE ?", branchID, bizDate+"%").
		Count(&n).Error, "数快照行数")
	return n
}

// TestStockOpenings_SaveDropsStaleSnapshots 锁「改期初 ⇒ 作废起点之前的旧快照」。
//
// # 场景
//
//	期初(基线日) = 2026-10-05。
//	库里躺着两份快照:10-02(**链外**)+ 10-06(链内)。
//	门店再保存一次期初(哪怕值没变)⇒ 链的起点重新确定。
//
// # 期望
//
//	10-02 的快照**必须被删掉**(它是旧起点下的产物,不属于这条链)
//	10-06 的快照**必须还在**(链内的由重算/物化覆盖,不该在这里被清)。
//
// # 判别力
//
//	去掉 SaveStockOpenings 里的 dropStaleCostSnapshots ⇒ 10-02 的快照还在
//	⇒ 本用例立刻红。
func TestStockOpenings_SaveDropsStaleSnapshots(t *testing.T) {
	baseDay := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	nextDay := baseDay.AddDate(0, 0, 1) // 2026-10-06

	svc, ctx := fmChainSetup(t, map[string][]map[string]any{})
	svc.SetClock(fmClock(nextDay))

	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-05",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}

	// 两份快照:一份在链外(10-02),一份在链内(10-06)。
	fmChainSeedSnapshot(t, "S001", "2026-10-02", model.CutBelly, "-9.2", "-146.51", "15.9249")
	fmChainSeedSnapshot(t, "S001", "2026-10-06", model.CutBelly, "90", "1620", "18")
	if fmSnapCount(t, "S001", "2026-10-02") != 1 {
		t.Fatalf("前置:10-02 的快照应该存在")
	}

	// 门店再保存一次期初(起点重新确定)⇒ 链外的旧快照必须作废。
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-05",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings(第二次): %v", err)
	}

	if n := fmSnapCount(t, "S001", "2026-10-02"); n != 0 {
		t.Errorf("★ 期初生效日(10-05)之前的旧快照必须作废(它是旧起点下的产物),还剩 %d 行", n)
	}
	if n := fmSnapCount(t, "S001", "2026-10-06"); n != 1 {
		t.Errorf("★ 链内(基线日之后)的快照不该在这里被清,还剩 %d 行(应为 1)", n)
	}
}
