// Package service / promotion_plan.go —— 促销**方案头**(图4)的读服务。
//
// # 为什么单独一个 service
//
// pricing 域已有两个 service:
//   - PriceService   查价 / 促销**行**(某商品当前能买到的最低价)
//   - PromotionService 本域 promotions 表的读写
//
// 图4 要的是**方案头**(方案号 / 名称 / 摘要 / 审核 / 终止),与两者都不是一回事。
// ⇒ 单独一个 service,免得把"查一个商品的价"和"查一份方案单"混在一个对象里。
//
// # 本轮**只读**
//
// PLAN §3.3 明写「只读(列表 + 详情)」⇒ 这里**不实现**任何写端点。
//
// # 数据来源按「门店配没配数据源」分流(判据唯一:internal/cubesource)
//
//	配了源 ⇒ 走 cube(promotion_plan model,源表 t_pub_plan_master)
//	没配源 ⇒ 走本域 PG(promotion_plans 表)
//	问不到 / 被停用 ⇒ **503**,不回落本地。
//
// # ⚠️ approve_flag **必须**翻译,不许透传源码
//
// 源列 `approve_flag` 是 varchar(1),实测取值 `2` = 已审核。
//
//	把源码透给前端 ⇒ 前端要自己知道 "2" 是什么意思,
//	而这个知识散落在每个客户端里 ⇒ 迟早有一处写成 "2" = 未审核,
//	症状是"列表显示未审核、点进去显示已审核",且没有任何报错。
//
// ⇒ 翻译规则写死在 [approveStatusOf] 一处,译文只有四个取值:
//
//	draft    未审核
//	approved 已审核
//	stopped  已终止
//	unknown  源库给了看不懂的值(不猜)
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/cuberead"
	"github.com/YunBright/supertrade/internal/cubesource"
	"github.com/YunBright/supertrade/internal/pricing/model"
)

// PromotionPlanQuery 是促销方案列表的查询条件。
type PromotionPlanQuery struct {
	// From / To 是**制单日期**区间(YYYY-MM-DD,闭区间)。空 = 不过滤。
	From string
	To   string
	// PlanNo 是方案编号(精确)。空 = 不过滤。
	PlanNo string
	// PlanName 是方案名称(子串,大小写无关)。空 = 不过滤。
	PlanName string
	// ApproveStatus 是审核状态(draft / approved / stopped / unknown)。空 = 不过滤。
	ApproveStatus string
	// CreatedBy 是制单人(精确)。空 = 不过滤。
	CreatedBy string
	// MemberLevel 是会员等级(思迅 vip_type,精确)。空 = 不过滤。
	//
	// ⚠️ **实测两个源库的取值恒为"无"**(思迅促销方案表不区分会员等级)
	// ⇒ 过滤它几乎不会改变结果 —— 但**参数必须真的落到过滤里**:
	// 前端已经在发它,后端却静默忽略 ⇒ 用户点了筛选、列表没变,**没有任何提示**。
	// ⇒ 与"合同外的参数"不同:这个参数两库都有(cube 的 promotion_plan.member_level),
	// 所以能实现,就实现(实现不了也要显式说,不能静默吞)。
	MemberLevel string
	// ExcludeExpired 为 true 时剔除"已过结束日期"的方案。
	//
	// ⚠️ 默认 true:图4 的默认视图是"当前有效的方案",
	// 而促销方案是时效数据 —— 2021 年的方案和今天的方案混在一张列表里没法用。
	ExcludeExpired bool
	// Sort 是排序字段(白名单,见 promotionPlanSortColumns)。空 = 默认制单日期。
	//
	// ⚠️ 非法字段 ⇒ **400**(不是静默回落):静默回落 = 用户点了排序、
	// 列表没变、零提示,与"参数被忽略"无法区分。
	Sort string
	// Order 是排序方向(asc / desc)。空 = 默认 desc(倒序)。
	Order string
	// Page 从 1 开始;PageSize 是每页条数。
	Page     int
	PageSize int
}

// normalized 把越界的分页参数拉回合法区间(兜底,不是校验)。
func (q PromotionPlanQuery) normalized() PromotionPlanQuery {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = 50
	}
	if q.PageSize > 200 {
		q.PageSize = 200
	}
	return q
}

