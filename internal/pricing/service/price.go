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
// # 本地优先、cube 回落(2026-10-21)
//
// 门店 10 是**自营店,没有 cube 源** ⇒ 它的价格必须落在本地表。
// 门店 00 / 01 仍然有 cube 源 ⇒ 过渡期继续走 cube。
//
// ⇒ 判据是**"该门店本地有没有价格数据"**,不是"这个商品本地有没有价":
//
//	本地有该门店的数据 ⇒ 走本地(某个商品查不到 = 数据缺口,显性暴露)
//	本地没有该门店的数据 ⇒ 回落 cube
//
// 为什么不用"按商品判":门店 10 若某个商品没导到价,
// 按商品判会回落 cube —— 而门店 10 的 cube 路径必然 503,
// 于是"数据缺口"被翻译成"库存服务不可用",排查方向完全跑偏。
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
	// SourceLocal = 结论来自本地表(门店自营)。
	SourceLocal = "local"
	// SourceCube = 结论回落到 cube(过渡期:门店有 cube 源但本地无数据)。
	SourceCube = "cube"
)

// PriceService 组合"零售价 + 当前生效特价"。
type PriceService struct {
	cube cubeclient.Client
	// prices / promos 是本地表服务;nil = 本服务没有本地表(纯 cube 回落)。
	prices *PriceListService
	promos *PromotionService
	// now 注入时钟,单测固定时间用;nil = time.Now。
	now func() time.Time
}

// NewPriceService 构造 PriceService。cube 为 nil 时所有查询返 ErrNoCube。
func NewPriceService(cube cubeclient.Client) *PriceService {
	return &PriceService{cube: cube}
}

// WithStore 挂上本地表服务(启用"本地优先、cube 回落")。
//
// ⚠️ 不挂也能跑(纯 cube 回落)—— 但那样自营门店永远拿不到价。
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
	// 存在的意义:门店 10 的价是本地的、门店 00 的价是回落的,
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

	// ① 本地优先。
	local, err := s.localQuote(ctx, branchID, bc, day)
	if err != nil {
		return nil, err
	}
	if local.HasLocalData {
		q.Source = SourceLocal
		if local.Price != nil {
			p := local.Price.PriceYuan.String()
			q.RetailPrice = &p
		}
		if local.Promo != nil {
			q.Promo = promoFromModel(local.Promo)
		}
		return q, nil
	}

	// ② 本地没有该门店的数据 ⇒ 回落 cube(过渡期:门店 00 / 01)。
	if s.cube == nil {
		return nil, ErrNoCube
	}
	q.Source = SourceCube

	// 零售价来自商品档案。产品不存在时**不**整个失败 ——
	// 没有商品档案就没有零售价,但促销可能仍然查得到(促销表独立存在)。
	// 反过来做会把"档案缺失"变成"整条价格查询 404",而调用方真正想问的是促销。
	if p, err := s.cube.GetProduct(ctx, bc); err == nil && p != nil {
		if p.PriceYuan != nil {
			f := p.PriceYuan.String()
			q.RetailPrice = &f
		}
	} else if err != nil && !errors.Is(err, cubeclient.ErrProductNotFound) {
		// 只有**链路故障**才上抛。"商品不存在"在这里不是错误。
		return nil, err
	}

	if promo, err := s.cube.GetActivePromotion(ctx, bc, day); err != nil {
		return nil, err
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
	return q, nil
}

// localQuote 是"本地表的查价结论"。
type localQuote struct {
	// Price 是生效售价;nil = 该商品此刻没有价。
	Price *model.PriceList
	// Promo 是当前生效促销;nil = 没有。
	Promo *model.Promotion
	// HasLocalData 表示**该门店本地有价格数据** ⇒ 不回落 cube。
	HasLocalData bool
}

// localQuote 查本地表。
//
// ⚠️ HasAnyPrices 只在"什么都没查到"时才问 ——
//
//	查到了行就说明该门店有本地数据,不必再多打一次 COUNT。
func (s *PriceService) localQuote(ctx context.Context, branchID, productID, day string) (localQuote, error) {
	var out localQuote
	if branchID == "" || s.prices == nil || s.promos == nil {
		return out, nil
	}
	at, err := time.ParseInLocation("2006-01-02", day, time.Local)
	if err != nil {
		// 基准日解析不出来 ⇒ 当作"本地没有",让调用方回落 cube。
		return out, nil
	}
	price, err := s.prices.ActivePrice(ctx, branchID, "", productID, at)
	if err != nil {
		return out, err
	}
	out.Price = price
	promo, err := s.promos.ActivePromotion(ctx, branchID, productID, at)
	if err != nil {
		return out, err
	}
	out.Promo = promo
	if price != nil || promo != nil {
		out.HasLocalData = true
		return out, nil
	}
	has, err := s.prices.HasAnyPrices(ctx, branchID)
	if err != nil {
		return out, err
	}
	out.HasLocalData = has
	return out, nil
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
