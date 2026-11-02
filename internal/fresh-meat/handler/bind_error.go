package handler

// internal/fresh-meat/handler/bind_error.go —— **绑定错误说人话**。
//
// # 为什么不能直接 err.Error()
//
// gin 的绑定错误长这样:
//
//	Key: 'RecordWholePigInput.EarTag' Error:Field validation for 'EarTag'
//	failed on the 'required' tag
//
// ⇒ 那是 **Go 结构体的 key 路径**：门店既看不懂，也不知道该改哪个输入框
//   ⇒ 只能截图问人 ⇒ 典型的"技术故障"其实只是"少填了一个框"。
//
// # 口径
//
//	① 字段校验失败（缺必填 / 值不合法）⇒ 说清**哪些字段**有问题（中文）；
//	② JSON 本身就不合法 ⇒ 说清"格式不对"，不把解析器的原文抛给门店。

import (
	"errors"
	"fmt"
	"strings"

	"github.com/go-playground/validator/v10"
)

// bindFieldLabels 把 Go 字段名翻成**门店看得懂**的中文。
//
// ⚠️ 键是 **Go 字段名**（validator 的 `Field()` 给的就是它，不是 json 名）。
// ⚠️ 翻不到就退回字段名本身（"PigID"）⇒ 至少不是 key 路径那种天书。
var bindFieldLabels = map[string]string{
	// 整猪
	"EarTag":                "耳标号",
	"GrossWeightKg":         "毛重",
	"PurchaseUnitPriceYuan": "采购单价",
	"SupplierID":            "供应商",
	"ArrivedAt":             "到店时间",
	"HalfPig":               "半头猪",
	"OffalIncluded":         "带下水",
	"PurchaseGroupID":       "采购批次",
	// 分割补录
	"PigID":       "猪的 id",
	"CutType":     "部位",
	"Barcode":     "条码",
	"WeightKg":    "重量",
	"ExpiresAt":   "到期时间",
	"CubeProductID": "货号",
	// 盘点 / 搬台
	"TakenDate":      "归属日",
	"ActualRemainKg": "柜台实盘 kg",
	"IsComplete":     "是否完成",
	"Cuts":           "盘点明细",
	"QtyKg":          "数量 kg",
	"Lines":          "明细行",
	// 期初 / 大宗 / 其它
	"ColdStoreKg":   "冷库 kg",
	"UnitPriceYuan": "单价",
	"BizDate":       "生效日",
	"Note":          "备注",
	"ProductCode":   "品项",
	"Counterparty":  "交易对手",
	"Mode":          "结算方式",
	"Items":         "明细",
	"Date":          "日期",
	"Source":        "来源",
	"Location":      "位置",
	"Status":        "状态",
	"Reason":        "原因",
}

// badJSONMessage 把 gin 的绑定错误翻成**门店看得懂**的一句话。
//
// ⚠️ 只回给门店"哪个字段有问题"，不吐 Go 的 key 路径（见文件头注释）。
func badJSONMessage(err error) string {
	if err == nil {
		return "请求内容不对"
	}
	var verrs validator.ValidationErrors
	if errors.As(err, &verrs) && len(verrs) > 0 {
		names := make([]string, 0, len(verrs))
		seen := map[string]bool{}
		for _, fe := range verrs {
			label := bindFieldLabels[fe.Field()]
			if label == "" {
				label = fe.Field()
			}
			if seen[label] {
				continue
			}
			seen[label] = true
			names = append(names, label)
		}
		if len(names) == 0 {
			return "请求内容不对"
		}
		return fmt.Sprintf("这些必填项没填或填错了：%s", strings.Join(names, "、"))
	}
	// 不是字段校验错误 ⇒ 多半是 JSON 本身不合法（解析器原文对门店没意义）。
	return "请求格式不对（内容不是合法的 JSON，或字段类型不对）"
}
