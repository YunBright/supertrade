// Package service 是 cash-recon 的业务装配层。
//
// # 与 engine 的边界
//
//	engine  → 纯函数匹配(无 DB / 无 time.Now / 无 rand)
//	service → 取数、校验、落库、审计日志
//
// 把"取数"放在 service 而不是 engine,是这个域最关键的一条:
// 收银流水**不落本地库**(有 cube 源查 cube / 无源读 transaction 的 pay_flows),
// 而匹配必须能在 dry-run 下跑 —— 预览与落库共用 engine,只是 service 不写库。
package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cashrecon/engine"
	"github.com/YunBright/supertrade/internal/cashrecon/model"
	"github.com/YunBright/supertrade/internal/cuberead"
	"github.com/YunBright/supertrade/internal/cubesource"
)

// DefaultMaxRangeDays 是单次查询/匹配允许的最大区间。
//
// 按**逐日**聚合设计(单日 ≤ 20 行),31 天是一次月度对账的自然上限;
// 再大只会把 cube 查询放大成慢查询,而收益是"一次看三个月"这种没人用的视图。
const DefaultMaxRangeDays = 31

// Config 是 cash-recon 的运行参数(env 形状见 deploy/cash-recon.env.example)。
type Config struct {
	// MaxRangeDays 是查询/匹配区间上限(默认 31)。
	MaxRangeDays int
	// CubeTimeout 是**单日** cube 查询的超时(默认 30s)。
	CubeTimeout time.Duration
	// CacheTTL 是收银流水(按 门店|业务日)的进程内缓存时长(默认 5m)。
	CacheTTL time.Duration
	// DefaultToleranceDays 是 T+N 到账窗口默认值(默认 1;引擎再截到 7)。
	DefaultToleranceDays int
	// DefaultCashTolerance decimal 是长短款容差默认值(元)。
	DefaultCashTolerance decimal.Decimal
	// Logger 可注入(测试用);nil ⇒ slog.Default()。
	Logger *slog.Logger
	// Clock 可注入(测试用);nil ⇒ time.Now。
	Clock func() time.Time
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		MaxRangeDays:         DefaultMaxRangeDays,
		CubeTimeout:          30 * time.Second,
		CacheTTL:             5 * time.Minute,
		DefaultToleranceDays: 1,
		DefaultCashTolerance: decimal.NewFromInt(5),
	}
}

// Service 是 cash-recon 的业务单例。
type Service struct {
	db  *gorm.DB
	cfg Config

	// resolver 解析"这个门店配了哪个数据源";reader 读 cube。
	// 两者都未装配 ⇒ 一律按"没配数据源"读本地 PG(单测/纯本地部署)。
	resolver cubesource.Resolver
	reader   cuberead.Reader

	// cashCache 是 cube 收银流水的进程内缓存(见 cashflow.go)。
	cashCache *cashFlowCache
}

// New 构造 service(默认配置)。
func New(db *gorm.DB) *Service {
	return NewWithConfig(db, DefaultConfig())
}

