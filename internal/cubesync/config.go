// Package cubesync —— cube → catalog 档案同步器。
//
// # 它是"编排方"
//
// 导出归 cube-router(它知道 branch → cube 实例的路由),
// 导入归 catalog(它拥有 products / categories / product_barcodes / suppliers 表),
// 本包**只做编排**:拉数据 → 整形 → 写进去 → 出报告。
//
// ⇒ 好处:本包**不需要连任何数据库**。它只需要两个下游的地址 + 一个服务账号。
//
// # 配置全部走环境变量
//
// 门店**不写死在代码里**:源门店与目标门店是两个变量,
// 同一份二进制既能跑 "00 → 00 定时覆盖",也能跑 "00 → 10 一次性初始化"。
//
// # 为什么自己实现 token / job client,而不复用 salesagg/collect
//
// 那一包是 salesagg 的内部实现(采集凭据 + 采集 job),
// 耦合进来会让 cube-sync 依赖一个不相干的域。
// 两处逻辑都很短,各自实现反而更清楚。
package cubesync

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/archivedto"
)

// Config 是同步器的运行配置。
type Config struct {
	// SourceBranchID 是**源门店**(cube 侧)。它的 cube 源由 cube-router 的
	// branch_cube_sources 决定(门店 00 → cube-sixun-ysx-fb)。
	SourceBranchID string
	// TargetBranchID 是**目标门店**(catalog 侧)。
	TargetBranchID string
	// CubeRouterAppID 是导出方的 **dapr app-id**(supertrade-cube-router)。
	//
	// ⚠️ 它是**寻址用**的 dapr app-id,不是 JWT 的 aud。
	// aud 用的是被调服务的 cmdbootstrap AppID("cube-router"/"catalog"),
	// 两者由 auth 的 jwt.service_audience 一次性覆盖(见 token.go 的注释)。
	CubeRouterAppID string
	// CatalogAppID 是导入方的 **dapr app-id**(supertrade-catalog)。
	CatalogAppID string
	// PricingAppID 是**价格域**导入方的 dapr app-id(supertrade-pricing)。
	//
	// ⚠️ 售价与促销**不归 catalog** —— catalog.products 不存价(两个真相源)。
	// ⇒ 它们走另一个下游,见 Owner 的路由表。
	PricingAppID string
	// InventoryAppID 是**库存域**导入方的 dapr app-id(supertrade-inventory)。
	//
	// ⚠️ 库存快照归 inventory（它拥有 stock_levels 表）⇒ 第三个归属方。
	InventoryAppID string
	// MemberAppID 是**会员域**导入方的 dapr app-id(supertrade-member)。
	//
	// ⚠️ 会员档案归 member（它拥有 members / member_types 表）⇒ 第四个归属方。
	MemberAppID string
	// Scopes 是本次同步的资料域(**按依赖顺序**)。
	Scopes []string
	// IncrementalScopes 是**支持增量**的资料域(只导"上次同步之后变过"的行)。
	//
	// ⚠️ **白名单,不是黑名单** —— 只有列在这里的 scope 才带 since 水位:
	//
	//	- 支持增量的 scope(cube 侧有"最后变动时间"维度)⇒ 带 since;
	//	- 不支持的 ⇒ **绝不能带** ⇒ 带了就变成"只导一部分却推进了水位"
	//	  ⇒ 下次漏数据,而且**全程不报错**。
	//
	// ⚠️ 实测(2026-10-22,源库 ysx/hbposv10):
	//
	//	scope        源表行数   一天变动   变动时间戳
	//	stock        23837      91        t_im_branch_stock.oper_date ✅
	//	products     27569      4         t_bd_item_info.modify_date ✅(但 cube 未暴露该维度)
	//	prices       27569      4         同上
	//	promotions   31371      -         ❌ 无 modify_date(但按生效期只剩 741 行)
	//	members      4133       5         t_rm_vip_info.modify_date ✅(但 cube 未暴露该维度)
	//
	// ⇒ 目前**只有 stock** 能真增量(cube 的 stock model 暴露了 stock.updated_at)。
	//   products/members 要增量得先给 cube 的 model 加"最后修改时间"维度。
	//
	// ⚠️ **默认空** —— 没配就全部走全量(旧行为)。
	//   ⇒ 上线时"忘了配"的后果是"慢",不是"漏数据"。
	IncrementalScopes []string
	// WatermarkFile 是水位文件路径(相对工作目录)。
	//
	// ⚠️ 水位是**搬运方**的状态(它知道"我上次搬到哪里")⇒ 放编排器自己这儿,
	//   编排器"不连任何数据库"的设计原则不受影响。
	//   - 文件丢失/损坏 ⇒ 读不到水位 ⇒ **退化成全量**(安全方向);
	//   - 多实例 ⇒ 各存各的 ⇒ 最多重复搬,不会漏(重复是幂等 upsert)。
	//
	// ⚠️ 空 ⇒ **不启用水位** ⇒ 所有 scope 都走全量(安全默认)。
	WatermarkFile string
	// WatermarkOverlapSeconds 是水位的**回退秒数**(防"同秒内多次更新"漏掉)。
	//
	// ⚠️ 实测源库的 oper_date 精确到秒 ⇒ 同一秒内的两次更新,
	//   "gt 上次水位"会漏掉后一次 ⇒ 水位回退几秒 ⇒ 那一秒会被**重导一次**
	//   (upsert 幂等 ⇒ 无害)。
	//
	// ⚠️ 0 是**合法值**(不要"回退")—— 允许零值的读取见 envIntAllowZero。
	WatermarkOverlapSeconds int
	// WatermarkMaxAgeDays 是"距上次**全量校准**"的最长天数:超了就强制全量。
	//
	// ⚠️ 这是"一次漏了永久漂移"的兜底:增量只保证"不漏",
	//   但**任何一次故障漏掉的行都会一直缺**,除非定期全量校准一次。
	//   0 = 不限(不建议)。
	WatermarkMaxAgeDays int
	// IncrementalLag 是**按 scope 覆盖**源数据滞后(秒)。
	//
	// ⚠️ 默认值来自 archivedto.ChangedMemberLag(当前全部为 0,
	//   因为 cube 去 DuckDB 后所有 model 都是 live ⇒ 源数据实时)。
	//   ⇒ 这个覆盖项是给"将来某个 scope 引入了缓存/物化"准备的:
	//   那时只要在这里配一行,水位就会自动回退,不会漏数据。
	//
	// ⚠️ 配**小于**源的刷新间隔 ⇒ 漏数据,且零报错 ——
	//   这是"宁可多导一万倍,也不能漏一行"的又一个实例。
	IncrementalLag map[string]int
	// ForceFull 是"本轮**无视水位**,强制全量"。
	//
	// ⚠️ 只给**人工触发**用(定时回调不接受这个开关):
	//   它会改"今晚搬什么" ⇒ 必须是显式的一次性决定,不能藏在定时任务里。
	//
	// # 为什么需要它(而不是"删水位文件")
	//
	//	① 换门店 / 重建目标库 ⇒ 增量**导不出基础数据**
	//	   (增量只导"变过的行",没变过的行永远不会出现)
	//	   ⇒ 必须显式跑一次全量。
	//	② 怀疑数据漂了 ⇒ 需要"立刻全量对齐",而不是等 7 天的定期校准。
	//
	// ⚠️ 强制全量**也会刷新"上次全量校准"时间** ⇒ 定期校准的计时从这一轮重新开始
	//   (语义正确:确实刚做过全量)。
	ForceFull bool
	// JobPrefix 是 dapr job 名的前缀(SYNC_JOB_PREFIX)。
	//
	// ⚠️ **必须与其它编排器不同名**:dapr scheduler 的 job 按名字 upsert,
	// 同名 ⇒ 两个编排器互相覆盖 ⇒ "只有一个在跑"且没有任何日志会说这件事。
	// 空 = 用 DefaultJobPrefix("cube-sync")。
	JobPrefix string
	// Mode 是 incremental / overwrite。
	Mode string
	// Cron 是 dapr job 的调度表达式(**6 字段,UTC**)。
	//
	// ⚠️ **6 字段**(秒 分 时 日 月 周)。写 5 字段会被 dapr 直接拒
	// (HTTP 500 "expected exactly 6 fields, found 5"),而**主 job 不受影响** ——
	// 症状是"夜间校准静默消失",增量漏的数据永远没兜底。
	//
	// ⚠️ dapr scheduler **按 UTC 解释** cron(调度器容器是 distroless,读不到 TZ)。
	// 而用户脑子里的"每天凌晨 3 点"是**本地时间** ——
	// 差 8 小时,而症状只是"同步时间不对",没有任何报错。
	// ⇒ 配置里可以直接写本地时刻 "03:30",由 normalizeCron 换算成 UTC。
	Cron string
	// CronSpec 是配置里的**原始**调度表达式(只用于日志/报告展示)。
	CronSpec string
	// PageSize 是导出时每页行数。
	PageSize int
	// JobToken 是 job 回调的**共享密钥**。
	//
	// ⚠️ dapr scheduler 回调**不带 JWT** ⇒ 回调端点必须落在 cmdbootstrap 的
	// **免鉴权窗口**(RegisterDapr)。而免鉴权窗口意味着"同网段任何主机都能 POST"。
	// ⇒ 密钥是唯一的收口:job data 里带一份,回调时比对。
	// 不比对 ⇒ 谁都能触发一次全量覆盖导入。
	JobToken string
	// ServiceAccountID / ServiceAccountSecret 是机器身份(client credentials)。
	ServiceAccountID     string
	ServiceAccountSecret string
	// AuthAppID 是签发 token 的 auth 引擎 dapr app-id。
	AuthAppID string
	// CallbackHost / CallbackPort 是 job 回调打到的地址。
	//
	// ⚠️ 用 **app 端口**(APP_PORT),不是 sidecar 端口(DAPR_HTTP_PORT):
	// 后者是 sidecar 的,填进去会打到自己都打不通的地方。
	CallbackHost string
	CallbackPort string
	// SidecarBase 是本机 sidecar 的 HTTP 根地址(空 = 没有 sidecar)。
	SidecarBase string
}

