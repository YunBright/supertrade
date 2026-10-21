package service

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"
)

// AmountString 是"金额字段"的 JSON 表示。
//
// # 为什么不让它就是个 string
//
// 下游的金额字段有两种可能的形态:JSON 字符串("22.20",shopspring/decimal 默认)
// 与 JSON 数字(22.2,某些服务打开了 decimal.MarshalJSONWithoutQuotes)。
// 直接解码进 string 会在数字形态下报"cannot unmarshal number into Go value of type string",
// 于是**整个聚合挂掉** —— 症状是"某个服务的金额字段一出现,聚合就 500"。
//
// # 为什么写的时候还是字符串
//
// JSON number 到 Dart 侧会变 double,0.1 + 0.2 那类误差会直接进账。
// ⇒ 写时一律字符串,前端用 decimal 解析。
type AmountString string

// UnmarshalJSON 接受 JSON 字符串或数字,统一存成字符串。
func (a *AmountString) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*a = ""
		return nil
	}
	if trimmed[0] == '"' {
		var v string
		if err := json.Unmarshal(trimmed, &v); err != nil {
			return err
		}
		*a = AmountString(v)
		return nil
	}
	// JSON number:用 decimal 规范化(避免 22.200000000000003 这种浮点尾巴)。
	d, err := decimal.NewFromString(string(trimmed))
	if err != nil {
		return fmt.Errorf("posgateway: 金额字段不是合法数字(%s): %w", string(trimmed), err)
	}
	*a = AmountString(d.String())
	return nil
}

// MarshalJSON 始终写成 JSON 字符串。
func (a AmountString) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(a))
}

// String 返回金额的字符串形态。
func (a AmountString) String() string { return string(a) }
