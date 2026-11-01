// Package model / stocking.go —— 备货系数表（2026-10-31 用户口径）。
//
// # 它解决什么
//
// 采购量是否合理，不能只看「进货量 / 窗口销量」这一个粗口径 —— 不同 SKU 的
// **补货节奏**（几天进一次货）与**需求波动**（日销量稳不稳）差别很大。
// 备货系数把这两件事收敛成两个可维护的数：
//
//	Multiplier 备货倍数：合理备货量 = 周期销售数 × 倍数（应对需求波动/到货延迟）
//	PeriodDays 备货周期：两次进货的间隔（按该 SKU 的历史进货频率算）
//
// 审批流水线在给出 LLL 研判前，会把「备货系数 + 当前库存 + 周期销售数 +
// 合理备货量」一并放进 AuditInput ⇒ 机器审核能判断"这次进货是不是进多了"。
//
// # 为什么落在本域 PG 而不是 cube
//
// ① 它是**审核用的经营参数**，不是门店业务单据；
// ② 读取方就是 settlement-auditor 自己（同进程同库 ⇒ 零跨服务调用）；
// ③ 统计口径（周期/倍数）是**本系统算的**，源库里没有这个字段。
//
// # 算法（见 service/stocking.go）
//
//	PeriodDays = 历史进货间隔的中位数（不足 2 次进货 ⇒ 默认 7 天），夹在 [3,60]
//	Multiplier = 1 + 0.5×日销量变异系数 + 周期长度补偿，夹在 [1.0,3.0]
//
// 系数是**慢变量**：随采购频次与售出速度缓慢变化，不需要每单重算。
// 缺失时才由审批流水线（或管理后台「重新生成」）触发计算。
package model

import (
	"time"

	"github.com/shopspring/decimal"
)

// 备货系数的来源。
const (
	// StockingSourceAuto 是系统按历史数据算出（审批缺失时触发 / 后台重新生成）。
	StockingSourceAuto = "auto"
	// StockingSourceManual 是人工在管理后台改过（重新生成会覆盖，除非手动锁定？）。
	//
	// ⚠️ 后台「重新生成」**会覆盖** auto 行，但**跳过** manual 行 ——
	//    人改过的参数是业务判断，不能被一次批量重算抹掉（要覆盖就得先删或改成 auto）。
	StockingSourceManual = "manual"
)

// StockingCoefficient 是一个 SKU 在某个门店的备货系数。
//
// ⚠️ 唯一键 (branch_id, product_id)：同一个 SKU 在不同门店的补货节奏不同。
type StockingCoefficient struct {
	ID       string `gorm:"primaryKey;size:36" json:"id"`
	BranchID string `gorm:"size:32;uniqueIndex:ux_stocking_coef,priority:1" json:"branch_id"`
	// ProductID 是**思迅商品货号**（cube 口径 item_no），与本服务其它信号一致。
	ProductID string `gorm:"size:64;uniqueIndex:ux_stocking_coef,priority:2" json:"product_id"`
	// ProductName 是冗余快照（纯展示；商品改名后不会自动跟）。
	ProductName string `gorm:"size:128" json:"product_name"`
	// Multiplier 是备货倍数（合理备货量 = 周期销售数 × Multiplier）。
	Multiplier decimal.Decimal `gorm:"type:numeric(6,2)" json:"multiplier"`
	// PeriodDays 是备货周期（天）——两次进货的间隔。
	PeriodDays int `json:"period_days"`
	// ---- 下面是生成那一刻的统计快照（供管理后台展示与排查）----
	// SampleDays 是算日均销量时用的采样天数。
	SampleDays int `json:"sample_days"`
	// AvgDailySales 是采样期内的日平均销量。
	AvgDailySales decimal.Decimal `gorm:"type:numeric(12,3)" json:"avg_daily_sales"`
	// PeriodSalesQty 是周期销售数 = AvgDailySales × PeriodDays。
	PeriodSalesQty decimal.Decimal `gorm:"type:numeric(14,3)" json:"period_sales_qty"`
	// ReceiptCount 是采样期内命中的进货次数（决定 PeriodDays 的可信度）。
	ReceiptCount int `json:"receipt_count"`
	// IntervalDays 是历史进货间隔中位数（PeriodDays 的原始值，便于对照）。
	IntervalDays int `json:"interval_days"`
	// DailySalesCV 是日销量变异系数（0 = 极稳定，越大越波动）。
	DailySalesCV decimal.Decimal `gorm:"type:numeric(6,3)" json:"daily_sales_cv"`
	// Source 是 StockingSourceAuto / StockingSourceManual。
	Source string `gorm:"size:16;index" json:"source"`
	// Note / OperatorID / ComputedAt 是维护留痕。
	Note       string    `gorm:"size:256" json:"note"`
	OperatorID string    `gorm:"size:64" json:"operator_id"`
	ComputedAt time.Time `json:"computed_at"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (StockingCoefficient) TableName() string { return "stocking_coefficients" }
