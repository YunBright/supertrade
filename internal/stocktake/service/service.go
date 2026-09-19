// Package service 实现 stocktake 服务的业务逻辑。
//
// 关键设计(REQUIREMENTS §2.1):
//   - 营业中盘点:不锁库,POS 正常销售
//   - book_qty 是每行录入时刻的 cube 库存快照(每行独立)
//   - 盘点单不跨店:录入每行校验 cube stock 在该 branch_id 下存在
//   - 差异表实时生成:counting/adjusted/approved 任意状态都允许
//
// 状态机:
//   counting ─submit─▶ adjusted ─approve─▶ approved
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/YunBright/supertrade/internal/cubeclient"
	"github.com/YunBright/supertrade/internal/stocktake/model"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// Service 是 stocktake 服务的业务聚合入口。
//
// 通过 New(db, cube) 构造,所有方法并发安全。
type Service struct {
	db   *gorm.DB
	cube cubeclient.Client
	now  func() time.Time // 注入时间,默认 time.Now().UTC()
}

// New 构造 Service。
//
// db 是 stocktake 服务的 PG 连接(GORM),cube 是 cube-gateway 客户端。
// cube 为 nil 时,AddLine 等会立即返回 ErrCubeUnavailable(便于测试)。
func New(db *gorm.DB, cube cubeclient.Client) *Service {
	return &Service{db: db, cube: cube, now: func() time.Time { return time.Now().UTC() }}
}

// SetClock 注入时间(测试用)。nil 恢复 time.Now().UTC()。
func (s *Service) SetClock(fn func() time.Time) {
	if fn == nil {
		fn = func() time.Time { return time.Now().UTC() }
	}
	s.now = fn
}

// ---- 业务错误(供 handler 映射 HTTP 状态码) ----

var (
	ErrHeaderNotFound       = errors.New("service: 盘点单不存在")
	ErrLineNotFound         = errors.New("service: 盘点明细不存在")
	ErrInvalidStatus        = errors.New("service: 盘点单状态不允许该操作")
	ErrInvalidTransition    = errors.New("service: 非法状态转移")
	ErrCubeUnavailable      = errors.New("service: cube client 未配置")
	ErrProductNotFound      = errors.New("service: 商品在 cube 中不存在") // 包装 cubeclient.ErrProductNotFound
	ErrStockNotFound        = errors.New("service: 该分店下的商品 stock 不存在(疑似跨店盘点)") // 包装 cubeclient.ErrStockNotFound
	ErrPlanItemNotFound     = errors.New("service: 计划盘点商品不存在")
	ErrPlanItemDuplicated   = errors.New("service: 同一商品已在计划清单中")
	ErrRecheckRequiresParent = errors.New("service: 复盘点单必须指定 parent_header_id")
	ErrInvalidOpType        = errors.New("service: 非法的 op_type")
)

// ---- Header ----

// CreateHeaderInput 创建盘点表入参。
//
// ParentHeaderID 仅在 Type == recheck 时必填(指向原始盘点单);
// type == plan 时忽略。
type CreateHeaderInput struct {
	BranchID      string
	CountDate     time.Time
	Type          model.StocktakeType
	OperatorID    string
	Remark        string
	ParentHeaderID string
}

// CreateHeader 创建盘点表(空表,不预填 SKU)。
//
// 主键由 Service 生成:`ST<yyyymmdd><seq>`,格式与 DESIGN §4.5.2 一致。
// 后续若高并发 seq 冲突,可换成 DB sequence 或 UUID(此处简化用时间戳纳秒)。
//
// 校验:type == recheck 必须传 parent_header_id(且该父单存在);
// type == plan / general / produce 不传 parent_header_id。
func (s *Service) CreateHeader(ctx context.Context, in CreateHeaderInput) (*model.StocktakeHeader, error) {
	if in.BranchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidStatus)
	}
	if in.OperatorID == "" {
		return nil, fmt.Errorf("%w: operator_id 必填", ErrInvalidStatus)
	}
	if in.Type == "" {
		in.Type = model.TypeGeneral
	}
	if in.CountDate.IsZero() {
		in.CountDate = s.now()
	}

	var parentIDPtr *string
	if in.Type == model.TypeRecheck {
		if in.ParentHeaderID == "" {
			return nil, ErrRecheckRequiresParent
		}
		parent, err := s.GetHeader(ctx, in.ParentHeaderID)
		if err != nil {
			return nil, fmt.Errorf("%w: parent_header_id 指向的盘点单无效", ErrRecheckRequiresParent)
		}
		_ = parent // 已确认存在
		parentIDPtr = &in.ParentHeaderID
	}

	now := s.now()
	id := generateHeaderID(now)

	h := &model.StocktakeHeader{
		ID:                  id,
		BranchID:            in.BranchID,
		CountDate:           in.CountDate,
		Status:              model.StatusCounting,
		Type:                in.Type,
		OperatorID:          in.OperatorID,
		ParentHeaderID:      parentIDPtr,
		TotalDiffQty:        decimal.Zero,
		TotalDiffAmountYuan: decimal.Zero,
		Remark:              in.Remark,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	if err := s.db.Create(h).Error; err != nil {
		return nil, fmt.Errorf("create header: %w", err)
	}
	return h, nil
}

