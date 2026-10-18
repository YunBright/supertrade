package collect

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// 期望值全部写成**字面量**（"sdc-b001" / "cube:read" / "00"），
// 不用 ServiceAccountPrefix 等被测常量推导 ——
// 否则把前缀改掉，用例跟着变、照样全绿。

// ---- ClientIDFor：client_id 必须由门店 id 纯函数推导 ----

// TestClientIDFor_LowercasesBranchID ⭐
//
// 门店 id 是**含大写**的（B001），而 auth 侧 ValidClientID 只允许
// `^[a-z0-9](?:[a-z0-9-]{1,62})[a-z0-9]$`。
// 不 lower 的后果是创建请求被 auth 拒掉，而症状是"开启采集一直失败"，
// 完全看不出是大小写问题。
func TestClientIDFor_LowercasesBranchID(t *testing.T) {
	cases := []struct{ branch, want string }{
		{"00", "sdc-00"},
		{"B001", "sdc-b001"},
		{"hbposv10", "sdc-hbposv10"},
		{"  B002  ", "sdc-b002"}, // 首尾空白先 trim
		{"branch-A1", "sdc-branch-a1"},
	}
	for _, tc := range cases {
		got, err := ClientIDFor(tc.branch)
		if err != nil {
			t.Errorf("ClientIDFor(%q) 报错 %v，期望成功", tc.branch, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ClientIDFor(%q) = %q，期望 %q", tc.branch, got, tc.want)
		}
	}
}

// TestClientIDFor_RejectsIllegalBranchID
//
// ⚠️ 与 JobName 的 sanitizeID **故意相反**：那边替换成 `_`，这边报错。
//
//	job 名替换错了的后果是名字难看（最多撞名，已有测试守着）；
//	client_id 替换成 `_` 会创建出一台**永远推不出门店 id** 的机器身份
//	—— 不可逆的孤儿账号，权限也没法归属。所以宁可当场拒绝。
func TestClientIDFor_RejectsIllegalBranchID(t *testing.T) {
	for _, bad := range []string{
		"",              // 空
		"   ",           // 全空白
		"B_001",         // 下划线（auth 的 client_id 不允许）
		"门店01",         // 非 ASCII
		"a b",           // 空格
		"a/b",           // 斜杠
		"-b001",         // 首字符连字符 → 尾部会紧邻 prefix 产生双连字符
		strings.Repeat("x", 70), // 推导后超 64
	} {
		got, err := ClientIDFor(bad)
		if err == nil {
			t.Errorf("ClientIDFor(%q) = %q 却没报错 —— 会留下一台推不出门店的孤儿账号", bad, got)
			continue
		}
		if !errors.Is(err, ErrInvalidBranchID) {
			t.Errorf("ClientIDFor(%q) 的错误不是 ErrInvalidBranchID: %v", bad, err)
		}
	}
}

// TestClientIDFor_RejectsTrailingHyphenBranch
//
// 门店 id 形如 "b-" 时，推导出 "sdc-b-" —— 尾字符是连字符，
// 违反 auth 的 `... [a-z0-9]$` 约束，会在**创建时**才被拒。
// 本地提前拒，用户才能拿到可读原因。
func TestClientIDFor_RejectsTrailingHyphenBranch(t *testing.T) {
	if _, err := ClientIDFor("b-"); err == nil {
		t.Fatal("门店 id 尾字符为连字符时必须报错（推导出 client_id 尾部也是连字符）")
	}
}

// ---- Cipher：AES-256-GCM + AAD 绑门店 ----

func newTestCipher(t *testing.T) *Cipher {
	t.Helper()
	key := make([]byte, masterKeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("生成测试主密钥: %v", err)
	}
	c, err := NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

// TestCipher_RoundTrip —— 加解密必须能回来，且明文不被改写。
func TestCipher_RoundTrip(t *testing.T) {
	c := newTestCipher(t)
	// 用 GenerateClientSecret 形态的密文（43 字符 base64url）
	const secret = "kZ8vQ2pL5nR7tY0wX3aB6cD9eF1gH4jK2mN5pQ7sT9uV0"

	ct, err := c.Seal(secret, "00")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if ct == secret {
		t.Fatal("密文与明文相同 —— 根本没加密")
	}
	if strings.Contains(ct, secret) {
		t.Fatal("密文里出现了明文片段")
	}
	got, err := c.Open(ct, "00")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != secret {
		t.Fatalf("Open = %q，期望原明文", got)
	}
}

// TestCipher_NonceNeverRepeats ⭐
//
// 同一主密钥下复用 nonce 会让 GCM 的机密性**彻底失效**
//（可由两对明文/密文直接异或还原），而且**不会报错**。
func TestCipher_NonceNeverRepeats(t *testing.T) {
	c := newTestCipher(t)
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		ct, err := c.Seal("同一个明文", "00")
		if err != nil {
			t.Fatalf("Seal #%d: %v", i, err)
		}
		if seen[ct] {
			t.Fatalf("第 %d 次加密产生了与之前相同的密文 —— nonce 被复用了", i)
		}
		seen[ct] = true
	}
}

// TestCipher_AADBlocksCrossBranchReplay ⭐
//
// AAD 绑门店：A 门店的密文搬到 B 门店解**必须失败**。
//
// 门店隔离由 X-Branch-ID → cube-router 完成（source.go 的 withBranch），
// 这一层防的是**凭据本身**被跨门店搬运 ——
// 混用时的症状是"拿 A 的机器身份去打 B 的门店"，
// 而那会表现成 403，被误判成"权限没授对"。
func TestCipher_AADBlocksCrossBranchReplay(t *testing.T) {
	c := newTestCipher(t)
	ct, err := c.Seal("secret-of-branch-00", "00")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := c.Open(ct, "B001"); err == nil {
		t.Fatal("A 门店的密文在 B 门店解开了 —— AAD 没有真正绑住门店")
	}
	// 正确门店仍然要能解开（防止"一律拒绝"这种假绿实现）
	if _, err := c.Open(ct, "00"); err != nil {
		t.Fatalf("同门店解密反而失败: %v", err)
	}
}

// TestCipher_DetectsTampering
//
// 密文被改动一位就必须解不开 —— 这正是选 GCM 而不是 CBC 的理由：
// CBC 改一个字节会静默解出垃圾，而垃圾恰好还是一个"合法的 secret 字符串"，
// 于是错误一路走到换 token 才暴露成 401，症状与真因（数据被改）完全无关。
func TestCipher_DetectsTampering(t *testing.T) {
	c := newTestCipher(t)
	ct, err := c.Seal("secret-value-here", "00")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(ct)
	if err != nil {
		t.Fatalf("密文不是合法 base64: %v", err)
	}
	raw[len(raw)-1] ^= 0x01 // 翻最后一个 bit
	if _, err := c.Open(base64.StdEncoding.EncodeToString(raw), "00"); err == nil {
		t.Fatal("密文被篡改后仍解密成功 —— 完整性校验没生效")
	}
}

// TestNewCipher_RejectsWrongKeyLength ⭐
//
// **拒绝降级**是关键：Go 的 aes.NewCipher 在 24 字节时会安静地用 AES-192，
// 在 16/32 字节间也可能有调用方误配。若这里只"接受任意长度"，
// 运维拿一个 base64 写错的短密钥上线，采集会以"解不开密文"失败，
// 而真因（密钥长度不对）不会出现在任何一行日志里。
func TestNewCipher_RejectsWrongKeyLength(t *testing.T) {
	for _, n := range []int{0, 1, 16, 24, 31, 33, 64} {
		if _, err := NewCipher(make([]byte, n)); err == nil {
			t.Errorf("NewCipher 接受了 %d 字节的密钥 —— 拒绝降级到更弱的密钥", n)
		}
	}
	if _, err := NewCipher(make([]byte, 32)); err != nil {
		t.Errorf("NewCipher 拒绝了正确的 32 字节密钥: %v", err)
	}
}

// ---- StoreCipherIssuer.Bearer：三种"拿不到凭据"必须给三条不同错误 ----

type stubStore struct {
	row *Credential
	err error
}

func (s stubStore) Get(context.Context, string) (*Credential, error) { return s.row, s.err }

type stubCipher struct {
	out string
	err error
}

func (s stubCipher) Secret(*Credential) (string, error) { return s.out, s.err }

type stubIssuer struct {
	got     string
	err     error
	calls   int
	branch  string
	clientI string
	secret  string
}

func (s *stubIssuer) AccessToken(_ context.Context, b, cid, sec string) (string, error) {
	s.calls++
	s.branch, s.clientI, s.secret = b, cid, sec
	if s.err != nil {
		return "", s.err
	}
	return s.got, nil
}

// TestBearer_DistinguishesNoRowDisabledAndUnknownStatus ⭐
//
// 三种状态必须给**三个可区分**的错误。
//
// 混成一句"采集凭据不可用"的话，前端 alert 会把三种情况都指向
// "去开启采集"，而第三种（状态拼错）用户开了也还是坏 ⇒ 死循环。
func TestBearer_DistinguishesNoRowDisabledAndUnknownStatus(t *testing.T) {
	cases := []struct {
		name    string
		row     *Credential
		wantErr error
	}{
		{"没有凭据行（从未开启）", nil, ErrCollectNotEnabled},
		{"状态是 disabled（用户主动关停）",
			&Credential{BranchID: "00", ClientID: "sdc-00", Status: string(StatusDisabled)},
			ErrCollectDisabled},
		{"状态是 not_configured",
			&Credential{BranchID: "00", ClientID: "sdc-00", Status: string(StatusNotConfigured)},
			ErrCollectNotEnabled},
		{"状态无法识别",
			&Credential{BranchID: "00", ClientID: "sdc-00", Status: "ENABLED"},
			nil}, // 断言"不是前两个"，且必须失败
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iss := &stubIssuer{got: "tok"}
			b := &StoreCipherIssuer{
				store:  stubStore{row: tc.row},
				cipher: stubCipher{out: "s"},
				issuer: iss,
			}
			_, err := b.Bearer(context.Background(), "00")
			if err == nil {
				t.Fatalf("期望失败，但成功了（issuer 被调用 %d 次）", iss.calls)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("错误 = %v，期望 errors.Is(%v)", err, tc.wantErr)
				}
				return
			}
			// 未知状态：不能被当成 enabled 放行。
			// 关停采集的全部意义就是"不再采"，放行意味着用户以为关着、实际在采。
			if errors.Is(err, ErrCollectNotEnabled) || errors.Is(err, ErrCollectDisabled) {
				t.Errorf("未知状态被归类成状态错误 %v —— 应报「状态无法识别」", err)
			}
			if iss.calls != 0 {
				t.Errorf("未知状态仍去换了 token（%d 次）", iss.calls)
			}
		})
	}
}

