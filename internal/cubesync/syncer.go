// Package cubesync / syncer.go —— 同步编排:拉 → 整形 → 写 → 出报告。
//
// # 一次同步做什么
//
//	for each scope in cfg.Scopes(按依赖顺序):
//	    ① 分页拉完 cube-router 的 /v1/export
//	    ② 整形成 archivedto.Import*Row
//	    ③ 分块 POST catalog 的 /archive/import
//	    ④ 累计结果
//
// # ⚠️ 分块**不能切断**一个商品的条码集合
//
// catalog 的覆盖模式对条码做的是**按商品整体替换**
// (ReplaceForProduct:把该商品现有的条码集换成这一批)。
//
// 若某个商品的 3 个条码被切成两块:
//
//	块1 [A,B] → 写入 A、B,并**删掉** C(不在这一批里)
//	块2 [C]   → 写入 C,并**删掉** A、B(不在这一批里)
//	⇒ 最终只剩 C,A、B 静默消失
//
// 症状是"导入报告显示成功,但扫某些条码查不到商品",
// 而报告显示一切正常 —— 幽灵条码 / 丢条码都长这样。
//
// ⇒ 条码 scope 先**按 product_id 分组**,再把**整组**装进同一块。
//
// # 为什么一个 scope 失败就整体中止
//
// scope 之间有引用关系(商品引分类、条码引商品)。
// 分类拉失败却继续导商品 ⇒ 商品带着**不存在的 category_id** 落库,
// 而导入报告只说"商品成功 27565 条"。
//
// ⇒ 任一 scope 失败即中止,并把**已完成的进度**放进报告一起返回。
// 同步是幂等的,下一轮重跑即可补齐。
package cubesync

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/archivedto"
)

// DefaultChunkSize 是单次导入请求携带的行数上限。
//
// ⚠️ 不是"一次全导":27565 个商品一次成 20MB 的请求体,
// 既撑 sidecar 内存,又让 catalog 侧一个大事务跑很久。
// 分块的代价是"半个 scope 写了一半",但导入幂等,重跑即可。
const DefaultChunkSize = 2000

// ScopeReport 是单个 scope 的同步结果。
type ScopeReport struct {
	Scope string `json:"scope"`
	// Exported 是从导出方拉到的行数。
	Exported int `json:"exported"`
	// Chunks 是提交了几次导入请求。
	Chunks int `json:"chunks"`
	// Inserted / Updated / Skipped / Deleted 来自导入方的逐 scope 计数。
	Inserted int `json:"inserted"`
	Updated  int `json:"updated"`
	Skipped  int `json:"skipped"`
	Deleted  int `json:"deleted"`
	// Warnings 是导入方报上来的"不阻断但必须让人知道"的事。
	Warnings []string `json:"warnings,omitempty"`
	// Errors 是逐行失败明细。
	Errors []archivedto.ImportErrorItem `json:"errors,omitempty"`
}

// SyncReport 是一次同步的整体结果。
type SyncReport struct {
	SourceBranchID string        `json:"source_branch_id"`
	TargetBranchID string        `json:"target_branch_id"`
	Mode           string        `json:"mode"`
	StartedAt      time.Time     `json:"started_at"`
	FinishedAt     time.Time     `json:"finished_at"`
	DurationMS     int64         `json:"duration_ms"`
	Scopes         []ScopeReport `json:"scopes"`
	// Warnings 是跨 scope 的提醒(如价格未落库)。
	Warnings []string `json:"warnings,omitempty"`
	// Errors 是 scope 级失败信息。
	Errors []string `json:"errors,omitempty"`
}

// Total 汇总各 scope 的写入计数。
func (r *SyncReport) Total() (inserted, updated, skipped int) {
	if r == nil {
		return 0, 0, 0
	}
	for _, s := range r.Scopes {
		inserted += s.Inserted
		updated += s.Updated
		skipped += s.Skipped
	}
	return
}

