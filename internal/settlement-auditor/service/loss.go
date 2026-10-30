// 盘损读取 —— 数量信号的"分母证据"。
//
// # 它解决什么问题
//
// 数量信号原本只拿**窗口销量**做分母(见 checks.go checkQuantity):
// 进货 10、窗口只卖 0.91 ⇒ 看起来"买了很多卖了很少" ⇒ block。
// 但门店同期**报损 8.4**(破损/过期/被偷),货其实有去向 —— 拿盘损单解释掉,
// 偏离就回到可接受范围(2026-10-31 用户口径)。
//
// # 为什么单独一个 Reader(而不是直接查 cube)
//
// 门店的数据源是**两套**(规则见 internal/cubesource 包注释):
//
//	配了数据源 ⇒ 业务数据在外部 ERP 的 cube 实例里(cube-router 路由);
//	没配       ⇒ 业务数据在本域 PG(自营店)。
//
// 盘损也是业务数据 ⇒ 必须走同一条判据,否则有源门店永远读不到盘损、
// 无源门店读的是空表 —— 与 2026-10-21 那次"同一问题三个服务三个答案"同类。
//
// # 两条路的**证据口径**必须一致
//
//	有源:  cube stock_loss(J0 报损单 + OO 库存调整减少),只认已审核;
//	无源:  stocktake_lines 的盘亏行(diff_qty < 0),只认已审核的盘点单。
//
// 两边都是"已审核的、真实的、落在窗口内的数量减少" ⇒ 从数量信号的角度
// 是同一类证据。差异只在于"外部 ERP 记的是库存单据,本系统记的是盘点差异"。
package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubesource"
)

// LossReader 是盘损读取的依赖面(单测注入 fake)。
//
// 返回 商品货号 → 亏损数量(恒为正,**已按口径收敛**:JO 全部 / OO 仅减少)。
type LossReader interface {
	SumLossByProduct(ctx context.Context, branchID string, productIDs []string, fromDate, toDate string) (map[string]decimal.Decimal, error)
}

// LossService 是 LossReader 的默认实现:有源门店走 cube,无源门店查本域 PG。
type LossService struct {
	sources cubesource.Resolver // 可 nil ⇒ 按"没配源"处理(查本域 PG)
	cube    CubeQuerier         // 可 nil ⇒ 有源门店调用会报错(装配缺失,不静默回落)
	db      *gorm.DB
	logger  *slog.Logger
}

// NewLossService 构造 LossService。
//
// logger == nil ⇒ 用 slog.Default();db == nil ⇒ 无源门店读不了(报错)。
func NewLossService(sources cubesource.Resolver, cube CubeQuerier, db *gorm.DB) *LossService {
	return &LossService{sources: sources, cube: cube, db: db, logger: slog.Default()}
}

// SetLogger 注入日志(slog.Default() 之外的自定义 logger)。
func (s *LossService) SetLogger(l *slog.Logger) {
	if l != nil {
		s.logger = l
	}
}

// SumLossByProduct 拉商品 [fromDate, toDate) 的盘损数量。
//
// ⚠️ 有源门店**不回落**本域 PG:配置了数据源的门店,本域 PG 里根本没有它的
// 业务数据,回落读到的是空表 —— 空表与"真的没有盘损"无法区分,
// 会把"查不了"伪装成"没有证据"。
func (s *LossService) SumLossByProduct(ctx context.Context, branchID string, productIDs []string, fromDate, toDate string) (map[string]decimal.Decimal, error) {
	if len(productIDs) == 0 {
		return nil, nil
	}
	useCube, err := cubesource.Decide(ctx, s.sources, branchID)
	if err != nil {
		return nil, err
	}
	if useCube {
		if s.cube == nil {
			// 配了源却没装 cube 读端 ⇒ 装配错误,大声失败。
			return nil, fmt.Errorf("%w: 盘损读取缺少 cube 读端", ErrCubeUnavailable)
		}
		rows, err := s.cube.SumLossByProduct(ctx, branchID, productIDs, fromDate, toDate)
		if err != nil {
			return nil, fmt.Errorf("cube 盘损查询: %w", err)
		}
		return aggregateLoss(rows), nil
	}
	return s.localSumLoss(ctx, branchID, productIDs, fromDate, toDate)
}

// stocktakeLossRow 是本地盘亏汇总的一行。
type stocktakeLossRow struct {
	ProductID string          `gorm:"column:product_id"`
	Quantity  decimal.Decimal `gorm:"column:quantity"`
}

// localSumLoss 查本域 PG 的盘亏行(无源门店)。
//
// # 查询为什么这样写(走索引)
//
//   - 驱动侧是 **stocktake_headers**:`branch_id` / `status` 各有一列索引
//     (见 internal/stocktake/model 的 gorm tag),再加上 `count_date` 区间
//     ⇒ 先按门店 + 状态 + 账期把盘点单缩到很小(一个门店一个账期通常个位数);
//   - 再 join stocktake_lines:`header_id` 有索引 ⇒ 只回表这几张单的明细行;
//   - 明细侧再滤 `diff_qty < 0`(盘亏)与 `product_id IN (...)`;
//   - `deleted_at IS NULL` 是**显式**写出来的:这条 SQL 走的是 Table/Joins
//     (不是 GORM 模型查询),软删过滤不会自动加。
//
// ⚠️ **复盘点单(recheck)不计入**:它与原盘是"同一批货的第二次清点",
//   两边都算等于把同一份损耗算两遍 ⇒ 反而可能把该 block 的算成可接受。
//   宁可少算证据(保守),不可多算。
func (s *LossService) localSumLoss(ctx context.Context, branchID string, productIDs []string, fromDate, toDate string) (map[string]decimal.Decimal, error) {
	if s.db == nil {
		return nil, fmt.Errorf("盘损读取: 本地 PG 未装配")
	}
	var rows []stocktakeLossRow
	err := s.db.WithContext(ctx).
		Table("stocktake_lines AS l").
		Select("l.product_id AS product_id, SUM(-l.diff_qty) AS quantity").
		Joins("JOIN stocktake_headers AS h ON h.id = l.header_id").
		Where("h.branch_id = ?", branchID).
		Where("h.status = ?", stocktakeStatusApproved).
		Where("h.type <> ?", stocktakeTypeRecheck).
		Where("h.count_date >= ? AND h.count_date < ?", fromDate, toDate).
		Where("l.product_id IN ?", productIDs).
		Where("l.diff_qty < 0").
		Where("l.deleted_at IS NULL").
		Group("l.product_id").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("本地盘损查询: %w", err)
	}
	return aggregateLocalLoss(rows), nil
}

// stocktakeTypeRecheck 是复盘点单的类型字面量
// (与 internal/stocktake/model.TypeRecheck 同值;本包不跨引 stocktake 模型)。
const stocktakeTypeRecheck = "recheck"

// aggregateLocalLoss 把本地汇总行收敛成 商品 → 亏损数量。
//
// 与 cube 侧 aggregateLoss 同一套防御:空货号 / 非正数量一律丢掉
// (SUM 出来的负数或 NULL 不该被当成"证据")。
func aggregateLocalLoss(rows []stocktakeLossRow) map[string]decimal.Decimal {
	out := make(map[string]decimal.Decimal, len(rows))
	for _, r := range rows {
		pid := strings.TrimSpace(r.ProductID)
		if pid == "" || !r.Quantity.GreaterThan(decimal.Zero) {
			continue
		}
		out[pid] = out[pid].Add(r.Quantity)
	}
	return out
}
