// Package cuberead 是「读 cube」这一层在各业务域之间**共用**的原语。
//
// # 为什么要有这个包
//
// 2026-10-22 这一批新增了 5 个读域(商品档案 / 流水 / 促销方案 / 采购收货 /
// 销售报表),每个域都要做同一批动作:
//
//	发一条 cube 查询 → 把返回行里的 decimal / 字符串 / 时间取出来
//	→ 按 FIELD_SPEC 的命名与精度格式化成 JSON 字符串
//
// 这三件事每域各写一遍,必然出现三种偏差:
//
//  1. **精度不一致**:一域写 StringFixed(2)、另一域写 StringFixed(4),
//     前端 parseDecimal() 之后算出来的钱差 0.01。
//  2. **空值语义不一致**:一域把 NULL 变成 "0.00",另一域变成 null。
//     ⚠️ 这是本仓反复踩的坑(见 catalog/service/picker_local.go 的
//     「未维护 ≠ 0 元」):NULL 变 0 会让仓管把 0 元当真价贴到货架上。
//  3. **时间解析不一致**:cube 的 MSSQL live 源把 datetime 返成
//     "08 23 2026  4:19PM"(依赖 DB language),而 duck 源返 ISO。
//     解析失败的行会被静默变成零值时间(0001-01-01)。
//
// ⇒ 把"取数 + 取值 + 格式化"这一层收敛到**一个**实现,各域只声明
// "我要查哪个 model / 哪些维度 / 哪些过滤"。
//
// # 金额一律 JSON 字符串
//
// PLAN §3.2 明写:「金额一律 JSON 字符串(后端 decimal),前端用 parseDecimal(),
// 不要当 number」。本包的 [Money] / [MoneyPtr] 负责这件事,业务域不许自己拼。
//
// # 精度按域定,不按类型定
//
//	4 位小数:商品档案的进价/批发价/零售价/会员价、流水里的单价与金额
//	          (源库 numeric 的原生精度就是 4,截成 2 位会丢分)
//	2 位小数:采购单据金额、报表金额与数量(图7 的口径是"元 / 两位")
//
// ⇒ 用 [Money] 与 [Money2],**不要**用同一个函数到处套。
package cuberead

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cubeclient"
)

// Reader 是「读一个 cube model」的最小能力。
//
// ⚠️ 收窄成一个方法的理由与 cubeclient.DaprCubeClient 收窄 daprInvoker 同源:
// 接口越窄,单测越能证明"到底往 cube 发了什么 query"。
//
// 生产实现是 *cubeclient.DaprCubeClient(它有 LoadCubeQueryPage);
// 单测注入一个记录 query 的 fake。
type Reader interface {
	// LoadCubeQueryPage 发一次 cube 查询,返回 data[] 与响应顶层的 total。
	//
	// total 只有在 CubeQuery.Total = true 时才是真值(不给时 cube 恒返 0 ——
	// 那是"没问",不是"总共 0 条")。
	LoadCubeQueryPage(ctx context.Context, modelName string, q cubeclient.CubeQuery) ([]map[string]any, int, error)
}

// Money 把 decimal 格式化成 **4 位小数的 JSON 字符串**。
//
// ⚠️ 返回 string 而不是 float64:JSON number 会把 4.9600 变成 4.96,
// 前端 parseDecimal() 之后再算就会差分。
//
// ⚠️ 精度用 4 位:源库 numeric 的原生精度就是 4(如 t_rm_saleflow.sale_price),
// 截成 2 位会丢掉分币以下的尾数(生鲜称重商品天天出现)。
func Money(d decimal.Decimal) string {
	return d.StringFixed(4)
}

// Money2 把 decimal 格式化成 **2 位小数的 JSON 字符串**。
//
// 用于采购单据与销售报表(图7 的口径是"元 / 两位小数")。
func Money2(d decimal.Decimal) string {
	return d.StringFixed(2)
}

// MoneyPtr 把 decimal 指针格式化成 JSON 字符串指针。
//
// nil(源库没维护)⇒ nil ⇒ 序列化成 **null**,不是 "0.0000"。
//
// ⚠️ 这就是「未维护 ≠ 0 元」这条约定的**唯一实现**:
// 业务域直接用指针类型承载金额,就别自己判 nil。
func MoneyPtr(d *decimal.Decimal) *string {
	if d == nil {
		return nil
	}
	s := Money(*d)
	return &s
}

// Money2Ptr 同 [MoneyPtr],但用 2 位小数。
func Money2Ptr(d *decimal.Decimal) *string {
	if d == nil {
		return nil
	}
	s := Money2(*d)
	return &s
}

// Dec 从 cube 返回的一行里取 decimal。
//
// ⚠️ MSSQL live 源的 decimal 经 go-mssqldb 以 []byte 返回,
// encoding/json 会把它序列化成 base64 ⇒ 必须走 cubeclient.ParseDecimal,
// 不要自己 parse 字符串(那会在某些驱动版本上静默得到 0)。
//
// 缺字段 / NULL ⇒ 零值 decimal(调用方要区分"没维护"时用 [DecPtr])。
func Dec(row map[string]any, key string) decimal.Decimal {
	return cubeclient.ParseDecimal(row[key])
}