// 默认值。门店的默认值取 **00**(思迅云商x 那个店)——
// 它是当前唯一有 cube 源的门店;目标门店默认同为 00(即"覆盖式同步")。
const (
	DefaultSourceBranchID = "00"
	DefaultTargetBranchID = "00"
	DefaultCubeRouterApp  = "supertrade-cube-router"
	DefaultCatalogApp     = "supertrade-catalog"
	DefaultPricingApp     = "supertrade-pricing"
	DefaultInventoryApp   = "supertrade-inventory"
	DefaultMemberApp      = "supertrade-member"
	DefaultMode           = "overwrite"
	// DefaultJobPrefix 是 dapr job 名的默认前缀。
	DefaultJobPrefix = "cube-sync"
	// DefaultCron 是**本地时刻**写法,由 normalizeCron 换算成 UTC 6 字段。
	DefaultCron      = "03:30"
	DefaultPageSize  = 5000
	DefaultAuthAppID = "userd"
	// DefaultWatermarkOverlapSeconds 是水位的默认回退秒数。
	//
	// ⚠️ 5 秒:实测源库的 oper_date 精确到秒 ⇒ 同一秒内的两次更新,
	//   "gt 上次水位"会漏掉后一次 ⇒ 回退几秒 ⇒ 那一秒会被重导一次
	//   (upsert 幂等 ⇒ 无害)。
	DefaultWatermarkOverlapSeconds = 5
	// DefaultWatermarkMaxAgeDays 是"距上次全量校准"的默认天数。
	//
	// ⚠️ 7 天:增量只保证"不漏",但**任何一次**故障漏掉的行都会一直缺着
	//   ⇒ 必须定期全量校准一次。7 天 = 一周一次全量(实测 7 秒,
	//   且分批导入后不影响 POS 延迟)。
	DefaultWatermarkMaxAgeDays = 7
)