// PromotionPlanItem 是促销方案的一行(PLAN §3.3 的响应项)。
//
// ⚠️ 字段名是**冻结契约**(前端按它解析)⇒ 不许改名。
type PromotionPlanItem struct {
	// PlanNo 是方案编号(思迅 plan_no)。
	PlanNo string `json:"plan_no"`
	// PlanName 是方案名称。
	PlanName string `json:"plan_name"`
	// PlanMemo 是方案摘要。
	PlanMemo string `json:"plan_memo"`
	// BeginDate / EndDate 是方案起止日期(YYYY-MM-DD)。
	BeginDate string `json:"begin_date"`
	EndDate   string `json:"end_date"`
	// CreatedAt 是制单时刻(RFC3339)。
	CreatedAt string `json:"created_at"`
	// CreatedBy 是制单人。
	CreatedBy string `json:"created_by"`
	// ApproveStatus 是**翻译后**的审核状态(draft / approved / stopped / unknown)。
	ApproveStatus string `json:"approve_status"`
	// ApproveStatusLabel 是审核状态的中文标签(前端不硬编码文案)。
	ApproveStatusLabel string `json:"approve_status_label"`
	// ApprovedAt 是审核时刻(RFC3339);没审核过 ⇒ null。
	ApprovedAt *string `json:"approved_at"`
	// ApprovedBy 是审核人。
	ApprovedBy string `json:"approved_by"`
	// MemberLevel 是会员等级(思迅 vip_type,实测恒 "无")。
	MemberLevel string `json:"member_level"`
	// StoppedBy 是终止人。
	StoppedBy string `json:"stopped_by"`
	// StoppedAt 是终止时刻(RFC3339);没终止过 ⇒ null。
	StoppedAt *string `json:"stopped_at"`
}

// PromotionPlanPage 是促销方案列表的响应。
//
// ⚠️ **信封形状是本仓自定的**(PLAN §3.3 只给了"列表行",没给信封):
// 与商品档案(§3.1)保持同一套 —— items / count / page / page_size / total。
type PromotionPlanPage struct {
	Items []PromotionPlanItem `json:"items"`
	// Count 是**本页**条数。
	Count int `json:"count"`
	// Page / PageSize 回显请求值。
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
	// Total 是过滤后的总条数。
	Total int `json:"total"`
	// DataSource 是 "cube" / "local"。
	//
	// ⚠️ 这是 PLAN §3.3 之外的附加字段(理由同商品档案:
	// 没有它,调用方无法区分"这家店没维护方案"与"这条路查的是别家店")。
	DataSource string `json:"data_source,omitempty"`
}

// ErrPromotionPlanNotFound 方案不存在。
var ErrPromotionPlanNotFound = errors.New("pricing: 促销方案不存在")

// PromotionPlanService 是促销方案头的读服务。
type PromotionPlanService struct {
	// db 是本域 PG(没配数据源的门店走本地时用)。
	db *gorm.DB
	// sources 解析"这个门店配了哪个数据源"(见 source.go)。
	sources SourceResolver
	// reader 是 cube 的通用读端(model + 维度 + 过滤 + 分页)。
	reader CubeQueryReader
	logger *slog.Logger
}

// NewPromotionPlanService 构造 PromotionPlanService。
func NewPromotionPlanService(db *gorm.DB, logger *slog.Logger) *PromotionPlanService {
	if logger == nil {
		logger = slog.Default()
	}
	return &PromotionPlanService{db: db, logger: logger}
}

// SetCubeSource 注入数据源解析器与 cube 读端。
//
// ⚠️ **两个都注入**才走 cube 路径;只注入一个 ⇒ 有数据源的门店仍走本地,
// 症状与"没接数据源"完全一样(查到的是旧数据),且**没有任何报错**。
func (s *PromotionPlanService) SetCubeSource(resolver SourceResolver, reader CubeQueryReader) {
	if s == nil {
		return
	}
	s.sources = resolver
	s.reader = reader
}

// readPath 判定该门店的促销方案该走哪条路。
//
// ⚠️ 问不到路由 / 数据源被停用 ⇒ **503**,不回落本地
// (回落 = 把一次链路故障显示成"这家店没有促销方案")。
func (s *PromotionPlanService) readPath(ctx context.Context, branchID string) (bool, error) {
	useCube, err := cubesource.Decide(ctx, s.sources, branchID)
	if err == nil {
		if useCube && s.reader == nil {
			// 配了源却没装读端 ⇒ 装配错误,大声失败(不能回落本地)。
			return false, fmt.Errorf("%w: pricing 缺少 cube 读端", model.ErrCubeSourceUnavailable)
		}
		return useCube, nil
	}
	switch {
	case errors.Is(err, cubesource.ErrSourceDisabled):
		return false, fmt.Errorf("%w: 门店 %s 的数据源已停用",
			model.ErrCubeSourceDisabled, strings.TrimSpace(branchID))
	case errors.Is(err, cubesource.ErrSourceUnavailable):
		return false, fmt.Errorf("%w: %v", model.ErrCubeSourceUnavailable, err)
	default:
		// 权限类错误原样上抛(403 / 401)。
		return false, err
	}
}

