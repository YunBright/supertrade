package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/salesdaily"
)

// 采集时刻。
//
// ⚠️ 首采 22:45 对应"用户早上 6:00–22:45 营业"：
//
//	早于 22:45 取到的是**半个营业日**，而它会被当成当天的事实落库。
//	这个数字与第二天补齐后的数字长得不一样，却没有任何标记 ——
//	所以宁可选一个业务上明确的收档时刻。
const (
	// InitialCollectTime 是当日首次采集时刻（收档）。
	InitialCollectTime = "22:45"
	// CorrectedCollectTime 是次日校正采集时刻。
	//
	// ⚠️ 不是 00:00 而是 01:05：思迅日结往往在午夜后几分钟才完成，
	//	而**刚补完的那几分钟正是数据最不稳定的时候**。
	//	多点余量换一次采到完整数据的概率，比省 65 分钟划算。
	//
	//	它校正的是**昨天**（见 PrevOperDate），不是今天。
	CorrectedCollectTime = "01:05"
)

// JobNamePrefix 是采集 job 的名字前缀。
//
// ⚠️ 必须是**独立**前缀，不能与推送 job 混用：
//
//	同名 POST 返回 409（见 Register 的注释），而"两个 job 共用名字"的表现是
//	其中一个永远注册不上去，而日志里另一条完全正常。
const JobNamePrefix = "sales-daily-collect"

// JobName 返回某个门店某个来源的 job 名。
//
// ⚠️ 门店 id 会被写进 job 名，而 job 名会出现在 dapr 的 URL 路径里，
//
//	所以必须做**文件名安全性**处理 —— 只挡 `/` 不够，
//	`\`、`?`、`#` 在 URL 与文件系统里同样能改变解析结果。
//	而门店 id 来自 auth（可被运营录入），不是可证的 uuid。
func JobName(branchID string, source string) string {
	return JobNamePrefix + "-" + sanitizeID(branchID) + "-" + source
}

// sanitizeID 把门店 id 压成可以安全出现在 URL 路径与文件名里的形态。
//
// ⚠️ 替换成 `_` 而不是"报错"：门店 id 是既有主键，
//
//	报错会让一个含特殊字符的门店**彻底采不到数据**，
//	而症状是"这个门店的月报永远缺一大截"。
//	替换后至少还能采，代价只是名字变难看（DisplayName 不受影响）。
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

// JobClient 是 dapr scheduler 的注册/删除能力。
type JobClient interface {
	Register(ctx context.Context, name, schedule, address string, data map[string]any) error
	Delete(ctx context.Context, name string) error
}

// DaprJobClient 经本机 sidecar 的 HTTP 调 dapr scheduler。
type DaprJobClient struct {
	sidecarBase string
	http        *http.Client
}

// NewDaprJobClient 构造。sidecarBase 为空表示没有 sidecar（裸跑）。
func NewDaprJobClient(sidecarBase string) *DaprJobClient {
	base := strings.TrimRight(strings.TrimSpace(sidecarBase), "/")
	if base == "" {
		return nil
	}
	return &DaprJobClient{
		sidecarBase: base,
		http:        &http.Client{Timeout: 10 * time.Second},
	}
}

var _ JobClient = (*DaprJobClient)(nil)

// jobBody 是 POST /v1.0-alpha1/jobs/<name> 的请求体。
type jobBody struct {
	Schedule string         `json:"schedule"`
	Data     map[string]any `json:"data,omitempty"`
	Address  string         `json:"address,omitempty"`
}

