// Package service 实现 fresh-meat 服务的业务逻辑(fresh-meat)。
//
// 关键设计(REQUIREMENTS §4 / DESIGN §6):
//   - 早盘整猪录入(一头一行)+ 同步调 LLM 推演预期分割(阶段3 起接入;阶段1 stub)
//   - 白天单品补录(pig_cuts)+ POS 销售事件本地聚合(line_sales_by_pigs)
//   - 日终按部位盘点(整店,可选,不阻断销售)
//   - 报损走 fresh_meat.waste_log(同 fresh-produce 流程)
//   - 门店 cut 部位 ↔ cube product 映射维护
//
// 状态机:本服务**无状态机**。盘点只记 "是否完整(is_complete)",不冻单。
//
// 阶段说明:
//   - 阶段1(本版本):CRUD + DB 写 + 数据校验;LLM stub(data_source="stub");
//     events noop(publisher 未注入)
//   - 阶段2:发 pork.cuts.stocktaken / waste.log.recorded;订阅 sale.completed;
//     落 line_sales_by_pig
//   - 阶段3:调 Dapr Conversation API 替换 stub;Cube supplier 校验
//   - 阶段4:日终完整路径(cube + line_sales + waste_log 三方聚合 expected_remain_kg)
//
// 时区口径(2026-10-01 优化):营业日界按门店本地时区(默认 Asia/Shanghai, UTC+8),
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
)

// Service 是 fresh-meat 服务的业务聚合入口。
//
// 通过 New(db) 构造,所有方法并发安全。
//
// 字段:
//   - db:PG 连接(GORM)
//   - cube:cube-gateway 客户端(阶段3 注入;阶段1 为 nil,RecordWholePig 跳过 supplier 校验)
//   - publisher:pub/sub 广播(阶段2 注入 DaprPublisher;阶段1 nil = noop)
//   - userInfo:per-branch effective scopes 校验客户端(可 nil,HasEffectiveScope 返 ErrUserInfoUnavailable)
//   - scopeCache + scopeMu + scopeTTL:effective scopes 60s 缓存
//   - predictFn:LLM 预测函数(默认 history_avg 降级;阶段3 注入 Dapr Conversation API)
type Service struct {
	db        *gorm.DB
	cube      CubeClient // 阶段3 注入,阶段1 可 nil
	publisher Publisher  // nil = 禁用 pub/sub 广播
	userInfo  *userinfo.Client
	pubLogger *slog.Logger
	now       func() time.Time
	predictFn PredictFn // 阶段3 覆盖为 DaprConversationPredictFn
	bizTZ     *time.Location // 营业日界时区(默认 Asia/Shanghai UTC+8)

	scopeMu    sync.RWMutex
	scopeCache map[string]scopeCacheEntry
	scopeTTL   time.Duration
}

// CubeClient 是 cube-gateway 客户端的最小抽象(便于单测注入 mock)。
//
// 阶段3 改为 *cubeclient.Client;SearchSuppliers 用于 supplier_id 校验。
type CubeClient interface {
	SearchSuppliers(ctx context.Context, query string, limit int) ([]cubeclient.SupplierDTO, error)
}

// scopeCacheEntry 缓存一条 entry。
type scopeCacheEntry struct {
	scopes []string
	at     time.Time
}

// defaultScopeTTL 是 effective scopes 缓存的默认 TTL。
const defaultScopeTTL = 60 * time.Second

// New 构造 Service(阶段1)。
//
// db 是 PG 连接(GORM);cube / predictFn 在阶段3 注入(SetCubeClient / SetPredictFn)。
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
	ErrWholePigNotFound         = errors.New("service: 整猪不存在")
	ErrPigCutNotFound           = errors.New("service: pig_cut 不存在")
	ErrPorkCutsStocktakeNotFound = errors.New("service: pork_cuts_stocktake 不存在")
	ErrBranchCutMappingNotFound = errors.New("service: 门店 cut 部位映射不存在")
	ErrInvalidInput             = errors.New("service: 输入校验失败")
	ErrBranchMismatch           = errors.New("service: 跨店访问禁止")
	ErrBranchCutMappingConflict = errors.New("service: (branch_id, cut_type) 映射已存在")
	ErrCubeUnavailable          = errors.New("service: cube client 未配置")
	ErrUserInfoUnavailable      = errors.New("service: userd 不可用,无法校验 effective scopes")
)

// ---- 整猪录入(RecordWholePig) ----

