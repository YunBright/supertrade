// 2026-10-30:锁「**改期初日期 ⇒ 同事务清掉链外的旧账**」(owner 要求)。
//
// # owner 原话
//
//	「按理若要修改期初日期,需要把起初日期当天和之前的所有采购录入(猪)、
//	  搬台、日终盘点都删除干净,但是之前没有这样的验证就修改入库了,
//	  所以趁此机会可以在修改期初日期时增加这个事务判断」
//
// # 为什么要清
//
//	系统**本来就拒收**链起点当天及之前的营业日动作(assertAfterChainStart:
//	"期初结转的生效日是链的起点(基线日),不是营业日")。
//	⇒ 但那条守卫只拦**新录的** ⇒ 改期初之前已经录进去的留在库里
//	⇒ 它们污染新链(实测:店 00 在 10-02 搬了猪手 9.2kg、没有对应的猪
//	  ⇒ 冷链里猪手 = −9.2kg ⇒ 顺着"没录期初的部位"漏进基线日的期初)。
//
// # 三条口径
//
//	① 链起点当天及之前(= assertAfterChainStart 的同一条边界)的**链外记录**要清;
//	② 链起点**之后**的记录**不许动**(那是链内的账);
//	③ 保存被拒(校验不过)时**不许清**(没生效的保存不能动数据)。
package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
	"github.com/shopspring/decimal"
)

// fcCountCounterLoad 数某店某个归属日的搬台流水行数。
func fcCountCounterLoad(t *testing.T, branchID, bizDate string) int64 {
	t.Helper()
	day, err := time.ParseInLocation("2006-01-02", bizDate, time.UTC)
	mustNoErr(t, err, "parse bizDate")
	var n int64
	mustNoErr(t, testDBSingleton.Model(&model.CounterLoadRecord{}).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?",
			branchID, day.AddDate(0, 0, -1), day.AddDate(0, 0, 2)).
		Count(&n).Error, "数搬台流水")
	return n
}

// fcPositionQty 读位置表某个 SKU/位置上的重量。
func fcPositionQty(t *testing.T, branchID, sku string, loc model.StockLocation) decimal.Decimal {
	t.Helper()
	var rec model.SkuStockPosition
	err := testDBSingleton.Where("branch_id = ? AND cube_product_id = ? AND location = ?",
		branchID, sku, string(loc)).First(&rec).Error
	if err != nil {
		return decimal.Zero
	}
	return rec.QtyKg
}

// fcSetupChainCleanup 起一个"有源门店"的环境 + 登记猪手的货号映射。
func fcSetupChainCleanup(t *testing.T) (*service.Service, context.Context) {
	t.Helper()
	svc, ctx := fmChainSetup(t, map[string][]map[string]any{})
	if _, err := svc.CreateBranchCutMapping(ctx, "S001",
		model.CreateBranchCutMappingInput{
			CutType: model.CutTrotter, CubeProductID: "P-TROT"}); err != nil {
		t.Fatalf("CreateBranchCutMapping trotter: %v", err)
	}
	return svc, ctx
}

// TestStockOpenings_SaveCleansChainExternalRecords 锁 ①:改期初 ⇒ 清掉链外的搬台。
//
// # 场景(复刻生产:店 00)
//
//	链外有一笔 10-02 的搬台(猪手 9.2kg,没有对应的猪)⇒ 冷链里猪手 = −9.2kg。
//	门店随后把期初生效日定成 **10-05**(= 链的起点)。
//
// # 期望
//
//	10-02 的搬台**被删掉**(它是链外的旧账)
//	⇒ 位置表也跟着回退(货"搬回冷库"):柜台 0 / 冷库 0(原来被搬到柜台的 9.2kg 撤销)。
//
// # 判别力
//
//	去掉 SaveStockOpenings 里的 dropChainExternalRecords 调用 ⇒ 10-02 的搬台还在
//	⇒ 本用例立刻红。
func TestStockOpenings_SaveCleansChainExternalRecords(t *testing.T) {
	svc, ctx := fcSetupChainCleanup(t)
	svc.SetClock(fmClock(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)))

	// 链外的一笔搬台(这时还没有期初 ⇒ 没有链起点 ⇒ 允许录)。
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		TakenDate: "2026-10-02",
		Lines:     []service.CounterLoadLine{{CubeProductID: "P-TROT", QtyKg: d("9.2")}},
	}, "u-1"); err != nil {
		t.Fatalf("CounterLoad: %v", err)
	}
	if n := fcCountCounterLoad(t, "S001", "2026-10-02"); n != 1 {
		t.Fatalf("前置:10-02 的搬台应该存在(实际 %d 行)", n)
	}

	// 门店把期初生效日定成 10-05 ⇒ 链的起点 = 10-05 ⇒ 10-02 是链外。
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-05",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}

	if n := fcCountCounterLoad(t, "S001", "2026-10-02"); n != 0 {
		t.Errorf("★ 链起点(10-05)之前/当天的旧账必须清干净,10-02 的搬台还剩 %d 行", n)
	}
	// 位置表:搬台把 9.2kg 搬到了柜台 ⇒ 删掉它 ⇒ 货"搬回冷库" ⇒ 两处都该是 0。
	if q := fcPositionQty(t, "S001", "P-TROT", model.StockLocationCounter); !q.IsZero() {
		t.Errorf("★ 删掉链外的搬台 ⇒ 柜台余额要回退,got %s", q)
	}
	if q := fcPositionQty(t, "S001", "P-TROT", model.StockLocationColdStore); !q.IsZero() {
		t.Errorf("★ 删掉链外的搬台 ⇒ 冷库余额要回退,got %s", q)
	}
}