// GetHeader 查盘点表(不带 lines)。
func (s *Service) GetHeader(_ context.Context, id string) (*model.StocktakeHeader, error) {
	var h model.StocktakeHeader
	if err := s.db.First(&h, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrHeaderNotFound
		}
		return nil, err
	}
	return &h, nil
}

// GetHeaderWithLines 查盘点表 + 所有明细行(按 created_at 升序)。
func (s *Service) GetHeaderWithLines(_ context.Context, id string) (*model.StocktakeHeader, error) {
	h, err := s.GetHeader(context.Background(), id)
	if err != nil {
		return nil, err
	}
	var lines []model.StocktakeLine
	if err := s.db.Where("header_id = ?", id).Order("created_at ASC").Find(&lines).Error; err != nil {
		return nil, err
	}
	h.Lines = lines
	return h, nil
}

// Submit counting → adjusted。
//
// 冻结总差异(写入 total_diff_qty / total_diff_amount_yuan 供后续查询);
// 调整后明细不可改。
func (s *Service) Submit(_ context.Context, id string) (*model.StocktakeHeader, error) {
	h, err := s.GetHeader(context.Background(), id)
	if err != nil {
		return nil, err
	}
	if h.Status != model.StatusCounting {
		return nil, fmt.Errorf("%w: submit 只允许从 counting", ErrInvalidTransition)
	}
	summary, err := s.summaryOf(context.Background(), id)
	if err != nil {
		return nil, err
	}
	now := s.now()
	if err := s.db.Model(&model.StocktakeHeader{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":                   model.StatusAdjusted,
			"total_diff_qty":           summary.TotalDiffQty,
			"total_diff_amount_yuan":   summary.TotalDiffAmountYuan,
			"updated_at":               now,
		}).Error; err != nil {
		return nil, err
	}
	return s.GetHeader(context.Background(), id)
}

// Approve adjusted → approved(需 auditor_id)。
//
// 入参后 Approved 不可改。
func (s *Service) Approve(_ context.Context, id, auditorID string) (*model.StocktakeHeader, error) {
	if auditorID == "" {
		return nil, fmt.Errorf("%w: auditor_id 必填", ErrInvalidStatus)
	}
	h, err := s.GetHeader(context.Background(), id)
	if err != nil {
		return nil, err
	}
	if h.Status != model.StatusAdjusted {
		return nil, fmt.Errorf("%w: approve 只允许从 adjusted", ErrInvalidTransition)
	}
	now := s.now()
	if err := s.db.Model(&model.StocktakeHeader{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":     model.StatusApproved,
			"auditor_id": auditorID,
			"updated_at": now,
		}).Error; err != nil {
		return nil, err
	}
	return s.GetHeader(context.Background(), id)
}

// ---- Line ----

// AddLineInput 添加盘点明细入参。
//
// OpType:create / overwrite / accumulate 三种;default = create(同旧行为)。
//   - create:首次录入(明细行不存在时,服务端忽略 overwrite/accumulate)
//   - overwrite:覆盖(同 create 行为,只是 audit 上区分)
//   - accumulate:累加(若行已存在,本函数将转给 UpdateLine 走 accumulate 分支)
//
// ActorID / ActorName 用于写 StocktakeLineOperation;为空时归为 "unknown"。
// Method:scan / manual / import,默认 manual。
type AddLineInput struct {
	ProductID string
	ActualQty decimal.Decimal
	DiffReason model.DiffReason
	Remark     string
	OpType     model.LineOpType // default OpCreate
	Method     model.OpMethod   // default MethodManual
	ActorID    string
	ActorName  string
}

