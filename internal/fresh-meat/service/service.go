// Package service 实现 fresh-meat 服务的业务逻辑(fresh-meat)。
//
// 关键设计(REQUIREMENTS §4 / DESIGN §6):
//   - 早盘整猪录入(一头一行)+ 同步调 LLM 推演预期分割
//   - 白天单品补录(pig_cuts)+ POS 销售事件本地聚合(line_sales_by_pigs)
//   - 日终按部位盘点(整店,可选,不阻断销售)
//   - 报损走 fresh_meat.waste_log(同 fresh-produce 流程)
//   - 门店 cut 部位 ↔ cube product 映射维护
//
// 状态机:本服务**无状态机**。盘点只记 "是否完整(is_complete)",不冻单。
//
// 时区口径:营业日界按门店本地时区(默认 Asia/Shanghai, UTC+8),
// 4:30 北京时间进场的猪落进"今日"窗口而非 UTC 前一天。Env FRESHMEAT_BIZ_TZ 可覆盖。
package service

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/YunBright/authkit/userinfo"
	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/fresh-meat/model"
	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Service 是 fresh-meat 服务的业务聚合入口。
//
// 通过 New(db) 构造,所有方法并发安全。
//
// 字段:
//   - db:PG 连接(GORM)
//   - cube:cube-gateway 客户端(可 nil,RecordWholePig 跳过 supplier 校验)
//   - publisher:pub/sub 广播(可 nil,启用 = DaprPublisher)
//   - userInfo:per-branch effective scopes 校验客户端(可 nil,HasEffectiveScope 返 ErrUserInfoUnavailable)
//   - scopeCache + scopeMu + scopeTTL:effective scopes 60s 缓存
//   - predictFn:LLM 预测函数(默认 history_avg 降级;SetPredictFn 注入 Dapr Conversation API)
type Service struct {
	db        *gorm.DB
	cube      CubeClient // 可 nil
	publisher Publisher  // nil = 禁用 pub/sub 广播
	userInfo  *userinfo.Client
	pubLogger *slog.Logger
	now       func() time.Time
	predictFn PredictFn      // SetPredictFn 覆盖
	bizTZ     *time.Location // 营业日界时区(默认 Asia/Shanghai UTC+8)

	scopeMu    sync.RWMutex
	scopeCache map[string]scopeCacheEntry
	scopeTTL   time.Duration
}

// CubeClient 是 cube-gateway 客户端的最小抽象(便于单测注入 mock)。
//
// SearchSuppliers 用于 supplier_id 校验。
//
// BatchCurrentPrices 用于「预期毛利(预售)结转」取**当前售价**
// (零售价 + 促销价;有促销价按促销价)—— 见 service/presale.go。
type CubeClient interface {
	SearchSuppliers(ctx context.Context, query string, limit int) ([]cubeclient.SupplierDTO, error)
	// BatchCurrentPrices 批量查当前售价;**没维护售价**的商品不在返回里。
	BatchCurrentPrices(ctx context.Context, productIDs []string, onDate string) (map[string]cubeclient.ProductPriceQuote, error)
}

// scopeCacheEntry 缓存一条 entry。
type scopeCacheEntry struct {
	scopes []string
	at     time.Time
}

// defaultScopeTTL 是 effective scopes 缓存的默认 TTL。
const defaultScopeTTL = 60 * time.Second

// New 构造 Service。
//
// db 是 PG 连接(GORM);cube / predictFn 通过 SetCubeClient / SetPredictFn 注入。
// bizTZ 默认 Asia/Shanghai(UTC+8);通过 SetBizTZ 覆盖。
func New(db *gorm.DB) *Service {
	return &Service{
		db:         db,
		pubLogger:  slog.Default(),
		now:        func() time.Time { return time.Now() },
		scopeCache: make(map[string]scopeCacheEntry),
		scopeTTL:   defaultScopeTTL,
		predictFn:  defaultPredictFn, // 默认 history_avg 降级路径
		bizTZ:      time.FixedZone("Asia/Shanghai", 8*3600),
	}
}

// SetBizTZ 注入营业日界时区(nil = 保持当前)。
//
// 优先用 time.LoadLocation(IANA 名, e.g. "Asia/Shanghai");失败 fallback 到 UTC。
// env FRESHMEAT_BIZ_TZ 由 cmdbootstrap / main.go 调本方法注入。
func (s *Service) SetBizTZ(loc *time.Location) {
	if loc == nil {
		return
	}
	s.bizTZ = loc
	slog.Info("fresh-meat: bizTZ set", "name", loc.String())
}

// BizTZ 暴露当前营业日界时区(handler 解析 ?date=YYYY-MM-DD 用)。
//
// 永不返 nil:Service.New() 已默认 Asia/Shanghai;只读访问。
func (s *Service) BizTZ() *time.Location {
	if s.bizTZ == nil {
		return time.UTC
	}
	return s.bizTZ
}

// businessDayBounds 营业日窗口:[当天 00:00 bizTZ, +24h bizTZ)。
//
// day 任意 time;取其在 bizTZ 下的年月日;4:30 北京时间进场 → 当日窗口内(in_kg > 0)。
func (s *Service) businessDayBounds(day time.Time) (start, end time.Time) {
	loc := s.bizTZ
	if loc == nil {
		loc = time.UTC
	}
	local := day.In(loc)
	start = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	end = start.Add(24 * time.Hour)
	return
}

// previousBizDayBounds 上一营业日窗口:[start-24h, start)。
//
// 用于昨夜库存结转:RecordPorkCutsStocktake 拉上一日 is_complete=true 的 actual_remain_kg。
func (s *Service) previousBizDayBounds(today time.Time) (start, end time.Time) {
	start, end = s.businessDayBounds(today)
	return start.Add(-24 * time.Hour), start
}

// SetClock 注入时间(测试用)。
func (s *Service) SetClock(fn func() time.Time) {
	if fn == nil {
		fn = func() time.Time { return time.Now().UTC() }
	}
	s.now = fn
}

// SetUserInfo 注入 userinfo 客户端。
func (s *Service) SetUserInfo(c *userinfo.Client) {
	s.userInfo = c
}

// SetScopeTTL 注入 effective scopes 缓存 TTL。
func (s *Service) SetScopeTTL(ttl time.Duration) {
	if ttl <= 0 {
		ttl = defaultScopeTTL
	}
	s.scopeMu.Lock()
	s.scopeTTL = ttl
	s.scopeMu.Unlock()
}

// ---- 业务错误(供 handler 映射 HTTP 状态码) ----

var (
	ErrWholePigNotFound          = errors.New("service: 整猪不存在")
	ErrPigCutNotFound            = errors.New("service: pig_cut 不存在")
	ErrPorkCutsStocktakeNotFound = errors.New("service: pork_cuts_stocktake 不存在")
	ErrBranchCutMappingNotFound  = errors.New("service: 门店 cut 部位映射不存在")
	ErrPromoSkuNotFound          = errors.New("service: 特价档货号不存在")
	ErrInvalidInput              = errors.New("service: 输入校验失败")
	ErrBranchMismatch            = errors.New("service: 跨店访问禁止")
	ErrBranchCutMappingConflict  = errors.New("service: (branch_id, cut_type) 映射已存在")
	// ⚠️ 文本要**中性**:冲突可能是「code 撞了」也可能是「部位撞了」,
	// 由调用处补具体那句。写成"code 已存在"会让部位冲突的报错
	// 变成「大宗品项 code 已存在: 部位 fat 已经登记过大宗品项…」—— 自相矛盾。
	ErrBulkProductConflict = errors.New("service: 大宗品项冲突")
	ErrCubeUnavailable     = errors.New("service: cube client 未配置")
	ErrUserInfoUnavailable = errors.New("service: userd 不可用,无法校验 effective scopes")
)