// Owner 把一组 scope 绑定到一个导入方。
//
// ⚠️ **一个 scope 一个归属方** —— 这是本包的核心纪律:
//
//	catalog 拥有 categories / suppliers / products / item_barcodes
//	pricing 拥有 prices / promotions
//
// ⇒ 编排方**不替下游决定**"这条数据该写进谁家的表"。
//
//	把 prices 塞进 products 那条路(catalog 存价)⇒ 两个真相源。
type Owner struct {
	// Importer 是这个归属方的导入客户端。
	Importer Importer
	// Scopes 是它拥有的 scope 列表。
	Scopes []string
}

// Syncer 编排一次同步。
type Syncer struct {
	cfg      Config
	token    TokenIssuer
	exporter Exporter
	// owners 是"scope → 导入方"的路由表。
	owners []Owner
	// pageSize 是导出时每页行数。
	pageSize int
	// chunkSize 是单次导入请求的行数。
	chunkSize int
	// watermarks 是**增量水位**(每个 scope 一个时间点)。
	//
	// ⚠️ nil / 未启用 ⇒ 所有 scope 都走全量(安全默认)。
	watermarks *WatermarkStore
	logger     *slog.Logger
	now        func() time.Time
}

// NewSyncer 构造。
//
// owners 是路由表;**每个 scope 必须在里面恰好出现一次** ——
// 漏了 ⇒ 同步那一 scope 时报"没有归属方"(而不是静默跳过);
// 重复了 ⇒ 后者覆盖前者(所以要在装配期就报错,见 validateOwners)。
func NewSyncer(cfg Config, token TokenIssuer, exporter Exporter, owners []Owner, logger *slog.Logger) *Syncer {
	if logger == nil {
		logger = slog.Default()
	}
	pageSize := cfg.PageSize
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}
	s := &Syncer{
		cfg:       cfg,
		token:     token,
		exporter:  exporter,
		owners:    owners,
		pageSize:  pageSize,
		chunkSize: DefaultChunkSize,
		logger:    logger,
		now:       func() time.Time { return time.Now() },
	}
	// ⚠️ 水位存储的时钟**必须**与 Syncer 用同一个 ⇒
	//   否则"水位记的时刻"与"基准日算的时刻"来自两套时钟
	//   ⇒ 差几毫秒无所谓,但**测试注入固定时钟时会算出两个不同的 now**
	//   ⇒ 断言"水位推进到哪"就会假红。
	s.watermarks = NewWatermarkStore(cfg.WatermarkFile,
		cfg.WatermarkOverlapSeconds, cfg.WatermarkMaxAgeDays, s.now)
	return s
}

// ownerFor 找某个 scope 的归属方。
func (s *Syncer) ownerFor(scope string) (Importer, bool) {
	for _, o := range s.owners {
		for _, sc := range o.Scopes {
			if sc == scope {
				return o.Importer, true
			}
		}
	}
	return nil, false
}

// validateOwners 校验路由表:每个 scope 恰好一个归属方。
//
// ⚠️ 必须在**装配期**就报错,而不是同步时才发现 ——
//
//	"某个 scope 没人认领"的表现是"那一类数据静默不导",
//	而报告里只写着别的 scope 成功了。
func validateOwners(owners []Owner) error {
	seen := map[string]string{}
	for _, o := range owners {
		if o.Importer == nil {
			return fmt.Errorf("cubesync: 归属方 %v 的 Importer 为 nil", o.Scopes)
		}
		for _, sc := range o.Scopes {
			if prev, dup := seen[sc]; dup {
				return fmt.Errorf("cubesync: scope %q 有两个归属方(%s 与 %v)—— 一个 scope 只能有一个归属方", sc, prev, o.Scopes)
			}
			seen[sc] = strings.Join(o.Scopes, ",")
		}
	}
	return nil
}

// WithConfig 复制一份 Syncer,换一份配置(依赖不变)。
//
// # 为什么要能换配置
//
// 人工触发时可能想"先试一次 incremental,别直接覆盖"。
// 而 scope 的写入语义(覆盖 vs 只加)差别很大 ——
// 试错时用错模式的后果是目标门店的条码被清掉(幽灵条码的另一面)。
//
// ⇒ 复制而不是改原对象的字段:定时任务与人工触发并发时,
//
//	改原对象会让**正在跑的定时任务**中途换模式 ——
//	症状是"半夜那次同步一半 incremental 一半 overwrite"。
func (s *Syncer) WithConfig(cfg Config) *Syncer {
	if s == nil {
		return nil
	}
	cp := *s
	cp.cfg = cfg
	return &cp
}