// AddLine 按 product_id 录入一行盘点明细,实时拉 cube 库存作快照。
//
// 流程(REQUIREMENTS §2.1.5):
//  1. 校验 header 存在且 status == counting
//  2. cube GetProduct(查 product 元信息,给前端确认商品存在)
//  3. cube GetStock(branchID, productID) → 实时库存快照
//     跨店阻断:stock 不存在 → ErrStockNotFound
//  4. diff_qty = actual - book; diff_amount = diff × avg_cost
//  5. 落库(含 book_qty / book_qty_at)
//  6. 同一事务 insert StocktakeLineOperation(create / overwrite / accumulate)
//  7. type == recheck 时,Remark 强制 "复盘"
func (s *Service) AddLine(ctx context.Context, headerID string, in AddLineInput) (*model.StocktakeLine, error) {
	if s.cube == nil {
		return nil, ErrCubeUnavailable
	}
	h, err := s.GetHeader(ctx, headerID)
	if err != nil {
		return nil, err
	}
	if h.Status != model.StatusCounting {
		return nil, fmt.Errorf("%w: 只能在 counting 状态录入", ErrInvalidStatus)
	}

	// 1. 拉 cube product
	product, err := s.cube.GetProduct(ctx, in.ProductID)
	if err != nil {
		if errors.Is(err, cubeclient.ErrProductNotFound) {
			return nil, fmt.Errorf("%w: %v", ErrProductNotFound, err)
		}
		return nil, fmt.Errorf("cube get product: %w", err)
	}

	// 2. 拉 cube stock 快照(跨店阻断)
	snap, err := s.cube.GetStock(ctx, h.BranchID, in.ProductID)
	if err != nil {
		if errors.Is(err, cubeclient.ErrStockNotFound) {
			return nil, fmt.Errorf("%w: %v", ErrStockNotFound, err)
		}
		return nil, fmt.Errorf("cube get stock: %w", err)
	}

	// 3. 计算 diff
	diffQty := in.ActualQty.Sub(snap.Quantity)
	diffAmount := diffQty.Mul(snap.AvgCostYuan)

	opType := in.OpType
	if opType == "" {
		opType = model.OpCreate
	}
	if !validOpType(opType) {
		return nil, fmt.Errorf("%w: op_type=%q", ErrInvalidOpType, opType)
	}
	method := in.Method
	if method == "" {
		method = model.MethodManual
	}

	now := s.now()
	remarkStr := in.Remark
	if h.Type == model.TypeRecheck {
		// 复盘点单:Remark 服务端强制为 "复盘",忽略客户端传入
		remarkStr = "复盘"
	}
	var remarkPtr *string
	if remarkStr != "" {
		remarkPtr = &remarkStr
	}

	line := &model.StocktakeLine{
		ID:             uuid.NewString(),
		HeaderID:       h.ID,
		ProductID:      product.ID,
		ProductName:    product.Name,
		Unit:           product.Unit,
		Spec:           product.Spec,
		BookQty:        snap.Quantity,
		BookQtyAt:      snap.UpdatedAt,
		ActualQty:      in.ActualQty,
		DiffQty:        diffQty,
		AvgCostYuan:    snap.AvgCostYuan,
		DiffAmountYuan: diffAmount,
		DiffReason:     in.DiffReason,
		Remark:         remarkPtr,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	actorID := in.ActorID
	if actorID == "" {
		actorID = "unknown"
	}
	actorName := in.ActorName
	if actorName == "" {
		actorName = "unknown"
	}
	op := &model.StocktakeLineOperation{
		ID:        uuid.NewString(),
		HeaderID:  h.ID,
		LineID:    line.ID,
		OpType:    opType,
		ActorID:   actorID,
		ActorName: actorName,
		PrevQty:   decimal.Zero,
		NewQty:    in.ActualQty,
		QtyDelta:  in.ActualQty,
		OpAt:      now,
		Method:    method,
		Remark:    remarkStr,
		CreatedAt: now,
	}

	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(line).Error; err != nil {
			return fmt.Errorf("create line: %w", err)
		}
		if err := tx.Create(op).Error; err != nil {
			return fmt.Errorf("create operation: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return line, nil
}

// UpdateLineInput 改明细入参(只允许改 actual_qty / diff_reason / remark)。
//
// book_qty / book_qty_at 不可改(快照已固定)。改 actual 后 diff 重算。
//
// OpType:overwrite / accumulate / create。default = overwrite。
//   - overwrite:actual_qty = 新值(覆盖)
//   - accumulate:actual_qty = 旧值 + delta
//     此时 in.ActualQty 含义为"delta"(累加量,可能为负数做减)
//
// ActorID / ActorName / Method 同 AddLineInput。
type UpdateLineInput struct {
	ActualQty  *decimal.Decimal // nil 表示不更新;在 accumulate 模式下含义是 delta
	DiffReason *model.DiffReason
	Remark     *string
	OpType     model.LineOpType
	Method     model.OpMethod
	ActorID    string
	ActorName  string
}

// UpdateLine 改单条明细(actual_qty / diff_reason),重算 diff。
//
// accumulate 模式:入参 ActualQty 是增量 delta,内部 target = prev + delta;
// overwrite 模式:入参 ActualQty 是新值,内部 target = ActualQty。
//
// 同一事务内 insert 一条 StocktakeLineOperation(op_type=overwrite|accumulate);
// type == recheck 时,Remark 强制 "复盘"。
func (s *Service) UpdateLine(ctx context.Context, lineID string, in UpdateLineInput) (*model.StocktakeLine, error) {
	var line model.StocktakeLine
	if err := s.db.First(&line, "id = ?", lineID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrLineNotFound
		}
		return nil, err
	}
	h, err := s.GetHeader(ctx, line.HeaderID)
	if err != nil {
		return nil, err
	}
	if h.Status != model.StatusCounting {
		return nil, fmt.Errorf("%w: 只能在 counting 状态改", ErrInvalidStatus)
	}

	opType := in.OpType
	if opType == "" {
		opType = model.OpOverwrite
	}
	if !validOpType(opType) {
		return nil, fmt.Errorf("%w: op_type=%q", ErrInvalidOpType, opType)
	}
	method := in.Method
	if method == "" {
		method = model.MethodManual
	}

	updates := map[string]any{"updated_at": s.now()}
	var targetQty decimal.Decimal
	var prevQty = line.ActualQty
	if in.ActualQty != nil {
		switch opType {
		case model.OpAccumulate:
			targetQty = prevQty.Add(*in.ActualQty)
		default: // overwrite / create
			targetQty = *in.ActualQty
		}
		updates["actual_qty"] = targetQty
		diffQty := targetQty.Sub(line.BookQty)
		updates["diff_qty"] = diffQty
		updates["diff_amount_yuan"] = diffQty.Mul(line.AvgCostYuan)
	} else {
		targetQty = prevQty
	}
	if in.DiffReason != nil {
		updates["diff_reason"] = *in.DiffReason
	}
	if h.Type == model.TypeRecheck {
		// 复盘点单:Remark 服务端强制为 "复盘",忽略客户端传入
		updates["remark"] = "复盘"
	} else if in.Remark != nil {
		updates["remark"] = *in.Remark
	}

	actorID := in.ActorID
	if actorID == "" {
		actorID = "unknown"
	}
	actorName := in.ActorName
	if actorName == "" {
		actorName = "unknown"
	}
	now := s.now()
	remarkStr := ""
	if h.Type == model.TypeRecheck {
		remarkStr = "复盘"
	} else if in.Remark != nil {
		remarkStr = *in.Remark
	}
	op := &model.StocktakeLineOperation{
		ID:        uuid.NewString(),
		HeaderID:  h.ID,
		LineID:    lineID,
		OpType:    opType,
		ActorID:   actorID,
		ActorName: actorName,
		PrevQty:   prevQty,
		NewQty:    targetQty,
		QtyDelta:  targetQty.Sub(prevQty),
		OpAt:      now,
		Method:    method,
		Remark:    remarkStr,
		CreatedAt: now,
	}

	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.StocktakeLine{}).
			Where("id = ?", lineID).
			Updates(updates).Error; err != nil {
			return fmt.Errorf("update line: %w", err)
		}
		if err := tx.Create(op).Error; err != nil {
			return fmt.Errorf("create operation: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.getLine(ctx, lineID)
}

// DeleteLine 删单条明细(counting 状态才允许)。
//
// 同一事务 insert StocktakeLineOperation(op_type=delete)。
func (s *Service) DeleteLine(ctx context.Context, lineID string, actorID, actorName string, method model.OpMethod) error {
	var line model.StocktakeLine
	if err := s.db.First(&line, "id = ?", lineID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrLineNotFound
		}
		return err
	}
	h, err := s.GetHeader(ctx, line.HeaderID)
	if err != nil {
		return err
	}
	if h.Status != model.StatusCounting {
		return fmt.Errorf("%w: 只能在 counting 状态删", ErrInvalidStatus)
	}

	if method == "" {
		method = model.MethodManual
	}
	if actorID == "" {
		actorID = "unknown"
	}
	if actorName == "" {
		actorName = "unknown"
	}

	now := s.now()
	op := &model.StocktakeLineOperation{
		ID:        uuid.NewString(),
		HeaderID:  h.ID,
		LineID:    lineID,
		OpType:    model.OpDelete,
		ActorID:   actorID,
		ActorName: actorName,
		PrevQty:   line.ActualQty,
		NewQty:    decimal.Zero,
		QtyDelta:  line.ActualQty.Neg(),
		OpAt:      now,
		Method:    method,
		CreatedAt: now,
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&line).Error; err != nil {
			return fmt.Errorf("delete line: %w", err)
		}
		if err := tx.Create(op).Error; err != nil {
			return fmt.Errorf("create operation: %w", err)
		}
		return nil
	})
}