// TestBearer_RejectsMismatchedClientID ⭐
//
// 库里 client_id 与按门店推导出的不一致时必须**拒绝**。
//
// 不放行的话，会拿着 A 的机器身份去打 B 的门店 ——
// 症状是 403，而排查会一路走到"是不是权限没授对"。
func TestBearer_RejectsMismatchedClientID(t *testing.T) {
	iss := &stubIssuer{got: "tok"}
	b := &StoreCipherIssuer{
		// 门店是 00，库里却存着 B001 的 client_id
		store:  stubStore{row: &Credential{BranchID: "00", ClientID: "sdc-b001", Status: string(StatusEnabled)}},
		cipher: stubCipher{out: "s"},
		issuer: iss,
	}
	_, err := b.Bearer(context.Background(), "00")
	if err == nil {
		t.Fatal("client_id 与门店不匹配却放行了 —— 会拿别的门店的机器身份取数")
	}
	if iss.calls != 0 {
		t.Errorf("不一致时仍去换了 token（%d 次）", iss.calls)
	}
}

// TestBearer_PassesDerivedClientIDToIssuer
//
// 成功路径上必须把 **client_id 与 secret 原样**交给签发器 ——
// 这是"凭据真的被用上了"的唯一正向证据。
func TestBearer_PassesDerivedClientIDToIssuer(t *testing.T) {
	iss := &stubIssuer{got: "the-token"}
	b := &StoreCipherIssuer{
		store: stubStore{row: &Credential{
			BranchID: "B001", ClientID: "sdc-b001", Status: string(StatusEnabled),
		}},
		cipher: stubCipher{out: "the-secret"},
		issuer: iss,
	}
	got, err := b.Bearer(context.Background(), "B001")
	if err != nil {
		t.Fatalf("Bearer: %v", err)
	}
	if got != "the-token" {
		t.Errorf("token = %q", got)
	}
	if iss.branch != "B001" || iss.clientI != "sdc-b001" || iss.secret != "the-secret" {
		t.Errorf("签发器收到 branch=%q client_id=%q secret=%q", iss.branch, iss.clientI, iss.secret)
	}
}