// DefaultScopes 是默认同步的资料域。
//
// ⚠️ 顺序有讲究:分类与供应商是商品的引用对象,条码引用商品。
// catalog 的导入端点也按这个顺序处理(先引用方后被引用方)。
//
// ⚠️ **prices / promotions 归 pricing 服务**(不是 catalog)——
//
//	一个 scope 一个归属方,路由表见 cubesync.Owner。
var DefaultScopes = []string{
	archivedto.ScopeCategories,
	archivedto.ScopeSuppliers,
	archivedto.ScopeProducts,
	archivedto.ScopeItemBarcodes,
	archivedto.ScopePrices,
	archivedto.ScopePromotions,
	// ⚠️ 会员 scope **排最后**:一个 scope 失败就整体中止(见 syncer.go),
	//   而会员导入依赖 auth 侧给 cube-sync 服务账号授 member:config ——
	//   放最后 ⇒ 万一没授权,商品/价格/库存那几轮**已经导完了**,
	//   不会因为会员这一段把整轮同步带崩。
	//
	// ⚠️ member_types 在 members **之前**:会员引用类别。
	archivedto.ScopeMemberTypes,
	archivedto.ScopeMembers,
}

// DefaultCatalogScopes 是 catalog 拥有的 scope。
func DefaultCatalogScopes() []string {
	return []string{
		archivedto.ScopeCategories,
		archivedto.ScopeSuppliers,
		archivedto.ScopeProducts,
		archivedto.ScopeItemBarcodes,
	}
}