// validOpType 校验 OpType 取值。
func validOpType(t model.LineOpType) bool {
	switch t {
	case model.OpCreate, model.OpOverwrite, model.OpAccumulate, model.OpDelete:
		return true
	}
	return false
}

func (s *Service) getLine(_ context.Context, lineID string) (*model.StocktakeLine, error) {
	var line model.StocktakeLine
	if err := s.db.First(&line, "id = ?", lineID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrLineNotFound
		}
		return nil, err
	}
	return &line, nil
}

// ---- Product Search ----

// SearchProductsInput 商品搜索入参(对齐 scan.html 后端 SearchProducts,REQUIREMENTS §2.1.4.1)。
type SearchProductsInput struct {
	Barcode  string // 扫码输入或 item_no
	BranchID string // 用于合并该门店 stock 维度
	Limit    int    // 5~12 位后缀匹配时使用;≥13 位自动取 1
}

// SearchProductRow 是 SearchProducts 返回的每个元素。
//
// 字段含义 / 权限过滤 / barcode 长度策略 全部对齐 collect-ai SearchProducts。
//
// 注意:所有可选字段用 *decimal.Decimal 指针,保证 JSON omitempty 生效;
// 零值 decimal.Decimal 不触发 omitempty(JSON 把所有 struct 都视为非零),
// 而权限字段没值时必须不输出。
type SearchProductRow struct {
	Barcode      string           `json:"barcode"`                  // = item_no (本期)
	ProductID    string           `json:"product_id"`
	ProductName  string           `json:"product_name"`
	Category     string           `json:"category,omitempty"`
	Brand        string           `json:"brand,omitempty"`
	Unit         string           `json:"unit,omitempty"`
	Price        *decimal.Decimal `json:"price,omitempty"`           // 扩展字段,本期 mock
	StockQty     *decimal.Decimal `json:"stock_qty,omitempty"`        // 需 inventory:view
	AvgCostYuan  *decimal.Decimal `json:"avg_cost_yuan,omitempty"`
	SupplierID   string           `json:"supplier_id,omitempty"`     // 需 supplier:view
	SupplierName string           `json:"supplier_name,omitempty"`
}

