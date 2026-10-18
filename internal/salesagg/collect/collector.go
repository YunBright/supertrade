package collect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/salesdaily"
)

// DateLayout 是营业日的字符串格式。
//
// ⚠️ 与思迅源库 `oper_date` 的 char(10)、以及事实包的 From/To 完全一致。
//
//	全程用字符串意味着没有时区换算、没有 date↔time 往返 ——
//	那类转换正是"日报少一天/多一天"最常见的来源。
const DateLayout = "2006-01-02"

// Store 是采集器的落库依赖（由 *salesdaily.Store 实现）。
//
// 收窄成一个方法而不是整个 *gorm.DB：
// 采集器只需要"把一份快照写进去"，让它拿到 db 就能自己开事务
// 的话，"四张表是否在同一事务里"就会退化成调用点的自觉。
type Store interface {
	Save(ctx context.Context, snap salesdaily.Snapshot, source salesdaily.CollectSource) error
}

// Collector 采集并落库一个营业日的完整销售事实。
type Collector struct {
	src   Source
	store Store
}

// NewCollector 构造采集器。store 为 nil 时 Run 只会采集不落库（便于排查取数）。
func NewCollector(src Source, store Store) *Collector {
	return &Collector{src: src, store: store}
}

// Result 是一次采集的产出摘要（供日志与调用方判断，不含数据本身）。
type Result struct {
	BranchID   string
	OperDate   string
	Source     salesdaily.CollectSource
	Saved      bool
	Categories int
	Suppliers  int
	Products   int

	// snap 是完整快照，**不导出**。
	//
	// ⚠️ 刻意不让它进 JSON：job 回调的响应里若带几千行商品明细，
	//   dapr invocation 与日志都会跟着膨胀，而调用方要的只是"采到没有"。
	//   需要明细时用 Collect()。
	snap salesdaily.Snapshot
}

// Run 采集一个营业日并落库。
//
// # ⚠️ 失败策略：**任何一维失败就整次不落库**
//
// 这是本包最重要的设计决策，也是它与 AI 报告那条链路**刻意相反**的地方。
//
// 报告链路（internal/salesinsight/build）失败时是"降级但照出"：
// 天气查不到就少一段天气、供应商字典查不到就标注"可能包含自用渠道"。
// 理由是报告是**一次性**的：有告警的报告仍然比没有报告有用。
//
// 落库不一样：写进去的这一行会被**长期**当作"当天的事实"，
// 而月报、正文、日报全都从它聚合。于是：
//
//	总额写成功、品类查询失败 → 月报按品类聚合时那一天整段消失，
//	                            数字偏小、**无报错、无降级标记**。
//	供应商缺名字 → 榜单里出现"供应商29"，运营以为配置坏了。
//
// 这类"半残事实"没有任何机制能事后发现，所以选择**不写**：
// 不写 → 消费侧读到 ErrNotFound → 回退查 cube → 数字仍然正确。
//
// 代价是一次失败的采集要等下一次（次日 01:00 的校正，或人工重跑）。
// 采集每天只跑两次，这个代价远小于"悄悄写进去一份残缺事实"。
func (c *Collector) Run(
	ctx context.Context, storeBranchID, operDate string, source salesdaily.CollectSource,
) (Result, error) {
	res, err := c.Collect(ctx, storeBranchID, operDate)
	if err != nil {
		return Result{BranchID: storeBranchID, OperDate: operDate, Source: source}, err
	}
	res.Source = source
	if c.store == nil {
		return res, nil
	}
	if err := c.store.Save(ctx, res.snapshot(), source); err != nil {
		return res, fmt.Errorf("collect: 落库 %s %s: %w", storeBranchID, operDate, err)
	}
	res.Saved = true
	return res, nil
}