// DefaultPricingScopes 是 pricing 拥有的 scope。
func DefaultPricingScopes() []string {
	return []string{
		archivedto.ScopePrices,
		archivedto.ScopePromotions,
	}
}

// DefaultInventoryScopes 是 inventory 拥有的 scope。
//
// ⚠️ **库存快照是时效数据**：导出来的是「同步时刻」的库存
// （cube 的 `stock` 是 live model，透传思迅源库当前值）⇒ **不带时间过滤**。
func DefaultInventoryScopes() []string {
	return []string{
		archivedto.ScopeStock,
	}
}

// DefaultMemberScopes 是 member 拥有的 scope。
//
// ⚠️ **顺序有讲究**：member_types 在 members **之前** ——
// 会员引用类别（members.card_type_id → member_types.id），
// 反过来就是"引用了还不存在的对象"。
//
// ⚠️ 会员档案**不是时效数据**（是"当前状态"而不是流水）⇒ 导出不带时间过滤。
func DefaultMemberScopes() []string {
	return []string{
		archivedto.ScopeMemberTypes,
		archivedto.ScopeMembers,
	}
}

// LoadConfig 从环境变量读配置。
//
// ⚠️ **服务账号缺失不报错** —— 让服务能起来并把"没配凭据"这件事
// 在第一次同步时以清晰的错误报出来;启动就退出的话,
// 症状只是"进程反复重启",看不出缺什么。
//
// ⚠️ 但**回调共享密钥缺失要报错**(见 Validate):那个端点在免鉴权窗口,
// 没有密钥就等于对整个内网开放。
func LoadConfig() Config {
	c := Config{
		SourceBranchID:       envStr("SYNC_SOURCE_BRANCH_ID", DefaultSourceBranchID),
		TargetBranchID:       envStr("SYNC_TARGET_BRANCH_ID", DefaultTargetBranchID),
		CubeRouterAppID:      envStr("CUBE_ROUTER_APP_ID", DefaultCubeRouterApp),
		CatalogAppID:         envStr("CATALOG_APP_ID", DefaultCatalogApp),
		PricingAppID:         envStr("PRICING_APP_ID", DefaultPricingApp),
		InventoryAppID:       envStr("INVENTORY_APP_ID", DefaultInventoryApp),
		MemberAppID:          envStr("MEMBER_APP_ID", DefaultMemberApp),
		Scopes:               envList("SYNC_SCOPES", DefaultScopes),
		// ⚠️ **增量白名单默认空** —— 没配就全走全量(旧行为)。
		//   ⇒ "忘了配"的后果是慢,不是漏数据。
		IncrementalScopes:       envList("SYNC_INCREMENTAL_SCOPES", nil),
		WatermarkFile:           envStr("SYNC_WATERMARK_FILE", ""),
		WatermarkOverlapSeconds: envIntAllowZero("SYNC_WATERMARK_OVERLAP_SECONDS", DefaultWatermarkOverlapSeconds),
		WatermarkMaxAgeDays:     envIntAllowZero("SYNC_WATERMARK_MAX_AGE_DAYS", DefaultWatermarkMaxAgeDays),
		JobPrefix:               envStr("SYNC_JOB_PREFIX", DefaultJobPrefix),
		Mode:                    envStr("SYNC_MODE", DefaultMode),
		CronSpec:                envStr("SYNC_CRON", DefaultCron),
		PageSize:                envInt("SYNC_PAGE_SIZE", DefaultPageSize),
		JobToken:                strings.TrimSpace(os.Getenv("SYNC_JOB_TOKEN")),
		ServiceAccountID:        strings.TrimSpace(os.Getenv("AUTH_SERVICE_CLIENT_ID")),
		ServiceAccountSecret:    strings.TrimSpace(os.Getenv("AUTH_SERVICE_CLIENT_SECRET")),
		AuthAppID:               envStr("AUTH_TOKEN_APP_ID", DefaultAuthAppID),
		CallbackHost:            envStr("SYNC_CALLBACK_HOST", "127.0.0.1"),
		CallbackPort:            strings.TrimPrefix(envStr("APP_PORT", ":8116"), ":"),
	}
	c.Cron = c.CronSpec
	if norm, err := normalizeCron(c.CronSpec); err == nil {
		c.Cron = norm
	}
	c.SidecarBase = sidecarBaseURL()
	return c
}