// SearchProductsMeta 权限可见性元数据。
type SearchProductsMeta struct {
	InvViewable       bool   `json:"inv_viewable"`
	SupplierViewable  bool   `json:"supplier_viewable"`
	BarcodeQuery      string `json:"barcode_query,omitempty"`
}

// SearchProductsOutput 搜索结果。
type SearchProductsOutput struct {
	Products []SearchProductRow `json:"products"`
	Count    int                `json:"count"`
	Meta     SearchProductsMeta `json:"meta"`
}

// SearchProducts 按 barcode 查商品,合并 stock / supplier 视图,
//
// 权限控制:
//   - invViewable=false → 不查 cube stock、不返 stock_qty / avg_cost_yuan
//   - supplierViewable=false → 不查 cube supplier、不返 supplier_id / supplier_name
//
// 注意:权限通过 invViewable / supplierViewable 参数传入,由 handler 从 claims.Scopes 读出。
func (s *Service) SearchProducts(_ context.Context, in SearchProductsInput, invViewable, supplierViewable bool) (*SearchProductsOutput, error) {
	if s.cube == nil {
		return nil, ErrCubeUnavailable
	}
	if in.BranchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", ErrInvalidStatus)
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 10
	}

	rows, err := s.cube.SearchProductsByBarcode(context.Background(), in.Barcode, in.BranchID, limit)
	if err != nil {
		return nil, fmt.Errorf("cube search products: %w", err)
	}

	// 准备 supplier 一次性查(本期只用同店 supplier;Cube 拉所有供货这家店的 supplier 太重,
	// 直接按 product.supplier_id 查 supplier 名,逐条 InMemoryClient.SearchSuppliers)
	out := &SearchProductsOutput{
		Products: make([]SearchProductRow, 0, len(rows)),
		Meta: SearchProductsMeta{
			InvViewable:      invViewable,
			SupplierViewable: supplierViewable,
			BarcodeQuery:     in.Barcode,
		},
	}
	for _, r := range rows {
		if r.Product == nil {
			continue
		}
		row := SearchProductRow{
			Barcode:     r.Product.Barcode,
			ProductID:   r.Product.ID,
			ProductName: r.Product.Name,
			Category:    r.Product.CategoryID, // 简化:返回 ID 即可;前端要做"分类 ID → 名称"映射的话后续 Phase
			Brand:       "",                  // cube 标准 product 无 brand 字段(扩展)
			Unit:        r.Product.Unit,
			Price:       nil,                  // cube 标准 product 无 price 字段(扩展,本期 mock)
		}
		if invViewable && r.Stock != nil {
			q := r.Stock.Quantity
			c := r.Stock.AvgCostYuan
			row.StockQty = &q
			row.AvgCostYuan = &c
		}
		if supplierViewable && r.Product.SupplierID != "" {
			row.SupplierID = r.Product.SupplierID
			// 查 supplier 名(本期 cube mock 已有)
			suppliers, _ := s.cube.SearchSuppliers(context.Background(), r.Product.SupplierID, 1)
			for _, sup := range suppliers {
				if sup.ID == r.Product.SupplierID {
					row.SupplierName = sup.Name
					break
				}
			}
		}
		out.Products = append(out.Products, row)
	}
	out.Count = len(out.Products)
	return out, nil
}