// ---- 整猪录入(RecordWholePig) ----

// RecordWholePig 录入一头猪 + 同步调 LLM。
//
// 流程:DB insert → 调 s.predictFn(pig, history) →
//   - 成功 → UPDATE whole_pig SET llm_advice_json=?, data_source='llm'
//   - 失败 / 超时 → 走 history_avg 兜底(UPDATE data_source='history_avg')
//
// **失败不视为业务错误**:返 200 + data_source=history_avg 给仓管。
func (s *Service) RecordWholePig(ctx context.Context, branchID string, in model.RecordWholePigInput, operatorID string) (*model.WholePig, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if operatorID == "" {
		return nil, fmt.Errorf("%w: operator_id 必填", ErrInvalidInput)
	}
	// ⚠️ **耳标号可选**（2026-10-24 门店口径:有些猪没有耳标/不扫码）。
	//
	// 数据库侧 ear_tag 只是普通索引(不是唯一键)⇒ 多头猪都留空不冲突 ✔。
	if strings.TrimSpace(in.SupplierID) == "" {
		return nil, fmt.Errorf("%w: supplier_id 必填", ErrInvalidInput)
	}
	if in.GrossWeightKg.IsZero() || in.GrossWeightKg.IsNegative() {
		return nil, fmt.Errorf("%w: gross_weight_kg 必须 > 0", ErrInvalidInput)
	}
	if in.PurchaseUnitPriceYuan.IsZero() || in.PurchaseUnitPriceYuan.IsNegative() {
		return nil, fmt.Errorf("%w: purchase_unit_price_yuan 必须 > 0", ErrInvalidInput)
	}
	if in.ArrivedAt.IsZero() {
		return nil, fmt.Errorf("%w: arrived_at 必填", ErrInvalidInput)
	}

	// ---- 链的起点:归属日不得早于期初结转的生效日(2026-10-25 门店反馈)----
	//
	// ⚠️ 起点之前**链还没开始** ⇒ 那几天的账算不出来 ⇒ 录了也是孤儿数据。
	//   起点**当天**可以营业(期初的余额就是那天的基线)⇒ 见 assertOnOrAfterChainStart。
	if err := s.assertAfterChainStart(ctx, branchID, in.ArrivedAt, "进猪"); err != nil {
		return nil, err
	}
	// ---- 跨日顺序校验(2026-10-24)----
	//
	// 门店口径:「不补录昨天,就无法录今天」⇒ 录猪之前,
	// **前一个营业日必须已盘点**(is_complete=true,含补录)⇒ 否则 400,
	// 并说清要补哪天(见 service/business_day.go)。
	//
	// ⚠️ 补录**往日**的猪是允许的(那是"补时间线"):判定看的是
	// "最近一次已盘点"与目标归属日的距离。
	if err := s.assertPrevBizDayStocktaken(ctx, branchID, in.ArrivedAt); err != nil {
		return nil, err
	}
	// ---- 冻结检查:已审计结转的期间不能再改(含补录)----
	if err := s.assertNotAudited(ctx, branchID, in.ArrivedAt, "进猪"); err != nil {
		return nil, err
	}

	// supplier_id 校验(需注入 cube client):调 cube.SearchSuppliers 拿前 1 个 supplier,
	// 若 ID 不匹配 → ErrInvalidInput。
	if s.cube != nil {
		if err := s.validateSupplierID(ctx, in.SupplierID); err != nil {
			return nil, err
		}
	}

	now := s.now()
	cost := in.GrossWeightKg.Mul(in.PurchaseUnitPriceYuan).Round(2)

	pig := &model.WholePig{
		ID:                    generateID(now, "WP"),
		EarTag:                in.EarTag,
		GrossWeightKg:         in.GrossWeightKg,
		PurchaseUnitPriceYuan: in.PurchaseUnitPriceYuan,
		PurchaseCostYuan:      cost,
		SupplierID:            in.SupplierID,
		BranchID:              branchID,
		ArrivedAt:             in.ArrivedAt,
		RecordedBy:            operatorID,
		DataSource:            model.DataSourceStub,
		HalfPig:               in.HalfPig,
		OffalIncluded:         in.OffalIncluded,
		PurchaseGroupID:       strings.TrimSpace(in.PurchaseGroupID),
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	if err := s.db.WithContext(ctx).Create(pig).Error; err != nil {
		return nil, fmt.Errorf("create whole_pig: %w", err)
	}

	// 营业日状态机:已进猪(状态由**实际动作**推进,前端不传)。
	if err := s.markBusinessDayPigIn(ctx, nil, branchID, in.ArrivedAt); err != nil {
		s.pubLogger.Error("RecordWholePig: 营业日状态推进失败(状态落后,下次动作会自愈)",
			"branch_id", branchID, "pig_id", pig.ID, "err", err)
	}

	// 查近 30 天 ±10% 重量段历史 + 调 LLM。
	history := s.findHistoryPigs(ctx, branchID, in.GrossWeightKg, in.ArrivedAt)
	result, predictErr := s.predictFn(ctx, pig, history)
	if predictErr != nil {
		s.pubLogger.Warn("RecordWholePig: predictFn failed, fallback to history_avg",
			"pig_id", pig.ID,
			"err", predictErr)
		result, predictErr = defaultPredictFn(ctx, pig, history)
		if predictErr != nil {
			s.pubLogger.Warn("RecordWholePig: history_avg fallback also failed",
				"pig_id", pig.ID,
				"err", predictErr)
			// 双失败:仍返 200 给仓管,DataSource 留 stub + llm_advice_json 留 NULL。
			return pig, nil
		}
	}

	// 写回 llm_advice_json + data_source。
	ds := model.DataSource(result.Source)
	if ds != model.DataSourceLLM && ds != model.DataSourceHistoryAvg {
		ds = model.DataSourceHistoryAvg
	}
	if err := s.db.WithContext(ctx).Model(pig).Updates(map[string]any{
		"llm_advice_json": datatypes.JSON(result.RawJSON),
		"data_source":     ds,
		"updated_at":      now,
	}).Error; err != nil {
		s.pubLogger.Warn("RecordWholePig: update llm_advice_json failed",
			"pig_id", pig.ID, "err", err)
		// DB 写失败不影响业务:返 pig 但 DB 上字段可能仍是 stub。
		return pig, nil
	}
	pig.LLMAdviceJSON = datatypes.JSON(result.RawJSON)
	pig.DataSource = ds
	return pig, nil
}

// validateSupplierID 通过 cube.SearchSuppliers 校验 supplier_id 存在。
//
// cube 内部以 ID 精确或 name 模糊查;若结果 ID 与请求 ID 一致 → 通过;
// 否则 → ErrInvalidInput(cube 不可用时返 ErrCubeUnavailable)。
func (s *Service) validateSupplierID(ctx context.Context, supplierID string) error {
	sups, err := s.cube.SearchSuppliers(ctx, supplierID, 1)
	if err != nil {
		s.pubLogger.Warn("validateSupplierID: cube.SearchSuppliers failed",
			"supplier_id", supplierID, "err", err)
		return fmt.Errorf("%w: %v", ErrCubeUnavailable, err)
	}
	for _, su := range sups {
		if su.ID == supplierID {
			return nil
		}
	}
	return fmt.Errorf("%w: supplier_id %s 在 cube 中不存在", ErrInvalidInput, supplierID)
}

// findHistoryPigs 查近 30 天 ±10% 重量段历史(同 branch)。
func (s *Service) findHistoryPigs(ctx context.Context, branchID string, grossKg decimal.Decimal, arrivedAt time.Time) []model.WholePig {
	from := arrivedAt.Add(-30 * 24 * time.Hour)
	nine := decimal.RequireFromString("0.9")
	eleven := decimal.RequireFromString("1.1")
	low := grossKg.Mul(nine)
	high := grossKg.Mul(eleven)
	var rows []model.WholePig
	err := s.db.WithContext(ctx).
		Where("branch_id = ? AND arrived_at >= ? AND gross_weight_kg >= ? AND gross_weight_kg <= ?",
			branchID, from, low, high).
		Order("arrived_at DESC").
		Limit(20).
		Find(&rows).Error
	if err != nil {
		s.pubLogger.Warn("findHistoryPigs: query failed",
			"branch_id", branchID, "err", err)
		return nil
	}
	return rows
}

// ---- 单品补录(RecordPigCut) ----

// RecordPigCut 单品补录(扫码补打部位条码)。
//
// 校验:
//   - pig 存在
//   - pig.branch_id == branchID(防跨店)
//   - (branch_id, cut_type) 已在 branch_cut_mapping 配置(防止未知 SKU)
//
// 不发事件;line_sales_by_pig 由 sale.completed 落。
func (s *Service) RecordPigCut(ctx context.Context, branchID string, in model.RecordPigCutInput, operatorID string) (*model.PigCut, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if operatorID == "" {
		return nil, fmt.Errorf("%w: operator_id 必填", ErrInvalidInput)
	}
	if strings.TrimSpace(in.PigID) == "" {
		return nil, fmt.Errorf("%w: pig_id 必填", ErrInvalidInput)
	}
	if in.CutType == "" {
		return nil, fmt.Errorf("%w: cut_type 必填", ErrInvalidInput)
	}
	if !in.CutType.Valid() {
		return nil, fmt.Errorf("%w: cut_type %q 不在 %d 类枚举内", ErrInvalidInput, in.CutType, len(model.AllCutTypes))
	}
	if in.WeightKg.IsZero() || in.WeightKg.IsNegative() {
		return nil, fmt.Errorf("%w: weight_kg 必须 > 0", ErrInvalidInput)
	}

	// pig.branch_id 校验
	var pig model.WholePig
	if err := s.db.First(&pig, "id = ?", in.PigID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrWholePigNotFound
		}
		return nil, err
	}
	if pig.BranchID != branchID {
		return nil, fmt.Errorf("%w: pig 不属于该 branch", ErrBranchMismatch)
	}
	// 冻结检查:已审计结转的期间不能再改(含补录)—— 分割补录属于那头猪的营业日。
	if err := s.assertNotAudited(ctx, branchID, pig.ArrivedAt, "分割补录"); err != nil {
		return nil, err
	}

	// (branch, cut_type) 必须在 branch_cut_mapping 配置中
	var mapping model.BranchCutMapping
	err := s.db.Where("branch_id = ? AND cut_type = ?", branchID, in.CutType).
		First(&mapping).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("%w: 未配置 (branch_id=%s, cut_type=%s) 的 cube 映射,请先调 POST /branch-cut-mappings", ErrInvalidInput, branchID, in.CutType)
		}
		return nil, err
	}

	now := s.now()
	cut := &model.PigCut{
		ID:        generateID(now, "PC"),
		PigID:     in.PigID,
		CutType:   in.CutType,
		BranchID:  branchID,
		Barcode:   in.Barcode,
		WeightKg:  in.WeightKg,
		ExpiresAt: in.ExpiresAt,
		CreatedAt: now,
	}
	// CubeProductID 是 SKU 级移动加权成本的"入库落点"。
	// 不填时留空 —— 计算侧会按 cut_type 归到该部位的主 SKU(兜底)。
	if sku := strings.TrimSpace(in.CubeProductID); sku != "" {
		// ⚠️ **必须用新的变量**:上面那次查询已经把 `mapping` 的主键填上了,
		// GORM 看到非零主键会**自动加上 id = ? 条件** ⇒
		// 查出来的是"同一个部位下的第一条映射",而不是本 SKU 那条。
		//
		// ⇒ 症状:一个部位挂两个 SKU 时(例:fat = 00032 板油 + 03808 水油),
		// 第二个 SKU 的分割补录被 400 拒("不在映射里")—— 明明配好了。
		var skuMapping model.BranchCutMapping
		if err := s.db.WithContext(ctx).
			Where("branch_id = ? AND cut_type = ? AND cube_product_id = ?",
				branchID, in.CutType, sku).First(&skuMapping).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, fmt.Errorf("%w: cube_product_id=%s 不在 (branch_id=%s, cut_type=%s) 的映射里",
					ErrInvalidInput, sku, branchID, in.CutType)
			}
			return nil, err
		}
		cut.CubeProductID = sku
	}
	if err := s.db.WithContext(ctx).Create(cut).Error; err != nil {
		return nil, fmt.Errorf("create pig_cut: %w", err)
	}
	return cut, nil
}