// Validate 校验配置是否可用。
func (c Config) Validate() error {
	if c.SourceBranchID == "" {
		return fmt.Errorf("cubesync: SYNC_SOURCE_BRANCH_ID 不能为空")
	}
	if c.TargetBranchID == "" {
		return fmt.Errorf("cubesync: SYNC_TARGET_BRANCH_ID 不能为空")
	}
	if len(c.Scopes) == 0 {
		return fmt.Errorf("cubesync: SYNC_SCOPES 不能为空")
	}
	if c.Mode != "incremental" && c.Mode != "overwrite" {
		return fmt.Errorf("cubesync: SYNC_MODE=%q 非法(只能是 incremental / overwrite)", c.Mode)
	}
	// ⚠️ cron 必须是 6 字段 —— 写 5 字段 dapr 直接 500,
	// 而主 job 不受影响 ⇒ "夜间校准静默消失"。
	if _, err := normalizeCron(c.CronSpec); err != nil {
		return fmt.Errorf("cubesync: SYNC_CRON 非法: %w", err)
	}
	if c.ServiceAccountID == "" || c.ServiceAccountSecret == "" {
		return fmt.Errorf("cubesync: 缺服务账号(AUTH_SERVICE_CLIENT_ID / AUTH_SERVICE_CLIENT_SECRET)")
	}
	if c.JobToken == "" {
		return fmt.Errorf("cubesync: 缺 SYNC_JOB_TOKEN(回调端点在免鉴权窗口,没有共享密钥就等于谁都能触发同步)")
	}
	// ⚠️ 增量白名单里的 scope **必须**在 cube 侧有"最后变动时间"成员。
	//
	//	否则:导出端会**忽略** since(仍全量导出)⇒ 而水位被推进了
	//	⇒ 下次只导"最近变过的一部分"却当成全量 ⇒ **漏数据,且全程零报错**。
	//
	// ⇒ 这条校验把"配错一个 scope 名"从**永久漏数据**变成**启动失败**。
	for _, sc := range c.IncrementalScopes {
		if !archivedto.SupportsIncremental(sc) {
			return fmt.Errorf("cubesync: SYNC_INCREMENTAL_SCOPES 里的 %q 不支持增量"+
				"(导出端没有该 scope 的'最后变动时间'成员 ⇒ 带上 since 会被忽略而水位仍被推进 ⇒ 漏数据);"+
				"当前可用的增量 scope:%v", sc, archivedto.IncrementalScopes())
		}
	}
	return nil
}

// CallbackAddress 是 job 回调的完整地址。
func (c Config) CallbackAddress(path string) string {
	return "http://" + c.CallbackHost + ":" + c.CallbackPort + path
}

// JobCallbackURL 是 dapr 回调打到的完整地址。
//
// ⚠️ **登记在案而已** —— dapr 的 HTTP app channel **不读 job spec 的
// address 字段**(见 handler.go 里 JobCallbackPath 的注释:
// pkg/runtime/scheduler/internal/cluster/streamer.go → invokeApp 只传 name + data)。
// 真正生效的路径恒为 /job/<job 名>。
//
// 这里仍然填成**真实路径**,是为了让"读 job spec 的人"看到真相,
// 而不是让它继续误导下一个改这里的人。
func (c Config) JobCallbackURL() string {
	return c.CallbackAddress(JobCallbackPath(c.JobName()))
}

