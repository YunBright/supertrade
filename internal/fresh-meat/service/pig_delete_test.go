package service_test

// 2026-10-25：**删掉录错了的猪**。
//
// # 门店反馈
//
// 早盘最常见的错：**多录一头** / **毛重打错**。
// ⇒ 没有删除就只能"将错就错" ⇒ 那一头会进成本池 ⇒ 当天毛利全歪，
// 而且**没人知道是哪头猪的锅**。
//
// # 三条不变量
//
//	① **干净**的猪可以删（没分割补录、没销售归属、不在已审计期间）；
//	② 有分割补录 ⇒ 不许删（会留下对不上的分割行）；
//	③ 删掉当天**最后一头**猪 ⇒ 营业日状态退回 `no_pig`
//	   （状态机平时只进不退，删除是唯一的例外 —— 门店要看到"还没录"）。

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/YunBright/supertrade/internal/fresh-meat/service"
)

// TestDeleteWholePig_CleanPigCanBeDeleted 锁 ①③。
func TestDeleteWholePig_CleanPigCanBeDeleted(t *testing.T) {
	svc, ctx, _ := fmBizSetup(t)

	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-DEL", GrossWeightKg: d("180"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: fmDay1,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	bd, err := svc.GetBusinessDay(ctx, "S001", fmDay1)
	mustNoErr(t, err, "GetBusinessDay")
	if !bd.HasPig || bd.PigCount != 1 {
		t.Fatalf("录猪后营业日应是 has_pig=true / pig_count=1, got %+v", bd)
	}

	// 删掉。
	if err := svc.DeleteWholePig(ctx, "S001", pig.ID); err != nil {
		t.Fatalf("★ 干净的猪应该能删: %v", err)
	}
	if _, err := svc.GetWholePig(ctx, "S001", pig.ID); err == nil {
		t.Errorf("★ 删掉的猪不该还能查到")
	} else if !errors.Is(err, service.ErrWholePigNotFound) {
		t.Errorf("删掉后应报 not_found, got %v", err)
	}

	// ③ 状态退回"没进猪"（门店在每日作业里要看到 ① 步骤还是"未做"）。
	bd, err = svc.GetBusinessDay(ctx, "S001", fmDay1)
	mustNoErr(t, err, "GetBusinessDay(after delete)")
	if bd.HasPig || bd.PigCount != 0 {
		t.Errorf("★ 删掉最后一头猪后 has_pig 应回到 false / pig_count=0, got %+v", bd)
	}
}

// TestDeleteWholePig_RefusesWhenHasCuts 锁 ②。
func TestDeleteWholePig_RefusesWhenHasCuts(t *testing.T) {
	svc, ctx, sku := fmBizSetup(t)

	pig, err := svc.RecordWholePig(ctx, "S001", model.RecordWholePigInput{
		EarTag: "EB-CUT", GrossWeightKg: d("180"), PurchaseUnitPriceYuan: d("20"),
		SupplierID: "SUP-1", ArrivedAt: fmDay1,
	}, "u-1")
	mustNoErr(t, err, "RecordWholePig")

	// 分割补录一行 ⇒ 这头猪不再"干净"。
	if _, err := svc.RecordPigCut(ctx, "S001", model.RecordPigCutInput{
		PigID: pig.ID, CutType: model.CutBelly, Barcode: "BC-DEL-1",
		WeightKg: d("50"), ExpiresAt: fmDay1.Add(48 * time.Hour), CubeProductID: sku,
	}, "u-1"); err != nil {
		t.Fatalf("分割补录: %v", err)
	}

	err = svc.DeleteWholePig(ctx, "S001", pig.ID)
	if err == nil {
		t.Fatalf("★ 已分割补录的猪不许删（会留下对不上的分割行）")
	}
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Errorf("应是输入校验错(400), got %v", err)
	}
	if !strings.Contains(err.Error(), "分割") {
		t.Errorf("★ 报错要说清为什么不能删（门店才知道下一步做什么）, got %v", err)
	}
	// 猪还在。
	if _, err := svc.GetWholePig(ctx, "S001", pig.ID); err != nil {
		t.Errorf("★ 删失败时猪必须还在, got %v", err)
	}
}

// TestDeleteWholePig_NotFound 锁"删不存在的猪 ⇒ 404"。
func TestDeleteWholePig_NotFound(t *testing.T) {
	svc, ctx, _ := fmBizSetup(t)
	err := svc.DeleteWholePig(ctx, "S001", "NO-SUCH-PIG")
	if err == nil {
		t.Fatalf("删不存在的猪应报错")
	}
	if !errors.Is(err, service.ErrWholePigNotFound) {
		t.Errorf("应是 not_found, got %v", err)
	}
}