// ---- 日终盘点(RecordPorkCutsStocktake) ----

// RecordPorkCutsStocktake 录入日终盘点(2026-10-01 优化:per-SKU + bizTZ + opening 昨夜结转)。
//
// 必填字段(per-SKU):
//   - cube_product_id:盘 SKU 必填;后端校验在 branch_cut_mapping 中存在
//   - cut_type:必须在枚举内(model.AllCutTypes)
//   - actual_remain_kg:>= 0
//
// 日终完整路径:
//   - 服务端调用 computeExpectedRemainKgByCut 算 expected_per_cut(含昨夜 opening + waste)
//   - 把 expected_per_cut 按 caller actual 占比摊销到每 SKU(per-SKU expected)
//   - 计算 variance = actual - expected(per-SKU)
//   - is_complete=true 时发 pork.cuts.stocktaken(带 OpeningKg / WasteKg)
//
// 失败兜底:服务端计算失败 → 仍落 cuts(Expected/Variance 全 0)+ DataSource="actual_fallback"。
//
// # 归属日(2026-10-24 门店口径)
//
// TakenDate 指定这张盘点单归属**哪一天**(空 = 今天):
//
//   - 「每袋有自己的产生日,这个产生日就是盘点归属日(也就是补录指定日)」
//   - 「不装录昨天,就无法录今天」⇒ 补录是**常态**,不是例外。
//
// ⇒ 盘点单 / 按日结转记录 / 袋子,全部记在**归属日**那天。
// ⇒ 同一 (门店, 归属日) 只有**一张**盘点单(确定性主键 + upsert)。
//
// # 补录往日的记录**参与那一天的毛利回归**(时间线必须连续)
//
// 用户原话:「补录是为了避免错过日期无法正确填补空缺而导致**整条时间线错乱,
// 毛利就错完了**」⇒ 补录往日的按日结转记录会**照常进 FIFO 队列**
// ⇒ 那一天卖掉的货照样按 FIFO 摊回产出它的那头猪(见 service/pig_fifo.go)。
//
// # ⚠️ 余额按营业日推导:**尚未实现**(能力缺口,不是业务规则)
//
// 位置表(sku_stock_positions)与移动加权成本状态(sku_cost_states)目前仍是
// 「**当前余额**」口径 ⇒ 补录往日写下的**记录层**数据(盘点单 / 按日结转记录 /
// 袋子)已经进 FIFO 队列并参与毛利回归,但**那一天的结余与成本快照**
// 还没有按营业日重新推导。
//
// ⇒ 下一步(见交付报告「未做」一节):
// 结转记录 + 搬台记录 − 销售 ⇒ 按日结余 ⇒ 位置表与成本快照按营业日重算。
func (s *Service) RecordPorkCutsStocktake(ctx context.Context, branchID string, in model.RecordPorkCutsStocktakeInput, operatorID string) (*model.PorkCutsStocktake, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if operatorID == "" {
		return nil, fmt.Errorf("%w: operator_id 必填", ErrInvalidInput)
	}
	// ⚠️ 2026-10-24:cuts **可以是空数组**(当天一点货都没剩 ⇒ 也要有一张盘点单,
	// 标记『售罄』)⇒ 这里只校验"字段在不在"(nil = 根本没传),空数组是合法的。
	if in.Cuts == nil {
		return nil, fmt.Errorf("%w: cuts 必填(当天没有任何剩余时传空数组 [])", ErrInvalidInput)
	}

	now := s.now()

	// ---- 归属日(2026-10-24)----
	takenDate, isToday, err := s.resolveTakenDate(in.TakenDate, now)
	if err != nil {
		return nil, err
	}
	// ---- 冻结检查:已审计结转的期间不能再改(含补录)----
	if err := s.assertNotAudited(ctx, branchID, takenDate, "盘点"); err != nil {
		return nil, err
	}
	takenAt := takenDate
	if isToday {
		takenAt = now
	}
	originDate := datatypesDate(takenDate)

	// caller cuts 校验 + 收集 (cut, sku) → actual + 校验 mapping。
	type skuActual struct {
		CubeProductID string
		ActualKg      decimal.Decimal
	}
	callerActual := make(map[model.CutType][]skuActual, len(in.Cuts))
	// counterActualKg 是**柜台实盘**(per-SKU)—— 日终后要"搬进冷库过夜"。
	counterActualKg := make(map[string]decimal.Decimal, len(in.Cuts))
	// totalActualKg 是这一单盘出来的**总重量** ⇒ 用来判『售罄』。
	totalActualKg := decimal.Zero
	for i, ci := range in.Cuts {
		if ci.CutType == "" {
			return nil, fmt.Errorf("%w: cuts[%d].cut_type 必填", ErrInvalidInput, i)
		}
		if !ci.CutType.Valid() {
			return nil, fmt.Errorf("%w: cuts[%d].cut_type %q 不在 %d 类枚举", ErrInvalidInput, i, ci.CutType, len(model.AllCutTypes))
		}
		if strings.TrimSpace(ci.CubeProductID) == "" {
			return nil, fmt.Errorf("%w: cuts[%d].cube_product_id 必填", ErrInvalidInput, i)
		}
		if ci.ActualRemainKg.IsNegative() {
			return nil, fmt.Errorf("%w: cuts[%d].actual_remain_kg 必须 >= 0", ErrInvalidInput, i)
		}
		// 校验 (branch, cut_type, cube_product_id) 在 branch_cut_mapping 中存在。
		var m model.BranchCutMapping
		err := s.db.Where("branch_id = ? AND cut_type = ? AND cube_product_id = ?",
			branchID, ci.CutType, ci.CubeProductID).First(&m).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, fmt.Errorf("%w: cuts[%d] (%s, %s) 未在 branch_cut_mappings 配置",
					ErrInvalidInput, i, ci.CutType, ci.CubeProductID)
			}
			return nil, fmt.Errorf("query branch_cut_mapping: %w", err)
		}
		callerActual[ci.CutType] = append(callerActual[ci.CutType],
			skuActual{CubeProductID: ci.CubeProductID, ActualKg: ci.ActualRemainKg})
		counterActualKg[ci.CubeProductID] = counterActualKg[ci.CubeProductID].Add(ci.ActualRemainKg)
		totalActualKg = totalActualKg.Add(ci.ActualRemainKg)
	}

	// **售罄**(2026-10-24):「没有任何剩余也要创建一个空的,标记为『售罄』」
	// ⇒ cuts 为空,或所有行的 actual_remain_kg 都是 0。
	soldOut := !totalActualKg.IsPositive()

	// **鲜肉休市**(2026-10-24):「当天既没有卖猪、也没有搬冷库商品上猪肉台」
	// ⇒ 日终盘点标记为「鲜肉休市」。
	//
	// ⚠️ 与「售罄」是**两个不同的状态**:售罄 = 盘了但一点剩余都没有;
	//    休市 = 这天整个鲜肉板块没开张(没进猪、也没搬上台)。
	//
	// ⚠️ **互斥**(门店口径:「休市则没有(货),但需要一张休市盘点单」):
	//   休市日没有货可盘 ⇒ 不该同时标『售罄』⇒ 休市优先。
	restDay, err := s.isRestDay(ctx, branchID, takenDate)
	if err != nil {
		// 判不出来就不标(宁可少标,不要标错)。
		s.pubLogger.Warn("RecordPorkCutsStocktake: 鲜肉休市判定失败(本次不标休市)",
			"branch_id", branchID, "taken_date", dateKey(originDate), "err", err)
		restDay = false
	}
	if restDay {
		soldOut = false
	}

	// 服务端计算 expected_per_cut + opening + waste。
	//
	// ⚠️ 2026-10-23:改用**移动加权引擎**(全店口径),与毛利报告同一个引擎
	// ⇒ 不会出现"盘点单的应有"与"报表的应有"两套口径。
	//
	// ⚠️ 2026-10-24:算的是**归属日**那天(补录往日时 = 往日)。
	day := takenAt
	expectedPerCut, openingPerCut, wastePerCut, calcErr := s.expectedPerCutByEngine(ctx, branchID, day)
	dataSource := "actual"
	if calcErr != nil {
		s.pubLogger.Warn("RecordPorkCutsStocktake: expectedPerCutByEngine failed",
			"branch_id", branchID, "err", calcErr)
		dataSource = "actual_fallback"
		expectedPerCut = map[model.CutType]decimal.Decimal{}
		openingPerCut = map[model.CutType]decimal.Decimal{}
		wastePerCut = map[model.CutType]decimal.Decimal{}
	}

	// 生成 per-SKU CutSnapshot:按 caller actual 占比摊销 expected 到每 SKU。
	//
	// ⚠️ 2026-10-24(盘点口径拆分):**实盘只报柜台(猪肉台)**,
	// 冷库余额**单独一个字段**(cold_store_kg)给出 ⇒ 前端分开展示。
	// 「盘点盘的是猪肉台,不盘冷库」—— 门店口径。
	//
	// ⚠️ variance 仍是**全店口径**(柜台实盘 + 冷库余额 − 账面应有):
	// 账面应有(ExpectedClosingKg)来自移动加权引擎,是全店口径 ⇒ 两边要对齐。
	//
	// ⚠️ 冷库余额用**按营业日推导**的值(不是"当前余额")
	// ⇒ 补录往日的盘点时,冷库余额是**那一天**的,不是今天的。
	coldBalances, err := s.coldBalanceAtStocktake(ctx, branchID, takenDate, counterActualKg)
	if err != nil {
		// 推不出来就退回"当前余额"(宁可给个不准的,也不要没有)。
		s.pubLogger.Warn("RecordPorkCutsStocktake: 按营业日推导冷库余额失败(回退当前余额)",
			"branch_id", branchID, "taken_date", dateKey(originDate), "err", err)
		_, coldBalances, err = s.loadSkuPositions(ctx, branchID)
		if err != nil {
			return nil, fmt.Errorf("load sku positions: %w", err)
		}
	}
	var cutSnapshots []model.CutSnapshot
	// SKU → 标签号(2026-10-24):快照要带上 label,否则前端「载入并编辑」会清空标签号。
	labelOfSku := map[string]string{}
	for _, c := range in.Cuts {
		if l := strings.TrimSpace(c.Label); l != "" {
			if _, ok := labelOfSku[c.CubeProductID]; !ok {
				labelOfSku[c.CubeProductID] = l
			}
		}
	}
	for cutType, skuActuals := range callerActual {
		cutTotalActual := decimal.Zero
		for _, sa := range skuActuals {
			cutTotalActual = cutTotalActual.Add(sa.ActualKg)
		}
		cutExpected := expectedPerCut[cutType]
		for _, sa := range skuActuals {
			var skuExpected decimal.Decimal
			if cutErr := cutTotalActual.IsZero(); cutErr {
				skuExpected = cutExpected // 全为 0 实际,expected 直接给整个 cut
			} else if cutExpected.IsZero() {
				skuExpected = decimal.Zero
			} else {
				// 按 actual 占比摊销
				skuExpected = cutExpected.Mul(sa.ActualKg).Div(cutTotalActual)
			}
			counterKg := sa.ActualKg // 柜台实盘(仓管盘出来的)
			// ⚠️ 冷库余额是**按部位**的(门店口径:「这个模块只管部位数据」)
			// ⇒ 同一部位的多行 SKU 拿到的是**同一个部位冷库余额**
			//   (前端要按部位展示/去重,别把多行相加)。
			coldKg := coldBalances[string(cutType)]
			fullKg := counterKg.Add(coldKg)
			variance := fullKg.Sub(skuExpected)
			cutSnapshots = append(cutSnapshots, model.CutSnapshot{
				CutType:          cutType,
				CubeProductID:    sa.CubeProductID,
				ActualRemainKg:   counterKg,
				ColdStoreKg:      coldKg,
				TotalRemainKg:    fullKg,
				ExpectedRemainKg: skuExpected,
				VarianceKg:       variance,
				Label:            labelOfSku[sa.CubeProductID],
			})
		}
	}

	cutsJSON, err := json.Marshal(cutSnapshots)
	if err != nil {
		return nil, fmt.Errorf("marshal cuts: %w", err)
	}

	// ---- 盘点单:同一 (门店, 归属日) 只有一张 ⇒ upsert(2026-10-24)----
	//
	// ⚠️ 旧实现是 `generateID(now, "SK")` + Create ⇒ 同一天重复提交会**多出一张**
	// ⇒ 报表重复计数、"当天有几张盘点单"永远说不清。
	// ⇒ 改成**确定性主键**(门店 + 归属日)+ upsert ⇒ 重复提交 = 覆盖同一张。
	st := &model.PorkCutsStocktake{
		ID:         StocktakeID(branchID, dateKey(originDate)),
		BranchID:   branchID,
		TakenAt:    takenAt,
		TakenDate:  dateKey(originDate),
		TakenBy:    operatorID,
		IsComplete: in.IsComplete,
		SoldOut:    soldOut,
		RestDay:    restDay,
		Cuts:       cutsJSON,
		CreatedAt:  now,
	}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"taken_at":    takenAt,
			"taken_by":    operatorID,
			"is_complete": in.IsComplete,
			"sold_out":    soldOut,
			"rest_day":    restDay,
			"cuts":        cutsJSON,
		}),
	}).Create(st).Error; err != nil {
		return nil, fmt.Errorf("upsert pork_cuts_stocktake: %w", err)
	}
	// 同一 (门店, 归属日) 只留一张 ⇒ 清掉**历史遗留**的其它行
	// (旧实现每次提交都新建一张 ⇒ 同一天可能攒了好几张)。
	if err := s.dropDuplicateStocktakes(ctx, branchID, takenDate, st.ID); err != nil {
		s.pubLogger.Warn("RecordPorkCutsStocktake: 清理同一天的重复盘点单失败",
			"branch_id", branchID, "stocktake_id", st.ID, "err", err)
	}

	// ---- 派生状态(余额 / 成本状态)----
	//
	// ⚠️ 这几步操作的是「按营业日推导」的结余快照与成本日快照
	// (sku_stock_daily / sku_cost_daily),**不再**直接改「当前余额」(位置表)。
	//
	// ⚠️ 这几步**失败不回滚盘点单** —— 盘点是仓管已经完成的事实,
	// 搬运/结转/快照都是**派生状态**,下一轮盘点会重算(快照是 upsert)。
	//
	// ⚠️ 但失败必须**大声**:2026-10-23 实测踩过 —— PG 的
	// `ON CONFLICT DO UPDATE SET "qty_kg" = qty_kg + $1` 是**歧义**的
	// (目标表与 excluded 都有这一列)⇒ 位置表的每一次写入都失败
	// ⇒ 而这里的错误只是 WARN ⇒ **整个两位置库存模型静默失效**
	// (库存查询永远空、服务 200 正常、零报错)。
	// ⇒ 派生状态失败一律记 **Error**,并把"下次盘点会自愈"写进日志。
	if isToday {
		// ① 执行"过夜搬运"(猪肉台 → 冷库;全店余额不变,只换位置)
		if err := s.applyOvernightMove(ctx, branchID, counterActualKg); err != nil {
			s.pubLogger.Error("RecordPorkCutsStocktake: 过夜搬运失败(库存位置没更新,下次盘点会自愈)",
				"branch_id", branchID, "stocktake_id", st.ID, "err", err)
		}
		// ② 大宗品项的**结转记录(袋子)**在 registerStockBags 里登记
		// (2026-10-25:不再单独写 bulk_productions 台账 —— 同一事实只记一处,
		//  否则结算按台账扣、零售按袋子扣 ⇒ 同一批货会被卖两遍)。
		// ③ 按营业日推导**结余快照**(柜台实盘 / 冷库余额分开)+ 重算成本日快照。
		if err := s.recomputeCostSnapshots(ctx, branchID, takenDate); err != nil {
			s.pubLogger.Error("RecordPorkCutsStocktake: 结余快照重算失败(按日结余会缺,下次盘点会自愈)",
				"branch_id", branchID, "stocktake_id", st.ID, "err", err)
		}
		// ④ 落 SKU 成本日快照(次日期初的来源)+ 刷新**当前**成本状态。
		if _, err := s.PersistSkuCostDaily(ctx, branchID, st.TakenAt); err != nil {
			s.pubLogger.Error("RecordPorkCutsStocktake: 成本快照落库失败(次日期初会缺,下次盘点会自愈)",
				"branch_id", branchID, "stocktake_id", st.ID, "err", err)
		}
	} else {
		// ⑤ **补录往日**(2026-10-24)⇒ 重算那天的结余快照 + 成本日快照。
		//
		// ⇒ 「补录是为了避免错过日期无法正确填补空缺而导致**整条时间线错乱,
		//   毛利就错完了**」⇒ 那天的快照重算,**后面的日期跟着变**
		//   (次日期初 = 上日期末,链式)。
		//
		// ⚠️ **不动位置表**(sku_stock_positions)⇒ 今天的"当前余额"不会被补录写脏;
		// ⚠️ **不动 sku_cost_states**(当前成本状态)⇒ 只有"今天那次盘点"刷新它。
		if err := s.recomputeCostSnapshots(ctx, branchID, takenDate); err != nil {
			s.pubLogger.Error("RecordPorkCutsStocktake: 补录往日的结余/成本快照重算失败(那次的时间线会偏)",
				"branch_id", branchID, "stocktake_id", st.ID, "taken_date", dateKey(originDate), "err", err)
		}
		s.pubLogger.Info("RecordPorkCutsStocktake: 补录往日 ⇒ 记录层已写入 + 那天的结余/成本快照已重算",
			"branch_id", branchID, "taken_date", dateKey(originDate), "stocktake_id", st.ID)
	}
	// ⑥ **营业日状态机**:已盘点(带售罄/休市标志)。
	if err := s.markBusinessDayStocktaken(ctx, nil, branchID, takenDate, st.ID, soldOut, restDay); err != nil {
		s.pubLogger.Error("RecordPorkCutsStocktake: 营业日状态推进失败(状态落后,下次盘点会自愈)",
			"branch_id", branchID, "stocktake_id", st.ID, "err", err)
	}
	// ④ **按日结转记录登记**(2026-10-24):袋子(带标签号)+ 不装袋的散货。
	//
	// ⇒ 它是"**记录**"(归属日 + SKU + 重量 + 猪归属快照),两天都要写
	// (毛利 FIFO 回归到猪要靠它)。
	// ⇒ 袋子还让"次日整袋搬上台"成立,也让"当日搬上台了多少"有流水
	// (每日毛利的分摊重量要用它:搬上台的是老货,不能再摊一次猪成本)。
	if err := s.registerStockBags(ctx, branchID, takenAt, in.Cuts); err != nil {
		s.pubLogger.Error("RecordPorkCutsStocktake: 结转记录登记失败(标签号/归属丢失,下次盘点会自愈)",
			"branch_id", branchID, "stocktake_id", st.ID, "err", err)
	}

	// 仅当 is_complete=true 时发事件;sales-agg 据此切到"已盘点"标注。
	if st.IsComplete {
		s.publish(ctx, TopicPorkCutsStocktaken, &model.PorkCutsStocktakenEventData{
			StocktakeID: st.ID,
			BranchID:    st.BranchID,
			IsComplete:  st.IsComplete,
			Cuts:        cutSnapshots,
			OpeningKg:   openingStringMap(openingPerCut),
			WasteKg:     openingStringMap(wastePerCut),
			LLMReviewed: false,
			DataSource:  dataSource,
			CompletedAt: st.TakenAt,
			OperatorID:  st.TakenBy,
		})
	}
	return st, nil
}

