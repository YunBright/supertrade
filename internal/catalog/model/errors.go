// Package model / errors.go —— 「门店 → 数据源」相关的业务错误 sentinel。
//
// # 为什么单独一个文件
//
// 这三个错误描述的是**同一个判据**的三个结局,而这个判据在会员域
// (internal/member/model/errors.go)已经落地过一次。
//
// ⚠️ **同名同义**:同一个门店在 member 与 catalog 里必须得到**同一个解释**
// (配了源 ⇒ 走 cube;没配 ⇒ 走本地;问不到 ⇒ 503)。
// 两边各写一份字面量时,改了一边不会编译报错,
// 症状是"同一个门店在会员页能看、在商品页看不到",而两边都"有道理"。
//
// # 三个结局
//
//	ErrCubeSourceUnavailable —— 问不到路由(链路故障)⇒ 503。
//	ErrCubeSourceDisabled    —— 配了源但被管理员关停 ⇒ 503。
//	ErrExternalSourceReadOnly —— 档案在外部系统里 ⇒ 本域只读(写端点 409)。
package model

import "errors"

var (
	// ErrCubeSourceUnavailable:问不到"这个门店配了哪个数据源"(503)。
	//
	// ⚠️ 与"没配数据源"严格区分:没配是**正常状态**(自营店 10),
	// 问不到是**链路故障** ⇒ 后者必须让用户看到"暂不可用",
	// 而不是让它悄悄回落成本地 ——
	// 那正是 2026-10-21 生产故障的形状:
	// 自营店 10 的商品选择控件恒报 `cube 404 not found`,
	// 而门店 10 的本地表里明明有 27565 个商品。
	ErrCubeSourceUnavailable = errors.New("catalog: 数据源路由暂不可用")
	// ErrCubeSourceDisabled:该门店的数据源被管理员停用(503)。
	//
	// ⚠️ **不等于"没配"**:回落成"没配"会让用户看到一份空列表,
	// 而真相是"这个门店现在查不了"。
	ErrCubeSourceDisabled = errors.New("catalog: 该门店的数据源已停用")
	// ErrExternalSourceReadOnly:该门店的商品档案来自外部系统,本域只读(409)。
	//
	// ⚠️ code 与会员域同名(`member_source_is_external`):
	// 判据全服务一致 ⇒ 同一个门店的两种档案得到同一种解释。
	ErrExternalSourceReadOnly = errors.New("catalog: 该门店的商品档案来自外部系统,本域只读")
)
