package service_test

// 2026-10-25:**期初结转**的读写与删除。
//
// 门店口径:期初是**冷库链的起点**,只设一次,而且**必须由门店给数**
// ("上线前冷库里已经有的货"——系统里没有那份流水)。
//
// ⇒ 三条不变量:
//  ① 按**部位**存(不是按 SKU),一个部位一条;
//  ② 重复保存 = 覆盖(幂等),负数拒绝;
//  ③ 能**显式删除**某个部位的期初(否则只能写 0 ⇒ 界面上分不清
//     "真的有 0"和"压根没有期初"⇒ 列表里一直挂着一条 0 kg 的脏行)。

import (
	"context"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

func TestStockOpening_SaveListDelete(t *testing.T) {
	svc, _ := setupTestService(t)
	ctx := context.Background()
	svc.SetClock(func() time.Time {
		return time.Date(2026, 10, 25, 14, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*3600))
	})

	// ① 保存两个部位
	out, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, ColdStoreKg: d("12.5"), UnitPriceYuan: d("18"), Note: "上线前冷库里的五花肉"},
		{CutType: model.CutFat, ColdStoreKg: d("8"), UnitPriceYuan: d("6")},
	})
	mustNoErr(t, err, "SaveStockOpenings")
	if len(out.Openings) != 2 {
		t.Fatalf("应存 2 个部位, got %d: %+v", len(out.Openings), out.Openings)
	}
	var belly service.StockOpeningView
	for _, o := range out.Openings {
		if o.CutType == model.CutBelly {
			belly = o
		}
	}
	if !belly.ColdStoreKg.Equal(d("12.5")) {
		t.Errorf("五花肉期初应为 12.5kg, got %s", belly.ColdStoreKg)
	}
	if belly.BizDate != "2026-10-25" {
		t.Errorf("期初生效日默认应为今天, got %q", belly.BizDate)
	}

	// ② 重复保存 = 覆盖(幂等),不是新增一行
	out, err = svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, ColdStoreKg: d("10"), UnitPriceYuan: d("18")},
	})
	mustNoErr(t, err, "SaveStockOpenings 覆盖")
	if len(out.Openings) != 2 {
		t.Fatalf("覆盖后应仍是 2 个部位, got %d: %+v", len(out.Openings), out.Openings)
	}
	for _, o := range out.Openings {
		if o.CutType == model.CutBelly && !o.ColdStoreKg.Equal(d("10")) {
			t.Errorf("五花肉期初应被覆盖成 10kg, got %s", o.ColdStoreKg)
		}
	}

	// ③ 负数拒绝
	if _, err := svc.SaveStockOpenings(ctx, "S001", []service.StockOpeningInput{
		{CutType: model.CutBelly, ColdStoreKg: d("-1"), UnitPriceYuan: d("18")},
	}); err == nil {
		t.Errorf("期初冷库量不能为负")
	}

	// ④ 显式删除某个部位 ⇒ 它从链的起点里消失
	out, err = svc.DeleteStockOpening(ctx, "S001", model.CutBelly)
	mustNoErr(t, err, "DeleteStockOpening")
	if len(out.Openings) != 1 || out.Openings[0].CutType != model.CutFat {
		t.Fatalf("删掉五花肉后期初应只剩 fat, got %+v", out.Openings)
	}
	// 删不存在的 ⇒ 报错(别静默成功)
	if _, err := svc.DeleteStockOpening(ctx, "S001", model.CutBelly); err == nil {
		t.Errorf("删不存在的期初应报错")
	}
}
