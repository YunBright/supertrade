package compare

import (
	"errors"
	"strings"
	"testing"

	"github.com/YunBright/supertrade/internal/salesagg/service"
)

// strPtr 是 *string 字面量的便捷函数。
func strPtr(s string) *string { return &s }

// validSave 是一份合法的保存入参，各用例在它上面做单项破坏。
func validSave() SaveInput {
	return SaveInput{
		Name:      "门店对比模板",
		Dimension: DimCategory,
		Stores:    []string{"S001", "S002"},
		Rows: []RowJSON{{
			Key:   "r1",
			Label: "猪肉档",
			Items: map[string]*string{"S001": strPtr("01"), "S002": nil},
		}},
	}
}

// TestDimensionConstantsMatchService 锁 compare 与 service 两个包的维度
// 口径一致。
//
// compare 存模板时按这里的白名单校验，service.CompareData 按它自己的
// 白名单再拒一次 —— 两张表一旦漂移，症状是"保存成功、取数 400"
// （compare 多收了 service 不认的维度）或反过来"前端可选维度存不进去"。
// 两包互不 import 是刻意的（保持存储层零依赖），所以只能靠测试跨包断言。
func TestDimensionConstantsMatchService(t *testing.T) {
	if DimCategory != service.DimCategory || DimProduct != service.DimProduct {
		t.Fatalf("compare 维度常量 (%q,%q) 与 service (%q,%q) 漂移",
			DimCategory, DimProduct, service.DimCategory, service.DimProduct)
	}
	for _, d := range []string{DimCategory, DimProduct} {
		if !ValidDimension(d) || !service.ValidDimension(d) {
			t.Errorf("维度 %q 应同时被两包认可", d)
		}
	}
}

func TestValidateSave_Ok(t *testing.T) {
	if err := ValidateSave(validSave()); err != nil {
		t.Fatalf("ValidateSave(合法入参) = %v, want nil", err)
	}
	// 名字恰好 30 字（边界内，含中文按字计）。
	in := validSave()
	in.Name = strings.Repeat("名", MaxName)
	if err := ValidateSave(in); err != nil {
		t.Fatalf("30 字模板名应合法, got %v", err)
	}
}

// TestValidateSave_Matrix 单项破坏矩阵：每条只破坏一个约束，
// 且必须是 ErrInvalid（handler 靠 errors.Is 映射 400）。
func TestValidateSave_Matrix(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*SaveInput)
	}{
		{"1 家门店", func(in *SaveInput) { in.Stores = in.Stores[:1] }},
		{"4 家门店", func(in *SaveInput) { in.Stores = []string{"A", "B", "C", "D"} }},
		{"重复门店", func(in *SaveInput) { in.Stores = []string{"S001", "S001"} }},
		{"空白门店 ID", func(in *SaveInput) { in.Stores = []string{"S001", "  "} }},
		{"0 行", func(in *SaveInput) { in.Rows = nil }},
		{"31 行", func(in *SaveInput) {
			in.Rows = nil
			for i := 0; i < MaxRows+1; i++ {
				in.Rows = append(in.Rows, RowJSON{Key: string(rune('a' + i%26)) + string(rune('a'+i/26)), Label: "行"})
			}
		}},
		{"空白模板名", func(in *SaveInput) { in.Name = "   " }},
		{"31 字模板名", func(in *SaveInput) { in.Name = strings.Repeat("名", MaxName+1) }},
		{"未知维度", func(in *SaveInput) { in.Dimension = "brand" }},
		{"空维度", func(in *SaveInput) { in.Dimension = "" }},
		{"空行 key", func(in *SaveInput) { in.Rows[0].Key = " " }},
		{"31 字行名", func(in *SaveInput) { in.Rows[0].Label = strings.Repeat("行", MaxName+1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validSave()
			tc.mutate(&in)
			err := ValidateSave(in)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("ValidateSave err = %v, want 包含 ErrInvalid", err)
			}
		})
	}
}

// TestValidatePreview_PreviewAllowsZeroRows 预览与保存的关键差异：
// 0 行合法（空草稿先看各店列通不通再配行），31 行仍非法。
func TestValidatePreview_PreviewAllowsZeroRows(t *testing.T) {
	ok := PreviewBody{Dimension: DimCategory, Stores: []string{"S001", "S002"}, Rows: nil}
	if err := ValidatePreview(ok); err != nil {
		t.Fatalf("预览 0 行应合法, got %v", err)
	}
	tooMany := PreviewBody{Dimension: DimCategory, Stores: []string{"S001", "S002"}, Rows: make([]RowJSON, MaxRows+1)}
	if err := ValidatePreview(tooMany); !errors.Is(err, ErrInvalid) {
		t.Fatalf("预览 31 行 err = %v, want ErrInvalid", err)
	}
	badDim := PreviewBody{Dimension: "brand", Stores: []string{"S001", "S002"}}
	if err := ValidatePreview(badDim); !errors.Is(err, ErrInvalid) {
		t.Fatalf("预览未知维度 err = %v, want ErrInvalid", err)
	}
}