// TestBearer_PropagatesIssuerError
//
// 换 token 失败必须**原样返回**：handler 要据此判"是没开启"还是"真出错"，
// 包一层通用错误会把两者混成 503。
func TestBearer_PropagatesIssuerError(t *testing.T) {
	boom := errors.New("采集凭据：向 auth 换 token 返回 401")
	iss := &stubIssuer{err: boom}
	b := &StoreCipherIssuer{
		store: stubStore{row: &Credential{BranchID: "00", ClientID: "sdc-00", Status: string(StatusEnabled)}},
		cipher: stubCipher{out: "s"},
		issuer: iss,
	}
	_, err := b.Bearer(context.Background(), "00")
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v，期望原样传出 %v", err, boom)
	}
}

// ---- Reconcile：只注册已开启，关停要真删 ----

// TestReconcileSkipsBranchesNotEnabled ⭐
//
// 未开启采集的门店**不能**拿到 job。
//
// 若仍按在营门店全量注册，后果不只是浪费：用户点了"开启采集"之后
// job 数根本没变（本来就全注册了），于是他会怀疑"这功能是不是没做"。
func TestReconcileSkipsBranchesNotEnabled(t *testing.T) {
	bl := &fakeBranchLister{branches: []Branch{{ID: "00"}, {ID: "01"}, {ID: "02"}}}
	jc := newFakeJobClient()
	// 只有 01 开过采集
	r := NewReconciler(bl, &fakeEnabledLister{ids: []string{"01"}}, jc, "http://addr")

	res, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Jobs != 2 {
		t.Fatalf("注册 job 数 = %d，期望 2（只有 01 开启）", res.Jobs)
	}
	if contains(jc.registered, "sales-daily-collect-00-initial") {
		t.Error("未开启的 00 拿到了 job")
	}
	if contains(jc.registered, "sales-daily-collect-02-corrected") {
		t.Error("未开启的 02 拿到了 job")
	}
	if !contains(jc.registered, "sales-daily-collect-01-initial") {
		t.Errorf("已开启的 01 没拿到 job，实际 %v", jc.registered)
	}
	if res.Enabled != 1 || res.Disabled != 2 {
		t.Errorf("Enabled=%d Disabled=%d，期望 1/2", res.Enabled, res.Disabled)
	}
}