// isRestDay 判定这一天是不是「**鲜肉休市**」(2026-10-24 门店口径)。
//
// 口径(用户原话):当天**既没有卖猪、也没有搬冷库商品上猪肉台**
// ⇒ 日终盘点标记为「鲜肉休市」。
//
// # 两个数据源
//
// ① **有没有进猪**:whole_pigs.arrived_at 落在归属日的营业日窗口内;
// ② **有没有搬冷库商品上猪肉台**:**搬台流水**(counter_loads,归属日 = 当天)
//
//	—— 两种搬法都记(整袋搬 / 按 SKU+kg 搬)⇒ 不会漏判。
//
// ⚠️ 老数据(没有搬台流水之前)回退到 stock_bags.moved_at(只有整袋搬留痕)。
func (s *Service) isRestDay(ctx context.Context, branchID string, takenDate time.Time) (bool, error) {
	startOfDay, endOfDay := s.businessDayBounds(takenDate)

	// ① 当天没有进猪
	var pigs int64
	if err := s.db.WithContext(ctx).Model(&model.WholePig{}).
		Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, startOfDay, endOfDay).
		Count(&pigs).Error; err != nil {
		return false, fmt.Errorf("count whole_pigs(rest day): %w", err)
	}
	if pigs > 0 {
		return false, nil // 有进猪 ⇒ 不是休市
	}

	// ② 当天没有搬冷库商品上猪肉台
	//
	// ⚠️ 2026-10-24:改用**搬台流水**(两种搬法都记)⇒ 不再漏判
	// "按 SKU+kg 搬上台"的日子(之前完全没有流水)。
	var loads int64
	loadStart, loadEnd := s.businessDayBounds(takenDate)
	if err := s.db.WithContext(ctx).Model(&model.CounterLoadRecord{}).
		Where("branch_id = ? AND biz_date >= ? AND biz_date < ?", branchID, loadStart, loadEnd).
		Count(&loads).Error; err != nil {
		return false, fmt.Errorf("count counter_loads(rest day): %w", err)
	}
	if loads > 0 {
		return false, nil // 搬上台了 ⇒ 不是休市
	}

	// ③ 老数据回退:整袋搬的 moved_at(没有流水表的时代)
	var moved int64
	if err := s.db.WithContext(ctx).Model(&model.StockBag{}).
		Where("branch_id = ? AND moved_at >= ? AND moved_at < ?", branchID, startOfDay, endOfDay).
		Count(&moved).Error; err != nil {
		return false, fmt.Errorf("count moved stock_bags(rest day): %w", err)
	}
	if moved > 0 {
		return false, nil // 搬上台了 ⇒ 不是休市
	}

	return true, nil
}