// ---- Diff Report ----

// ComputeDiffReport 实时生成差异表(任意状态可调)。
//
// counting:随录入逐行变化
// adjusted:冻结(明细不变)
// approved:不可改,但仍可生成历史快照供 BI
func (s *Service) ComputeDiffReport(_ context.Context, headerID string) (*model.DiffReport, error) {
	h, err := s.GetHeader(context.Background(), headerID)
	if err != nil {
		return nil, err
	}

	var lines []model.StocktakeLine
	if err := s.db.Where("header_id = ?", headerID).Order("created_at ASC").Find(&lines).Error; err != nil {
		return nil, err
	}

	summary, byReason, diffLines := aggregateLines(lines)
	return &model.DiffReport{
		HeaderID:    h.ID,
		BranchID:    h.BranchID,
		CountDate:   h.CountDate,
		Status:      h.Status,
		Summary:     summary,
		ByReason:    byReason,
		Lines:       diffLines,
		GeneratedAt: s.now(),
	}, nil
}

// summaryOf 只算 summary(供 submit 写入 total_diff_* 冗余字段)。
func (s *Service) summaryOf(_ context.Context, headerID string) (model.DiffSummary, error) {
	var lines []model.StocktakeLine
	if err := s.db.Where("header_id = ?", headerID).Find(&lines).Error; err != nil {
		return model.DiffSummary{}, err
	}
	summary, _, _ := aggregateLines(lines)
	return summary, nil
}

