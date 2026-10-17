package handler

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/YunBright/supertrade/internal/salesinsight/build"
	"github.com/YunBright/supertrade/internal/salesinsight/calendar"
	"github.com/YunBright/supertrade/internal/salesinsight/fact"
	"github.com/YunBright/supertrade/internal/salesinsight/insight"
	"github.com/YunBright/supertrade/internal/salesinsight/weather"
)

// BranchNamer 把门店 ID 解析成名称。
type BranchNamer interface {
	Name(ctx context.Context, branchID string) (string, bool, error)
}

// ServiceImpl 是 handler.Service 的实现。
type ServiceImpl struct {
	builder *build.Builder
	engine  *insight.Engine
	// branchName 解析：把 X-Branch-ID 变成可读名称（可选）。
	branchNames BranchNamer
	now         func() time.Time
}

// NewServiceImpl 构造。
func NewServiceImpl(b *build.Builder, e *insight.Engine) *ServiceImpl {
	return &ServiceImpl{
		builder: b,
		engine:  e,
		now:     time.Now,
	}
}

// WithBranchNamer 注入门店名解析。
func (s *ServiceImpl) WithBranchNamer(n BranchNamer) *ServiceImpl {
	s.branchNames = n
	return s
}

// WithNow 注入时钟。
func (s *ServiceImpl) WithNow(f func() time.Time) *ServiceImpl {
	if f != nil {
		s.now = f
	}
	return s
}

// Generate 生成一期报告。
//
// # 步骤
//
//  1. 解析周期（含同期口径）
//  2. 组装事实包
//  3. 取上期摘要（可选）
//  4. 交给推理引擎（内部自带降级）
func (s *ServiceImpl) Generate(ctx context.Context, in GenerateInput) (insight.Insight, insight.Audit, error) {
	if s.builder == nil || s.engine == nil {
		return insight.Insight{}, insight.Audit{}, fmt.Errorf("service: 未装配（builder/engine 为 nil）")
	}

	period, err := calendar.ResolvePeriod(in.Kind, s.now())
	if err != nil {
		return insight.Insight{}, insight.Audit{}, err
	}

	branchName := ""
	if s.branchNames != nil {
		if n, ok, err := s.branchNames.Name(ctx, in.StoreBranchID); err == nil && ok {
			branchName = n
		}
		// ⚠️ 解析失败**不**报错：门店名只是卡片抬头上的一个词，
		//   为它让整份报告失败不划算。
	}

	opt := build.Options{
		StoreBranchID:   in.StoreBranchID,
		BranchName:      branchName,
		Period:          period,
		TopN:            in.TopN,
		Location:        locationFromEnv(),
		SelfUseKeywords: selfUseKeywordsFromEnv(),
	}

	f, err := s.builder.Build(ctx, opt)
	if err != nil {
		return insight.Insight{}, insight.Audit{}, err
	}

	// ---- 上期摘要 ----
	//
	// ⚠️ 来源是**调用方**（GenerateInput.PrevSummary）而不是本服务去读
	//   state store：那份数据归 wechat-bot 所有，本服务是无状态推理服务。
	//
	// 留空是正常分支（首个周期、或调用方没存摘要）：
	// prompt 会明确写"没有上一期的结论记录"，
	// 环比的数字部分由事实包的 Deltas 独立提供，不依赖它。
	prevSummary := in.PrevSummary
	if prevSummary != nil &&
		strings.TrimSpace(prevSummary.Summary) == "" && len(prevSummary.Highlights) == 0 {
		prevSummary = nil
	}

	ins, audit := s.engine.Analyze(ctx, insight.AnalyzeInput{
		Fact:        *f,
		PrevSummary: prevSummary,
	})
	// ⚠️ 无论降级与否都返回 nil error：
	//   "生成成功但用了规则模式"是一个**成功**的结果，
	//   调用方需要拿到结论去发卡片，而不是把它当成失败重试
	//   （重试只会再打一次 LLM，且大概率还是同样的降级）。
	return ins, audit, nil
}

// FactDigest 暴露事实包文本（调试与契约测试用）。
func (s *ServiceImpl) FactDigest(f *fact.Fact) string { return insight.FactDigest(*f) }

// locationFromEnv 从环境变量读门店位置。
//
// ⚠️ 没有默认坐标 —— 0/0 在几内亚湾，天气 API 会返回那里的天气
//
//	且**不报错**。所以坐标缺失时返回零值 Location，
//	weather.Location.Valid() 会判为无效，下游据此降级。
func locationFromEnv() weather.Location {
	lat, _ := strconv.ParseFloat(strings.TrimSpace(os.Getenv("STORE_LAT")), 64)
	lon, _ := strconv.ParseFloat(strings.TrimSpace(os.Getenv("STORE_LON")), 64)
	return weather.Location{
		Name: strings.TrimSpace(os.Getenv("STORE_CITY")),
		Lat:  lat,
		Lon:  lon,
	}
}

// selfUseKeywordsFromEnv 读自用关键词覆盖。
//
// ⚠️ 只有在**显式设置**时才覆盖默认值：
//
//	环境变量为空字符串时返回 nil（用默认表），
//	而返回空切片会禁用关键词规则 —— 那是一个更危险的语义，
//	不该由"没配这个变量"这种常见疏漏意外触发。
//
// 要禁用请用 `SELFUSE_KEYWORDS_DISABLED=1` 显式表达。
func selfUseKeywordsFromEnv() []string {
	raw := strings.TrimSpace(os.Getenv("SELFUSE_KEYWORDS"))
	if raw == "" {
		return nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '，' || r == ' ' || r == '\t' || r == '|'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