// promotionPlanDims 是 cube `promotion_plan` model 的维度。
//
// ⚠️ 契约假设(cube 仓并行实现中):成员名取思迅 canonical 名
// (PLAN §1.3 的表),金额/日期不在这里(方案头没有金额维度)。
func promotionPlanDims() []string {
	return []string{
		"promotion_plan.plan_no", "promotion_plan.plan_name", "promotion_plan.plan_memo",
		"promotion_plan.begin_date", "promotion_plan.end_date",
		"promotion_plan.created_at", "promotion_plan.created_by",
		"promotion_plan.approved_at", "promotion_plan.approved_by",
		"promotion_plan.stopped_at", "promotion_plan.stopped_by",
		"promotion_plan.approve_flag", "promotion_plan.member_level",
	}
}

// List 分页查促销方案。
func (s *PromotionPlanService) List(ctx context.Context, branchID string, q PromotionPlanQuery) (*PromotionPlanPage, error) {
	useCube, err := s.readPath(ctx, branchID)
	if err != nil {
		return nil, err
	}
	q = q.normalized()
	// ⚠️ 排序参数**先校验后使用**:非法字段静默回落成默认排序 =
	// "用户点了按方案名称排、列表还是按日期排",零提示。
	sortCol, err := promotionPlanSortColumnOf(q.Sort)
	if err != nil {
		return nil, err
	}
	dir, err := promotionPlanSortDirection(q.Order)
	if err != nil {
		return nil, err
	}
	if useCube {
		return s.cubeList(ctx, q, sortCol, dir)
	}
	return s.localList(ctx, branchID, q, sortCol, dir)
}

// cubeList 走 cube 的促销方案列表。
func (s *PromotionPlanService) cubeList(ctx context.Context, q PromotionPlanQuery, sortCol promotionPlanSortColumn, dir string) (*PromotionPlanPage, error) {
	cq := cubeclient.CubeQuery{
		Measures:   []string{"promotion_plan.count"},
		Dimensions: promotionPlanDims(),
		// ⚠️ 排序成员必须**同时在 SELECT 里**(cube 的硬约束)——
		// sortCol.CubeDim 一定在 promotionPlanDims() 里,否则 cube 返 400。
		Order:  promotionPlanCubeOrder(sortCol, dir),
		Limit:  q.PageSize,
		Offset: (q.Page - 1) * q.PageSize,
		Total:  true,
	}
	cq.Filters = append(cq.Filters, promotionPlanFilters(q)...)
	rows, total, err := s.reader.LoadCubeQueryPage(ctx, "promotion_plan", cq)
	if err != nil {
		return nil, err
	}
	items := make([]PromotionPlanItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, promotionPlanItemOf(r))
	}
	return &PromotionPlanPage{
		Items:      items,
		Count:      len(items),
		Page:       q.Page,
		PageSize:   q.PageSize,
		Total:      total,
		DataSource: "cube",
	}, nil
}

// promotionPlanFilters 构造促销方案的过滤条件。
//
// ⚠️ **exclude_expired 打在 end_date 上**(不是 created_at):
// 促销方案是时效数据,"过期"指的是**结束日期已过**,不是制单日期早。
func promotionPlanFilters(q PromotionPlanQuery) []cubeclient.CubeFilter {
	var out []cubeclient.CubeFilter
	if q.From != "" {
		out = append(out, cuberead.Filter("promotion_plan.created_at",
			"gte", q.From+" 00:00:00"))
	}
	if q.To != "" {
		out = append(out, cuberead.Filter("promotion_plan.created_at",
			"lte", q.To+" 23:59:59"))
	}
	if q.PlanNo != "" {
		out = append(out, cuberead.Filter("promotion_plan.plan_no",
			cubeclient.OperatorEquals, q.PlanNo))
	}
	if q.PlanName != "" {
		out = append(out, cuberead.Filter("promotion_plan.plan_name",
			cubeclient.OperatorContainsCI, strings.TrimSpace(q.PlanName)))
	}
	if q.ApproveStatus != "" {
		// ⚠️ 状态是**翻译后**的值,而 cube 侧存的是源码 approve_flag ⇒
		// 这里只能按源码集合过滤(翻译规则见 approveStatusOf)。
		out = append(out, cuberead.Filter("promotion_plan.approve_flag",
			cubeclient.OperatorIn, toAnySlice(approveFlagsFor(q.ApproveStatus))...))
	}
	if q.CreatedBy != "" {
		out = append(out, cuberead.Filter("promotion_plan.created_by",
			cubeclient.OperatorEquals, q.CreatedBy))
	}
	if q.MemberLevel != "" {
		// ⚠️ 会员等级是**源值**(思迅 vip_type),两库都映射了 ⇒ 可以直接等值过滤。
		out = append(out, cuberead.Filter("promotion_plan.member_level",
			cubeclient.OperatorEquals, q.MemberLevel))
	}
	if q.ExcludeExpired {
		out = append(out, cuberead.Filter("promotion_plan.end_date",
			"gte", time.Now().Format("2006-01-02")))
	}
	return out
}