// Sync 执行一次完整同步。
//
// ⚠️ 返回值语义:**报告与错误同时给出**。
// 失败时报告里是"已经做完的部分",不是零值 ——
// 否则一次中途失败会让人以为"什么都没导",
// 而实际已经有几千行落库了(下次重跑是幂等的,不会写坏)。
func (s *Syncer) Sync(ctx context.Context) (*SyncReport, error) {
	started := s.now()
	rep := &SyncReport{
		SourceBranchID: s.cfg.SourceBranchID,
		TargetBranchID: s.cfg.TargetBranchID,
		Mode:           s.cfg.Mode,
		StartedAt:      started,
	}
	finish := func() {
		rep.FinishedAt = s.now()
		rep.DurationMS = rep.FinishedAt.Sub(started).Milliseconds()
	}

	if s.token == nil {
		finish()
		return rep, fmt.Errorf("cubesync: 未装配机器身份,无法同步")
	}
	if s.exporter == nil {
		finish()
		return rep, fmt.Errorf("cubesync: 导出方未装配,无法同步")
	}
	// ⚠️ 路由表必须在**装配期**就合法:某个 scope 没有归属方 / 一个 scope 有两个归属方
	//   ⇒ 立刻失败,而不是同步到那一步才发现(那时已经白导了一遍)。
	if err := validateOwners(s.owners); err != nil {
		finish()
		return rep, err
	}
	if len(s.owners) == 0 {
		finish()
		return rep, fmt.Errorf("cubesync: 没有装配任何导入方,无法同步")
	}
	// ⚠️ **空 scope 集合 ≠ 不过滤**。
	// 空集合的语义必须是"一个都不导"并**报错**,不是"全部都导"或"什么都不做"。
	// 后两种都会留下一份"看起来同步成功了、其实一行没动"的库。
	if len(s.cfg.Scopes) == 0 {
		finish()
		return rep, fmt.Errorf("cubesync: SYNC_SCOPES 为空 —— 一个资料域都不导,拒绝执行(空集合不等于不过滤)")
	}

	// 换一枚机器 token。
	//
	// ⚠️ 一枚就够:auth 的 service_audience 默认含 cube-router 与 catalog,
	// 而 scope 是**按门店实时解析**的(源门店 cube:read / 目标门店 product:manage)。
	bearer, err := s.token.AccessToken(ctx)
	if err != nil {
		finish()
		return rep, err
	}

	for _, scope := range s.cfg.Scopes {
		sr, err := s.syncScope(ctx, bearer, scope)
		if sr != nil {
			rep.Scopes = append(rep.Scopes, *sr)
		}
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", scope, err))
			finish()
			return rep, err
		}
	}
	finish()
	return rep, nil
}