// Register 实现 JobClient。
//
// # ⚠️ 同名 POST 返回 **409**，不是 upsert
//
// ✅ MEASURED 2026-10-14（wechat-bot 生产）：同名 POST 返 409，
// 第二次调用（含进程重启）结果与第一次完全一致。
// → 所以必须**先 DELETE 再 POST**，否则每次启动都会失败。
func (c *DaprJobClient) Register(ctx context.Context, name, schedule, address string, data map[string]any) error {
	if c == nil {
		return nil
	}
	// ① 尽力删旧 job。错误**只记不返回**：它的唯一目的是给 POST 扫清 409，
	//    而 409 由 POST 自己如实报告。让清理步骤有能力否决整个流程，
	//    是 wechat-bot 第一次部署"DELETE 报 connection refused 就 return"的成因。
	_ = c.Delete(ctx, name)

	raw, err := json.Marshal(jobBody{Schedule: schedule, Data: data, Address: address})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.sidecarBase+"/v1.0-alpha1/jobs/"+name, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("collect: 注册 job %s 失败: %w", name, err)
	}
	defer drainBody(resp)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("collect: 注册 job %s 返回 %d", name, resp.StatusCode)
	}
	return nil
}

// Delete 实现 JobClient。
func (c *DaprJobClient) Delete(ctx context.Context, name string) error {
	if c == nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.sidecarBase+"/v1.0-alpha1/jobs/"+name, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("collect: 删除 job %s 失败: %w", name, err)
	}
	defer drainBody(resp)
	// 404 = 本来就没有，是 no-op（重注册场景的正常路径）。
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("collect: 删除 job %s 返回 %d", name, resp.StatusCode)
	}
	return nil
}

func drainBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

// CronExpr 把**本地**时刻换算成 dapr scheduler 用的 UTC cron。
//
// # ⚠️ 这段换算是整个定时链路里唯一会悄悄错掉的地方
//
// ✅ MEASURED 2026-10-14（wechat-bot 生产）：
//
//	① 调度器容器是 distroless（无 shell），读不到 TZ，**cron 按 UTC 解释**；
//	② **必须写满 6 段** `秒 分 时 日 月 周`，传 5 段被拒：
//	   `job failed validation: expected exactly 6 fields, found 5`。
//
// 写死本地时间的后果是"整整差 8 小时"，而症状只是"采集时间不对"，
// 没有任何报错 —— 所以换算必须集中在这里，任何地方都不许再把本地
// 时刻直接塞进 cron。
//
// # 为什么偏移量在注册时现算，而不是写死 +8
//
// 把"为什么是 8"写死在代码里，等于把部署位置焊死在实现里：
// 换个机房、换台机器就是错的，而错误表现仍然只是时间不对。
// 现算则跟着主机走，零维护成本。
func CronExpr(localHHMM string) (string, error) {
	h, m, err := parseHM(localHHMM)
	if err != nil {
		return "", err
	}
	_, offsetSec := time.Now().Zone()
	offsetMin := offsetSec / 60
	localMin := h*60 + m
	// mod 1440 取到 [0,1440)：负偏移（西区）必须 ((x%1440)+1440)%1440 才不会出负数。
	utcMin := ((localMin-offsetMin)%1440 + 1440) % 1440
	return fmt.Sprintf("0 %d %d * * *", utcMin%60, utcMin/60), nil
}

// parseHM 把 "22:45" 拆成时、分。
//
// ⚠️ 严格性用**回写比对**保证，而不是靠 time.Parse：
//
//	`time.Parse("15:04", "9:45")` 是**成功**的（Go 的解析器对该布局容忍
//	单数字小时），于是配置里会同时存在 "9:45" 与 "09:45" 两种写法，
//	而之后所有比较都要考虑两种。判据：**在入口把格式收紧到唯一一种**，
//	之后的代码才不用到处防御。
func parseHM(v string) (int, int, error) {
	s := strings.TrimSpace(v)
	h, m, err := parseTwoDigits(s)
	if err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("时刻格式应为 HH:MM（00-23:00-59），收到 %q", v)
	}
	if fmt.Sprintf("%02d:%02d", h, m) != s {
		return 0, 0, fmt.Errorf("时刻必须是两位数补零的 HH:MM，收到 %q", v)
	}
	return h, m, nil
}

