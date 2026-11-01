// internal/fresh-meat/service/stock_openings_cleanup.go —— **改期初日期 ⇒ 同事务清掉链外的旧账**。
//
// # 为什么要有这个(2026-10-30 owner 原话)
//
//	「按理若要修改期初日期,需要把起初日期当天和之前的所有采购录入(猪)、搬台、
//	  日终盘点都删除干净,但是之前没有这样的验证就修改入库了,
//	  所以趁此机会可以在修改期初日期时增加这个事务判断」
//
// # 现状:守卫只拦"新录的",不拦"已经录进去的"
//
//	系统**本来就拒收**链起点当天及之前的营业日动作
//	(assertAfterChainStart:"期初结转的生效日是链的起点(基线日),不是营业日")。
//	⇒ 但那条守卫只对**新录入**生效。
//	⇒ 门店在**改期初之前**录进去的记录留在库里 ⇒ 它们污染新链:
//	  - **搬台**   ⇒ 实时结余链(冷链)被做成负的
//	    (实测:店 00 在 10-02 搬了猪手 9.2kg、没有对应的猪
//	     ⇒ 冷链里猪手余额 = −9.2kg ⇒ 顺着"没录期初的部位"漏进基线日的期初);
//	  - **日终盘点** ⇒ 它的柜台实盘会顺着冷链带进链里。
//
//	⇒ 改期初 = **换链的起点** ⇒ 起点当天及之前的旧账必须删干净
//	⇒ 而且必须与"写期初"在**同一个事务**里
//	  (否则又出现"新期初 + 旧记录"并存 —— 那正是这次事故的形态)。
//
// # 边界:链起点**之前**(严格小于)
//
//	⚠️ owner 原话是"起初日期**当天和之前**"—— 这里取**之前**是有一个硬约束的:
//	  链起点当天的记录是**链的第一天的业务**,不是链外旧账:
//	  期初 = 那天**早上**的冷库,日终盘点 = 那天**傍晚**的实盘
//	  ⇒ 那天的冷库 = 期初 + 盘到的量(现有口径锁在
//	    counter_load_rows_test.go::TestOpening_SeedsTheDailyBalanceChain:
//	    期初 100 + 盘点 10 = 110)⇒ 删掉链起点当天的记录会**把链算错**。
//	  ⇒ 只清**之前**的:那些才是链外的旧账。
//	  (生产实测:店 00 要清的是 10-02 的搬台 —— 正好在链起点 10-28 之前 ✔)
//
// # 边界:按**营业日**归属,不是按 UTC 日期
//
//	店 00 的 2 头猪 arrived_at = 2026-10-28 16:00Z = **10-29 00:00 +08**
//	⇒ 营业日是 10-29(链内)⇒ 不能删。
//	⇒ 一律用"营业日窗口"与 bizTZ 归属来判,别拿 UTC 日期硬比。
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"gorm.io/gorm"
)

