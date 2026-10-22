// Package service / export.go —— 库存快照的**归档导出**（archivedto 契约）。
//
// # 它是什么
//
// `GET /archive/export?scope=stock&offset=&limit=` —— 把某门店的库存快照
// 导成**结构化数据**，给一次性归档同步（cubesync）用。
//
// ⚠️ **输出契约 = internal/archivedto**（与 cube-router 的 /v1/export **同一份字段名**）。
// 三方（导出方 / 编排方 / 导入方）各写一份 struct 的代价是**静默丢数据**：
// JSON 反序列化对不认识的字段静默忽略、对缺失的字段留零值
// ⇒ "导入报告显示成功 N 条，但字段全是空的"且**零报错**。
//
// # 有源 ⇒ 转发 cube-router；无源 ⇒ 本域 PG
//
//	门店配了数据源 ⇒ 该门店的库存在外部系统里 ⇒ **转发** cube-router 的
//	                  `GET /v1/export?scope=stock`（响应体**原样**透传）。
//	                  ⚠️ 配了源但 cube 里没有 ⇒ 导出**空数据**（不回落本地）。
//	门店没配数据源   ⇒ 从本域 stock_levels 读，按**同一份契约**组装。
//
// # 为什么转发而不是自己查 cube
//
// 门店 → cube 实例的映射只有 cube-router 知道（branch_cube_sources），
// 而"某个 scope 怎么拼 cube 查询、怎么把 cube 行整形成契约行"也只有它有一份实现
// ⇒ 两边各写一份的后果是"同一个 scope 在两条路上导出不同的列"。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/archivedto"
	"github.com/YunBright/supertrade/internal/inventory/model"
)

// 导出的分页默认值 / 上限（与 cube-router 的 /v1/export 同款）。
//
// ⚠️ 上限 10000：一次拉全店库存会让 sidecar 内存爆
// （门店 00 实测 27565 个商品），拉全量是调用方的循环。
const (
	// ExportDefaultLimit 是导出的默认每页行数。
	ExportDefaultLimit = 5000
	// ExportMaxLimit 是导出的每页行数上限。
	ExportMaxLimit = 10000
)

// ArchiveExporter 把导出请求转发给 cube-router 的 /v1/export。
//
// 实现：CubeRouterExporter（见 cube_export.go）。
type ArchiveExporter interface {
	// ExportRaw 转发一次导出请求，**原样**返回被调方的响应体。
	//
	// ⚠️ 原样返回（不重新组装）的原因：cube-router 的响应里还有 as_of 等字段，
	// 重新组装会把它们悄悄丢掉 ⇒ 两条路径（转发 / 本地）的响应形状就不一样了。
	ExportRaw(ctx context.Context, in ExportInput) ([]byte, error)
}

// ExportInput 是导出的入参。
type ExportInput struct {
	// BranchID 是门店（只认 X-Branch-ID header）。
	BranchID string
	// Scope 是资料域；inventory 只拥有 archivedto.ScopeStock。
	Scope string
	// Offset / Limit 是分页位置。
	Offset int
	Limit  int
	// AsOf 是基准日(yyyy-mm-dd)。库存**不是时效数据**（cube 的 stock 是 live 快照）
	// ⇒ 它不参与过滤，只是回给调用方的一个字段（与 cube-router 的响应同形）。
	AsOf string
	// Since 是**增量水位**（yyyy-mm-dd hh:mm:ss）：只导"这个时间点之后变过"的行。
	//
	// ⚠️ 空 = 全量（默认行为，不变）。
	// ⚠️ 源库 `t_im_branch_stock.oper_date` 覆盖**全部**库存变动来源
	//   （销售/退货/采购入库/盘点/调拨/损耗）⇒ 增量不会漏；
	//   而"从收银流水反推商品"会漏采购与盘点 ⇒ 本域不采用那种口径。
	Since string
	// Bearer 是调用者的 JWT（原样透传给 cube-router，让它做 per-branch scope 解析）。
	Bearer string
}

// StockExportPage 是本域组装的导出响应体。
//
// ⚠️ **直接内嵌 archivedto.ExportPage**（而不是再抄一份字段名）——
// 抄一份的后果是"archivedto 加了字段，这里没加"，
// 而导出的字段名对不上是**静默**的（导入端对不认识的字段直接忽略）。
//
// 额外带一个 as_of：cube-router 的 /v1/export 响应里也有它
// ⇒ 两条路径（转发 / 本地）的响应形状一致。
type StockExportPage struct {
	archivedto.ExportPage
	// AsOf 是基准日(yyyy-mm-dd)。
	AsOf string `json:"as_of"`
}

// SetArchiveExporter 注入归档导出转发器（有源门店时用它转发给 cube-router）。
func (s *StockService) SetArchiveExporter(exp ArchiveExporter) {
	s.exporter = exp
}

