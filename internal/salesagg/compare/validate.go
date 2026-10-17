package compare

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrInvalid 是全部校验错误的哨兵（handler 用 errors.Is 映射
// 400 compare_template_invalid）。每个校验失败都包一层它。
var ErrInvalid = errors.New("compare: 模板配置不合法")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// 门店对比的业务边界。
//
// 前端（Flutter SalesCompareDraft / 编辑页）与这里各持一份；前端约束是
// 体验优化，后端是最终防线 —— 改动时两边必须同步，否则前端会把能存的
// 配置拒掉，或把存进去的配置渲染崩。
const (
	MinStores = 2
	MaxStores = 3
	// MinRows 只约束**保存**；预览允许 0 行（空草稿也要能取数，
	// 用户先看各店列通不通再配行），见 ValidatePreview。
	MinRows = 1
	MaxRows = 30
	// MaxName 是模板名 / 行名的字数上限（rune 数，不是字节 —— 中文名按字算）。
	MaxName = 30
)

// 维度与 service 包（报表）同一口径：category / product。
// 两处独立定义是因为 compare 不 import service（保持存储包零依赖）；
// 常量漂移会被 compare/validate_test.go 与 service/compare_test.go 的
// 交叉断言同时暴露。
const (
	DimCategory = "category"
	DimProduct  = "product"
)

// ValidDimension 报告 d 是否是受支持的对比维度。
func ValidDimension(d string) bool {
	return d == DimCategory || d == DimProduct
}

// SaveInput 是创建 / 整体替换模板的请求体。
//
// revision 只在 PUT 时由调用方带（乐观锁）；POST 上它被忽略。
// stores/rows 的 wire 形状与模板 JSON 里的同名字段一致。
type SaveInput struct {
	Name      string    `json:"name"`
	Dimension string    `json:"dimension"`
	Stores    []string  `json:"stores"`
	Rows      []RowJSON `json:"rows"`
	Revision  *int64    `json:"revision"`
}

// PreviewBody 是草稿预览（POST /compare/preview）的请求体：
// 配置内联 + 日期。没有 name —— 预览的是数据形状，不是要保存的资源。
type PreviewBody struct {
	Dimension string    `json:"dimension"`
	Stores    []string  `json:"stores"`
	Rows      []RowJSON `json:"rows"`
	Preset    string    `json:"preset"`
	From      string    `json:"from"`
	To        string    `json:"to"`
}

// ValidateSave 校验创建 / 整体替换入参（rows 1~30 + name 非空）。
func ValidateSave(in SaveInput) error {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return invalid("模板名不能为空")
	}
	if utf8.RuneCountInString(name) > MaxName {
		return invalid("模板名最多 %d 个字", MaxName)
	}
	return validateConfig(in.Dimension, in.Stores, in.Rows, MinRows)
}

// ValidatePreview 校验预览入参（rows 0~30，无 name）。
func ValidatePreview(in PreviewBody) error {
	return validateConfig(in.Dimension, in.Stores, in.Rows, 0)
}

// validateConfig 是两组校验的公共部分。
func validateConfig(dimension string, stores []string, rows []RowJSON, minRows int) error {
	if !ValidDimension(dimension) {
		return invalid("不支持的维度 %q（可选 category / product）", dimension)
	}
	if len(stores) < MinStores || len(stores) > MaxStores {
		return invalid("门店数量必须在 %d~%d 之间", MinStores, MaxStores)
	}
	seen := make(map[string]bool, len(stores))
	for _, st := range stores {
		st = strings.TrimSpace(st)
		if st == "" {
			return invalid("门店 ID 不能为空")
		}
		if seen[st] {
			return invalid("门店 %s 重复", st)
		}
		seen[st] = true
	}
	if len(rows) < minRows {
		return invalid("至少需要 %d 行对比项", minRows)
	}
	if len(rows) > MaxRows {
		return invalid("对比行最多 %d 行", MaxRows)
	}
	for i, r := range rows {
		if strings.TrimSpace(r.Key) == "" {
			return invalid("第 %d 行缺少 key", i+1)
		}
		if n := utf8.RuneCountInString(strings.TrimSpace(r.Label)); n > MaxName {
			return invalid("第 %d 行行名最多 %d 个字", i+1, MaxName)
		}
	}
	return nil
}