// DecPtr 从 cube 返回的一行里取 decimal 指针。
//
// 缺字段 / NULL ⇒ nil(⇒ JSON null)。**"没维护"与"0.00"在这里分开**。
func DecPtr(row map[string]any, key string) *decimal.Decimal {
	v, ok := row[key]
	if !ok || v == nil {
		return nil
	}
	d := cubeclient.ParseDecimal(v)
	return &d
}

// Str 从 cube 返回的一行里取字符串(去首尾空白)。
//
// 缺字段 / NULL ⇒ ""。
//
// ⚠️ 思迅的 char 定长列带尾随空格(hbposv7 的 item_no 补空格、ysx 不补),
// 而 cube 查询层只保证 SELECT 侧 RTRIM ⇒ 这里再 trim 一次才敢当 map 键。
func Str(row map[string]any, key string) string {
	v, ok := row[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case []byte:
		return strings.TrimSpace(string(t))
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

// Bool 从 cube 返回的一行里取布尔值。
//
// cube 的 boolean 列可能是 bool / "1" / "0" / 数字 ⇒ 都认。
// 缺字段 ⇒ false。
func Bool(row map[string]any, key string) bool {
	v, ok := row[key]
	if !ok || v == nil {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "1" || strings.EqualFold(t, "true")
	case float64:
		return t != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	case decimal.Decimal:
		return !t.IsZero()
	default:
		return false
	}
}

// Time 把 cube 返回的时间串规范化成 **RFC3339**(带 +08:00 偏移)。
//
// ⚠️ cube 的 MSSQL live 源把 datetime 返成 "08 23 2026  4:19PM"
// (依赖 DB language 的布局),而 duck 源返 "2021-09-17T09:55:13"。
// 解析失败的串**原样返回**(不返零值时间)——
// 零值时间会被前端渲染成 0001-01-01,看起来像"很久以前的单"。
//
// # ⚠️ 无时区的串按 **UTC+8 记账**,不是从 UTC 换算
//
// time.Parse 对无时区的串返回 **UTC** ⇒ 直接 Format 会平移 8 小时
// ("09 17 2021 9:55AM" 变成 17:55)。
//
// 而思迅源库是按 UTC+8 记账的 ⇒ 这里把**墙钟时间直接标成 +08:00**
// (而不是换算时刻)。否则每一笔单的时间都会差 8 小时。
//
// 返回值带 +08:00 是因为 PLAN §3.2 的示例("2026-10-22T21:54:52+08:00")就是 +08:00。
func Time(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	cst := time.FixedZone("CST", 8*3600)
	// 带时区的 ISO:时刻不变,只换显示时区。
	for _, layout := range []string{
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02 15:04:05Z07:00",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.In(cst).Format(time.RFC3339)
		}
	}
	// 无时区的:MSSQL live 源 / 源库本地时间 ⇒ 按 UTC+8 记账。
	for _, layout := range []string{
		"01 _2 2006 3:04PM",
		"1 _2 2006 3:04PM", // 月不补零兜底(实测源数据月恒为 2 位,防脏数据)
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			local := time.Date(t.Year(), t.Month(), t.Day(),
				t.Hour(), t.Minute(), t.Second(), 0, cst)
			return local.Format(time.RFC3339)
		}
	}
	return s
}

// Date 把 cube 返回的时间串规范化成 **YYYY-MM-DD**。
//
// 解析失败保留原串(理由同 [Time])。
func Date(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, layout := range []string{
		"01 _2 2006 3:04PM",
		"1 _2 2006 3:04PM",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return s
}

// Filter 构造一条 cube 过滤条件(算子名用常量,别写裸字符串)。
//
// ⚠️ 算子名是**跨仓契约**(cube 编译成 SQL 语义),
// 拼错了只有到运行时才知道(400 unsupported operator)。
func Filter(member, operator string, values ...any) cubeclient.CubeFilter {
	return cubeclient.CubeFilter{Member: member, Operator: operator, Values: values}
}

// InFilter 构造一条 `in` 过滤;**空集合**用一条恒假条件表达。
//
// ⚠️ cube 的 `in` 至少要一个值(空集合直接 400),而"按空集合过滤"的语义是
// **命中 0 行**。当成"不过滤"会返回全店数据(实测门店 00 是 27565 行),
// 而且**全程不报错**。
//
// 哨兵值取的是 cubeclient 内部那个常量同款(不可能是真实 id 的字符串)。
func InFilter(member string, values []string) cubeclient.CubeFilter {
	vals := make([]any, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return cubeclient.CubeFilter{
			Member:   member,
			Operator: cubeclient.OperatorEquals,
			Values:   []any{"__no_match_in_empty_set__"},
		}
	}
	return cubeclient.CubeFilter{
		Member:   member,
		Operator: cubeclient.OperatorIn,
		Values:   vals,
	}
}