func parseTwoDigits(s string) (int, int, error) {
	if len(s) != 5 || s[2] != ':' {
		return -1, -1, fmt.Errorf("长度/位置不对")
	}
	h, err := twoDigits(s[:2])
	if err != nil {
		return -1, -1, err
	}
	m, err := twoDigits(s[3:])
	if err != nil {
		return -1, -1, err
	}
	return h, m, nil
}

func twoDigits(s string) (int, error) {
	if len(s) != 2 {
		return -1, fmt.Errorf("不是两位")
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1, fmt.Errorf("非数字")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// ReconcileResult 是一次对账的结果。
type ReconcileResult struct {
	Branches  int
	Jobs      int
	Failed    []string
	Registers int
}

// Reconciler 按 auth 的在营门店清单，注册/重注册每家门店的两个采集 job。
//
// # dapr 没有 list API —— 所以孤儿 job 清不掉（已知限制）
//
// ✅ MEASURED 2026-10-14：`GET /v1.0-alpha1/jobs`（不带名字）返回
// 404 ERR_DIRECT_INVOKE，这一版没有列举形态。
//
// → 无法枚举"曾经给哪些门店建过 job"，所以门店停用后它的 job
// 不会被自动删除，会继续每天跑两次并（很可能）因该门店已从
// `branch_cube_sources` 摘掉而失败。
//
// **为什么接受**：代价是每天两次失败日志 + 两次无效 cube 调用；
// 而要修它就得引入一张"我们自己建过哪些 job"的表 ——
// 为此多一张表和一套删除逻辑，收益不抵。
// 真要清理，运维按 `sales-daily-collect-<门店>-initial` 手工 DELETE 即可。
type Reconciler struct {
	branches BranchLister
	jobs     JobClient
	// address 是 job 回调地址（app 自己的 127.0.0.1:port）。
	address string
}

// NewReconciler 构造。任一依赖为 nil 时 Reconcile 返回明确错误。
func NewReconciler(branches BranchLister, jobs JobClient, address string) *Reconciler {
	return &Reconciler{branches: branches, jobs: jobs, address: strings.TrimRight(strings.TrimSpace(address), "/")}
}

// Reconcile 对齐 job 与在营门店清单。
//
// ⚠️ 幂等：注册一律走 DELETE → POST，所以重复调用不会累积。
func (r *Reconciler) Reconcile(ctx context.Context) (ReconcileResult, error) {
	if r == nil || r.branches == nil {
		return ReconcileResult{}, fmt.Errorf("collect: 门店列表未配置")
	}
	if r.jobs == nil {
		return ReconcileResult{}, fmt.Errorf("collect: dapr scheduler 不可用（没有 sidecar）")
	}
	branches, err := r.branches.ListActiveBranches(ctx)
	if err != nil {
		return ReconcileResult{}, err
	}

	initialCron, err := CronExpr(InitialCollectTime)
	if err != nil {
		return ReconcileResult{}, err
	}
	correctedCron, err := CronExpr(CorrectedCollectTime)
	if err != nil {
		return ReconcileResult{}, err
	}

	res := ReconcileResult{Branches: len(branches)}
	for _, b := range branches {
		for _, spec := range []struct{ source, cron string }{
			{string(salesdaily.CollectSourceInitial), initialCron},
			{string(salesdaily.CollectSourceCorrected), correctedCron},
		} {
			name := JobName(b.ID, spec.source)
			data := map[string]any{
				"trigger": "dapr-job",
				"job":     name,
				"branch":  b.ID,
				"source":  spec.source,
			}
			if err := r.jobs.Register(ctx, name, spec.cron, r.address, data); err != nil {
				res.Failed = append(res.Failed, name)
				continue
			}
			res.Jobs++
			res.Registers++
		}
	}
	sort.Strings(res.Failed)
	return res, nil
}
