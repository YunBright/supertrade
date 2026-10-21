// Package service / archive.go —— master-data 域的**归档导出 + 批量导入**。
//
// # 端点
//
//	GET  /archive/export?scope=stores|employees|shifts   导出(守卫 store:view)
//	POST /archive/import                                 导入(守卫 store:manage)
//
// # 这三个域**不在 cube 里**
//
//	cube 只有商品/分类/供应商/条码/价格/促销/库存/会员/会员类别 +
//	销售结算类 model。门店档案 / 收银员档案 / 班次**一个都不在**
//	⇒ 它们的读端点**不参与**「有源走 cube、无源走本地」的路由(永远走本地)。
//
//	但**导入导出**仍按同一套范式走(见 internal/archiveroute):
//	  - 没配 cube 源 ⇒ 导本地 / 写本地;
//	  - 配了 cube 源 ⇒ 导出**空数据** + 导入**拒绝(409 只读)**。
//
// # ⚠️ 为什么"配了源 ⇒ 导出空"而不是"照常导本地"
//
//	配了源的门店,它在外部系统(思迅等)里也有自己的门店/员工/班次
//	⇒ 本域 PG 里那几行是**切换前的旧数据**。
//	把它们导出去 = 把"旧的"当成"现在的",而导入端再写回本域
//	⇒ 制造一份**谁也不看、但看起来是真的**的数据。
//
//	⇒ 空数组是诚实的:"这个域的数据不在本域"。
//
// # ⚠️ 导出的字段取舍
//
//	**不含** created_by / updated_by / created_at / updated_at / deleted_at:
//	审计列由导入端重新生成(import)。导出它们 = 把**别处**的审计痕迹
//	搬进本域,让"谁改的"变成假线索。
//
//	**不含任何凭据列** —— 本域 employees 表**本来就没有**口令哈希 / API key
//	/人脸特征:收银员的身份与令牌在 auth.users,这里只存业务属性
//	(工号/姓名/角色/折扣权限/收银台)。⇒ 没有可导出的敏感列。
//	这个结论由 TestExportRows_NoCredentialColumns 锁住:
//	将来若有人往 employees 加了 password_hash 之类的列,那个测试会红。
package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/YunBright/supertrade/internal/archiveroute"
	"github.com/YunBright/supertrade/internal/masterdata/model"
)

// 归档的资料域(scope)名。
//
// ⚠️ 导出端点的 scope= 与导入端点的 scope **逐字对应**。
const (
	// ScopeStores 是门店档案。
	ScopeStores = "stores"
	// ScopeEmployees 是收银员业务档案。
	ScopeEmployees = "employees"
	// ScopeShifts 是班次。
	ScopeShifts = "shifts"
)

// 归档端点的守卫 scope(与 auth 的 permissions seed 逐字相同)。
const (
	// ScopeStoreView 是导出(store:view)。
	ScopeStoreView = "store:view"
	// ScopeStoreManage 是导入(store:manage)。
	ScopeStoreManage = "store:manage"
)

// importActor 是导入写入的审计人。
const importActor = "import"

// ArchiveService 是 master-data 的归档导出 / 批量导入服务。
type ArchiveService struct {
	db *gorm.DB
	// resolver 解析"这个门店配了哪个数据源"。
	//
	// ⚠️ 未注入 ⇒ 一律按本地处理(自营店场景);装配见 cmd/master-data/main.go。
	resolver archiveroute.Resolver
	now      func() time.Time
}

// NewArchiveService 构造 ArchiveService。
func NewArchiveService(db *gorm.DB) *ArchiveService {
	return &ArchiveService{db: db, now: func() time.Time { return time.Now() }}
}

// SetSourceResolver 注入数据源解析器。
func (a *ArchiveService) SetSourceResolver(r archiveroute.Resolver) {
	a.resolver = r
}

// ---- 导出行类型(本域自己的 DTO,不扩 archivedto)----

// StoreRow 是门店档案的导出行。
type StoreRow struct {
	BranchID           string `json:"branch_id"`
	Code               string `json:"code"`
	Name               string `json:"name"`
	PosEnabled         bool   `json:"pos_enabled"`
	StocktakeCycleDays int    `json:"stocktake_cycle_days"`
	MemberEnabled      bool   `json:"member_enabled"`
	CounterNos         string `json:"counter_nos"`
	Remark             string `json:"remark"`
}

