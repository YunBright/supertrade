// Package service / ids.go —— 业务主键生成。
//
// 主键是 varchar(36) 的 uuid v4（与 bankaccount / stocktake / settlement-auditor 同款）。
// 批号 / 单号这类**给人看**的编号不在这里（见 batches.go 的 CR-<branch>-<day>-<seq>）。
package service

import "github.com/google/uuid"

// uuidV4 生成业务主键。
func uuidV4() string { return uuid.NewString() }
