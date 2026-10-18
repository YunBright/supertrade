package collect

import (
	"context"
	"strings"

	"github.com/YunBright/supertrade/internal/salesdaily"
)

// branchJobRegistrar 是 JobRegistrar 的生产实现（就地注册/删除某门店的 job）。
//
// # 为什么"开启"要就地注册，而不是等下一次对账
//
// 对账是定时跑的。若开启后要等到下一次对账才生效，用户会看到
// "我明明点了开启，job 数没变" —— 而正确行为与错误行为在界面上完全一样。
//
// ⚠️ 就地注册失败**不撤销**已保存的凭据：
//
//	凭据已经启用、对账也会注册它，立刻返回错误并回滚只会让状态更乱
//	（凭据说"开了"，界面说"失败"）。正确处理是报出"凭据已启用但任务注册失败，
//	请重试"，让对账去补齐。
type branchJobRegistrar struct {
	jobs    JobClient
	address string
}

// NewBranchJobRegistrar 构造。jobs 为 nil 时返回 nil（裸跑无 sidecar）。
func NewBranchJobRegistrar(jobs JobClient, address string) JobRegistrar {
	if jobs == nil {
		return nil
	}
	return &branchJobRegistrar{jobs: jobs, address: strings.TrimRight(strings.TrimSpace(address), "/")}
}

var _ JobRegistrar = (*branchJobRegistrar)(nil)

// crons 是采集的两个时刻，顺序与 Reconcile 保持一致。
func crons() ([]string, error) {
	initial, err := CronExpr(InitialCollectTime)
	if err != nil {
		return nil, err
	}
	corrected, err := CronExpr(CorrectedCollectTime)
	if err != nil {
		return nil, err
	}
	return []string{initial, corrected}, nil
}

// RegisterBranchJobs 实现 JobRegistrar。
func (r *branchJobRegistrar) RegisterBranchJobs(ctx context.Context, branchID string) error {
	if r == nil || r.jobs == nil {
		return nil
	}
	schedule, err := crons()
	if err != nil {
		return err
	}
	clientID, err := ClientIDFor(branchID)
	if err != nil {
		return err
	}
	sources := []salesdaily.CollectSource{
		salesdaily.CollectSourceInitial,
		salesdaily.CollectSourceCorrected,
	}
	for i, src := range sources {
		name := JobName(branchID, string(src))
		data := map[string]any{
			"trigger":          "dapr-job",
			"job":              name,
			"branch":           branchID,
			"source":           string(src),
			"service_account":  clientID,
		}
		if err := r.jobs.Register(ctx, name, schedule[i], r.address, data); err != nil {
			return err
		}
	}
	return nil
}

// DeleteBranchJobs 实现 JobRegistrar。
func (r *branchJobRegistrar) DeleteBranchJobs(ctx context.Context, branchID string) error {
	if r == nil || r.jobs == nil {
		return nil
	}
	for _, src := range []salesdaily.CollectSource{
		salesdaily.CollectSourceInitial,
		salesdaily.CollectSourceCorrected,
	} {
		if err := r.jobs.Delete(ctx, JobName(branchID, string(src))); err != nil {
			return err
		}
	}
	return nil
}