// NewWithConfig 构造 service(可注入配置)。
func NewWithConfig(db *gorm.DB, cfg Config) *Service {
	if cfg.MaxRangeDays <= 0 {
		cfg.MaxRangeDays = DefaultMaxRangeDays
	}
	if cfg.CubeTimeout <= 0 {
		cfg.CubeTimeout = 30 * time.Second
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	if cfg.DefaultToleranceDays <= 0 {
		cfg.DefaultToleranceDays = 1
	}
	if cfg.DefaultCashTolerance.IsZero() {
		cfg.DefaultCashTolerance = decimal.NewFromInt(5)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &Service{db: db, cfg: cfg, cashCache: newCashFlowCache(cfg.CacheTTL, cfg.Clock)}
}

// Config 返回当前配置(只读)。
func (s *Service) Config() Config { return s.cfg }

// SetCashSource 注入数据源解析器与 cube 读端。
//
// ⚠️ 两者必须**同一个** cubeclient 实例:门店隔离靠 X-Branch-ID 路由链,
// 解析器问门店配了哪个源、读端按同一个门店路由 —— 两条链路各建一个
// 客户端,会在排查时多出一份"看起来都合理"的答案。
func (s *Service) SetCashSource(resolver cubesource.Resolver, reader cuberead.Reader) {
	s.resolver = resolver
	s.reader = reader
}

// now 取当前时刻(注入时钟,引擎不取 time.Now)。
func (s *Service) now() time.Time { return s.cfg.Clock() }

// PreviewInput 是 dry-run 的入参。
type PreviewInput struct {
	BranchID    string
	BizDateFrom time.Time
	BizDateTo   time.Time
}

// Preview 组装引擎输入并返回匹配结果,**零写库**。
//
// 收银流水实时查(cube / 本地 PG),银行流水读本域已导入的行。
// 与 apply 共用 engine.Run ⇒ 预览看到的匹配与落库结果逐字段相同。
func (s *Service) Preview(ctx context.Context, in PreviewInput) (engine.Output, error) {
	return s.runMatch(ctx, in.BranchID, in.BizDateFrom, in.BizDateTo)
}

// ListBatches 返回门店的批次列表(按创建时间倒序)。
func (s *Service) ListBatches(ctx context.Context, branchID string, limit int) ([]model.ReconBatch, error) {
	if branchID == "" {
		return nil, model.ErrInvalidInput
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var rows []model.ReconBatch
	err := s.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("created_at DESC").
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// GetBatch 按 id 取单个批次(必须属于该门店)。
func (s *Service) GetBatch(ctx context.Context, branchID, id string) (model.ReconBatch, error) {
	if branchID == "" || id == "" {
		return model.ReconBatch{}, model.ErrInvalidInput
	}
	var row model.ReconBatch
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND id = ?", branchID, id).
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return model.ReconBatch{}, model.ErrNotFound
	}
	if err != nil {
		return model.ReconBatch{}, err
	}
	return row, nil
}

// validateRange 校验门店 + 日期区间。
//
// ⚠️ 区间上限写死在这里、不是只在前端拦:cube 查询的成本由**后端**承担,
// 前端拦不住的调用方(脚本/契约测试)照样能把区间拉到仨月。
func (s *Service) validateRange(branchID string, from, to time.Time) error {
	if branchID == "" {
		return model.ErrInvalidInput
	}
	if from.IsZero() || to.IsZero() {
		return model.ErrInvalidInput
	}
	from = dayOnly(from)
	to = dayOnly(to)
	if to.Before(from) {
		return model.ErrInvalidInput
	}
	if int(to.Sub(from).Hours()/24)+1 > s.cfg.MaxRangeDays {
		return model.ErrRangeTooLarge
	}
	return nil
}

// dayOnly 去掉时刻,统一成 UTC 日期口径。
//
// 业务日与到账日都是**日历日**:带着时刻比较会让 23:59:59 的行漏出窗口。
func dayOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// parseDay 解析 YYYY-MM-DD。
func parseDay(v string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: 日期必须是 YYYY-MM-DD, got %q", model.ErrInvalidInput, v)
	}
	return dayOnly(t), nil
}

// bankFlows 读本域已导入的银行/渠道流水(dry-run 与 apply 同一条口径)。
func (s *Service) bankFlows(ctx context.Context, branchID string, from, to time.Time) ([]engine.BankFlow, error) {
	var rows []model.ReconBankFlow
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND value_date >= ? AND value_date <= ?", branchID, from, to).
		Order("value_date, channel, id").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]engine.BankFlow, 0, len(rows))
	for _, r := range rows {
		out = append(out, engine.BankFlow{
			ID:            r.ID,
			ExternalID:    r.ExternalID,
			Channel:       r.Channel,
			ValueDate:     r.ValueDate,
			Amount:        r.Amount,
			Fee:           r.Fee,
			SettleBatchNo: r.SettleBatchNo,
		})
	}
	return out, nil
}

// assertNotLocked 判断 [from, to] 是否落在已冻结区间内（是 ⇒ ErrPeriodFrozen）。
//
// 冻结（S2 的 freeze）写 recon_period_locks；本域所有"能改变流水金额"的写操作
// （导入、其他收款、实盘、付款登记）都要先过这一关 —— 否则审核计算之后再补一笔
// 流水，快照与账面就会永久对不上。
func (s *Service) assertNotLocked(ctx context.Context, branchID string, from, to time.Time) error {
	if branchID == "" || from.IsZero() || to.IsZero() {
		return model.ErrInvalidInput
	}
	var n int64
	err := s.db.WithContext(ctx).Model(&model.ReconPeriodLock{}).
		Where("branch_id = ? AND biz_date_from <= ? AND biz_date_to >= ?", branchID, dayOnly(to), dayOnly(from)).
		Count(&n).Error
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: %s ~ %s", model.ErrPeriodFrozen, from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
	return nil
}