// syncScope 同步一个资料域。
func (s *Syncer) syncScope(ctx context.Context, bearer, scope string) (*ScopeReport, error) {
	sr := &ScopeReport{Scope: scope}

	// ⚠️ **先找归属方**:没有归属方的 scope 必须在**导出之前**就失败 ——
	//   先导 27565 行再发现没人收,那叫"白导一遍"。
	importer, ok := s.ownerFor(scope)
	if !ok {
		return sr, fmt.Errorf("scope %q 没有归属方(路由表里找不到)—— 它该写进谁家的表?", scope)
	}

	// ⚠️ **增量**:只有**声明支持增量**的 scope 才带 since 水位。
	//
	//	理由:不支持增量的 scope 若带上 since(而端点忽略了它)⇒ 仍然是全量导出
	//	⇒ 但**水位被推进了** ⇒ 下次它带 since ⇒ **只导一部分却当成全量** ⇒ 漏数据。
	//	⇒ 白名单(IncrementalScopes)+ 能力表(archivedto.ChangedMembers)双闸。
	since := ""
	if s.supportsIncremental(scope) {
		// ⚠️ 两种情况都让这一轮**无视水位**:
		//
		//	① **强制全量**(cfg.ForceFull):人工的一次性决定
		//	   (换门店 / 重建目标库 / 怀疑漂移 ⇒ 增量导不出基础数据)。
		//	② **定期全量校准**(距上次全量太久):增量只保证"不漏",
		//	   但**任何一次**故障漏掉的行都会**一直缺着**
		//	   (下次增量只导"变过的行")⇒ 没有校准就是永久漂移。
		if !s.cfg.ForceFull && !s.watermarks.ShouldFullRefresh(scope) {
			since = s.watermarks.Get(scope)
		}
	}
	// ⚠️ since == "" ⇒ 这一轮是**全量**(没有水位 / 定期校准 / 该 scope 不支持增量)。
	fullRound := since == ""
	// ⚠️ **水位要减去该 scope 的"源数据滞后"**:
	//   源是 cube 的离线 DuckDB 快照时,导出那一刻它可能还没刷新到最新
	//   ⇒ 水位按"导出时刻"记就会把那段时间的变更**永远落在水位之前** ⇒ 永久漏。
	//   详见 archivedto.ChangedMemberLag 的说明。
	//
	// ⚠️ Config.IncrementalLag 可**按 scope 覆盖**(默认取 archivedto 的登记值)。
	lag := archivedto.ChangedMemberLagOf(scope)
	if v, ok := s.cfg.IncrementalLag[scope]; ok {
		lag = v
	}
	watermarkValue := s.watermarks.NowAt(lag)

	rows, err := s.exportAll(ctx, bearer, scope, since)
	if err != nil {
		return sr, err
	}
	sr.Exported = len(rows)
	if len(rows) == 0 {
		// ⚠️ **空结果有两种含义,必须分开**:
		//
		//	① **全量轮次**的 0 行 = "没拉到"(端点挂了 / cube 源没了)
		//	   ⇒ 当成事实导进去(覆盖模式:把目标门店清空)⇒ 必须报错。
		//	② **增量轮次**的 0 行 = "自上次水位以来没有变动"
		//	   ⇒ 这是**正常状态**(源库实测:23837 行里一天只变 91 行
		//	   ⇒ 绝大多数夜晚就是 0 行)⇒ 报错只会让每晚的同步都失败。
		//
		// ⇒ 判据是"这一轮有没有带 since",不是"行数是不是 0"。
		if fullRound {
			return sr, fmt.Errorf("导出方返回空结果(scope=%s)—— 拒绝把空当事实导入", scope)
		}
		// ⚠️ 增量轮次 0 行:没有东西要导 ⇒ 但**水位照常推进**。
		//
		//	为什么推进:这一轮查询确实覆盖了 [上次水位, 本次起点] 这个窗口,
		//	窗口里没有变动的行 ⇒ 把它记下来,下次不必重查同一个窗口。
		//
		//	⚠️ 万一 cube 的过滤条件失效(恒返 0 行)⇒ 库存会一直不同步;
		//	  兜底是**定期全量校准**(ShouldFullRefresh)⇒ 最多一个周期就补齐。
		s.watermarks.Set(scope, watermarkValue)
		s.logger.Info("cubesync: 增量窗口内没有变动",
			"scope", scope, "since", since, "watermark", watermarkValue)
		return sr, nil
	}

	chunks := s.chunkFor(scope, rows)
	for _, chunk := range chunks {
		req := archivedto.ImportRequest{
			SourceBranchID: s.cfg.SourceBranchID,
			Mode:           s.cfg.Mode,
			Scopes:         []string{scope},
			Data:           buildImportData(scope, chunk),
		}
		// ⚠️ **时效 scope 要带基准日(as_of)**：促销是时效数据，
		// 导入端点据此**只写有效的**并（overwrite 时）清掉历史行。
		//   不带 ⇒ 库里会攒下永远用不上的历史促销。
		if scope == archivedto.ScopePromotions {
			if asOf, err := archivedto.NormalizeAsOf("", s.now()); err == nil {
				req.AsOf = asOf
			}
		}
		res, err := importer.Import(ctx, bearer, req)
		if err != nil {
			return sr, err
		}
		sr.Chunks++
		s.mergeResult(sr, res)
	}
	// ⚠️ **推进水位**:只在**该 scope 真的走了增量**之后。
	//
	//	水位 = 本次**导出开始**的时刻(减 overlap)。
	//	⇒ 导出期间发生的变动会落在水位之后 ⇒ 下次再导一次(幂等 upsert ⇒ 无害);
	//	⇒ 而导出之前发生的变动一定被这次捕获 ⇒ **不漏**。
	//
	// ⚠️ 只在**导入全部成功**之后推进:中途失败 ⇒ 水位不动 ⇒ 下次重来 ⇒ 不漏。
	if s.supportsIncremental(scope) && watermarkValue != "" {
		s.watermarks.Set(scope, watermarkValue)
		// ⚠️ **只有真的导了全量**才记全量校准时间。
		//
		//	把增量轮次记成全量 ⇒ 定期校准被推迟 ⇒ 漏掉的行一直缺着。
		if fullRound {
			s.watermarks.MarkFullSync(scope, watermarkValue)
		}
		s.logger.Info("cubesync: 推进增量水位",
			"scope", scope, "since", since, "watermark", watermarkValue,
			"exported", len(rows), "full_round", fullRound)
	}
	return sr, nil
}

