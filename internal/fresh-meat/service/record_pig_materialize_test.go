package service_test

// 回归锁:**录猪(进猪)必须触发成本链物化**(2026-11-01 修)。
//
// # 为什么这条必须锁
//
//	成本链是**逐日接续**的 ⇒ 任何一天的进货/产出变了,后面所有天的成本都要跟着变。
//	⇒ 写路径(录猪/删猪/大宗/搬台/期初结转)之后必须把链重算一遍,
//	  否则门店看到的成本是旧的。
//
// ⚠️ **录猪这条路径以前没挂**(只挂了盘点/搬台/大宗/删猪/期初结转)
// ⇒ 生产实测(店 00):11-01 14:24 录了两头猪,而成本日快照的**最后写入停在 10-31 23:40**
// ⇒ 11-01 整天**没有快照**⇒ 依赖快照的读路径(长期毛利页)读不到当天。
//
// ⚠️ 判别力:把 RecordWholePig 里的 recomputeCostSnapshots 去掉 ⇒ 本用例必红。

import (
	"testing"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
)

// TestRecordWholePigTriggersChainMaterialize 锁:录猪 ⇒ 成本链物化被触发。
func TestRecordWholePigTriggersChainMaterialize(t *testing.T) {
	svc, ctx := ssSetup(t)
	// ⚠️ 注入**同步**执行器(生产是异步)⇒ 断言不用等后台任务。
	//	默认的单测执行器是**禁用**的 ⇒ 这里必须显式注入,否则物化不会发生。
	materialized := 0
	svc.SetChainMaterializer(func(f func()) {
		materialized++
		f()
	})

	svc.SetClock(fmClock(ssDay1))
	if _, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "PIG-TRIGGER", GrossWeightKg: d("100"), PurchaseUnitPriceYuan: d("16"),
		SupplierID: "SUP-1", ArrivedAt: ssDay1,
	}, "u-1"); err != nil {
		t.Fatalf("RecordWholePig: %v", err)
	}

	if materialized == 0 {
		t.Fatal("★ 录猪后必须触发成本链物化" +
			"(以前没挂 ⇒ 成本日快照停在旧口径,长期毛利页读不到当天)")
	}
}