// localList 走本域 PG 的促销方案列表。
func (s *PromotionPlanService) localList(ctx context.Context, branchID string, q PromotionPlanQuery, sortCol promotionPlanSortColumn, dir string) (*PromotionPlanPage, error) {
	if s.db == nil {
		return nil, model.ErrLocalStoreNotReady
	}
	tx := s.db.WithContext(ctx).Model(&model.PromotionPlan{}).Where("branch_id = ?", branchID)
	if q.From != "" {
		tx = tx.Where("created_at >= ?", q.From+" 00:00:00")
	}
	if q.To != "" {
		tx = tx.Where("created_at <= ?", q.To+" 23:59:59")
	}
	if q.PlanNo != "" {
		tx = tx.Where("plan_no = ?", q.PlanNo)
	}
	if q.PlanName != "" {
		tx = tx.Where("LOWER(plan_name) LIKE ?", "%"+strings.ToLower(strings.TrimSpace(q.PlanName))+"%")
	}
	if q.ApproveStatus != "" {
		tx = tx.Where("approve_status = ?", q.ApproveStatus)
	}
	if q.CreatedBy != "" {
		tx = tx.Where("created_by = ?", q.CreatedBy)
	}
	if q.MemberLevel != "" {
		tx = tx.Where("member_level = ?", q.MemberLevel)
	}
	if q.ExcludeExpired {
		tx = tx.Where("end_date IS NULL OR end_date >= ?", time.Now().Format("2006-01-02"))
	}
	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, err
	}
	var rows []model.PromotionPlan
	// ⚠️ 排序走白名单表达式(列名来自 promotionPlanSortColumns,不接受用户原文)。
	if err := tx.Order(promotionPlanLocalOrderExpr(sortCol.LocalColumn, dir)).
		Offset((q.Page - 1) * q.PageSize).
		Limit(q.PageSize).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]PromotionPlanItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, promotionPlanItemOfLocal(r))
	}
	return &PromotionPlanPage{
		Items:      items,
		Count:      len(items),
		Page:       q.Page,
		PageSize:   q.PageSize,
		Total:      int(total),
		DataSource: "local",
	}, nil
}

// Get 查单个促销方案(按方案编号)。
func (s *PromotionPlanService) Get(ctx context.Context, branchID, planNo string) (*PromotionPlanItem, error) {
	useCube, err := s.readPath(ctx, branchID)
	if err != nil {
		return nil, err
	}
	planNo = strings.TrimSpace(planNo)
	if planNo == "" {
		return nil, model.ErrInvalidInput
	}
	if useCube {
		rows, _, err := s.reader.LoadCubeQueryPage(ctx, "promotion_plan", cubeclient.CubeQuery{
			Measures:   []string{"promotion_plan.count"},
			Dimensions: promotionPlanDims(),
			Filters: []cubeclient.CubeFilter{
				cuberead.Filter("promotion_plan.plan_no", cubeclient.OperatorEquals, planNo),
			},
			Limit: 2,
		})
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, ErrPromotionPlanNotFound
		}
		item := promotionPlanItemOf(rows[0])
		return &item, nil
	}
	if s.db == nil {
		return nil, model.ErrLocalStoreNotReady
	}
	var row model.PromotionPlan
	err = s.db.WithContext(ctx).
		Where("branch_id = ? AND plan_no = ?", branchID, planNo).
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return nil, ErrPromotionPlanNotFound
	}
	if err != nil {
		return nil, err
	}
	item := promotionPlanItemOfLocal(row)
	return &item, nil
}

