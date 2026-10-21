// Package service 实现 pricing 域的业务规则。
//
// # 业务边界(dapr 微服务的第一原则)
//
// pricing 域只回答"**这个商品现在该印什么价**":
//
//	零售价  ← 本地 price_lists(门店自营)/ cube product.avg_price_yuan(过渡期)
//	特价    ← 本地 promotions(门店自营)/ cube promotion(过渡期)
//
// 它**不拥有**商品档案 —— barcode / 品名 / 单位归 catalog。
// 所以这里返回的 DTO 只有价格与促销信息,商品身份字段(barcode)只是入参回显。
//
// # 为什么促销要在这里而不是 catalog
//
// 商品档案是低频数据(每天约 10 行变更),促销是**分钟级时效**数据
// (促销单随时录入/停用,价签要印的是"按下打印那一刻"的价)。
// 两者放一个服务会让"促销挂了"变成"整个商品档案查询挂",
// 而它们本可以各自独立伸缩与降级。
//
// # 读路径:「配没配数据源」(2026-10-21)
//
//	门店配了数据源(富百家 00 / 佳乐 01)⇒ 走 cube(该门店的**实时**价与促销)
//	没配数据源(自营店 10)             ⇒ 走本域 PG(price_lists / promotions)
//
// ⇒ 判据是**"这个门店配了数据源吗"**,不是"这个门店本地有没有价格数据"。
//
// ⚠️ 旧判据("本地有没有数据")在生产上给出了**两天前的快照价**:
//
//	门店 00 的价格已被主同步实例(00→00)灌进本地 PG(27565 行,快照 2026-10-20 19:24)
//	⇒ 查价命中本地 ⇒ 返回快照价,而不是思迅的当前价。
//
// ⇒ 两个真相 = 两个错法。判据统一到"配没配数据源"
// (与 member / catalog / inventory 同一套,见 internal/cubesource)。
//
// # 金额:字符串出入,内部 decimal
//
// JSON number 到了 Dart 侧会被 jsonDecode 变成 double,精度当场丢掉
// (9.90 → 9.899999999999999)。⇒ 对外一律**字符串**。
// (tradewind_app 的 `_toDouble` 已同时接受 num 与 String,故不破坏现有消费方。)
package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/pricing/model"
)

// ErrInvalidBarcode 表示入参条码不合法。
//
// 判定规则与 cubeclient.SearchProductsByBarcode 一致(< 5 位返空),
// 放在这一层是为了让"输入了个 3 位数"得到一个**明确的 400**,
// 而不是变成一次注定查不到结果的查询。
var ErrInvalidBarcode = errors.New("pricing: 条码长度不足(至少 5 位)")

// ErrNoCube 表示 pricing 没有配置 cube client。
var ErrNoCube = errors.New("pricing: cube client 未配置")

// 价格结论的来源。
const (
	// SourceLocal = 结论来自本地表(门店没配数据源 ⇒ 数据在本域 PG)。
	SourceLocal = "local"
	// SourceCube = 结论来自 cube(门店配了数据源 ⇒ 实时价)。
	SourceCube = "cube"
)

// PriceService 组合"零售价 + 当前生效特价"。
type PriceService struct {
	cube cubeclient.Client
	// prices / promos 是本地表服务;nil = 本服务没有本地表(纯 cube 模式)。
	prices *PriceListService
	promos *PromotionService
	// sources 解析"这个门店配了哪个数据源";nil = 未装配(按"没配"处理)。
	//
	// ⚠️ 判据的**唯一**来源。装配见 SetCubeSource。
	sources SourceResolver
	// reader 是 cube 的分页读端(导出用);nil = 未装配。
	reader CubeQueryReader
	// now 注入时钟,单测固定时间用;nil = time.Now。
	now func() time.Time
}

// NewPriceService 构造 PriceService。cube 为 nil 时所有查询返 ErrNoCube。
func NewPriceService(cube cubeclient.Client) *PriceService {
	return &PriceService{cube: cube}
}

// WithStore 挂上本地表服务(没配数据源的门店从这里取数)。
//
// ⚠️ 不挂也能跑(纯 cube 模式)—— 但那样自营门店永远拿不到价。
func (s *PriceService) WithStore(prices *PriceListService, promos *PromotionService) *PriceService {
	if s == nil {
		return nil
	}
	s.prices = prices
	s.promos = promos
	return s
}

