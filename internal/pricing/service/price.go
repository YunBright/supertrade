// Package service 实现 pricing 域的业务规则。
//
// # 业务边界(dapr 微服务的第一原则)
//
// pricing 域只回答"**这个商品现在该印什么价**":
//
//	零售价  ← cube product.avg_price_yuan(商品档案)
//	特价    ← cube promotion(思迅 t_pub_plan_flow,当前生效)
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
// # 为什么不加本地缓存
//
// 加缓存就必须在"促销变更"时有失效通道,而当前促销变更发生在**思迅 ERP 外部**
// (门店在思迅里录促销单),本系统收不到任何通知。
// 与其缓存一份"不知道什么时候过期的价",不如每次查源库 ——
// cube promotion 是 live model,查询本身就是透传,不存在快照过期问题。
package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/cubeclient"
)

// ErrInvalidBarcode 表示入参条码不合法。
//
// 判定规则与 cubeclient.SearchProductsByBarcode 一致(< 5 位返空),
// 放在这一层是为了让"输入了个 3 位数"得到一个**明确的 400**,
// 而不是变成一次注定查不到结果的查询。
var ErrInvalidBarcode = errors.New("pricing: 条码长度不足(至少 5 位)")

// PriceService 组合"零售价 + 当前生效特价"。
type PriceService struct {
	cube cubeclient.Client
	// now 注入时钟,单测固定时间用;nil = time.Now。
	now func() time.Time
}

// NewPriceService 构造 PriceService。cube 为 nil 时所有查询返 ErrNoCube。
func NewPriceService(cube cubeclient.Client) *PriceService {
	return &PriceService{cube: cube}
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

// ErrNoCube 表示 pricing 没有配置 cube client。
var ErrNoCube = errors.New("pricing: cube client 未配置")

// Quote 是"某个条码当前的价格结论"。
//
// RetailPrice 与 PromoPrice 的关系(与前端 POP 渲染的取值顺序一致):
//
//	无 Promo  → 印 PromoPrice 之外的值,标签显示「零售价」
//	有 Promo  → 印 PromoPrice,RetailPrice 以划线原价显示
type Quote struct {
	// Barcode 回显入参,便于前端把响应与列表行对上。
	Barcode string `json:"barcode"`

	// RetailPrice 是商品档案里的当前售价(元)。
	//
	// ⚠️ nil = 该商品**没维护售价**(cube 没给),与"售价 0 元"是两件事。
	// 前端据此显示「未维护售价」而不是印 ¥0.00 —— 后者会被当成真价贴出去。
	RetailPrice *float64 `json:"retail_price,omitempty"`

	// Promo 是当前生效促销;nil = 没有生效促销(绝大多数商品的常态)。
	Promo *ActivePromo `json:"promo,omitempty"`

	// AsOf 是本次结论的基准日(ISO yyyy-mm-dd)。
	AsOf string `json:"as_of"`
}

// ActivePromo 是已确认"当前生效"的促销(已按基准日过滤过)。
type ActivePromo struct {
	// Price 是**促销价**(元)。
	Price float64 `json:"price"`
	// OrigPrice 是**原价**(元);思迅源列为 NULL 时不返,前端回退到 RetailPrice。
	OrigPrice *float64 `json:"orig_price,omitempty"`
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
// onDate 留空 = 今天。传非法日期格式时按今天处理(不报错)——
// 判定基准日本身是内部概念,不该因为格式问题让整次打印失败。
func (s *PriceService) Quote(ctx context.Context, barcode string, onDate string) (*Quote, error) {
	bc := strings.TrimSpace(barcode)
	if len(bc) < 5 {
		return nil, ErrInvalidBarcode
	}
	if s.cube == nil {
		return nil, ErrNoCube
	}
	day := onDate
	if !isISODate(day) {
		day = s.clock().Format("2006-01-02")
	}

	q := &Quote{Barcode: bc, AsOf: day}

	// 零售价来自商品档案。产品不存在时**不**整个失败 ——
	// 没有商品档案就没有零售价,但促销可能仍然查得到(促销表独立存在)。
	// 反过来做会把"档案缺失"变成"整条价格查询 404",而调用方真正想问的是促销。
	if p, err := s.cube.GetProduct(ctx, bc); err == nil && p != nil {
		if p.PriceYuan != nil {
			f := p.PriceYuan.InexactFloat64()
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
			Price:     promo.Price.InexactFloat64(),
			PlanNo:    promo.PlanNo,
			PlanName:  promo.PlanName,
			BeginDay:  promo.BeginDay,
			EndDay:    promo.EndDay,
			DateLabel: DefaultPromoDateLabels[0],
		}
		if promo.OrigPrice != nil {
			f := promo.OrigPrice.InexactFloat64()
			ap.OrigPrice = &f
		}
		q.Promo = ap
	}
	return q, nil
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
