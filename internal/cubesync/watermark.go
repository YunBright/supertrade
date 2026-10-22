package cubesync

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/archivedto"
)

// WatermarkStore 是**同步水位**的存储(每个 scope 两个时间点)。
//
// # 它是什么
//
//	"这个 scope 我上次同步到哪个时间点了" ⇒ 下次只导**之后变过**的东西。
//
//	⚠️ **为什么按"变动时间"而不是"收银流水"**
//
//	库存变动**不止来自销售**:采购入库(+)、盘点调整(±)、调拨(±)、损耗(-)、
//	退货(+)都改库存。
//	⇒ 若按"上次同步以来的**收银流水**"反推商品 ⇒ 这些改动**永远同步不到**
//	⇒ 本地库存会**越来越偏**,而且**没有任何一行报错**。
//
//	⇒ 而源库的 `t_im_branch_stock.oper_date`(= cube 的 stock.updated_at)
//	**覆盖全部变动来源** ⇒ 直接用它做水位,既准又简单。
//
// # 落盘格式
//
//	{
//	  "watermarks":   {"stock": "2026-10-22 12:00:00"},
//	  "full_sync_at": {"stock": "2026-10-15 04:30:00"}
//	}
//
//	watermarks   = 上次导出的**起点**(增量过滤用的 since);
//	full_sync_at = 上次**全量**校准的时刻(增量只是"增量",漏了就是永久漂移
//	              ⇒ 必须定期全量兜底,见 ShouldFullRefresh)。
//
// # 丢文件 ⇒ 退化成全量(安全方向)
type WatermarkStore struct {
	path string
	// now 可注入(测试用)。
	now func() time.Time
	// overlapSec 是水位回退秒数(防"同秒内多次更新"漏掉)。
	overlapSec int
	// maxAgeDays 是"距上次全量校准"的最长天数;0 = 不限。
	maxAgeDays int
}

// watermarkFile 是落盘结构。
//
// ⚠️ **两个 map 而不是一张平表** —— 平表会让 scope 名与保留键
//   (如 "_full_at")混在一张表里,而 scope 名是**外部输入**
//   (SYNC_INCREMENTAL_SCOPES)⇒ 平表迟早会撞名。
type watermarkFile struct {
	// Watermarks 是 scope → 上次导出的起点。
	Watermarks map[string]string `json:"watermarks"`
	// FullSyncAt 是 scope → 上次全量校准的时刻。
	FullSyncAt map[string]string `json:"full_sync_at"`
}

// NewWatermarkStore 构造水位存储。
//
// ⚠️ path 为空 ⇒ 不启用水位(每次都全量)⇒ 这是**安全**的默认。
func NewWatermarkStore(path string, overlapSec, maxAgeDays int, now func() time.Time) *WatermarkStore {
	if now == nil {
		now = time.Now
	}
	if overlapSec < 0 {
		overlapSec = 0
	}
	return &WatermarkStore{
		path:       strings.TrimSpace(path),
		now:        now,
		overlapSec: overlapSec,
		maxAgeDays: maxAgeDays,
	}
}

// Enabled 返水位是否启用。
func (w *WatermarkStore) Enabled() bool {
	return w != nil && w.path != ""
}

// Get 取某个 scope 的水位(空 = 没有水位 ⇒ 该 scope 走全量)。
//
// ⚠️ **读不到就返空,绝不返一个猜的时间** ——
//
//	猜错的方向有两种,后果完全相反:
//	  - 猜**太新** ⇒ 漏数据(永久漂移);
//	  - 猜**太旧** ⇒ 多导(幂等,无害)。
//	⇒ 宁可多导 ⇒ 读不到 / 损坏,一律当"没有水位"(全量)。
func (w *WatermarkStore) Get(scope string) string {
	if !w.Enabled() {
		return ""
	}
	all, err := w.load()
	if err != nil {
		return ""
	}
	raw := strings.TrimSpace(all.Watermarks[scope])
	if raw == "" {
		return ""
	}
	t, err := time.ParseInLocation(archivedto.SinceLayout, raw, time.Local)
	if err != nil {
		return ""
	}
	return t.Format(archivedto.SinceLayout)
}

// Set 记某个 scope 的水位。
//
// ⚠️ 写失败**不返回错误**:水位只是优化,写不进去只意味着"下次多导一次"。
//   ⇒ 把它当错误会让整个同步失败,反而更糟。
func (w *WatermarkStore) Set(scope, watermark string) {
	w.update(func(all *watermarkFile) {
		if all.Watermarks == nil {
			all.Watermarks = map[string]string{}
		}
		all.Watermarks[scope] = strings.TrimSpace(watermark)
	})
}

// MarkFullSync 记某个 scope 的**全量校准**时刻。
//
// ⚠️ 只在**真的导了全量**时调用 ——
//   导的是增量却记成全量 ⇒ 下次校准被推迟 ⇒ 漏掉的行一直缺着。
func (w *WatermarkStore) MarkFullSync(scope, at string) {
	w.update(func(all *watermarkFile) {
		if all.FullSyncAt == nil {
			all.FullSyncAt = map[string]string{}
		}
		all.FullSyncAt[scope] = strings.TrimSpace(at)
	})
}