// resolveTakenDate 解析盘点单的**归属日**(2026-10-24)。
// - takenDate 为空 ⇒ 今天(营业日界时区,bizTZ);
// - 否则按 "YYYY-MM-DD" 解析(格式错 ⇒ ErrInvalidInput,400)。
//
// 返回 (归属日的零点, 是不是今天)。
//
// ⚠️ 为什么归属日要单独一个字段:门店要能**补录往日**
// (「每袋有自己的产生日,这个产生日就是盘点归属日(也就是补录指定日)」)
// ⇒ 盘点单 / 结转记录 / 袋子都要记在**那一天**,而不是提交那天。
func (s *Service) resolveTakenDate(takenDate string, now time.Time) (time.Time, bool, error) {
	loc := s.bizTZOrUTC()
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	td := strings.TrimSpace(takenDate)
	if td == "" {
		return today, true, nil
	}
	parsed, err := time.ParseInLocation("2006-01-02", td, loc)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("%w: taken_date 必须是 YYYY-MM-DD(收到 %q)", ErrInvalidInput, td)
	}
	// ⚠️ 2026-10-24:归属日**不能是未来** ⇒ 「补录日必须 ≤ 今天」。
	// (往前补是允许的 —— 那正是"补时间线"。)
	if parsed.After(today) {
		return time.Time{}, false, fmt.Errorf("%w: 归属日 %s 不能是未来(只能补录今天及以前)", ErrInvalidInput, td)
	}
	return parsed, parsed.Equal(today), nil
}