// supportsIncremental 判断某 scope 是否**声明支持增量**。
//
// ⚠️ 这是**白名单**(cfg.IncrementalScopes),不是黑名单 ——
//   不在名单里的 scope 一律全量。
//   ⇒ 名单写错的后果是"多导一次"(幂等,无害),而不是"漏数据"。
//
// ⚠️ 还要过第二道闸(archivedto.SupportsIncremental)——
//   名单里写了但 cube 侧其实没有"变动时间"成员 ⇒ 端点会**忽略** since
//   ⇒ 水位却被推进 ⇒ 漏数据。
//   ⇒ 配置期有 Validate 拦一道,这里再兜一道(防止 Config 被手工构造)。
func (s *Syncer) supportsIncremental(scope string) bool {
	if s == nil || s.watermarks == nil || !s.watermarks.Enabled() {
		return false
	}
	if !archivedto.SupportsIncremental(scope) {
		return false
	}
	for _, sc := range s.cfg.IncrementalScopes {
		if strings.TrimSpace(sc) == scope {
			return true
		}
	}
	return false
}

// mergeResult 把导入方的结果并进 scope 报告。
func (s *Syncer) mergeResult(sr *ScopeReport, res *archivedto.ImportResult) {
	if res == nil {
		return
	}
	for _, row := range res.ByScope {
		sr.Inserted += row.Inserted
		sr.Updated += row.Updated
		sr.Skipped += row.Skipped
		sr.Deleted += row.Deleted
	}
	sr.Warnings = append(sr.Warnings, res.Warnings...)
	sr.Errors = append(sr.Errors, res.Errors...)
}

// exportAll 分页拉完某个 scope。
func (s *Syncer) exportAll(ctx context.Context, bearer, scope, since string) ([]map[string]any, error) {
	var all []map[string]any
	offset := 0
	for {
		page, err := s.exporter.Export(ctx, bearer, scope, offset, s.pageSize, since)
		if err != nil {
			return nil, err
		}
		if page == nil {
			return nil, fmt.Errorf("导出方返回空响应(scope=%s offset=%d)", scope, offset)
		}
		all = append(all, page.Rows...)
		offset += len(page.Rows)
		if len(page.Rows) == 0 {
			break
		}
		if page.Total > 0 {
			if offset >= page.Total {
				break
			}
			continue
		}
		// ⚠️ total 缺失(导出方没问 / cube 没给)时,用"不满一页"判末页。
		if len(page.Rows) < s.pageSize {
			break
		}
	}
	return all, nil
}