// ShouldFullRefresh 报告该 scope 这一轮是否该**走全量**(无视水位)。
//
// # 触发条件
//
//	① 从没做过全量校准 ⇒ 必须先来一次全量(否则本域从来没有完整基线);
//	② 距上次全量校准超过 maxAgeDays ⇒ 定期校准。
//
// # 为什么必须有 ②
//
//	增量只保证"**不漏**" —— 但**任何一次**故障漏掉的行都会**一直缺着**
//	(下次增量只导"变过的行",缺的那些行既没变过、也不会被再导)。
//	⇒ 没有定期全量,一次故障就变成**永久漂移**,而且没有任何报错。
//
// ⚠️ maxAgeDays = 0 ⇒ 不限期(只做 ①)。
func (w *WatermarkStore) ShouldFullRefresh(scope string) bool {
	if !w.Enabled() {
		return true
	}
	all, err := w.load()
	if err != nil {
		// 读不到 ⇒ 不知道上次校准过没有 ⇒ 当成"从没校准" ⇒ 走全量(安全方向)。
		return true
	}
	raw := strings.TrimSpace(all.FullSyncAt[scope])
	if raw == "" {
		return true
	}
	last, err := time.ParseInLocation(archivedto.SinceLayout, raw, time.Local)
	if err != nil {
		return true
	}
	if w.maxAgeDays <= 0 {
		return false
	}
	return w.now().Sub(last) > time.Duration(w.maxAgeDays)*24*time.Hour
}

// Now 返回"本次导出的起点"(= 水位该记的值)。
//
// ⚠️ **回退 overlap 秒**:同一秒内的多次更新,`gt 水位`会漏掉后一次
// ⇒ 回退几秒 ⇒ 那一秒会被重导一次(upsert 幂等 ⇒ 无害)。
func (w *WatermarkStore) Now() string {
	return w.NowAt(0)
}

// NowAt 返回"本次导出的起点"再减去给定的**源数据滞后**(秒)。
//
// # 为什么要有这个"滞后"参数
//
//	我们的水位语义是"**我导到哪了**",而 since 的语义是"**源数据最后变动的时刻**"。
//	两者**只在源数据是实时的**时候才相等。
//
//	源数据是 cube 的**离线 DuckDB 快照**(每 30 分钟增量刷新一次)时:
//	我们导出那一刻,cube 里的数据可能还是 30 分钟前的
//	⇒ 水位若按"导出时刻"记,那 30 分钟里的变更就**永远落在水位之前**
//	⇒ **永久漏**,且报告照样写"成功 N 条"。
//
// ⇒ 水位 = 导出起点 − 源数据滞后
// ⇒ 下一轮的窗口 = (上一轮导出起点 − 滞后, ∞)
// ⇒ 窗口**连续**地覆盖了源的刷新延迟 ⇒ 不漏。
//
// ⚠️ 滞后取**小于源的刷新间隔** ⇒ 漏数据,且零报错。
//   取大一点没有代价(窗口宽 ⇒ 重复导的行多 ⇒ 幂等 ⇒ 无害)。
func (w *WatermarkStore) NowAt(lagSeconds int) string {
	if w == nil {
		return ""
	}
	if lagSeconds < 0 {
		lagSeconds = 0
	}
	return w.now().Add(-time.Duration(w.overlapSec+lagSeconds) * time.Second).
		Format(archivedto.SinceLayout)
}

// update 读-改-写水位文件。
func (w *WatermarkStore) update(fn func(all *watermarkFile)) {
	if !w.Enabled() {
		return
	}
	all, err := w.load()
	if err != nil {
		all = &watermarkFile{}
	}
	if all.Watermarks == nil {
		all.Watermarks = map[string]string{}
	}
	if all.FullSyncAt == nil {
		all.FullSyncAt = map[string]string{}
	}
	fn(all)
	_ = w.save(all)
}

// load 读水位文件。
func (w *WatermarkStore) load() (*watermarkFile, error) {
	b, err := os.ReadFile(w.path)
	if err != nil {
		return nil, err
	}
	out := &watermarkFile{}
	if err := json.Unmarshal(b, out); err != nil {
		return nil, fmt.Errorf("水位文件解析失败(%s): %w", w.path, err)
	}
	if out.Watermarks == nil {
		out.Watermarks = map[string]string{}
	}
	if out.FullSyncAt == nil {
		out.FullSyncAt = map[string]string{}
	}
	return out, nil
}

// save 写水位文件(原子写:先写临时文件再 rename ⇒ 进程被杀不会留半个文件)。
func (w *WatermarkStore) save(all *watermarkFile) error {
	if dir := filepath.Dir(w.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// 排序后序列化 ⇒ 文件内容稳定(便于 diff 与排障)。
	b, err := json.MarshalIndent(sortedWatermarkFile(all), "", "  ")
	if err != nil {
		return err
	}
	tmp := w.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, w.path)
}

// sortedWatermarkFile 把两个 map 的键排好序,再重新装回去。
//
// ⚠️ Go 的 map 遍历顺序是**随机**的 ⇒ 不排序的话每写一次文件内容就变
//   ⇒ diff 噪声大,排障时根本看不出"到底哪条水位变了"。
func sortedWatermarkFile(in *watermarkFile) *watermarkFile {
	return &watermarkFile{
		Watermarks: sortedMap(in.Watermarks),
		FullSyncAt: sortedMap(in.FullSyncAt),
	}
}

func sortedMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[k] = in[k]
	}
	return out
}