// dropChainExternalRecords 清掉某店**链起点之前**的链外旧账(猪 / 搬台 / 日终盘点)。
//
// # 参数
//
//	frozenKey —— 该店**已审计冻结**的截止日(空 = 没冻结过)。
//	⚠️ 必须在**事务外**读好再传进来:SQLite 单测库只开 1 条连接
//	  ⇒ 事务内再走 s.db 读 ⇒ 拿不到连接 ⇒ **死锁**。
//
// # 口径
//
//	边界 = 链起点**之前**(严格小于;见文件头说明,链起点当天的记录属于链的第一天):
//	  - 猪       : 到店时间落在**链起点之前**的营业日窗口里;
//	  - 搬台     : `biz_date` < 链起点;
//	  - 日终盘点 : 归属营业日 < 链起点。
//
// # 为什么猪"删不干净"时只跳过、不报错
//
//	一头猪如果**已经分割补录**或**已经产生销售归属** ⇒ 删它会留下孤儿行
//	(分割行对不上猪、销售收入归到不存在的猪)⇒ 那是"删猪"接口本身的硬规则。
//	⇒ 这里不硬删:跳过 + 记日志。
//	⇒ 而且**不影响链**:链是**按部位**算的,链外的猪既不产出链上的分割,
//	  也不动位置表(猪不进位置表)⇒ 留着它只是"账上多一头链外的猪"。
//
// # 位置表的一致性
//
//	搬台删除时必须**反向调整位置表**(冷库 +qty、柜台 −qty)
//	—— 与 DeleteCounterLoad 同一套动作(货"搬回冷库")。
//	⇒ 不这么做的话,库存查询会看到"链里没了、柜台还挂着"的货。
func (s *Service) dropChainExternalRecords(ctx context.Context, tx *gorm.DB, branchID, chainStartKey, frozenKey string) error {
	if tx == nil {
		tx = s.db
	}
	loc := s.bizTZOrUTC()
	start, end := s.businessDayBounds(mustParseDateInLoc(chainStartKey, loc))

	// ⚠️ **历史审计期不许碰**:即便它落在链起点之前,也不能删。
	//   (链起点 = 最早的期初生效日 ⇒ 可能早于最后一次审计的截止日 ⇒ 那种情况下只清"没审计过"的部分。)
	//   ⚠️ 2026-11-01 owner：审计端点已下线 ⇒ 这里读的是**历史** `biz_audits` 行
	//     (只读锚点，见 audit_history.go)，不再是"可写入的冻结期"。
	frozen := frozenKey

	// ---- ① 搬台(biz_date <= 链起点)----
	//
	// ⚠️ 日期用**区间**而不是等值:datatypes.Date 落库是"带时区的零点",
	//   PG 里 date = timestamp 会按会话时区折算 ⇒ 等值不可靠。
	// ⚠️ 下界**必须放开**(链外可能是很久以前的搬台)⇒ 上界留一天余量,
	//   再在 Go 里按**业务时区**的日期过滤(≤ 链起点)。
	var loads []model.CounterLoadRecord
	if err := tx.WithContext(ctx).
		Where("branch_id = ? AND biz_date < ?", branchID, end.AddDate(0, 0, 1)).
		Find(&loads).Error; err != nil {
		return fmt.Errorf("query counter_loads(chain-external): %w", err)
	}
	for _, l := range loads {
		key := time.Time(l.BizDate).In(loc).Format("2006-01-02")
		if key >= chainStartKey {
			continue // 链起点当天 = 链的第一天 ⇒ 它的搬台是链内的账
		}
		if frozen != "" && key <= frozen {
			s.pubLogger.Warn("改期初:链外有落在**审计冻结期**里的搬台,冻结不可逆 ⇒ 跳过",
				"branch_id", branchID, "chain_start", chainStartKey,
				"id", l.ID, "biz_date", key, "audit_cutoff", frozen)
			continue
		}
		// 撤掉 ⇒ 货搬回冷库(冷库 +、柜台 −)—— 与 DeleteCounterLoad 同一套动作。
		if err := s.addPosition(ctx, tx, branchID, l.CubeProductID, model.StockLocationColdStore, l.QtyKg); err != nil {
			return err
		}
		if err := s.addPosition(ctx, tx, branchID, l.CubeProductID, model.StockLocationCounter, l.QtyKg.Neg()); err != nil {
			return err
		}
		if err := tx.WithContext(ctx).
			Where("branch_id = ? AND id = ?", branchID, l.ID).
			Delete(&model.CounterLoadRecord{}).Error; err != nil {
			return fmt.Errorf("delete counter_load %s: %w", l.ID, err)
		}
		s.pubLogger.Info("改期初:清掉链外的搬台流水",
			"branch_id", branchID, "chain_start", chainStartKey,
			"id", l.ID, "biz_date", key, "qty_kg", l.QtyKg.String())
	}

	// ---- ② 猪(营业日 < 链起点)----
	var pigs []model.WholePig
	if err := tx.WithContext(ctx).
		Where("branch_id = ? AND arrived_at < ?", branchID, start).
		Find(&pigs).Error; err != nil {
		return fmt.Errorf("query whole_pigs(chain-external): %w", err)
	}
	for _, p := range pigs {
		pigDay := p.ArrivedAt.In(loc).Format("2006-01-02")
		if frozen != "" && pigDay <= frozen {
			s.pubLogger.Warn("改期初:链外有落在**审计冻结期**里的进猪记录,冻结不可逆 ⇒ 跳过",
				"branch_id", branchID, "chain_start", chainStartKey,
				"pig_id", p.ID, "biz_date", pigDay, "audit_cutoff", frozen)
			continue
		}
		// 删不干净的(已分割/已销售归属)⇒ 跳过 + 记日志(见文件头说明)。
		var cuts, sales int64
		if err := tx.WithContext(ctx).Model(&model.PigCut{}).
			Where("pig_id = ?", p.ID).Count(&cuts).Error; err != nil {
			return fmt.Errorf("count pig_cuts(chain-external): %w", err)
		}
		if err := tx.WithContext(ctx).Model(&model.LineSalesByPig{}).
			Where("pig_id = ?", p.ID).Count(&sales).Error; err != nil {
			return fmt.Errorf("count line_sales_by_pigs(chain-external): %w", err)
		}
		if cuts > 0 || sales > 0 {
			s.pubLogger.Warn("改期初:链外有删不干净的猪(已分割/已销售归属),跳过",
				"branch_id", branchID, "chain_start", chainStartKey,
				"pig_id", p.ID, "cuts", cuts, "sales", sales)
			continue
		}
		if err := tx.WithContext(ctx).
			Delete(&model.WholePig{}, "branch_id = ? AND id = ?", branchID, p.ID).Error; err != nil {
			return fmt.Errorf("delete whole_pig %s: %w", p.ID, err)
		}
		s.pubLogger.Info("改期初:清掉链外的进猪记录",
			"branch_id", branchID, "chain_start", chainStartKey, "pig_id", p.ID)
	}

	// ---- ③ 日终盘点(归属营业日 <= 链起点)----
	var takes []model.PorkCutsStocktake
	if err := tx.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Find(&takes).Error; err != nil {
		return fmt.Errorf("query stocktakes(chain-external): %w", err)
	}
	for _, st := range takes {
		key := st.TakenAt.In(loc).Format("2006-01-02")
		if key >= chainStartKey {
			continue // 链起点当天 = 链的第一天 ⇒ 它的盘点是链内的账
		}
		if frozen != "" && key <= frozen {
			s.pubLogger.Warn("改期初:链外有落在**审计冻结期**里的日终盘点,冻结不可逆 ⇒ 跳过",
				"branch_id", branchID, "chain_start", chainStartKey,
				"stocktake_id", st.ID, "biz_date", key, "audit_cutoff", frozen)
			continue
		}
		if err := tx.WithContext(ctx).
			Delete(&model.PorkCutsStocktake{}, "branch_id = ? AND id = ?", branchID, st.ID).Error; err != nil {
			return fmt.Errorf("delete pork_cuts_stocktake %s: %w", st.ID, err)
		}
		s.pubLogger.Info("改期初:清掉链外的日终盘点单",
			"branch_id", branchID, "chain_start", chainStartKey,
			"stocktake_id", st.ID, "biz_date", key)
	}
	return nil
}