// TestReconcileDeletesJobsForDisabledBranches ⭐
//
// 关停采集 = **删掉 job**，不是"不再注册"。
//
// 不删的话 job 仍在 scheduler 里，每天照跑两次；
// 而采集会因为凭据是 disabled 而失败 —— 于是用户点了"关闭采集"之后
// 每天多出两条错误日志。他会认为"关闭没生效"。
func TestReconcileDeletesJobsForDisabledBranches(t *testing.T) {
	bl := &fakeBranchLister{branches: []Branch{{ID: "00"}, {ID: "01"}}}
	jc := newFakeJobClient()
	// 用户刚关掉了 01
	r := NewReconciler(bl, &fakeEnabledLister{ids: []string{"00"}}, jc, "http://addr")

	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, want := range []string{
		"sales-daily-collect-01-initial",
		"sales-daily-collect-01-corrected",
	} {
		if !contains(jc.deleted, want) {
			t.Errorf("关停后没有删除 job %s，实际删除 %v", want, jc.deleted)
		}
	}
	// 已开启的 00 不该被误删
	if contains(jc.deleted, "sales-daily-collect-00-initial") {
		t.Error("已开启的 00 的 job 被误删了")
	}
}

// TestReconcileFailsLoudlyWithoutEnabledLister
//
// 没装配"已开启门店"来源时必须报错。
//
// 退化成"不注册任何 job"的话，采集静默停摆：服务正常、报表照出、
// 日志里只有一行"对账完成"，而采集一次都没跑过。
func TestReconcileFailsLoudlyWithoutEnabledLister(t *testing.T) {
	bl := &fakeBranchLister{branches: []Branch{{ID: "00"}}}
	jc := newFakeJobClient()
	r := NewReconciler(bl, nil, jc, "http://addr")

	if _, err := r.Reconcile(context.Background()); err == nil {
		t.Fatal("缺已开启门店来源时必须报错，不能静默不注册")
	}
	if len(jc.registered) != 0 {
		t.Errorf("仍注册了 %d 个 job", len(jc.registered))
	}
}