// SetClock 注入时钟(测试用)。
func (s *PriceService) SetClock(fn func() time.Time) {
	if fn != nil {
		s.now = fn
	}
}

func (s *PriceService) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Quote 是"某个条码当前的价格结论"。
//
// RetailPrice 与 Promo.Price 的关系(与前端 POP 渲染的取值顺序一致):
//
//	无 Promo  → 印 PromoPrice 之外的值,标签显示「零售价」
//	有 Promo  → 印 PromoPrice,RetailPrice 以划线原价显示
type Quote struct {
	// Barcode 回显入参,便于前端把响应与列表行对上。
	Barcode string `json:"barcode"`

	// RetailPrice 是当前售价(元,**字符串**)。
	//
	// ⚠️ nil = 该商品**没维护售价**,与"售价 0 元"是两件事。
	// 前端据此显示「未维护售价」而不是印 ¥0.00 —— 后者会被当成真价贴出去。
	RetailPrice *string `json:"retail_price,omitempty"`

	// Promo 是当前生效促销;nil = 没有生效促销(绝大多数商品的常态)。
	Promo *ActivePromo `json:"promo,omitempty"`

	// AsOf 是本次结论的基准日(ISO yyyy-mm-dd)。
	AsOf string `json:"as_of"`

	// Source 是这个结论的来源:"local" / "cube"。
	//
	// 存在的意义:门店 10 的价是本地的、门店 00 的价是 cube 实时取的,
	// 现场问"这个价哪来的"时不必去翻日志。
	Source string `json:"source"`
}

// ActivePromo 是已确认"当前生效"的促销(已按基准日过滤过)。
type ActivePromo struct {
	// Price 是**促销价**(元,字符串)。
	Price string `json:"price"`
	// OrigPrice 是**原价**(元,字符串);思迅源列为 NULL 时不返,前端回退到 RetailPrice。
	OrigPrice *string `json:"orig_price,omitempty"`
	// PlanNo 促销单号 —— 印在价签上用于人工追溯"这个价哪来的"。
	PlanNo    string `json:"plan_no"`
	PlanName  string `json:"plan_name,omitempty"`
	BeginDay  string `json:"begin_day"`
	EndDay    string `json:"end_day"`
	DateLabel string `json:"date_label,omitempty"`
}

// DefaultPromoDateLabels 是价签上"活动时间"那一行的可选字段名。
//
// 取值与思迅/门店实际叫法对齐,前端在切特价弹窗里作为下拉选项。
// 前端是**唯一**渲染方,这里只保证取值集合稳定 —— 新增枚举必须同时
// 更新 Flutter 侧 PopDateLabel,否则前端会回退到「特价日期」。
var DefaultPromoDateLabels = []string{"特价日期", "店特日期", "活动日期"}

// Quote 查询一个条码的当前价格结论。
//
// branchID 是**门店**;空 = 不查本地表(纯 cube)。
// onDate 留空 = 今天。传非法日期格式时按今天处理(不报错)——
// 判定基准日本身是内部概念,不该因为格式问题让整次打印失败。
//
// # ⚠️ 走哪条路由「配没配数据源」决定(不是"本地有没有数据")
//
//	配了数据源 ⇒ cube(实时价);没配 ⇒ 本域 PG。
//
// ⚠️ 解析失败(问不到路由)⇒ **503**,不回落本地:
// 回落会把一次链路故障显示成「这个商品没维护售价」,
// 而真相是"路由问不到"。两个真相 = 两个错法。
func (s *PriceService) Quote(ctx context.Context, branchID, barcode, onDate string) (*Quote, error) {
	bc := strings.TrimSpace(barcode)
	if len(bc) < 5 {
		return nil, ErrInvalidBarcode
	}
	day := onDate
	if !isISODate(day) {
		day = s.clock().Format("2006-01-02")
	}

	q := &Quote{Barcode: bc, AsOf: day}

	// ① 判据:这个门店配了数据源吗?
	_, useCube, err := s.readPath(ctx, branchID)
	if err != nil {
		return nil, err
	}

	// ② 有源 ⇒ cube(实时价 + 当前生效促销)。
	if useCube {
		return q, s.fillFromCube(ctx, q, bc, day)
	}

	// ③ 没源 ⇒ 本域 PG。
	return q, s.fillFromLocal(ctx, q, branchID, bc, day)
}