// dropDuplicateStocktakes 清掉同一 (门店, 归属日) 下的**其它**盘点单。
//
// ⚠️ 为什么需要:旧实现每次提交都 `Create` 一张新单 ⇒ 同一天可能攒好几张
// ⇒ 报表重复计数、"当天到底盘了几次"说不清。
// ⇒ 2026-10-24 起新单走确定性主键 + upsert,但**历史遗留的多张**要清掉。
//
// ⚠️ 作用域:同一个营业日窗口 + 不同 id ⇒ 不会碰到别的日期的单。
func (s *Service) dropDuplicateStocktakes(ctx context.Context, branchID string, takenDate time.Time, keepID string) error {
	dayStart, dayEnd := s.businessDayBounds(takenDate)
	res := s.db.WithContext(ctx).
		Where("branch_id = ? AND taken_at >= ? AND taken_at < ? AND id <> ?", branchID, dayStart, dayEnd, keepID).
		Delete(&model.PorkCutsStocktake{})
	if res.Error != nil {
		return fmt.Errorf("delete duplicate stocktakes: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		s.pubLogger.Info("RecordPorkCutsStocktake: 清掉同一天的重复盘点单(旧实现每次提交都新建)",
			"branch_id", branchID, "taken_date", takenDate.Format("2006-01-02"), "rows", res.RowsAffected)
	}
	return nil
}

// openingStringMap / wasteStringMap 把 CutType-keyed map 转 string-keyed map(JSON-friendly)。
func openingStringMap(in map[model.CutType]decimal.Decimal) map[string]decimal.Decimal {
	out := make(map[string]decimal.Decimal, len(in))
	for k, v := range in {
		out[string(k)] = v
	}
	return out
}

// ---- 报损(RecordWasteLog) ----