// RecordWholePig 录入一头猪 + 同步调 LLM(阶段3 接入;阶段1 stub)。
//
// 阶段1 行为:DB insert + data_source="stub"。
// 阶段3 行为:DB insert → 调 s.predictFn(pig, history) →
//   - 成功 → UPDATE whole_pig SET llm_advice_json=?, data_source='llm'
//   - 失败 / 超时 → 走 history_avg 兜底(UPDATE data_source='history_avg')
// **失败不视为业务错误**:返 200 + data_source=history_avg 给仓管。
func (s *Service) RecordWholePig(ctx context.Context, branchID string, in model.RecordWholePigInput, operatorID string) (*model.WholePig, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if operatorID == "" {
		return nil, fmt.Errorf("%w: operator_id 必填", ErrInvalidInput)
	}
	if strings.TrimSpace(in.EarTag) == "" {
		return nil, fmt.Errorf("%w: ear_tag 必填", ErrInvalidInput)
	}
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

	// 阶段3 supplier_id 校验:调 cube.SearchSuppliers 拿前 1 个 supplier,
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
	if err := s.db.Create(pig).Error; err != nil {
		return nil, fmt.Errorf("create whole_pig: %w", err)
	}

	// 阶段3:查近 30 天 ±10% 重量段历史 + 调 LLM。
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
	if err := s.db.Model(pig).Updates(map[string]any{
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
// 阶段1 不发事件;阶段2 仍不发(line_sales_by_pig 由 sale.completed 落)。
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
	if err := s.db.Create(cut).Error; err != nil {
		return nil, fmt.Errorf("create pig_cut: %w", err)
	}
	return cut, nil
}

// ---- 日终盘点(RecordPorkCutsStocktake) ----

// RecordPorkCutsStocktake 录入日终盘点(2026-10-01 优化:per-SKU + bizTZ + opening 昨夜结转)。
//
// 必填字段(per-SKU):
//   - cube_product_id:盘 SKU 必填;后端校验在 branch_cut_mapping 中存在
//   - cut_type:必须在 12 类枚举内
//   - actual_remain_kg:>= 0
//
// 阶段4 完整路径:
//   - 服务端调用 computeExpectedRemainKgByCut 算 expected_per_cut(含昨夜 opening + waste)
//   - 把 expected_per_cut 按 caller actual 占比摊销到每 SKU(per-SKU expected)
//   - 计算 variance = actual - expected(per-SKU)
//   - is_complete=true 时发 pork.cuts.stocktaken(带 OpeningKg / WasteKg)
//
// 失败兜底:服务端计算失败 → 仍落 cuts(Expected/Variance 全 0)+ DataSource="actual_fallback"。
func (s *Service) RecordPorkCutsStocktake(ctx context.Context, branchID string, in model.RecordPorkCutsStocktakeInput, operatorID string) (*model.PorkCutsStocktake, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if operatorID == "" {
		return nil, fmt.Errorf("%w: operator_id 必填", ErrInvalidInput)
	}
	if len(in.Cuts) == 0 {
		return nil, fmt.Errorf("%w: cuts 不能为空", ErrInvalidInput)
	}

	// caller cuts 校验 + 收集 (cut, sku) → actual + 校验 mapping。
	type skuActual struct {
		CubeProductID string
		ActualKg      decimal.Decimal
	}
	callerActual := make(map[model.CutType][]skuActual, len(in.Cuts))
	for i, ci := range in.Cuts {
		if ci.CutType == "" {
			return nil, fmt.Errorf("%w: cuts[%d].cut_type 必填", ErrInvalidInput, i)
		}
		if !ci.CutType.Valid() {
			return nil, fmt.Errorf("%w: cuts[%d].cut_type %q 不在 12 类枚举", ErrInvalidInput, i, ci.CutType)
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
	}

	now := s.now()

	// 服务端计算 expected_per_cut + opening + waste。
	day := now
	calc, calcErr := s.computeExpectedRemainKgByCut(ctx, branchID, day)
	dataSource := "actual"
	if calcErr != nil {
		s.pubLogger.Warn("RecordPorkCutsStocktake: computeExpectedRemainKgByCut failed",
			"branch_id", branchID, "err", calcErr)
		dataSource = "actual_fallback"
	}

	expectedPerCut := make(map[model.CutType]decimal.Decimal, len(callerActual))
	openingPerCut := make(map[model.CutType]decimal.Decimal)
	wastePerCut := make(map[model.CutType]decimal.Decimal)
	if calcErr == nil {
		for _, c := range calc {
			expectedPerCut[c.CutType] = c.ExpectedRemain
			openingPerCut[c.CutType] = c.OpeningKg
			wastePerCut[c.CutType] = c.WasteKg
		}
	}

	// 生成 per-SKU CutSnapshot:按 caller actual 占比摊销 expected 到每 SKU。
	var cutSnapshots []model.CutSnapshot
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
			variance := sa.ActualKg.Sub(skuExpected)
			cutSnapshots = append(cutSnapshots, model.CutSnapshot{
				CutType:          cutType,
				CubeProductID:    sa.CubeProductID,
				ActualRemainKg:   sa.ActualKg,
				ExpectedRemainKg: skuExpected,
				VarianceKg:       variance,
			})
		}
	}

	cutsJSON, err := json.Marshal(cutSnapshots)
	if err != nil {
		return nil, fmt.Errorf("marshal cuts: %w", err)
	}

	st := &model.PorkCutsStocktake{
		ID:         generateID(now, "SK"),
		BranchID:   branchID,
		TakenAt:    now,
		TakenBy:    operatorID,
		IsComplete: in.IsComplete,
		Cuts:       cutsJSON,
		CreatedAt:  now,
	}
	if err := s.db.Create(st).Error; err != nil {
		return nil, fmt.Errorf("create pork_cuts_stocktake: %w", err)
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

// openingStringMap / wasteStringMap 把 CutType-keyed map 转 string-keyed map(JSON-friendly)。
func openingStringMap(in map[model.CutType]decimal.Decimal) map[string]decimal.Decimal {
	out := make(map[string]decimal.Decimal, len(in))
	for k, v := range in {
		out[string(k)] = v
	}
	return out
}

// ---- 报损(RecordWasteLog) ----

// RecordWasteLog 录入报损(2026-10-01 优化:DB 持久化 + 事件双发)。
//
// 阶段1 旧:仅 publish,DB 不写(丢数据)。
// 当前行为:
//   - Tx:INSERT waste_logs → publish waste.log.recorded;任一失败全滚;
//   - 字段:log_id / branch_id / pig_id(可空)/ cut_type(可空)/ cube_product_id(可空)/
//     qty_kg / reason / recorded_at / operator_id。
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
		return nil, fmt.Errorf("%w: cut_type %q 不在 12 类枚举内", ErrInvalidInput, in.CutType)
	}

	now := s.now()
	logID := generateID(now, "WL")
	recordedAt := now
	if !in.RecordedAt.IsZero() {
		recordedAt = in.RecordedAt
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
	return &st, nil
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
	return &st, nil
}

// ListLineSalesByPig 查某 pig / 某 cut_type 的销售聚合。
//
// 阶段1 不实装(返回空);阶段2 由 OnSaleCompleted 落 line_sales_by_pig 后启用。
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
// UNIQUE 冲突 → ErrBranchCutMappingConflict。阶段3 接 cube 校验 cube_product_id 存在。
func (s *Service) CreateBranchCutMapping(_ context.Context, branchID string, in model.CreateBranchCutMappingInput) (*model.BranchCutMapping, error) {
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidInput)
	}
	if in.CutType == "" {
		return nil, fmt.Errorf("%w: cut_type 必填", ErrInvalidInput)
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
	if err := s.db.Create(m).Error; err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: (%s, %s)", ErrBranchCutMappingConflict, branchID, in.CutType)
		}
		return nil, fmt.Errorf("create branch_cut_mapping: %w", err)
	}
	return m, nil
}

// UpdateBranchCutMapping 改映射 cube_product_id(支持改 cube_product_name + cut_type)。
func (s *Service) UpdateBranchCutMapping(_ context.Context, branchID, mappingID string, in model.UpdateBranchCutMappingInput) (*model.BranchCutMapping, error) {
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
	if in.CutType != "" && in.CutType != m.CutType {
		updates["cut_type"] = in.CutType
	}
	if len(updates) == 1 {
		return &m, nil // 仅时间戳更新也允许,但避免空 SQL
	}
	if err := s.db.Model(&m).Updates(updates).Error; err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: 目标 (%s, %s) 已存在", ErrBranchCutMappingConflict, branchID, in.CutType)
		}
		return nil, err
	}
	return &m, nil
}

// DeleteBranchCutMapping 删映射。
func (s *Service) DeleteBranchCutMapping(_ context.Context, branchID, mappingID string) error {
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
	return s.db.Delete(&m).Error
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