// TestStockOpenings_SaveKeepsChainInternalRecords 锁 ②:链起点之后的记录不许动。
//
// # 场景
//
//	期初生效日 = 10-05(链的起点);门店在 **10-06** 又搬了一次台。
//	⇒ 10-06 是链内的账 ⇒ 改期初**不许**把它删掉。
func TestStockOpenings_SaveKeepsChainInternalRecords(t *testing.T) {
	svc, ctx := fcSetupChainCleanup(t)
	svc.SetClock(fmClock(time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)))

	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-05",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		TakenDate: "2026-10-06",
		Lines:     []service.CounterLoadLine{{CubeProductID: "P-TROT", QtyKg: d("3")}},
	}, "u-1"); err != nil {
		t.Fatalf("CounterLoad: %v", err)
	}

	// 再保存一次期初(改别的数值)⇒ 触发链外清理 ⇒ 链内的 10-06 必须还在。
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-05",
			ColdStoreKg: d("120"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings(第二次): %v", err)
	}
	if n := fcCountCounterLoad(t, "S001", "2026-10-06"); n != 1 {
		t.Errorf("★ 链起点(10-05)之后的搬台是链内的账,不许删,还剩 %d 行(应为 1)", n)
	}
}

// TestStockOpenings_SaveKeepsChainStartDayRecords 锁 ②的另一半:**链起点当天**的记录不许删。
//
// # 为什么(硬约束,不是偏好)
//
//	链起点当天 = **链的第一天**:期初 = 那天**早上**的冷库,
//	日终盘点 = 那天**傍晚**的实盘 ⇒ 那天的冷库 = 期初 + 盘到的量。
//	(现有口径锁在 counter_load_rows_test.go::TestOpening_SeedsTheDailyBalanceChain:
//	 期初 100 + 盘点 10 = 110)
//	⇒ 删掉链起点当天的记录 = **把链算错**。
//
// # 场景
//
//	期初生效日 = 10-05;门店在 **10-05 当天**搬了一次台。
//	⇒ 改期初时它必须还在。
func TestStockOpenings_SaveKeepsChainStartDayRecords(t *testing.T) {
	svc, ctx := fcSetupChainCleanup(t)
	svc.SetClock(fmClock(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)))

	// 链起点当天的一笔搬台(这时还没有期初 ⇒ 没有链起点 ⇒ 允许录)。
	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		TakenDate: "2026-10-05",
		Lines:     []service.CounterLoadLine{{CubeProductID: "P-TROT", QtyKg: d("2")}},
	}, "u-1"); err != nil {
		t.Fatalf("CounterLoad: %v", err)
	}

	// 期初生效日 = 10-05 ⇒ 那天是链的第一天 ⇒ 它的搬台不许被清。
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-05",
			ColdStoreKg: d("100"), UnitPriceYuan: d("18")},
	}); err != nil {
		t.Fatalf("SaveStockOpenings: %v", err)
	}
	if n := fcCountCounterLoad(t, "S001", "2026-10-05"); n != 1 {
		t.Errorf("★ 链起点当天的搬台是链的第一天的账,不许删,还剩 %d 行(应为 1)", n)
	}
}

// TestStockOpenings_RejectedSaveKeepsOldRecords 锁 ③:保存被拒 ⇒ 不许清任何东西。
//
// # 场景
//
//	链外有 10-02 的搬台;门店这次提交的期初**不合法**(有货却没填进价)
//	⇒ 保存被拒(400)⇒ **旧账不许被动**(没生效的保存不能改数据)。
func TestStockOpenings_RejectedSaveKeepsOldRecords(t *testing.T) {
	svc, ctx := fcSetupChainCleanup(t)
	svc.SetClock(fmClock(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)))

	if _, err := svc.CounterLoad(ctx, "S001", service.CounterLoadInput{
		TakenDate: "2026-10-02",
		Lines:     []service.CounterLoadLine{{CubeProductID: "P-TROT", QtyKg: d("9.2")}},
	}, "u-1"); err != nil {
		t.Fatalf("CounterLoad: %v", err)
	}

	// 有货却没填进价 ⇒ 必须被拒。
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, BizDate: "2026-10-05", ColdStoreKg: d("100")},
	}); err == nil {
		t.Fatalf("★ 有货没填进价的期初必须被拒")
	}
	if n := fcCountCounterLoad(t, "S001", "2026-10-02"); n != 1 {
		t.Errorf("★ 保存被拒 ⇒ 不许清链外的旧账,10-02 的搬台还剩 %d 行(应为 1)", n)
	}
}