// aggregateLines 计算 summary / byReason / diffLines。
//
// diffLines 只列 diff_qty != 0 的行(避免 noise)。
func aggregateLines(lines []model.StocktakeLine) (model.DiffSummary, []model.ReasonAgg, []model.DiffReportLine) {
	summary := model.DiffSummary{TotalLines: len(lines)}
	byReasonMap := map[model.DiffReason]*model.ReasonAgg{}
	reportLines := make([]model.DiffReportLine, 0, len(lines))

	for i := range lines {
		l := lines[i]
		summary.TotalDiffQty = summary.TotalDiffQty.Add(l.DiffQty)
		summary.TotalDiffAmountYuan = summary.TotalDiffAmountYuan.Add(l.DiffAmountYuan)

		cmp := l.DiffQty.Cmp(decimal.Zero)
		switch {
		case cmp < 0:
			summary.LossLines++
		case cmp > 0:
			summary.OverageLines++
		default:
			summary.NoDiffLines++
		}

		if l.DiffReason != "" {
			agg, ok := byReasonMap[l.DiffReason]
			if !ok {
				agg = &model.ReasonAgg{Reason: l.DiffReason}
				byReasonMap[l.DiffReason] = agg
			}
			agg.Lines++
			agg.Qty = agg.Qty.Add(l.DiffQty)
			agg.AmountYuan = agg.AmountYuan.Add(l.DiffAmountYuan)
		}

		if cmp != 0 {
			reportLines = append(reportLines, model.DiffReportLine{
				ProductID:      l.ProductID,
				ProductName:    l.ProductName,
				BookQty:        l.BookQty,
				BookQtyAt:      l.BookQtyAt,
				ActualQty:      l.ActualQty,
				DiffQty:        l.DiffQty,
				AvgCostYuan:    l.AvgCostYuan,
				DiffAmountYuan: l.DiffAmountYuan,
				DiffReason:     l.DiffReason,
			})
		}
	}

	// 排序 byReason(按 amount 绝对值降序,损失多的在前)
	byReason := make([]model.ReasonAgg, 0, len(byReasonMap))
	for _, agg := range byReasonMap {
		byReason = append(byReason, *agg)
	}
	sortReasonAggs(byReason)
	return summary, byReason, reportLines
}

// sortReasonAggs 按 amount 绝对值降序(便于 BI 直接展示)。
func sortReasonAggs(aggs []model.ReasonAgg) {
	// 简单选择排序(数据量小,n<20)
	for i := 0; i < len(aggs); i++ {
		maxIdx := i
		for j := i + 1; j < len(aggs); j++ {
			if aggs[j].AmountYuan.Abs().GreaterThan(aggs[maxIdx].AmountYuan.Abs()) {
				maxIdx = j
			}
		}
		aggs[i], aggs[maxIdx] = aggs[maxIdx], aggs[i]
	}
}

// headerIDSeq 是进程内的单调递增 seq,确保同一时刻创建的 header 主键不冲突。
//
// 测试用固定 clock 时,纳秒时间戳可能两次相同 → unique 冲突;
// 这里用 atomic counter 永远递增,代替 UnixNano 后 6 位。
var headerIDSeq atomic.Uint64

// generateHeaderID 生成 ST<yyyymmdd><seq>。
//
// seq 是进程内 atomic 计数器(永远递增),保证主键唯一;
// 高并发场景后续可换 DB sequence / UUID。
func generateHeaderID(now time.Time) string {
	seq := headerIDSeq.Add(1) % 1_000_000
	return fmt.Sprintf("ST%s%06d", now.UTC().Format("20060102"), seq)
}

// ---- Header List (盘点单列表) ----

// ListHeadersFilter 列表查询过滤条件。
//
// 空字符串表示"不过滤该字段";CountDate 用 YYYY-MM-DD 字符串格式。
type ListHeadersFilter struct {
	BranchID   string
	Status     model.StocktakeStatus
	Type       model.StocktakeType
	OperatorID string
	CountDate  string // YYYY-MM-DD;空=不过滤
}

// ListHeadersOutput 分页响应。
type ListHeadersOutput struct {
	Headers  []model.StocktakeHeader `json:"headers"`
	Page     int                     `json:"page"`
	PageSize int                     `json:"page_size"`
	Total    int64                   `json:"total"`
}

