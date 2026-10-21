// Package cubesync —— 配置的单元测试。
//
// # 测什么
//
//	① cron 规整:6 字段原样 / "HH:MM" 本地时刻换算 / 5 字段与"9:45"必须被拒
//	② Validate:缺共享密钥必须拒启动(免鉴权窗口没有它等于对内网开放)
//	③ job 名:门店 id 含特殊字符时必须被压成安全形态
package cubesync

import (
	"strings"
	"testing"
	"time"
)

// TestNormalizeCron 锁 cron 规整。
func TestNormalizeCron(t *testing.T) {
	t.Run("6 字段原样", func(t *testing.T) {
		got, err := normalizeCron("0 30 3 * * *")
		if err != nil {
			t.Fatalf("不该报错: %v", err)
		}
		if got != "0 30 3 * * *" {
			t.Errorf("6 字段应原样保留,实际 %q", got)
		}
	})
	t.Run("5 字段被拒", func(t *testing.T) {
		// 症状:5 字段被 dapr 拒(HTTP 500 "expected exactly 6 fields"),
		// 而**主 job 不受影响** ⇒ 夜间校准静默消失。
		if _, err := normalizeCron("30 3 * * *"); err == nil {
			t.Error("5 字段 cron 必须报错")
		}
	})
	t.Run("本地时刻换算成 UTC", func(t *testing.T) {
		got, err := normalizeCron("03:30")
		if err != nil {
			t.Fatalf("不该报错: %v", err)
		}
		if len(strings.Fields(got)) != 6 {
			t.Fatalf("换算结果必须是 6 字段,实际 %q", got)
		}
		// 反向验证:把结果换算回本地时刻,应回到 03:30。
		if !isLocalTime(got, 3, 30) {
			t.Errorf("换算结果 %q 反推回本地时间不是 03:30(当前时区 %v)",
				got, time.Now().Location())
		}
	})
	t.Run("非补零时刻被拒", func(t *testing.T) {
		// time.Parse("15:04","9:45") 是**成功**的 ⇒ 放任会让配置里
		// 同时存在 "9:45" 与 "09:45" 两种写法,之后所有比较都要考虑两种。
		if _, err := normalizeCron("9:45"); err == nil {
			t.Error("\"9:45\" 必须被拒(要求两位数补零)")
		}
	})
	t.Run("空值被拒", func(t *testing.T) {
		if _, err := normalizeCron("  "); err == nil {
			t.Error("空 cron 必须报错")
		}
	})
}

// isLocalTime 判断一个 6 字段 cron(秒 分 时 ...)在本地时刻是不是 h:m。
func isLocalTime(cron string, h, m int) bool {
	f := strings.Fields(cron)
	if len(f) != 6 {
		return false
	}
	_, offsetSec := time.Now().Zone()
	offsetMin := offsetSec / 60
	utcMin := (h*60 + m - offsetMin) % 1440
	if utcMin < 0 {
		utcMin += 1440
	}
	return f[1] == itoa(utcMin%60) && f[2] == itoa(utcMin/60)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// TestValidate 锁配置校验。
func TestValidate(t *testing.T) {
	base := func() Config {
		return Config{
			SourceBranchID: "00", TargetBranchID: "10",
			Scopes: []string{"products"}, Mode: "overwrite",
			CronSpec: "03:30", Cron: "0 30 3 * * *",
			ServiceAccountID: "cid", ServiceAccountSecret: "secret",
			JobToken: "tok",
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("齐整配置不该报错: %v", err)
	}

	cases := []struct {
		name string
		mut  func(c *Config)
		want string
	}{
		{"缺共享密钥", func(c *Config) { c.JobToken = "" }, "SYNC_JOB_TOKEN"},
		{"缺服务账号 id", func(c *Config) { c.ServiceAccountID = "" }, "服务账号"},
		{"缺服务账号密钥", func(c *Config) { c.ServiceAccountSecret = "" }, "服务账号"},
		{"非法 mode", func(c *Config) { c.Mode = "destroy" }, "SYNC_MODE"},
		{"空 scopes", func(c *Config) { c.Scopes = nil }, "SYNC_SCOPES"},
		{"空源门店", func(c *Config) { c.SourceBranchID = "" }, "SYNC_SOURCE_BRANCH_ID"},
		{"空目标门店", func(c *Config) { c.TargetBranchID = "" }, "SYNC_TARGET_BRANCH_ID"},
		{"5 字段 cron", func(c *Config) { c.CronSpec = "30 3 * * *" }, "SYNC_CRON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mut(&c)
			err := c.Validate()
			if err == nil {
				t.Fatal("应报错")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误文案应点明 %q,实际: %v", tc.want, err)
			}
		})
	}
}

// TestJobName 锁 job 名的安全化。
//
// ⚠️ 门店 id 会进 dapr 的 URL 路径 ⇒ "/"、"?"、"#" 都能改变解析结果。
func TestJobName(t *testing.T) {
	c := Config{SourceBranchID: "00", TargetBranchID: "10"}
	if got := c.JobName(); got != "cube-sync-00-10" {
		t.Errorf("job 名应为 cube-sync-00-10,实际 %q", got)
	}
	c.SourceBranchID = "a/b?c#d"
	got := c.JobName()
	for _, bad := range []string{"/", "?", "#"} {
		if strings.Contains(got, bad) {
			t.Errorf("job 名 %q 不应含 %q", got, bad)
		}
	}
}