// RecordWasteLog 录入报损。
//
// Tx:INSERT waste_logs → publish waste.log.recorded;任一失败全滚。
// 字段:log_id / branch_id / pig_id(可空)/ cut_type(可空)/ cube_product_id(可空)/
// qty_kg / reason / recorded_at / operator_id。
//   - 本仓内表 `fresh_meat.waste_logs`(9 字段 + 时间)由 AutoMigrate 建。
//
// 验参:branch_id / operator_id / qty_kg > 0 / reason 非空 / cut_type(若填)Valid。
func (s *Service) RecordWasteLog(ctx context.Context, branchID string, in model.RecordWasteLogInput, operatorID string) (*model.WasteLog, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if operatorID == "" {
		return nil, fmt.Errorf("%w: operator_id 必填", ErrInvalidInput)
	}
	if in.QtyKg.IsZero() || in.QtyKg.IsNegative() {
		return nil, fmt.Errorf("%w: qty_kg 必须 > 0", ErrInvalidInput)
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, fmt.Errorf("%w: reason 必填", ErrInvalidInput)
	}
	if in.CutType != "" && !in.CutType.Valid() {
		return nil, fmt.Errorf("%w: cut_type %q 不在 %d 类枚举内", ErrInvalidInput, in.CutType, len(model.AllCutTypes))
	}

	now := s.now()
	logID := generateID(now, "WL")
	recordedAt := now
	if !in.RecordedAt.IsZero() {
		recordedAt = in.RecordedAt
	}
	// 冻结检查:已审计结转的期间不能再改(含补录)。
	if err := s.assertNotAudited(ctx, branchID, recordedAt, "报损"); err != nil {
		return nil, err
	}

	rec := &model.WasteLog{
		ID:            logID,
		BranchID:      branchID,
		PigID:         strings.TrimSpace(in.PigID),
		CutType:       in.CutType,
		CubeProductID: strings.TrimSpace(in.CubeProductID),
		QtyKg:         in.QtyKg,
		Reason:        strings.TrimSpace(in.Reason),
		RecordedAt:    recordedAt,
		OperatorID:    operatorID,
		CreatedAt:     now,
	}

	// Tx:DB insert + publish;任一失败全滚。
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(rec).Error; err != nil {
			return fmt.Errorf("create waste_log: %w", err)
		}
		// Tx 内 publish:本服务 publish() 是异步 background ctx,失败仅 warn 不阻断;
		// 但在 Tx 内调用,若 publish 异步失败,DB 已提交无法回滚 — 这是可接受折衷。
		s.publish(ctx, TopicWasteLogRecorded, &model.WasteLogRecordedEventData{
			LogID:         logID,
			BranchID:      branchID,
			PigID:         rec.PigID,
			CutType:       rec.CutType,
			CubeProductID: rec.CubeProductID,
			QtyKg:         rec.QtyKg,
			Reason:        rec.Reason,
			RecordedAt:    rec.RecordedAt,
			OperatorID:    operatorID,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// ---- 查询(GET 用) ----

// GetWholePig 查整猪。
func (s *Service) GetWholePig(_ context.Context, branchID, pigID string) (*model.WholePig, error) {
	var p model.WholePig
	if err := s.db.First(&p, "id = ?", pigID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrWholePigNotFound
		}
		return nil, err
	}
	if p.BranchID != branchID {
		return nil, ErrBranchMismatch
	}
	return &p, nil
}

// ListWholePigsByDay 查某店某日整猪(按 arrived_at 日期范围)。
//
// day 是当地日期(YYYY-MM-DD),服务端转 [day 00:00 bizTZ, day+1 00:00 bizTZ)。
func (s *Service) ListWholePigsByDay(_ context.Context, branchID string, day time.Time) ([]model.WholePig, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	startOfDay, endOfDay := s.businessDayBounds(day)

	var pigs []model.WholePig
	err := s.db.Where("branch_id = ? AND arrived_at >= ? AND arrived_at < ?", branchID, startOfDay, endOfDay).
		Order("arrived_at ASC").
		Find(&pigs).Error
	if err != nil {
		return nil, err
	}
	return pigs, nil
}

// ListPigCutsByPig 查一头猪的所有部位补录。
func (s *Service) ListPigCutsByPig(_ context.Context, branchID, pigID string) ([]model.PigCut, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	// 先校验 pig 归属
	var pig model.WholePig
	if err := s.db.First(&pig, "id = ?", pigID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrWholePigNotFound
		}
		return nil, err
	}
	if pig.BranchID != branchID {
		return nil, ErrBranchMismatch
	}
	var cuts []model.PigCut
	if err := s.db.Where("pig_id = ?", pigID).Order("created_at ASC").Find(&cuts).Error; err != nil {
		return nil, err
	}
	return cuts, nil
}

// GetLatestPorkCutsStocktake 查某店某日盘点。
func (s *Service) GetLatestPorkCutsStocktake(_ context.Context, branchID string, day time.Time) (*model.PorkCutsStocktake, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	startOfDay, endOfDay := s.businessDayBounds(day)

	var st model.PorkCutsStocktake
	err := s.db.Where("branch_id = ? AND taken_at >= ? AND taken_at < ?", branchID, startOfDay, endOfDay).
		Order("taken_at DESC").
		First(&st).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrPorkCutsStocktakeNotFound
		}
		return nil, err
	}
	st.TakenDate = s.takenDateOf(st.TakenAt)
	return &st, nil
}

// takenDateOf 把 taken_at 折算成**归属日**("YYYY-MM-DD",营业日界时区)。
func (s *Service) takenDateOf(takenAt time.Time) string {
	return takenAt.In(s.bizTZOrUTC()).Format("2006-01-02")
}

// GetPorkCutsStocktake 按 ID 查盘点单。
func (s *Service) GetPorkCutsStocktake(_ context.Context, branchID, stocktakeID string) (*model.PorkCutsStocktake, error) {
	var st model.PorkCutsStocktake
	if err := s.db.First(&st, "id = ?", stocktakeID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrPorkCutsStocktakeNotFound
		}
		return nil, err
	}
	if st.BranchID != branchID {
		return nil, ErrBranchMismatch
	}
	st.TakenDate = s.takenDateOf(st.TakenAt)
	return &st, nil
}

// ListLineSalesByPig 查某 pig / 某 cut_type 的销售聚合。
//
// 由 OnSaleCompleted 落 line_sales_by_pig 后启用。
func (s *Service) ListLineSalesByPig(_ context.Context, branchID, pigID string, _ *model.CutType, _, _ time.Time) ([]model.LineSalesByPig, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if pigID == "" {
		return nil, fmt.Errorf("%w: pig_id 必填", ErrInvalidInput)
	}
	var rows []model.LineSalesByPig
	err := s.db.Where("branch_id = ? AND pig_id = ?", branchID, pigID).
		Order("sale_date DESC, created_at DESC").
		Find(&rows).Error
	return rows, err
}

// ---- BranchCutMapping CRUD ----

// ListBranchCutMappings 查某店所有 cut 映射。
func (s *Service) ListBranchCutMappings(_ context.Context, branchID string) ([]model.BranchCutMapping, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	var rows []model.BranchCutMapping
	err := s.db.Where("branch_id = ?", branchID).Order("cut_type ASC").Find(&rows).Error
	return rows, err
}

// CreateBranchCutMapping 新建 (branch, cut_type) → cube_product_id 映射。
//
// UNIQUE 冲突 → ErrBranchCutMappingConflict。cube 校验 cube_product_id 存在。
func (s *Service) CreateBranchCutMapping(ctx context.Context, branchID string, in model.CreateBranchCutMappingInput) (*model.BranchCutMapping, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if in.CutType == "" {
		return nil, fmt.Errorf("%w: cut_type 必填", ErrInvalidInput)
	}
	if !in.CutType.Valid() {
		return nil, fmt.Errorf("%w: cut_type %q 不在 %d 类枚举内", ErrInvalidInput, in.CutType, len(model.AllCutTypes))
	}
	if strings.TrimSpace(in.CubeProductID) == "" {
		return nil, fmt.Errorf("%w: cube_product_id 必填", ErrInvalidInput)
	}

	now := s.now()
	m := &model.BranchCutMapping{
		ID:            generateID(now, "BCM"),
		BranchID:      branchID,
		CutType:       in.CutType,
		CubeProductID: in.CubeProductID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.db.WithContext(ctx).Create(m).Error; err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: (%s, %s, cube_product_id=%s)", ErrBranchCutMappingConflict, branchID, in.CutType, in.CubeProductID)
		}
		return nil, fmt.Errorf("create branch_cut_mapping: %w", err)
	}
	// 推送"这个货号是猪肉部位"给通用盘点的守卫(见 scope_push.go)。
	s.pushScopeExclusion(ctx, branchID, m.CubeProductID, true)
	return m, nil
}

