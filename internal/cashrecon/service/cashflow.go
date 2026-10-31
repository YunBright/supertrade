// Package service / cashflow.go —— 收银流水取数（有源查 cube、无源读本地 PG）。
//
// # 为什么按「业务日」逐日查，而不是一次查整段
//
// cube 的 payflow 是 **live model**（实时打思迅源库，1.1M+ 行）。一次查询覆盖
// 31 天等于让 MSSQL 扫全部收款流水；而按日聚合后单日 ≤ 20 行（日 × pay_way）。
// ⇒ 区间上限 31 天（validateRange）+ 逐日查询 + 单日 30s 超时。
//
// # 为什么失败不回落本地
//
//	Configured=false              → 没配(正常自营店)⇒ 读本地
//	Configured=true, Enabled=false → 数据源被停用 ⇒ 503
//	解析失败 / cube 查询失败        → 503
//
// 回落 = 把一次链路故障显示成"这个门店没有流水"，对账会静默差一整天的钱。
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/YunBright/supertrade/internal/cashrecon/engine"
	"github.com/YunBright/supertrade/internal/cashrecon/model"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead"
	"github.com/YunBright/supertrade/internal/cubesource"
)

// 收银流水的 data_source 取值（响应里原样透出，前端据此提示口径）。
const (
	CashSourceCube  = "cube"
	CashSourceLocal = "local"
)

// CashDay 是「门店 × 业务日 × pay_way」的收银净额聚合。
//
// 与 engine.CashAggregate 同形，只是多带原始 pay_way 的映射结果（报表要展示）。
type CashDay struct {
	BizDate   time.Time
	PayWay    string
	Method    string
	AmountNet decimal.Decimal
	TxnCount  int
}

// defaultMethodMap 是 cube pay_way → 标准 method 的默认映射。
//
// 实测算到的 pay_way 取值：SWX(思迅Pay-微信) / CHG(找零) / LEP / SZF(储值) /
// GIF / SUP / SAV / RMB。映射规则可由 `recon_rules.payment_method_map` 覆盖。
//
// ⚠️ 未命中一律 `unmapped`（引擎会产出 `unmapped_pay_way` 差异），**不猜**：
// 猜错一次就是把"这个渠道对不上"静默归到另一个渠道上。
//
// ⚠️ 银行卡：cube 现有 pay_way 枚举里**没有**明确的银行卡编码（SZF 是储值卡），
// 因此 `bank_card` 暂不由默认映射产出 —— 待真实数据确认后写进规则（见
// docs/POS收银待开发功能文档.md）。
var defaultMethodMap = map[string]string{
	"RMB": model.MethodCash,
	"SWX": model.MethodWechat,
	"SZF": model.MethodStoredValue,
	"CHG": model.MethodChange,
	"LEP": model.MethodUnmapped,
	"GIF": model.MethodUnmapped,
	"SUP": model.MethodUnmapped,
	"SAV": model.MethodUnmapped,
}

// methodMap 合并默认映射与门店规则（规则可覆盖，但不会删掉默认项）。
func methodMap(r Rules) map[string]string {
	out := make(map[string]string, len(defaultMethodMap)+len(r.MethodMap))
	for k, v := range defaultMethodMap {
		out[strings.ToUpper(strings.TrimSpace(k))] = v
	}
	for k, v := range r.MethodMap {
		out[strings.ToUpper(strings.TrimSpace(k))] = v
	}
	return out
}

// methodOf 把 pay_way 映射成标准 method；未命中 → unmapped。
func methodOf(m map[string]string, payWay string) string {
	if v, ok := m[strings.ToUpper(strings.TrimSpace(payWay))]; ok {
		return v
	}
	return model.MethodUnmapped
}

// CashFlows 拉 [from, to]（含两端）的收银聚合，返回结果与 data_source。
//
// rules 由调用方一次加载（避免逐日重复读库）。
func (s *Service) CashFlows(ctx context.Context, branchID string, from, to time.Time, rules Rules) ([]engine.CashAggregate, string, error) {
	if err := s.validateRange(branchID, from, to); err != nil {
		return nil, "", err
	}
	useCube, err := s.readPath(ctx, branchID)
	if err != nil {
		return nil, "", err
	}
	if useCube {
		rows, err := s.cubeCashFlows(ctx, branchID, from, to, rules)
		if err != nil {
			return nil, "", err
		}
		return cashAggregatesOf(rows), CashSourceCube, nil
	}
	rows, err := s.localCashFlows(ctx, branchID, from, to, rules)
	if err != nil {
		return nil, "", err
	}
	return cashAggregatesOf(rows), CashSourceLocal, nil
}

