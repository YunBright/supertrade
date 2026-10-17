// Package compare 实现门店对比模板的持久化（表 sales_compare_templates）。
//
// 模板是**全局共享资源**：所有登录用户可读/可建/可改/可删，不属于任何
// 门店、也不带 owner 概念（wiki 式协作）。它只存「门店 + 维度 + 对比行
// 映射」，不存日期 —— 日期是每次取数的参数，两者生命周期分离。
//
// # 为什么 stores / rows 存 JSON 文本而不是 jsonb
//
// 单元测试用 SQLite 内存库，jsonb 方言不兼容（会让"生产 PG / 测试 SQLite"
// 走两套模型）；且模板行数 ≤30、门店 ≤3，永远整读整写，jsonb 的部分
// 更新能力毫无用武之地。
package compare

import (
	"encoding/json"
	"fmt"
	"time"
)

// Template 是 GORM 模型（sales_compare_templates）。
//
// 该结构**不直接**做 HTTP 序列化：stores/rows 在库里是 JSON 文本，
// 对外必须是解开的数组 —— 转换见 ToJson / TemplateFromInput。
type Template struct {
	ID            string    `gorm:"column:id;type:varchar(64);primaryKey"`
	Name          string    `gorm:"column:name;type:varchar(64)"`
	Dimension     string    `gorm:"column:dimension;type:varchar(16)"`
	StoresJSON    string    `gorm:"column:stores;type:text"`
	RowsJSON      string    `gorm:"column:rows;type:text"`
	Revision      int64     `gorm:"column:revision"`
	CreatedBy     string    `gorm:"column:created_by;type:varchar(64)"`
	CreatedByName string    `gorm:"column:created_by_name;type:varchar(64)"`
	UpdatedBy     string    `gorm:"column:updated_by;type:varchar(64)"`
	UpdatedByName string    `gorm:"column:updated_by_name;type:varchar(64)"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
}

// TableName 指定表名（purge registry 也引用这个字符串）。
func (Template) TableName() string { return "sales_compare_templates" }

// RowJSON 是模板里一行对比项的 wire 形状。
//
// Items 的 value 用 *string：前端把「该店未设置编码」存成 null
//（SalesCompareItem.items 是 Map<String, String?>），nil 指针与之对应；
// 若用 string，null 会变成解析错误，把"没设置"误判成坏请求。
type RowJSON struct {
	Key   string             `json:"key"`
	Label string            `json:"label"`
	Items map[string]*string `json:"items"`
}

// TemplateJSON 是模板对外的完整 JSON 形状（契约真相源 =
// Flutter SalesCompareTemplate.fromJson，字段名不得擅改）。
type TemplateJSON struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Dimension     string    `json:"dimension"`
	Stores        []string  `json:"stores"`
	Rows          []RowJSON `json:"rows"`
	Revision      int64     `json:"revision"`
	CreatedBy     string    `json:"created_by"`
	CreatedByName string    `json:"created_by_name"`
	UpdatedBy     string    `json:"updated_by"`
	UpdatedByName string    `json:"updated_by_name"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ToJson 把持久化模型转成对外形状。
//
// stores/rows 是我们**自己**序列化进去的（保存前已过校验），
// 解不开说明库被人改过 —— 返回错误而不是静默丢字段。
func (t *Template) ToJson() (TemplateJSON, error) {
	out := TemplateJSON{
		ID:            t.ID,
		Name:          t.Name,
		Dimension:     t.Dimension,
		Stores:        []string{},
		Rows:          []RowJSON{},
		Revision:      t.Revision,
		CreatedBy:     t.CreatedBy,
		CreatedByName: t.CreatedByName,
		UpdatedBy:     t.UpdatedBy,
		UpdatedByName: t.UpdatedByName,
		CreatedAt:     t.CreatedAt,
		UpdatedAt:     t.UpdatedAt,
	}
	if t.StoresJSON != "" {
		if err := json.Unmarshal([]byte(t.StoresJSON), &out.Stores); err != nil {
			return TemplateJSON{}, fmt.Errorf("compare: 模板 %s stores 列反序列化: %w", t.ID, err)
		}
	}
	if out.Stores == nil {
		out.Stores = []string{}
	}
	if t.RowsJSON != "" {
		if err := json.Unmarshal([]byte(t.RowsJSON), &out.Rows); err != nil {
			return TemplateJSON{}, fmt.Errorf("compare: 模板 %s rows 列反序列化: %w", t.ID, err)
		}
	}
	if out.Rows == nil {
		out.Rows = []RowJSON{}
	}
	return out, nil
}

// TemplateFromInput 把保存入参组装成持久化模型。
//
// ID / Revision / CreatedBy / UpdatedBy 由 store 层与 handler 在
// Create / Update 时填，这里只组装业务列。不可解的失败只剩内存
// marshal（不会发生），错误仅为签名完整。
func TemplateFromInput(in SaveInput) (*Template, error) {
	t := &Template{
		Name:      in.Name,
		Dimension: in.Dimension,
	}
	storesB, err := json.Marshal(in.Stores)
	if err != nil {
		return nil, fmt.Errorf("compare: stores 序列化: %w", err)
	}
	t.StoresJSON = string(storesB)
	rowsB, err := json.Marshal(in.Rows)
	if err != nil {
		return nil, fmt.Errorf("compare: rows 序列化: %w", err)
	}
	t.RowsJSON = string(rowsB)
	return t, nil
}