// ExportStock 导出库存快照（archivedto 契约）。
//
// 返回**响应体字节**（调用方原样写出）：
//   - 有源 ⇒ cube-router 的响应体**原样透传**
//   - 无源 ⇒ 本域按同一份契约组装的响应体
func (s *StockService) ExportStock(ctx context.Context, in ExportInput) ([]byte, error) {
	if strings.TrimSpace(in.BranchID) == "" {
		return nil, model.ErrInvalidInput
	}
	scope := strings.TrimSpace(in.Scope)
	if scope != archivedto.ScopeStock {
		// ⚠️ inventory 只拥有 stock 这一个 scope；别的 scope 归别的服务
		//   （商品档案归 catalog、售价归 pricing、会员归 member）
		//   ⇒ 这里放行会让调用方拿到"一个字段都不对"的导出。
		return nil, fmt.Errorf("%w: scope=%q 不支持;可用:%s",
			model.ErrUnsupportedExportScope, scope, archivedto.ScopeStock)
	}

	_, useCube, err := s.resolveCubeSource(ctx, in.BranchID)
	if err != nil {
		return nil, err
	}
	if useCube {
		if s.exporter == nil {
			// ⚠️ 装配漏了转发器 ⇒ 不能回落本地（那会导出"没人看的库存"）
			//   ⇒ 老实报"暂不可用"。
			return nil, fmt.Errorf("%w: 未装配归档导出转发器", model.ErrCubeSourceUnavailable)
		}
		return s.exporter.ExportRaw(ctx, in)
	}
	return s.exportLocalStock(ctx, in)
}

// exportLocalStock 从本域 stock_levels 导出（没配数据源的门店）。
func (s *StockService) exportLocalStock(ctx context.Context, in ExportInput) ([]byte, error) {
	offset := in.Offset
	if offset < 0 {
		offset = 0
	}
	limit := in.Limit
	if limit <= 0 {
		limit = ExportDefaultLimit
	}
	if limit > ExportMaxLimit {
		limit = ExportMaxLimit
	}

	tx := s.db.WithContext(ctx).Model(&model.StockLevel{}).Where("branch_id = ?", in.BranchID)
	// ⚠️ **本地路径也必须认 since**(两条路径口径一致)。
	//
	//	有源门店走 cube-router ⇒ cube 查询加 `stock.updated_at gt <since>`
	//	⇒ 只导"变过的行"。
	//	无源门店从本域 stock_levels 读 ⇒ 若**忽略** since
	//	⇒ 同一个端点在两种门店下语义不同(一个过滤、一个不过滤)
	//	⇒ 症状:增量同步对无源门店"看起来没生效"(每次都是全量),
	//	  而响应里**没有任何字段**能看出差别。
	//
	//	⇒ 本域用 `updated_at`(本域最后一次被写入的时刻)做代理。
	//	  它是"本域这一行上次变过是什么时候" ⇒ 语义正确。
	//
	// ⚠️ 代理口径的边界:本域的 updated_at 是**导入时刻**,不是源库的变动时刻
	//   ⇒ 无源门店的增量是"本域视角的变动",不是"源库视角的变动"。
	//   对"本域 → 目标门店"的搬运,前者才是对的。
	if since := strings.TrimSpace(in.Since); since != "" {
		// ⚠️ **解析成 time.Time 再比**,不要把字符串直接丢给 PG。
		//
		//	`updated_at > '2026-10-22 20:56:55'` 能不能work 取决于驱动怎么送参数
		//	(pgx 会把未知类型参数推断成 timestamp,但换驱动/换写法就不一定)
		//	⇒ 自己解析 ⇒ 时间列比较永远是"时间列比时间"。
		t, err := time.ParseInLocation(archivedto.SinceLayout, since, time.Local)
		if err != nil {
			return nil, fmt.Errorf("%w: since 不是合法时间点(%q)", model.ErrInvalidInput, since)
		}
		tx = tx.Where("updated_at > ?", t)
	}
	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, err
	}
	var rows []model.StockLevel
	if err := tx.Order("product_id asc").
		Offset(offset).
		Limit(limit).
		Find(&rows).Error; err != nil {
		return nil, err
	}

	// ⚠️ **数量与金额一律字符串**（archivedto 契约）：
	//   JSON number 到了 Dart 侧会被 jsonDecode 变成 double，精度当场丢。
	//   （cube-router 的导出也是这个约定，见它的 exportValue。）
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			"product_id":    r.ProductID,
			"qty":           r.Qty.String(),
			"avg_cost_yuan": r.AvgCostYuan.String(),
		})
	}

	asOf := strings.TrimSpace(in.AsOf)
	if asOf == "" {
		asOf = time.Now().Format(archivedto.AsOfLayout)
	}
	page := StockExportPage{
		ExportPage: archivedto.ExportPage{
			Scope:    archivedto.ScopeStock,
			BranchID: in.BranchID,
			// ⚠️ 本域导出没有 cube 实例（数据就在本域 PG）⇒ 空字符串。
			CubeSource: "",
			Rows:       out,
			Count:      len(out),
			Total:      int(total),
			Offset:     offset,
			Limit:      limit,
		},
		AsOf: asOf,
	}
	return json.Marshal(page)
}

// ParseExportPaging 解析导出端点的 offset / limit 参数。
//
// ⚠️ 非法值**兜底**而不是报错（与 cube-router 同款）：
// 导出是"拉全量"的循环，offset/limit 传错一个不该让整批同步失败。
func ParseExportPaging(offsetRaw, limitRaw string) (offset, limit int) {
	offset, _ = strconv.Atoi(strings.TrimSpace(offsetRaw))
	if offset < 0 {
		offset = 0
	}
	limit, _ = strconv.Atoi(strings.TrimSpace(limitRaw))
	if limit <= 0 {
		limit = ExportDefaultLimit
	}
	if limit > ExportMaxLimit {
		limit = ExportMaxLimit
	}
	return offset, limit
}