// chainStartAfterSave 算**这次保存之后**的链起点(期初结转的最早生效日)。
//
// # 为什么要在写之前先算
//
//	清理链外旧账要用它当边界 ⇒ 而边界由"这次写入的期初 + 库里已有的期初"决定
//	⇒ 必须先算出来(写在事务里就没法用"保存后的状态"当边界了)。
//
// # 口径
//
//	每个部位**只算一条**(期初是一部位一条,upsert 覆盖)⇒
//	同一部位既有旧记录又有新记录时,**以这次写入的为准**。
func (s *Service) chainStartAfterSave(ctx context.Context, tx *gorm.DB, branchID string,
	items []StockOpeningInput, loc *time.Location,
) (string, error) {
	if tx == nil {
		tx = s.db
	}
	// 库里已有的期初:被这次写入覆盖的部位用新日期,其余保留旧日期。
	openings, err := s.loadOpeningsFrom(ctx, tx, branchID)
	if err != nil {
		return "", err
	}
	dates := map[string]string{} // cutType → 生效日
	for cut, op := range openings {
		dates[cut] = op.dateKey
	}
	for _, it := range items {
		key := ""
		if k := it.BizDate; k != "" {
			parsed, perr := time.ParseInLocation("2006-01-02", k, loc)
			if perr != nil {
				continue // 校验阶段已经报过错了,这里不重复报
			}
			key = parsed.Format("2006-01-02")
		} else {
			key = s.now().In(loc).Format("2006-01-02")
		}
		dates[string(it.CutType)] = key
	}
	best := ""
	for _, key := range dates {
		if best == "" || key < best {
			best = key
		}
	}
	return best, nil
}