// UpdateBranchCutMapping 改映射 cube_product_id(支持改 cube_product_name + cut_type)。
func (s *Service) UpdateBranchCutMapping(ctx context.Context, branchID, mappingID string, in model.UpdateBranchCutMappingInput) (*model.BranchCutMapping, error) {
	var m model.BranchCutMapping
	if err := s.db.First(&m, "id = ?", mappingID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrBranchCutMappingNotFound
		}
		return nil, err
	}
	if m.BranchID != branchID {
		return nil, ErrBranchMismatch
	}
	now := s.now()
	updates := map[string]any{"updated_at": now}
	if in.CubeProductID != "" {
		updates["cube_product_id"] = in.CubeProductID
	}
	effectiveCut := m.CutType
	if in.CutType != "" {
		if !in.CutType.Valid() {
			return nil, fmt.Errorf("%w: cut_type %q 不在 %d 类枚举内", ErrInvalidInput, in.CutType, len(model.AllCutTypes))
		}
		if in.CutType != m.CutType {
			updates["cut_type"] = in.CutType
			effectiveCut = in.CutType
		}
	}
	if len(updates) == 1 {
		return &m, nil // 仅时间戳更新也允许,但避免空 SQL
	}
	if err := s.db.WithContext(ctx).Model(&m).Updates(updates).Error; err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: 目标 (%s, %s) 已存在", ErrBranchCutMappingConflict, branchID, effectiveCut)
		}
		return nil, err
	}
	// 货号改了 ⇒ 旧货号不再算猪肉(撤销排除),新货号算(推送排除)。
	if in.CubeProductID != "" && in.CubeProductID != m.CubeProductID {
		s.pushScopeExclusion(ctx, branchID, m.CubeProductID, false)
	}
	s.pushScopeExclusion(ctx, branchID, m.CubeProductID, true)
	return &m, nil
}

// DeleteBranchCutMapping 删映射。
func (s *Service) DeleteBranchCutMapping(ctx context.Context, branchID, mappingID string) error {
	var m model.BranchCutMapping
	if err := s.db.First(&m, "id = ?", mappingID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrBranchCutMappingNotFound
		}
		return err
	}
	if m.BranchID != branchID {
		return ErrBranchMismatch
	}
	if err := s.db.WithContext(ctx).Delete(&m).Error; err != nil {
		return err
	}
	// 映射没了 ⇒ 这个货号不再是猪肉部位 ⇒ 通用盘点可以盘它了。
	s.pushScopeExclusion(ctx, branchID, m.CubeProductID, false)
	return nil
}

// ---- effective scopes ----

// GetEffectiveScopes 拿用户在某 branch 下的 effective scopes(per-branch 三元权限)。
//
// key = (userID, branchID),TTL 沿用 s.scopeTTL(默认 60s)。
//
// 错误:
//   - userID == "" / branchID == "" → ErrInvalidInput
//   - s.userInfo == nil              → ErrUserInfoUnavailable(503)
//   - userinfo.GetBranchPermissions 失败 → ErrUserInfoUnavailable
func (s *Service) GetEffectiveScopes(ctx context.Context, userID, branchID string) ([]string, error) {
	if userID == "" {
		return nil, fmt.Errorf("%w: user_id 必填", ErrInvalidInput)
	}
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if s.userInfo == nil {
		return nil, ErrUserInfoUnavailable
	}

	ck := scopeCacheKey(userID, branchID)

	s.scopeMu.RLock()
	entry, ok := s.scopeCache[ck]
	ttl := s.scopeTTL
	s.scopeMu.RUnlock()
	if ok && time.Since(entry.at) < ttl {
		return entry.scopes, nil
	}

	s.scopeMu.Lock()
	defer s.scopeMu.Unlock()
	if entry, ok := s.scopeCache[ck]; ok && time.Since(entry.at) < ttl {
		return entry.scopes, nil
	}

	p, err := s.userInfo.GetBranchPermissions(ctx, userID, branchID)
	if err != nil {
		s.pubLogger.Warn("userinfo.GetBranchPermissions failed",
			"user_id", userID, "branch_id", branchID, "err", err)
		return nil, ErrUserInfoUnavailable
	}
	scopes := pickBranchScopes(p, branchID)
	scopes = append([]string(nil), scopes...)
	s.scopeCache[ck] = scopeCacheEntry{scopes: scopes, at: s.now()}
	return scopes, nil
}

// HasEffectiveScope 判断用户在该 branch 下是否拥有某 scope。
func (s *Service) HasEffectiveScope(ctx context.Context, userID, branchID, scope string) (bool, error) {
	scopes, err := s.GetEffectiveScopes(ctx, userID, branchID)
	if err != nil {
		return false, err
	}
	for _, sc := range scopes {
		if sc == scope {
			return true, nil
		}
	}
	return false, nil
}

// InvalidateScopeCache 失效某 (user, branch) 缓存。
//
// 订阅 auth.user.access_changed 时调。
func (s *Service) InvalidateScopeCache(userID, branchID string) {
	if userID == "" || branchID == "" {
		return
	}
	s.scopeMu.Lock()
	defer s.scopeMu.Unlock()
	delete(s.scopeCache, scopeCacheKey(userID, branchID))
}

// scopeCacheKey 把 (userID, branchID) 编为 cache key。
func scopeCacheKey(userID, branchID string) string {
	return userID + "|" + branchID
}

// pickBranchScopes 从 BranchPermissions 矩阵里挑出目标 branch 行的 scopes。
//
// 未命中返 nil(让 handler 走 403)。
func pickBranchScopes(p *userinfo.BranchPermissions, branchID string) []string {
	if p == nil || len(p.Branches) == 0 {
		return nil
	}
	for _, row := range p.Branches {
		if row.BranchID == branchID {
			return row.Scopes
		}
	}
	return nil
}

// ---- 工具 ----

// generateID 生成 `<prefix><yyyymmdd><8-hex random>`(同 stocktake 风格)。
//
// 8 byte crypto/rand 转 16 hex — 实际取 4 byte 8 hex 足够;stocktake 风格。
func generateID(now time.Time, prefix string) string {
	var b [4]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		ts := uint64(now.UTC().UnixNano())
		b[0] = byte(ts >> 24)
		b[1] = byte(ts >> 16)
		b[2] = byte(ts >> 8)
		b[3] = byte(ts)
	}
	return fmt.Sprintf("%s%s%08x", prefix, now.UTC().Format("20060102"), b)
}

// strings_TrimEmpty 是 strings.TrimSpace(x) == "" 的缩写,避免引入 strings 包聚合 import。
func strings_TrimEmpty(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
			return false
		}
	}
	return true
}

// isUniqueViolation 检测 PostgreSQL 唯一性冲突(代码 23505)。
//
// SQLite 行为不严格一致;单测 fallback 用 errors.Is 检查 mock 注入的 sentinel。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return contains(msg, "23505") || contains(msg, "UNIQUE constraint failed") || contains(msg, "duplicate key")
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