// CashDayRows 返回带 pay_way/method 的明细聚合（支付方式分析页用）。
func (s *Service) CashDayRows(ctx context.Context, branchID string, from, to time.Time) ([]CashDay, string, error) {
	if err := s.validateRange(branchID, from, to); err != nil {
		return nil, "", err
	}
	rules, err := s.LoadRules(ctx, branchID)
	if err != nil {
		return nil, "", err
	}
	useCube, err := s.readPath(ctx, branchID)
	if err != nil {
		return nil, "", err
	}
	if useCube {
		rows, err := s.cubeCashFlows(ctx, branchID, from, to, rules)
		return rows, CashSourceCube, err
	}
	rows, err := s.localCashFlows(ctx, branchID, from, to, rules)
	return rows, CashSourceLocal, err
}

// cashAggregatesOf 把 CashDay 转成引擎输入。
func cashAggregatesOf(rows []CashDay) []engine.CashAggregate {
	out := make([]engine.CashAggregate, 0, len(rows))
	for _, r := range rows {
		out = append(out, engine.CashAggregate{
			BizDate:   r.BizDate,
			PayWay:    r.PayWay,
			Method:    r.Method,
			AmountNet: r.AmountNet,
			TxnCount:  r.TxnCount,
		})
	}
	return out
}

// readPath 判定读路径（三态，禁止回落）。
func (s *Service) readPath(ctx context.Context, branchID string) (bool, error) {
	useCube, err := cubesource.Decide(ctx, s.resolver, branchID)
	if err != nil {
		switch {
		case errors.Is(err, cubesource.ErrSourceDisabled):
			return false, fmt.Errorf("%w: %v", model.ErrCubeSourceDisabled, err)
		case errors.Is(err, cubesource.ErrSourceUnavailable):
			return false, fmt.Errorf("%w: %v", model.ErrCubeSourceUnavailable, err)
		default:
			// 权限类错误（cube:read 缺失）原样上抛，handler 按 503 之前的 403 语义处理。
			return false, err
		}
	}
	if useCube && s.reader == nil {
		// 装配缺失是**部署错误**，不是业务结论 —— 明确报 503，不静默读本地。
		return false, fmt.Errorf("%w: 缺少 cube 读端", model.ErrCubeSourceUnavailable)
	}
	return useCube, nil
}