// TestReconcileJobDataCarriesServiceAccountButNeverSecret ⭐
//
// job data 里必须有 client_id（关联锚点），且**绝不能**有 secret。
//
// secret 进 job data 就会明文落到 scheduler 的存储后端（ETCD），
// 多副本下每副本一份，备份与运维 cat 都会泄露。
// 这是本设计里唯一一个"加了也不会报错、但后果不可逆"的点。
func TestReconcileJobDataCarriesServiceAccountButNeverSecret(t *testing.T) {
	bl := &fakeBranchLister{branches: []Branch{{ID: "B001"}}}
	jc := newFakeJobClient()
	r := NewReconciler(bl, allEnabled(bl), jc, "http://addr")

	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// ⚠️ job 名**保留大小写**（sanitizeID 的白名单含 A-Z），
	// 只有 client_id 会 lower —— 两者不能混为一谈。
	d := jc.dataOf["sales-daily-collect-B001-initial"]
	if d == nil {
		t.Fatalf("没找到 job data，实际注册 %v", jc.registered)
	}
	if d["service_account"] != "sdc-b001" {
		t.Errorf("service_account = %v，期望 sdc-b001", d["service_account"])
	}
	for k, v := range d {
		low := strings.ToLower(k)
		if strings.Contains(low, "secret") || strings.Contains(low, "password") ||
			strings.Contains(low, "token") {
			t.Errorf("job data 里出现了敏感键 %q = %v —— 它会明文进 scheduler 的存储后端", k, v)
		}
	}
}

// TestReconcileSkipsBranchWhoseClientIDCannotBeDerived
//
// 门店 id 含非法字符 ⇒ 推不出 client_id ⇒ **不注册并点名**。
//
// 不能"照常注册让它每天失败"：那会每天两条换 token 失败日志，
// 而真因（门店 id 本身有问题）被完全淹没。
func TestReconcileSkipsBranchWhoseClientIDCannotBeDerived(t *testing.T) {
	bl := &fakeBranchLister{branches: []Branch{{ID: "bad_id"}}}
	jc := newFakeJobClient()
	r := NewReconciler(bl, allEnabled(bl), jc, "http://addr")

	res, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("单店推不出 client_id 不该让整次对账失败: %v", err)
	}
	if len(jc.registered) != 0 {
		t.Errorf("仍注册了 %v", jc.registered)
	}
	found := false
	for _, f := range res.Failed {
		if strings.Contains(f, "bad_id") && strings.Contains(f, "client_id") {
			found = true
		}
	}
	if !found {
		t.Errorf("Failed 里没有点名 bad_id 的 client_id 问题，实际 %v", res.Failed)
	}
}