// ListHeaders 查盘点表列表(不含 lines)。
//
// 默认 page=1, page_size=20,max page_size=100。
// 按 count_date DESC, id DESC 排序(最新在前)。
func (s *Service) ListHeaders(_ context.Context, f ListHeadersFilter, page, pageSize int) (*ListHeadersOutput, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	q := s.db.Model(&model.StocktakeHeader{})
	if f.BranchID != "" {
		q = q.Where("branch_id = ?", f.BranchID)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.Type != "" {
		q = q.Where("type = ?", f.Type)
	}
	if f.OperatorID != "" {
		q = q.Where("operator_id = ?", f.OperatorID)
	}
	if f.CountDate != "" {
		// 精确匹配 date 字段;Postgres:date 类型 / SQLite:text 都能 string 比对
		q = q.Where("count_date = ?", f.CountDate)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, fmt.Errorf("count headers: %w", err)
	}

	var headers []model.StocktakeHeader
	if err := q.Order("count_date DESC, id DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&headers).Error; err != nil {
		return nil, fmt.Errorf("list headers: %w", err)
	}
	return &ListHeadersOutput{
		Headers:  headers,
		Page:     page,
		PageSize: pageSize,
		Total:    total,
	}, nil
}

// ---- Line Operation History (明细操作历史) ----

// ListLineOperations 查盘点单的所有操作历史,按 op_at DESC(最新在前)。
//
// limit 默认 50,最大 200。
func (s *Service) ListLineOperations(_ context.Context, headerID string, limit int) ([]model.StocktakeLineOperation, error) {
	if _, err := s.GetHeader(context.Background(), headerID); err != nil {
		return nil, err
	}
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var ops []model.StocktakeLineOperation
	if err := s.db.Where("header_id = ?", headerID).
		Order("op_at DESC, id DESC").
		Limit(limit).
		Find(&ops).Error; err != nil {
		return nil, fmt.Errorf("list operations: %w", err)
	}
	return ops, nil
}

// ---- Plan Items (计划盘点商品) ----

// PlanItemInput 单条计划商品。
type PlanItemInput struct {
	ProductID   string
	ProductName string
	Unit        string
	Barcode     string
	SortOrder   int
}

// GetPlanItems 查盘点单的全部计划商品(按 sort_order ASC, created_at ASC)。
func (s *Service) GetPlanItems(_ context.Context, headerID string) ([]model.StocktakePlanItem, error) {
	if _, err := s.GetHeader(context.Background(), headerID); err != nil {
		return nil, err
	}
	var items []model.StocktakePlanItem
	if err := s.db.Where("header_id = ?", headerID).
		Order("sort_order ASC, created_at ASC").
		Find(&items).Error; err != nil {
		return nil, fmt.Errorf("list plan items: %w", err)
	}
	return items, nil
}

// AddPlanItemsInput 批量入参。
type AddPlanItemsInput struct {
	Items []PlanItemInput
}

// AddPlanItems 批量加计划商品。
//
// 任一重复 (header_id, product_id) 立即整体回滚 → ErrPlanItemDuplicated;
// product_name / unit / barcode 若客户端未传,服务端从 cube 现拉(便于 UI 直接展示)。
func (s *Service) AddPlanItems(ctx context.Context, headerID string, in AddPlanItemsInput) ([]model.StocktakePlanItem, error) {
	if len(in.Items) == 0 {
		return nil, nil
	}
	h, err := s.GetHeader(ctx, headerID)
	if err != nil {
		return nil, err
	}

	now := s.now()
	out := make([]model.StocktakePlanItem, 0, len(in.Items))

	err = s.db.Transaction(func(tx *gorm.DB) error {
		for _, it := range in.Items {
			if it.ProductID == "" {
				return fmt.Errorf("%w: product_id 必填", ErrInvalidStatus)
			}

			// product_name / unit / barcode 缺省时从 cube 拉
			name := it.ProductName
			unit := it.Unit
			barcode := it.Barcode
			if s.cube != nil && (name == "" || barcode == "") {
				product, err := s.cube.GetProduct(ctx, it.ProductID)
				if err != nil && !errors.Is(err, cubeclient.ErrProductNotFound) {
					return fmt.Errorf("cube get product: %w", err)
				}
				if product != nil {
					if name == "" {
						name = product.Name
					}
					if unit == "" {
						unit = product.Unit
					}
					if barcode == "" {
						barcode = product.Barcode
					}
				}
			}
			if name == "" {
				name = it.ProductID // 兜底
			}

			item := &model.StocktakePlanItem{
				ID:          uuid.NewString(),
				HeaderID:    h.ID,
				ProductID:   it.ProductID,
				ProductName: name,
				Unit:        unit,
				Barcode:     barcode,
				SortOrder:   it.SortOrder,
				CreatedAt:   now,
			}
			if err := tx.Create(item).Error; err != nil {
				// uniqueIndex:idx_plan_header_product 命中 → 整体回滚
				if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "unique") {
					return fmt.Errorf("%w: product_id=%s", ErrPlanItemDuplicated, it.ProductID)
				}
				return fmt.Errorf("create plan item: %w", err)
			}
			out = append(out, *item)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeletePlanItem 删单条计划商品。
func (s *Service) DeletePlanItem(_ context.Context, headerID, itemID string) error {
	res := s.db.Where("id = ? AND header_id = ?", itemID, headerID).
		Delete(&model.StocktakePlanItem{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrPlanItemNotFound
	}
	return nil
}