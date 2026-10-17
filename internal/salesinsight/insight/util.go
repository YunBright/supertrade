package insight

import (
	"errors"
	"strconv"
	"strings"
)

// 本文件是 insight 包的**基础工具**。
//
// 为什么单独一个文件：本包其它文件刻意只 import 标准库的少数几个包，
// 而这些小工具被多处使用。集中在这里是为了让"裁剪/格式化"的规则
// 只有一份实现 —— 它们散落各处时，规则会慢慢漂移
// （有的地方保留省略号，有的地方不保留），
// 最终同一条内容在不同环节显示出不同长度。

// errUnparseable 表示无法从 LLM 输出里提取 JSON。
var errUnparseable = errors.New("insight: 无法从 LLM 输出解析出 JSON")

// errEmptyInsight 表示解析成功但内容全空。
//
// 它与 errUnparseable 分开是有意义的：
//   - 解析失败 ⇒ 模型输出格式不对，可能是 prompt 或模型问题
//   - 内容全空 ⇒ 模型"听话地"返回了空对象，可能是事实包太空
// 两者的排查方向完全不同，后台审计里也应该能区分。
var errEmptyInsight = errors.New("insight: LLM 返回的内容全空")

// stringsTrimpSpace 是 TrimSpace 的别名（供本包 trimSpace 使用）。
func stringsTrimpSpace(s string) string { return strings.TrimSpace(s) }

// indexOf / lastIndexOf 是 strings.Index / LastIndex 的短别名。
func indexOf(s, sub string) int    { return strings.Index(s, sub) }
func lastIndexOf(s, sub string) int { return strings.LastIndex(s, sub) }

// itoa 是 strconv.Itoa 的短别名。
func itoa(n int) string { return strconv.Itoa(n) }

// joinCN 用中文顿号连接（符合中文语境，逗号在长列表里容易读错行）。
func joinCN(items []string) string { return strings.Join(items, "、") }

// fmtFloat 保留两位小数，去掉无意义的尾随零。
//
// ⚠️ 为什么不用 %v 或直接 strconv.FormatFloat(v, 'f', -1, 64)：
//   金额出现 "15929.650000000001" 这种浮点尾巴时，LLM 很可能把它
//   当成一个精确值转述，而店长看到的是一串噪声。
//   两位小数是商超金额的自然精度，与 salesagg 的展示口径一致。
func fmtFloat(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	// 去掉尾随的 0（15929.65 保持，15929.00 → 15929）
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
		if s == "" || s == "-" {
			s = "0"
		}
	}
	return s
}

// fmtPct 格式化百分比。
//
// ⚠️ 入参约定**不统一**是刻意的，调用方必须自己清楚：
//   - 传 12.5 → "12.5%"（已经是百分点，如 Deltas.AmountPct）
//   - 传 0.125 → "12.5%"（是小数，如 AmountShare）
// 两种都用同一个函数，靠调用点传不同的值。
// 这个约定写在 FactDigest 的各个调用处，改动时必须一起看。
func fmtPct(v float64) string {
	// 极端值保护：LLM 看到 "+Inf%" 或 "1e+10%" 会写进结论里
	if v > 1e6 || v < -1e6 {
		return "—"
	}
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return s + "%"
}