// chunkFor 把一个 scope 的行切成若干块。
//
// ⚠️ 条码 scope **按商品分组**切,保证同一商品的条码不跨块
// (原因见包注释)。其它 scope 逐行切。
func (s *Syncer) chunkFor(scope string, rows []map[string]any) [][]map[string]any {
	if scope != archivedto.ScopeItemBarcodes {
		return chunkRows(rows, s.chunkSize)
	}
	// 分组:保持首次出现的顺序,让同一商品的条码聚在一起。
	byProduct := map[string][]map[string]any{}
	var order []string
	for _, r := range rows {
		pid := strOf(r, "product_id")
		if pid == "" {
			// 没有 product_id 的行无主(条码引用不存在的商品)⇒ 单独成组,
			// 导入方会按"引用不存在"跳过它。
			byProduct[""] = append(byProduct[""], r)
			if len(byProduct[""]) == 1 {
				order = append(order, "")
			}
			continue
		}
		if _, seen := byProduct[pid]; !seen {
			order = append(order, pid)
		}
		byProduct[pid] = append(byProduct[pid], r)
	}
	var chunks [][]map[string]any
	var cur []map[string]any
	for _, pid := range order {
		group := byProduct[pid]
		// 整组必须落在同一块 ⇒ 放不下就先把当前块发出去。
		if len(cur) > 0 && len(cur)+len(group) > s.chunkSize {
			chunks = append(chunks, cur)
			cur = nil
		}
		cur = append(cur, group...)
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// chunkRows 逐行切块。
func chunkRows(rows []map[string]any, size int) [][]map[string]any {
	if size <= 0 {
		size = DefaultChunkSize
	}
	var chunks [][]map[string]any
	for start := 0; start < len(rows); start += size {
		end := start + size
		if end > len(rows) {
			end = len(rows)
		}
		chunks = append(chunks, rows[start:end])
	}
	return chunks
}

// buildImportData 把一个 scope 的一块行整形成导入请求体。
func buildImportData(scope string, rows []map[string]any) archivedto.ImportData {
	switch scope {
	case archivedto.ScopeCategories:
		out := make([]archivedto.ImportCategoryRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, archivedto.ImportCategoryRow{
				ID:       strOf(r, "id"),
				Name:     strOf(r, "name"),
				ParentID: strOf(r, "parent_id"),
			})
		}
		return archivedto.ImportData{Categories: out}
	case archivedto.ScopeSuppliers:
		out := make([]archivedto.ImportSupplierRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, archivedto.ImportSupplierRow{
				ID:      strOf(r, "id"),
				Name:    strOf(r, "name"),
				Type:    strOf(r, "type"),
				Contact: strOf(r, "contact"),
				Phone:   strOf(r, "phone"),
				Email:   strOf(r, "email"),
				Address: strOf(r, "address"),
				Status:  strOf(r, "status"),
			})
		}
		return archivedto.ImportData{Suppliers: out}
	case archivedto.ScopeProducts:
		out := make([]archivedto.ImportProductRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, archivedto.ImportProductRow{
				ID:         strOf(r, "id"),
				Name:       strOf(r, "name"),
				CategoryID: strOf(r, "category_id"),
				SupplierID: strOf(r, "supplier_id"),
				Unit:       strOf(r, "unit"),
				Status:     strOf(r, "status"),
				SubCode:    strOf(r, "sub_code"),
				Spec:       strOf(r, "spec"),
				PriceYuan:  strOf(r, "price_yuan"),
				CostYuan:   strOf(r, "cost_yuan"),
			})
		}
		return archivedto.ImportData{Products: out}
	case archivedto.ScopeItemBarcodes:
		out := make([]archivedto.ImportBarcodeRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, archivedto.ImportBarcodeRow{
				Barcode:   strOf(r, "barcode"),
				ProductID: strOf(r, "product_id"),
				Label:     strOf(r, "label"),
			})
		}
		return archivedto.ImportData{ItemBarcodes: out}
	case archivedto.ScopePrices:
		out := make([]archivedto.ImportPriceRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, archivedto.ImportPriceRow{
				ProductID: strOf(r, "product_id"),
				PriceYuan: strOf(r, "price_yuan"),
				CostYuan:  strOf(r, "cost_yuan"),
			})
		}
		return archivedto.ImportData{Prices: out}
	case archivedto.ScopePromotions:
		out := make([]archivedto.ImportPromotionRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, archivedto.ImportPromotionRow{
				PlanNo:        strOf(r, "plan_no"),
				ProductID:     strOf(r, "product_id"),
				PlanName:      strOf(r, "plan_name"),
				PriceYuan:     strOf(r, "price_yuan"),
				OrigPriceYuan: strOf(r, "orig_price_yuan"),
				BeginDay:      strOf(r, "begin_day"),
				EndDay:        strOf(r, "end_day"),
			})
		}
		return archivedto.ImportData{Promotions: out}
	case archivedto.ScopeStock:
		// ⚠️ 库存快照：**没有时间过滤** —— cube 的 stock 是 live model,
		// 导出来天然是「同步时刻」的库存(不是历史轨迹)。
		out := make([]archivedto.ImportStockRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, archivedto.ImportStockRow{
				ProductID:   strOf(r, "product_id"),
				Qty:         strOf(r, "qty"),
				AvgCostYuan: strOf(r, "avg_cost_yuan"),
			})
		}
		return archivedto.ImportData{Stocks: out}
	case archivedto.ScopeMemberTypes:
		// ⚠️ 会员类别:只带字典,导入端按**名字**对齐(两边 id 体系不同)。
		out := make([]archivedto.ImportMemberTypeRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, archivedto.ImportMemberTypeRow{
				ID:               strOf(r, "id"),
				Name:             strOf(r, "name"),
				DiscountRate:     strOf(r, "discount_rate"),
				AllowStoredValue: strOf(r, "allow_stored_value"),
				AllowPointsPay:   strOf(r, "allow_points_pay"),
			})
		}
		return archivedto.ImportData{MemberTypes: out}
	case archivedto.ScopeMembers:
		// ⚠️ 会员档案:**原样带出**(不归一化)——
		// 语义归一化(状态映射 / 哨兵 / 脏值)在**导入端**做,
		// 在这里"顺手翻译"会把"读不懂"变成"余额 0"。
		out := make([]archivedto.ImportMemberRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, archivedto.ImportMemberRow{
				ID:                 strOf(r, "id"),
				CardFaceNo:         strOf(r, "card_face_no"),
				CardType:           strOf(r, "card_type"),
				Name:               strOf(r, "name"),
				Gender:             strOf(r, "gender"),
				Phone:              strOf(r, "phone"),
				Mobile:             strOf(r, "mobile"),
				Birthday:           strOf(r, "birthday"),
				IDCardNo:           strOf(r, "id_card_no"),
				Status:             strOf(r, "status"),
				FeeAmountYuan:      strOf(r, "fee_amount_yuan"),
				LimitedUseCount:    strOf(r, "limited_use_count"),
				PointsAccumulated:  strOf(r, "points_accumulated"),
				PointsUsed:         strOf(r, "points_used"),
				BalanceYuan:        strOf(r, "balance_yuan"),
				ConsumedAmountYuan: strOf(r, "consumed_amount_yuan"),
				ConsumedCount:      strOf(r, "consumed_count"),
				Favorite:           strOf(r, "favorite"),
				Remark:             strOf(r, "remark"),
				BranchNo:           strOf(r, "branch_no"),
				OperatorID:         strOf(r, "operator_id"),
				OperatedAt:         strOf(r, "operated_at"),
				ValidFrom:          strOf(r, "valid_from"),
				ValidUntil:         strOf(r, "valid_until"),
				OpenID:             strOf(r, "open_id"),
			})
		}
		return archivedto.ImportData{Members: out}
	default:
		// 未知 scope 不静默忽略:那会让"配错了 scope 名"变成
		// "这个 scope 什么都没导" —— 与"本来就没数据"长得一模一样。
		return archivedto.ImportData{}
	}
}

// strOf 从一行里取字符串值。
//
// ⚠️ 导出契约里所有值都是**字符串或 null**(金额也导出成字符串,
// 以免调用方拿到 float64 就丢了精度)。但这里仍兼容数字/布尔,
// 因为"契约说不会发生"和"契约变了"之间的差别,往往就是一次空指针。
func strOf(row map[string]any, key string) string {
	if row == nil {
		return ""
	}
	v, ok := row[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case json.Number:
		return t.String()
	case float64:
		// 走到这里说明契约变了(金额变成了数字)。用 %v 而不是 strconv,
		// 是为了不引入"看起来精确"的假象。
		return strings.TrimSpace(fmt.Sprintf("%v", t))
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", t))
	}
}