// Collect 只采集，不落库。
func (c *Collector) Collect(ctx context.Context, storeBranchID, operDate string) (Result, error) {
	if c == nil || c.src == nil {
		return Result{}, errors.New("collect: 数据源未配置")
	}
	branch := strings.TrimSpace(storeBranchID)
	if branch == "" {
		return Result{}, errors.New("collect: 缺少门店（store = X-Branch-ID）")
	}
	date, err := NormalizeOperDate(operDate)
	if err != nil {
		return Result{}, err
	}

	// from == to：一次只采**一个**营业日。
	//
	// ⚠️ 刻意不接受区间：采集的单位是"日"，而月报聚合的是"若干个日行"。
	//   若这里能传区间，就会出现"把 30 天存成一行"的用法，
	//   而那样月报聚合会把 30 天算成 1 天 —— 与不加限量的目标正好相反。
	totals, err := c.src.TotalsSales(ctx, branch, date, date)
	if err != nil {
		return Result{}, fmt.Errorf("collect: 取 %s 总额: %w", date, err)
	}

	cats, err := c.src.CategorySales(ctx, branch, date, date)
	if err != nil {
		return Result{}, fmt.Errorf("collect: 取 %s 品类: %w", date, err)
	}

	prods, err := c.src.ProductSales(ctx, branch, date, date)
	if err != nil {
		return Result{}, fmt.Errorf("collect: 取 %s 商品: %w", date, err)
	}

	sups, err := c.src.SupplierSales(ctx, branch, date, date)
	if err != nil {
		return Result{}, fmt.Errorf("collect: 取 %s 供应商: %w", date, err)
	}
	if err := c.fillSupplierNames(ctx, branch, sups); err != nil {
		// ⚠️ 字典失败也整次不落库 —— 见 Run 的"失败策略"。
		return Result{}, fmt.Errorf("collect: 补 %s 供应商名称: %w", date, err)
	}

	return Result{
		BranchID:   branch,
		OperDate:   date,
		Categories: len(cats),
		Suppliers:  len(sups),
		Products:   len(prods),
		snap:       snapshotOf(branch, date, totals, cats, sups, prods),
	}, nil
}

// fillSupplierNames 用字典补全供应商名称。
//
// ⚠️ 查不到字典项的供应商**保留原行**、名字留空，不丢弃：
//
//	丢弃会让这家供应商的销量凭空从大盘里消失
//	（与 internal/salesinsight/build/suppliers.go 同一取舍）。
//	而"字典整体查不到"（error）与"这家没档案"（空名字）是两件事，
//	所以只有前者会让整次采集失败。
func (c *Collector) fillSupplierNames(ctx context.Context, branch string, rows []salesdaily.SupplierRow) error {
	if len(rows) == 0 {
		return nil
	}
	codes := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Code != "" {
			codes = append(codes, r.Code)
		}
	}
	names, err := c.src.SupplierNames(ctx, branch, codes)
	if err != nil {
		return err
	}
	for i := range rows {
		if n := strings.TrimSpace(names[rows[i].Code]); n != "" {
			rows[i].Name = n
		}
	}
	return nil
}

// snapshotOf 组装快照。
func snapshotOf(
	branch, date string,
	totals salesdaily.TotalsRow,
	cats []salesdaily.CategoryRow,
	sups []salesdaily.SupplierRow,
	prods []salesdaily.ProductRow,
) salesdaily.Snapshot {
	return salesdaily.Snapshot{
		BranchID:   branch,
		OperDate:   date,
		Totals:     totals,
		Categories: cats,
		Suppliers:  sups,
		Products:   prods,
	}
}

// snapshot 从 Result 里取出快照。
//
// ⚠️ Result 是**摘要**（计数 + 状态），快照本体不进它的导出字段 ——
//
//	那样会让 job 回调的 JSON 响应里塞进几千行商品明细。
//	只有本包内部经这个方法拿得到完整数据。
func (r Result) snapshot() salesdaily.Snapshot {
	return r.snap
}

// NormalizeOperDate 校验并修剪营业日。
//
// ⚠️ 空值**不**默认成今天：
//
//	调用方（dapr job 回调）忘了传日期时，默认成今天会静默地
//	把前一天的快照重采一遍覆盖掉 —— 而首次采集与校正采集的差别
//	恰恰是它们要校正的那一天。
//	"必须显式给出是哪一天"是这里唯一安全的选择。
func NormalizeOperDate(v string) (string, error) {
	d := strings.TrimSpace(v)
	if d == "" {
		return "", errors.New("collect: 必须显式给出营业日（oper_date），不默认取今天")
	}
	if _, err := time.Parse(DateLayout, d); err != nil {
		return "", fmt.Errorf("collect: 营业日格式应为 %s，收到 %q", DateLayout, d)
	}
	return d, nil
}

// PrevOperDate 返回 now 的**前一天**。
//
// # 用途：次日 01:00 的校正采集要修的是**昨天**
//
// ⚠️ 不是今天：01:00 时思迅日结补的是昨天的账。
//
//	若按"今天"校正，会把一个刚开门、几乎没销售的日子写进库，
//	而昨天那份残缺的首采**永远留在那里** ——
//	症状是月报某天明显偏低，且没有任何标记。
func PrevOperDate(now time.Time) string {
	y, m, d := now.AddDate(0, 0, -1).Date()
	return fmt.Sprintf("%04d-%02d-%02d", y, m, d)
}