// promotionPlanItemOf 把 cube 行组装成响应项。
func promotionPlanItemOf(r map[string]any) PromotionPlanItem {
	item := PromotionPlanItem{
		PlanNo:      cuberead.Str(r, "promotion_plan.plan_no"),
		PlanName:    cuberead.Str(r, "promotion_plan.plan_name"),
		PlanMemo:    cuberead.Str(r, "promotion_plan.plan_memo"),
		BeginDate:   cuberead.Date(cuberead.Str(r, "promotion_plan.begin_date")),
		EndDate:     cuberead.Date(cuberead.Str(r, "promotion_plan.end_date")),
		CreatedAt:   cuberead.Time(cuberead.Str(r, "promotion_plan.created_at")),
		CreatedBy:   cuberead.Str(r, "promotion_plan.created_by"),
		ApprovedBy:  cuberead.Str(r, "promotion_plan.approved_by"),
		MemberLevel: cuberead.Str(r, "promotion_plan.member_level"),
		StoppedBy:   cuberead.Str(r, "promotion_plan.stopped_by"),
	}
	if v := cuberead.Time(cuberead.Str(r, "promotion_plan.approved_at")); v != "" {
		item.ApprovedAt = &v
	}
	if v := cuberead.Time(cuberead.Str(r, "promotion_plan.stopped_at")); v != "" {
		item.StoppedAt = &v
	}
	item.ApproveStatus, item.ApproveStatusLabel = approveStatusOf(
		cuberead.Str(r, "promotion_plan.approve_flag"))
	return item
}

// promotionPlanItemOfLocal 把本地行组装成响应项(**同款结构**)。
func promotionPlanItemOfLocal(r model.PromotionPlan) PromotionPlanItem {
	item := PromotionPlanItem{
		PlanNo:      r.PlanNo,
		PlanName:    r.PlanName,
		PlanMemo:    r.PlanMemo,
		CreatedBy:   r.CreatedBy,
		ApprovedBy:  r.ApprovedBy,
		MemberLevel: r.MemberLevel,
		StoppedBy:   r.StoppedBy,
	}
	if r.BeginDate != nil {
		item.BeginDate = r.BeginDate.Format("2006-01-02")
	}
	if r.EndDate != nil {
		item.EndDate = r.EndDate.Format("2006-01-02")
	}
	if r.CreatedAt != nil {
		item.CreatedAt = r.CreatedAt.Format(time.RFC3339)
	}
	if r.ApprovedAt != nil {
		v := r.ApprovedAt.Format(time.RFC3339)
		item.ApprovedAt = &v
	}
	if r.StoppedAt != nil {
		v := r.StoppedAt.Format(time.RFC3339)
		item.StoppedAt = &v
	}
	item.ApproveStatus, item.ApproveStatusLabel = approveStatusOf(r.ApproveFlag)
	if r.ApproveStatus != "" {
		// 本地表存的是**译文**(model/promotion_plan.go 的约定)⇒ 直接用,
		// 标签按译文查(而不是再翻一次源码)。
		item.ApproveStatus = r.ApproveStatus
		item.ApproveStatusLabel = approveStatusLabel(r.ApproveStatus)
	}
	return item
}

// toAnySlice 把 []string 转成 []any(cube filter 的 values 是 []any)。
func toAnySlice(in []string) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}

// approveStatusOf 把源库 approve_flag 翻译成 (状态, 中文标签)。
//
// ⚠️ **判据写死在这里**,不许把源码透给前端(见包注释)。
//
// 实测(2026-10-22,ysx 库 2646 行):`2` = 已审核。
//
// ⚠️ `3` = 已终止 是**按语义推的**(源库只实测到 2)⇒ 写成 unknown 会更保守,
//   但图4 明确要 stopped 这个取值 ⇒ 这里按 3 = 已终止 处理,
//   并把这条假设写在这里,cube 侧一旦给出实测值就改这一处。
func approveStatusOf(flag string) (string, string) {
	switch strings.TrimSpace(flag) {
	case "1":
		return "draft", "未审核"
	case "2":
		return "approved", "已审核"
	case "3":
		return "stopped", "已终止"
	case "":
		return "unknown", "未知"
	default:
		return "unknown", "未知"
	}
}

// approveFlagsFor 反查某个译文状态对应的**源码**集合。
//
// ⚠️ 这是"按状态过滤"必须走源码的原因:cube 侧存的是 approve_flag,
//   而调用方给的是译文 ⇒ 必须反查,不能拿译文去比源码。
func approveFlagsFor(status string) []string {
	switch strings.TrimSpace(status) {
	case "draft":
		return []string{"1"}
	case "approved":
		return []string{"2"}
	case "stopped":
		return []string{"3"}
	case "unknown":
		return []string{""}
	default:
		return []string{"1", "2", "3"}
	}
}

// approveStatusLabel 给译文状态配中文标签。
func approveStatusLabel(status string) string {
	switch strings.TrimSpace(status) {
	case "draft":
		return "未审核"
	case "approved":
		return "已审核"
	case "stopped":
		return "已终止"
	default:
		return "未知"
	}
}