// cubeCashFlows 逐日查 cube 的 payflow（区间 ≤ 31 天，单日一个查询）。
func (s *Service) cubeCashFlows(ctx context.Context, branchID string, from, to time.Time, rules Rules) ([]CashDay, error) {
	m := methodMap(rules)
	var out []CashDay
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		rows, err := s.cubeCashDay(ctx, branchID, d, m, rules)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// cubeCashDay 查单个业务日。
func (s *Service) cubeCashDay(ctx context.Context, branchID string, day time.Time, m map[string]string, rules Rules) ([]CashDay, error) {
	key := cashCacheKey(branchID, day)
	if rows, ok := s.cashCache.get(key); ok {
		return rows, nil
	}

	qctx, cancel := context.WithTimeout(ctx, s.cfg.CubeTimeout)
	defer cancel()
	qctx = cubeclient.WithBranchID(qctx, branchID)

	dayStr := day.Format("2006-01-02")
	data, _, err := s.reader.LoadCubeQueryPage(qctx, "payflow", cubeclient.CubeQuery{
		Measures:   []string{"payflow.count", "payflow.total_pay_amount_yuan"},
		Dimensions: []string{"payflow.pay_way", "payflow.branch_no"},
		Filters: []cubeclient.CubeFilter{
			cuberead.Filter("payflow.paid_at", "gte", dayStr+" 00:00:00"),
			cuberead.Filter("payflow.paid_at", "lte", dayStr+" 23:59:59"),
		},
		// 日 × pay_way × 分店 ≤ 200（防御性上限：正常远低于它）。
		Limit: 200,
		Total: true,
	})
	if err != nil {
		// ⚠️ 不返回 0 行：查询失败必须与"当天没有流水"区分开。
		return nil, fmt.Errorf("%w: cube payflow %s: %v", model.ErrCubeQueryFailed, dayStr, err)
	}

	// 按配置的源分店号过滤（未配置 ⇒ 全收）。
	filterBranches := make(map[string]bool, len(rules.CashSourceBranchNos))
	for _, b := range rules.CashSourceBranchNos {
		filterBranches[strings.TrimSpace(b)] = true
	}

	agg := map[string]*CashDay{}
	for _, r := range data {
		srcBranch := cuberead.Str(r, "payflow.branch_no")
		if len(filterBranches) > 0 && !filterBranches[srcBranch] {
			continue
		}
		payWay := strings.TrimSpace(cuberead.Str(r, "payflow.pay_way"))
		if payWay == "" {
			continue
		}
		count := cuberead.Dec(r, "payflow.count").IntPart()
		row, ok := agg[payWay]
		if !ok {
			row = &CashDay{BizDate: day, PayWay: payWay, Method: methodOf(m, payWay)}
			agg[payWay] = row
		}
		row.AmountNet = row.AmountNet.Add(cuberead.Dec(r, "payflow.total_pay_amount_yuan"))
		row.TxnCount += int(count)
	}
	rows := make([]CashDay, 0, len(agg))
	for _, v := range agg {
		rows = append(rows, *v)
	}
	sortCashDays(rows)
	s.cashCache.put(key, rows)
	return rows, nil
}

// localCashFlows 读 transaction 域的 pay_flows（只读，同一 PG 实例）。
//
// ⚠️ `CAST(date(paid_at) AS TEXT)` 是为了**同时**在 PostgreSQL 与 sqlite 单测里
// 成立：PG 的 date 列扫进 string 会报 scan 错误，文本化后两边都是 YYYY-MM-DD。
//
// ⚠️ 金额取 `pay_amount * exchange_rate`（人民币口径，与 transaction 的
// rmb_amount 同一算法）。负数原样进净额（SUM 天然处理冲销/退款）。
func (s *Service) localCashFlows(ctx context.Context, branchID string, from, to time.Time, rules Rules) ([]CashDay, error) {
	if s.db == nil {
		return nil, model.ErrInvalidInput
	}
	m := methodMap(rules)
	var raw []struct {
		BizDate   string          `gorm:"column:biz_date"`
		PayWay    string          `gorm:"column:pay_way"`
		AmountNet decimal.Decimal `gorm:"column:amount_net"`
		TxnCount  int             `gorm:"column:txn_count"`
	}
	err := s.db.WithContext(ctx).Raw(`
SELECT CAST(date(paid_at) AS TEXT) AS biz_date, pay_way,
       SUM(pay_amount * exchange_rate) AS amount_net, COUNT(*) AS txn_count
  FROM pay_flows
 WHERE branch_id = ? AND paid_at >= ? AND paid_at < ?
 GROUP BY CAST(date(paid_at) AS TEXT), pay_way
 ORDER BY biz_date, pay_way`,
		branchID, from, to.AddDate(0, 0, 1),
	).Scan(&raw).Error
	if err != nil {
		return nil, fmt.Errorf("cash-recon: 本地收银流水查询失败: %w", err)
	}
	out := make([]CashDay, 0, len(raw))
	for _, r := range raw {
		d, err := parseDay(r.BizDate)
		if err != nil {
			return nil, err
		}
		out = append(out, CashDay{
			BizDate:   d,
			PayWay:    strings.TrimSpace(r.PayWay),
			Method:    methodOf(m, r.PayWay),
			AmountNet: r.AmountNet,
			TxnCount:  r.TxnCount,
		})
	}
	return out, nil
}

// sortCashDays 按 (日期, pay_way) 稳定排序（确定性输出）。
func sortCashDays(rows []CashDay) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0; j-- {
			a, b := rows[j-1], rows[j]
			less := false
			switch {
			case a.BizDate.Before(b.BizDate):
			case a.BizDate.After(b.BizDate):
				less = true
			case a.PayWay < b.PayWay:
			default:
				less = true
			}
			if !less {
				break
			}
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}
}

// InvalidateCashCache 清掉某门店的收银流水缓存（落库/冻结前调用）。
func (s *Service) InvalidateCashCache(branchID string) { s.cashCache.invalidate(branchID) }

// ---- 进程内缓存 ----

// cashCacheKey 是缓存键：门店|业务日。
func cashCacheKey(branchID string, day time.Time) string {
	return branchID + "|" + day.Format("20060102")
}

// cashFlowCache 是「门店|业务日」→ 聚合行的 TTL 缓存。
//
// ⚠️ 只缓存 **cube 路径**：本地 PG 查询本身很便宜，而缓存会让
// "刚导入的流水"在一段时间内不可见。
type cashFlowCache struct {
	ttl time.Duration
	now func() time.Time

	mu   sync.RWMutex
	rows map[string]cashCacheEntry
}

type cashCacheEntry struct {
	rows []CashDay
	at   time.Time
}

func newCashFlowCache(ttl time.Duration, now func() time.Time) *cashFlowCache {
	return &cashFlowCache{ttl: ttl, now: now, rows: make(map[string]cashCacheEntry)}
}

func (c *cashFlowCache) get(key string) ([]CashDay, bool) {
	c.mu.RLock()
	e, ok := c.rows[key]
	c.mu.RUnlock()
	if !ok || c.now().Sub(e.at) >= c.ttl {
		return nil, false
	}
	out := make([]CashDay, len(e.rows))
	copy(out, e.rows)
	return out, true
}

func (c *cashFlowCache) put(key string, rows []CashDay) {
	cp := make([]CashDay, len(rows))
	copy(cp, rows)
	c.mu.Lock()
	c.rows[key] = cashCacheEntry{rows: cp, at: c.now()}
	c.mu.Unlock()
}

func (c *cashFlowCache) invalidate(branchID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.rows {
		if strings.HasPrefix(k, branchID+"|") {
			delete(c.rows, k)
		}
	}
}