// fillFromCube 用 cube 的实时数据填结论。
//
// ⚠️ 零售价来自商品档案。产品不存在时**不**整个失败 ——
// 没有商品档案就没有零售价,但促销可能仍然查得到(促销表独立存在)。
// 反过来做会把"档案缺失"变成"整条价格查询 404",而调用方真正想问的是促销。
func (s *PriceService) fillFromCube(ctx context.Context, q *Quote, barcode, day string) error {
	if s.cube == nil {
		return ErrNoCube
	}
	q.Source = SourceCube

	if p, err := s.cube.GetProduct(ctx, barcode); err == nil && p != nil {
		if p.PriceYuan != nil {
			f := p.PriceYuan.String()
			q.RetailPrice = &f
		}
	} else if err != nil && !errors.Is(err, cubeclient.ErrProductNotFound) {
		// 只有**链路故障**才上抛。"商品不存在"在这里不是错误。
		return err
	}

	if promo, err := s.cube.GetActivePromotion(ctx, barcode, day); err != nil {
		return err
	} else if promo != nil {
		ap := &ActivePromo{
			Price:     promo.Price.String(),
			PlanNo:    promo.PlanNo,
			PlanName:  promo.PlanName,
			BeginDay:  promo.BeginDay,
			EndDay:    promo.EndDay,
			DateLabel: DefaultPromoDateLabels[0],
		}
		if promo.OrigPrice != nil {
			f := promo.OrigPrice.String()
			ap.OrigPrice = &f
		}
		q.Promo = ap
	}
	return nil
}

// fillFromLocal 用本域 PG 的数据填结论(没配数据源的门店走这里)。
//
// ⚠️ **不再问"该门店本地有没有价格数据"**:
// 那个判据在生产上让门店 00 返回了两天前的快照价(见包注释)。
//
// ⚠️ 本地**没维护价** ⇒ retail_price 缺省(nil),不是 "0":
// 前端据此显示「未维护售价」而不是印 ¥0.00 —— 后者会被当成真价贴出去。
func (s *PriceService) fillFromLocal(ctx context.Context, q *Quote, branchID, productID, day string) error {
	if s.prices == nil || s.promos == nil {
		// 本地表没装配 ⇒ 没法给本域结论(readPath 已把这种情况转成 cube,
		// 走到这里说明装配有其它问题)。报"未装配"而不是"没维护价"。
		return model.ErrLocalStoreNotReady
	}
	q.Source = SourceLocal

	at := dayStart(s.clock(), day)
	price, err := s.prices.ActivePrice(ctx, branchID, "", productID, at)
	if err != nil {
		return err
	}
	if price != nil {
		p := price.PriceYuan.String()
		q.RetailPrice = &p
	}
	promo, err := s.promos.ActivePromotion(ctx, branchID, productID, at)
	if err != nil {
		return err
	}
	if promo != nil {
		q.Promo = promoFromModel(promo)
	}
	return nil
}

// promoFromModel 把本地促销行转成对外 DTO。
func promoFromModel(p *model.Promotion) *ActivePromo {
	if p == nil {
		return nil
	}
	ap := &ActivePromo{
		Price:     p.PriceYuan.String(),
		PlanNo:    p.ID,
		PlanName:  p.PlanName,
		BeginDay:  p.BeginAt.Format("2006-01-02"),
		EndDay:    p.EndAt.Format("2006-01-02"),
		DateLabel: DefaultPromoDateLabels[0],
	}
	if !p.OrigPriceYuan.IsZero() {
		f := p.OrigPriceYuan.String()
		ap.OrigPrice = &f
	}
	return ap
}

// isISODate 判定 yyyy-mm-dd。
//
// 这里**不用** time.Parse:它对 "2026-13-45" 之类的非法日期会报错,
// 而我们要的只是"看起来像不像 ISO 日期" —— 真不合法交给下游源库去报错,
// 本地不必因为基准日格式问题拒绝服务。
func isISODate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, c := range s {
		if i == 4 || i == 7 {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// dayStart 把 "yyyy-mm-dd" 解析成那天的零点(本地时区)。
//
// ⚠️ 解析不出来(例如 "2026-13-45" 过了格式检查但不是真日期)⇒ 回落到今天。
// 基准日只是**判定口径**,不该让整次查价失败。
func dayStart(now time.Time, day string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", day, time.Local)
	if err != nil {
		return startOfDay(now)
	}
	return t
}