// JobName 是本同步任务的 job 名。
//
// ⚠️ 门店 id 会被写进 job 名,而 job 名会出现在 dapr 的 URL 路径里,
// 所以必须做**文件名安全性**处理 —— 只挡 "/" 不够,
// "\"、"?"、"#" 在 URL 与文件系统里同样能改变解析结果。
// (与 salesagg/collect.JobName 同款处理,理由同款。)
//
// ⚠️ 前缀可配(SYNC_JOB_PREFIX):cube-sync 与 cube-async 是两个编排器,
// 它们的 job 必须**不同名** —— 同名会互相覆盖调度(实测 dapr scheduler
// 的 job 是按名字 upsert 的)⇒ 结果是"两个编排器只有一个在跑",
// 而且没有任何一行日志会说这件事。
func (c Config) JobName() string {
	prefix := strings.TrimSpace(c.JobPrefix)
	if prefix == "" {
		prefix = DefaultJobPrefix
	}
	return prefix + "-" + sanitizeID(c.SourceBranchID) + "-" + sanitizeID(c.TargetBranchID)
}

// sanitizeID 把 id 压成可以安全出现在 URL 路径与文件名里的形态。
func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "_"
	}
	return out
}

// normalizeCron 把配置里的调度表达式规整成 dapr 要的 **6 字段 UTC cron**。
//
// # 两种写法都接受
//
//	"03:30"        → **本地时刻**,换算成 UTC("0 30 19 * * *")
//	"0 30 3 * * *"  → 已经是 6 字段,**原样使用**(按 UTC 解释)
//
// # 为什么要有"本地时刻"这一档
//
// dapr scheduler 的调度器容器是 distroless(读不到 TZ)⇒ **cron 按 UTC 解释**。
// 而"每天凌晨 3 点"在人嘴里是**本地时间**。
// 差 8 小时 ⇒ 同步在中午 11:30 跑,症状只是"时间不对",没有任何报错。
//
// ⇒ 默认值写成本地时刻 "03:30",由这里换算。
// 真要写 UTC 原始 cron 也可以,两种都合法。
func normalizeCron(v string) (string, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return "", fmt.Errorf("调度表达式不能为空")
	}
	if !strings.Contains(s, ":") {
		if len(strings.Fields(s)) != 6 {
			return "", fmt.Errorf("cron 必须是 **6 字段**(秒 分 时 日 月 周),收到 %q", s)
		}
		return s, nil
	}
	h, m, err := parseHM(s)
	if err != nil {
		return "", err
	}
	_, offsetSec := time.Now().Zone()
	offsetMin := offsetSec / 60
	localMin := h*60 + m
	// mod 1440 取到 [0,1440):负偏移(西区)必须 ((x%1440)+1440)%1440 才不会出负数。
	utcMin := ((localMin-offsetMin)%1440 + 1440) % 1440
	return fmt.Sprintf("0 %d %d * * *", utcMin%60, utcMin/60), nil
}

// parseHM 把 "HH:MM" 拆成时、分。
//
// ⚠️ 严格性用**回写比对**保证,而不是靠 time.Parse:
// `time.Parse("15:04", "9:45")` 是**成功**的(Go 的解析器对该布局容忍单数字小时),
// 于是配置里会同时存在 "9:45" 与 "09:45" 两种写法。
// → 在入口把格式收紧到唯一一种,之后的代码才不用到处防御。
func parseHM(v string) (int, int, error) {
	s := strings.TrimSpace(v)
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, fmt.Errorf("时刻格式应为 HH:MM(00-23:00-59),收到 %q", v)
	}
	h, err := twoDigits(s[:2])
	if err != nil {
		return 0, 0, err
	}
	m, err := twoDigits(s[3:])
	if err != nil {
		return 0, 0, err
	}
	if h > 23 || m > 59 {
		return 0, 0, fmt.Errorf("时刻超出范围,收到 %q", v)
	}
	return h, m, nil
}

func twoDigits(s string) (int, error) {
	if len(s) != 2 {
		return 0, fmt.Errorf("不是两位")
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("非数字")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// envStr 取环境变量,空时回落默认值。
func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// envInt 取整数环境变量,非法时回落默认值。
func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// envIntAllowZero 取整数环境变量,**允许 0**(非法或负数时回落默认值)。
//
// ⚠️ 为什么不能复用 envInt:envInt 把 0 当"非法"⇒ 回落默认值。
//
//	而"回退 0 秒"与"最长有效期 0 天(不限)"都是**合法且有意义的**配置:
//	  - 回退 0 秒 = 明确不要回退;
//	  - 有效期 0 天 = 明确不做定期全量校准。
//	⇒ 用 envInt 读它们,配置写 0 会被悄悄换成 5 / 7,
//	  而症状只是"行为与配置不一致",没有任何报错。
func envIntAllowZero(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

// envList 取逗号分隔的环境变量。
func envList(key string, def []string) []string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}