// EmployeeRow 是收银员档案的导出行。
//
// ⚠️ **没有**任何凭据字段(见文件头):口令哈希 / API key / 人脸特征
// 都不在本域 employees 表里。
type EmployeeRow struct {
	ID string `json:"id"`
	// EmployeeCode 是工号(门店内唯一的业务标识)。
	EmployeeCode string `json:"employee_code"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	Status       string `json:"status"`
	// DiscountRate 是折扣权限(0.95 = 95 折),**字符串**。
	//
	// ⚠️ 不用 JSON number:到 Dart 侧会变 double(0.95 → 0.9499999999999996)
	// ⇒ 折扣判定出错。整仓约定:比率/金额一律字符串。
	DiscountRate string `json:"discount_rate"`
	CounterNo    string `json:"counter_no"`
	Remark       string `json:"remark"`
}

// ShiftRow 是班次的导出行。
type ShiftRow struct {
	ID         string `json:"id"`
	EmployeeID string `json:"employee_id"`
	CounterNo  string `json:"counter_no"`
	OpenedAt   string `json:"opened_at"`
	ClosedAt   string `json:"closed_at,omitempty"`
	Status     string `json:"status"`
	// 三个金额一律**字符串**(同 EmployeeRow.DiscountRate 的理由)。
	OpeningAmountYuan  string `json:"opening_amount_yuan"`
	ExpectedAmountYuan string `json:"expected_amount_yuan"`
	ClosingAmountYuan  string `json:"closing_amount_yuan"`
	Remark             string `json:"remark"`
}

// ExportResult 是导出的响应体。
//
// ⚠️ 三个数组**永远存在**(空时是 `[]` 而不是缺省):
//	"这个 scope 没有数据"与"这个 scope 不在响应里"是两件事,
//	前者是业务结论,后者是契约事故。
type ExportResult struct {
	// Scope 是本次导出的资料域。
	Scope string `json:"scope"`
	// BranchID 是目标门店(只从 X-Branch-ID 取)。
	BranchID string `json:"branch_id"`
	// External = 该门店配了外部数据源 ⇒ 本次导出**空**。
	//
	// ⚠️ 这个字段是为了让调用方**分得清**"没有数据"与"数据不在本域"。
	External bool `json:"external"`
	// Stores / Employees / Shifts 是各 scope 的行;本次只填 scope 指向的那一个。
	Stores    []StoreRow    `json:"stores"`
	Employees []EmployeeRow `json:"employees"`
	Shifts    []ShiftRow    `json:"shifts"`
	// Count 是本次 scope 的行数。
	Count int `json:"count"`
}

// ImportRequest 是导入请求体(与导出响应**同形** ⇒ 导出-导入可往返)。
//
// ⚠️ **body 里的 branch_id 不作数**:目标门店只认 X-Branch-ID header
//	(body 带一份门店就能跨店写入)。
type ImportRequest struct {
	// Scope 是本次导入的资料域。
	Scope string `json:"scope"`
	// Stores / Employees / Shifts 是行数据;本次只处理 Scope 指向的那一个。
	Stores    []StoreRow    `json:"stores,omitempty"`
	Employees []EmployeeRow `json:"employees,omitempty"`
	Shifts    []ShiftRow    `json:"shifts,omitempty"`
}

// ImportErrorItem 是单行的失败明细。
type ImportErrorItem struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// ImportResult 是导入结果。
type ImportResult struct {
	Scope    string `json:"scope"`
	BranchID string `json:"branch_id"`
	Received int    `json:"received"`
	Inserted int    `json:"inserted"`
	Updated  int    `json:"updated"`
	Skipped  int    `json:"skipped"`
	// Errors 是逐行失败明细(可空)。
	Errors []ImportErrorItem `json:"errors,omitempty"`
	// Warnings 是不阻断但必须让人知道的事。
	Warnings []string `json:"warnings,omitempty"`
}

// Export 导出指定 scope 的数据。
//
// # 判据(唯一一份,见 internal/archiveroute)
//
//	没配 cube 源 ⇒ 导本地表;
//	配了 cube 源 ⇒ 导出**空**;
//	问不到路由   ⇒ 503(ErrUnavailable / ErrDisabled / ErrForbidden / ErrAuthRejected)。
//
// ⚠️ **绝不在"问不到"时回落本地**:那会把一次链路故障显示成
// 「这个门店没有门店档案」——与故障无关的结论。
func (a *ArchiveService) Export(ctx context.Context, branchID, scope string) (*ExportResult, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", model.ErrInvalidInput)
	}
	scope = strings.TrimSpace(scope)
	switch scope {
	case ScopeStores, ScopeEmployees, ScopeShifts:
	default:
		return nil, fmt.Errorf("%w: scope=%q 非法(只能是 %s / %s / %s)",
			model.ErrInvalidInput, scope, ScopeStores, ScopeEmployees, ScopeShifts)
	}

	mode, err := archiveroute.DecideExport(ctx, a.resolver, branchID)
	if err != nil {
		return nil, err
	}

	out := &ExportResult{
		Scope:     scope,
		BranchID:  branchID,
		External:  mode == archiveroute.ModeExternal,
		Stores:    []StoreRow{},
		Employees: []EmployeeRow{},
		Shifts:    []ShiftRow{},
	}
	if mode == archiveroute.ModeExternal {
		// ⚠️ 空,**不回落本地**(见文件头)。
		return out, nil
	}

	switch scope {
	case ScopeStores:
		rows, err := a.exportStores(ctx, branchID)
		if err != nil {
			return nil, err
		}
		out.Stores = rows
		out.Count = len(rows)
	case ScopeEmployees:
		rows, err := a.exportEmployees(ctx, branchID)
		if err != nil {
			return nil, err
		}
		out.Employees = rows
		out.Count = len(rows)
	case ScopeShifts:
		rows, err := a.exportShifts(ctx, branchID)
		if err != nil {
			return nil, err
		}
		out.Shifts = rows
		out.Count = len(rows)
	}
	return out, nil
}

// exportStores 导出门店档案(按 branch_id 过滤)。
func (a *ArchiveService) exportStores(ctx context.Context, branchID string) ([]StoreRow, error) {
	var rows []model.Store
	if err := a.db.WithContext(ctx).Where("branch_id = ?", branchID).Order("branch_id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]StoreRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, StoreRow{
			BranchID:           r.BranchID,
			Code:               r.Code,
			Name:               r.Name,
			PosEnabled:         r.PosEnabled,
			StocktakeCycleDays: r.StocktakeCycleDays,
			MemberEnabled:      r.MemberEnabled,
			CounterNos:         r.CounterNos,
			Remark:             r.Remark,
		})
	}
	return out, nil
}

// exportEmployees 导出收银员档案(按 branch_id 过滤)。
func (a *ArchiveService) exportEmployees(ctx context.Context, branchID string) ([]EmployeeRow, error) {
	var rows []model.Employee
	if err := a.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("branch_id asc, employee_code asc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]EmployeeRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, EmployeeRow{
			ID:           r.ID,
			EmployeeCode: r.EmployeeCode,
			Name:         r.Name,
			Role:         r.Role,
			Status:       r.Status,
			DiscountRate: r.DiscountRate.String(),
			CounterNo:    r.CounterNo,
			Remark:       r.Remark,
		})
	}
	return out, nil
}

// exportShifts 导出班次(按 branch_id 过滤)。
//
// ⚠️ 导出**全部**班次(含已交班的),不只未交班的:
//	归档是"数据恢复",恢复的是**历史**,不是"此刻谁在班"。
func (a *ArchiveService) exportShifts(ctx context.Context, branchID string) ([]ShiftRow, error) {
	var rows []model.Shift
	if err := a.db.WithContext(ctx).
		Where("branch_id = ?", branchID).
		Order("opened_at desc").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ShiftRow, 0, len(rows))
	for _, r := range rows {
		row := ShiftRow{
			ID:                 r.ID,
			EmployeeID:         r.EmployeeID,
			CounterNo:          r.CounterNo,
			OpenedAt:           r.OpenedAt.UTC().Format(time.RFC3339),
			Status:             r.Status,
			OpeningAmountYuan:  r.OpeningAmountYuan.String(),
			ExpectedAmountYuan: r.ExpectedAmountYuan.String(),
			ClosingAmountYuan:  r.ClosingAmountYuan.String(),
			Remark:             r.Remark,
		}
		if r.ClosedAt != nil {
			row.ClosedAt = r.ClosedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	return out, nil
}

// Import 把导出的行**幂等 upsert** 回本域表。
//
// # ⚠️ 这是「数据恢复」,不是「业务重放」
//
//	只写数据:不调 OpenShift / Upsert 那些带业务规则的 service 方法,
//	不推事件、不改其它域的余额、不推进任何状态机。
//
//	具体的"不做什么":
//	  - 班次**不**校验"一个收银员同时只能有一个未交班的班次"
//	    (归档里可能真的有两条未交班的记录 —— 那是**历史事实**,不是要修的错);
//	  - 收银员**不**校验"门店内工号唯一"
//	    (同上:冲突是历史事实,恢复它比拒绝它更诚实);
//	  - 不发任何 dapr 事件(本域本来也不发,但明确写在这里以防将来加)。
//
// # 幂等
//
//	按**主键** upsert:stores(branch_id) / employees(id, branch_id) / shifts(id)。
//	重复导入同一份归档 ⇒ 行数不变、内容不变。
func (a *ArchiveService) Import(ctx context.Context, branchID string, req ImportRequest) (*ImportResult, error) {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, fmt.Errorf("%w: branch_id 必填", model.ErrInvalidInput)
	}
	scope := strings.TrimSpace(req.Scope)
	switch scope {
	case ScopeStores, ScopeEmployees, ScopeShifts:
	default:
		return nil, fmt.Errorf("%w: scope=%q 非法(只能是 %s / %s / %s)",
			model.ErrInvalidInput, scope, ScopeStores, ScopeEmployees, ScopeShifts)
	}

	// 判据:配了 cube 源 ⇒ 本域只读(409)。
	//
	// ⚠️ **解析失败 ⇒ 放行**:导入路径自己会再查一次目标数据,
	// 写不进去会诚实地失败(而不是"写成功了却写在别处")。
	mode, err := archiveroute.DecideImport(ctx, a.resolver, branchID)
	if err != nil {
		slog.Warn("master-data: 问不到门店数据源,导入按本域处理",
			"branch_id", branchID, "err", err)
		mode = archiveroute.ModeLocal
	}
	if mode == archiveroute.ModeExternal {
		return nil, fmt.Errorf("%w: 门店 %s 的主数据来自外部数据源,请在原系统操作",
			archiveroute.ErrExternalReadOnly, branchID)
	}

	res := &ImportResult{Scope: scope, BranchID: branchID}
	switch scope {
	case ScopeStores:
		a.importStores(ctx, branchID, req.Stores, res)
	case ScopeEmployees:
		a.importEmployees(ctx, branchID, req.Employees, res)
	case ScopeShifts:
		a.importShifts(ctx, branchID, req.Shifts, res)
	}
	return res, nil
}

// importStores 幂等写入门店档案。
//
// ⚠️ **stores 的主键就是 branch_id** ⇒ 一份导入里所有行都落到 header 那家店
//	(其它门店的行会被拒,见下)。
//
// ⚠️ **跨店写入必须拒**:stores 行里带一个别的 branch_id,就能改别家店的
//	经营参数(盘点周期 / 是否启用会员 / 收银台)——
//	而守门的 store:manage 只覆盖 **header 那家店**。
func (a *ArchiveService) importStores(ctx context.Context, branchID string, rows []StoreRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var found []model.Store
	if err := a.db.WithContext(ctx).Where("branch_id = ?", branchID).Find(&found).Error; err == nil {
		for _, f := range found {
			existing[f.BranchID] = true
		}
	}

	now := a.now()
	for _, r := range rows {
		id := strings.TrimSpace(r.BranchID)
		if id == "" {
			id = branchID
		}
		if id != branchID {
			res.Skipped++
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("stores:门店 %s 的档案不在本次导入范围(目标门店是 %s),已跳过", id, branchID))
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "只能导入当前门店的门店档案"})
			continue
		}
		name := strings.TrimSpace(r.Name)
		if name == "" {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "门店名为空"})
			continue
		}
		cycle := r.StocktakeCycleDays
		if cycle <= 0 {
			cycle = 7
		}
		row := model.Store{
			BranchID:           branchID,
			Code:               strings.TrimSpace(r.Code),
			Name:               name,
			PosEnabled:         r.PosEnabled,
			StocktakeCycleDays: cycle,
			MemberEnabled:      r.MemberEnabled,
			CounterNos:         r.CounterNos,
			Remark:             r.Remark,
			CreatedBy:          importActor,
			UpdatedBy:          importActor,
			CreatedAt:          now,
			UpdatedAt:          now,
		}
		err := a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "branch_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"code", "name", "pos_enabled", "stocktake_cycle_days",
				"member_enabled", "counter_nos", "remark", "updated_by", "updated_at",
			}),
		}).Create(&row).Error
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: err.Error()})
			continue
		}
		if existing[id] {
			res.Updated++
		} else {
			res.Inserted++
			existing[id] = true
		}
	}
}

// importEmployees 幂等写入收银员档案。
func (a *ArchiveService) importEmployees(ctx context.Context, branchID string, rows []EmployeeRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.Employee
	if err := a.db.WithContext(ctx).Where("branch_id = ? AND id IN ?", branchID, ids).Find(&found).Error; err == nil {
		for _, f := range found {
			existing[f.ID] = true
		}
	}

	now := a.now()
	for _, r := range rows {
		id := strings.TrimSpace(r.ID)
		code := strings.TrimSpace(r.EmployeeCode)
		name := strings.TrimSpace(r.Name)
		if id == "" || code == "" || name == "" {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "id / 工号 / 姓名必填"})
			continue
		}
		role := strings.TrimSpace(r.Role)
		if role == "" {
			role = RoleCashier
		}
		status := strings.TrimSpace(r.Status)
		if status == "" {
			status = EmployeeStatusActive
		}
		// ⚠️ 折扣率解析不了 ⇒ 记 1(不打折)并给一条 warning,
		//   **不**跳整行:档案身份比折扣权限重要。
		rate := decimal.NewFromInt(1)
		if t := strings.TrimSpace(r.DiscountRate); t != "" {
			d, err := decimal.NewFromString(t)
			if err != nil {
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("employees:收银员 %s 的 discount_rate %q 解析不了,按 1(不打折)处理", id, t))
			} else {
				rate = d
			}
		}
		row := model.Employee{
			ID:           id,
			BranchID:     branchID,
			EmployeeCode: code,
			Name:         name,
			Role:         role,
			Status:       status,
			DiscountRate: rate,
			CounterNo:    strings.TrimSpace(r.CounterNo),
			Remark:       r.Remark,
			CreatedBy:    importActor,
			UpdatedBy:    importActor,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		err := a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}, {Name: "branch_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"employee_code", "name", "role", "status", "discount_rate",
				"counter_no", "remark", "updated_by", "updated_at",
			}),
		}).Create(&row).Error
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: err.Error()})
			continue
		}
		if existing[id] {
			res.Updated++
		} else {
			res.Inserted++
		}
	}
}

// importShifts 幂等写入班次。
//
// ⚠️ **不校验"一个收银员只能有一个未交班的班次"**:
//	那是**开班**的业务规则;归档恢复的是历史事实(见 Import 的注释)。
func (a *ArchiveService) importShifts(ctx context.Context, branchID string, rows []ShiftRow, res *ImportResult) {
	res.Received = len(rows)
	if len(rows) == 0 {
		return
	}
	existing := map[string]bool{}
	var ids []string
	for _, r := range rows {
		ids = append(ids, strings.TrimSpace(r.ID))
	}
	var found []model.Shift
	if err := a.db.WithContext(ctx).Where("branch_id = ? AND id IN ?", branchID, ids).Find(&found).Error; err == nil {
		for _, f := range found {
			existing[f.ID] = true
		}
	}

	now := a.now()
	for _, r := range rows {
		id := strings.TrimSpace(r.ID)
		employeeID := strings.TrimSpace(r.EmployeeID)
		openedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(r.OpenedAt))
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "opened_at 不是 RFC3339 时间"})
			continue
		}
		if id == "" || employeeID == "" {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "id / employee_id 必填"})
			continue
		}
		var closedAt *time.Time
		if t := strings.TrimSpace(r.ClosedAt); t != "" {
			parsed, err := time.Parse(time.RFC3339, t)
			if err != nil {
				res.Skipped++
				res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: "closed_at 不是 RFC3339 时间"})
				continue
			}
			closedAt = &parsed
		}
		status := strings.TrimSpace(r.Status)
		if status == "" {
			status = ShiftStatusOpen
		}
		row := model.Shift{
			ID:                id,
			BranchID:          branchID,
			EmployeeID:        employeeID,
			CounterNo:         strings.TrimSpace(r.CounterNo),
			OpenedAt:          openedAt,
			ClosedAt:          closedAt,
			Status:            status,
			OpeningAmountYuan: parseAmountLoose(r.OpeningAmountYuan),
			ExpectedAmountYuan: parseAmountLoose(r.ExpectedAmountYuan),
			ClosingAmountYuan:  parseAmountLoose(r.ClosingAmountYuan),
			Remark:             r.Remark,
			CreatedBy:          importActor,
			UpdatedBy:          importActor,
			CreatedAt:          now,
			UpdatedAt:          now,
		}
		err = a.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"employee_id", "counter_no", "opened_at", "closed_at", "status",
				"opening_amount_yuan", "expected_amount_yuan", "closing_amount_yuan",
				"remark", "updated_by", "updated_at",
			}),
		}).Create(&row).Error
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, ImportErrorItem{ID: id, Error: err.Error()})
			continue
		}
		if existing[id] {
			res.Updated++
		} else {
			res.Inserted++
		}
	}
}

// parseAmountLoose 解析金额字符串;解析不了按 0。
//
// ⚠️ 与 member 的做法一致:金额是"钱的多少",错的代价远小于丢掉整条记录。
//   解析不了的行**照样落库**,并在报告里留一条 warning。
func parseAmountLoose(s string) decimal.Decimal {
	t := strings.TrimSpace(s)
	if t == "" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(t)
	if err != nil {
		return decimal.Zero
	}
	return d
}
