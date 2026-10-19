package collect

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 编排层的判别力测试。
//
// ⚠️ 这里守的是"用户点了『开启采集』之后，系统处于什么状态"，
// 而不是一个函数的返回值。期望值全部用字面量
//（"sdc-00" / "cube:read" / "00"），不用被测常量推导。

// ---- fakes ----

type fakeCubes struct {
	name    string
	enabled bool
	err     error
	calls   int
}

func (f *fakeCubes) ResolveCubeSource(context.Context, string) (string, bool, error) {
	f.calls++
	return f.name, f.enabled, f.err
}

type fakeAccounts struct {
	created   int
	reset     int
	disabled  int
	enableSet int
	secret    string
	createErr error
	resetErr  error
	disableErr error
	gotClient string
	gotBranch string
	gotPerms  []string
	gotBearer string
}

func (f *fakeAccounts) CreateScoped(_ context.Context, bearer, clientID, _, branchID string, perms []string) (string, error) {
	f.created++
	f.gotBearer = bearer
	f.gotClient = clientID
	f.gotBranch = branchID
	f.gotPerms = perms
	if f.createErr != nil {
		return "", f.createErr
	}
	return f.secret, nil
}

func (f *fakeAccounts) ResetSecret(_ context.Context, bearer, clientID string) (string, error) {
	f.reset++
	f.gotBearer = bearer
	f.gotClient = clientID
	if f.resetErr != nil {
		return "", f.resetErr
	}
	return f.secret + "-rotated", nil
}

func (f *fakeAccounts) SetDisabled(_ context.Context, bearer, clientID string, disabled bool) error {
	if disabled {
		f.disabled++
	} else {
		f.enableSet++
	}
	f.gotBearer = bearer
	f.gotClient = clientID
	return f.disableErr
}

type fakeJobs struct {
	registered []string
	deleted    []string
	regErr     error
	delErr     error
}

func (f *fakeJobs) RegisterBranchJobs(_ context.Context, branchID string) error {
	if f.regErr != nil {
		return f.regErr
	}
	f.registered = append(f.registered, branchID)
	return nil
}

func (f *fakeJobs) DeleteBranchJobs(_ context.Context, branchID string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, branchID)
	return nil
}

// memStore 是凭据存储的**内存实现**（测试用）。
//
// ⚠️ 之所以要它：*CredentialStore 依赖 *gorm.DB，本仓没有 DB harness。
//   而编排层的全部语义（"前置没过 ⇒ 不建号"、"重复开启 ⇒ 什么都不做"、
//   "auth 拒绝 ⇒ 本地状态不改"）都落在这个存储的读写顺序上 ——
//   没有它，这些分支就结构上测不到。
type memStore struct {
	rows map[string]*Credential
	// cipher 让测试能解开落库的密文，验证「存的是密文且能解回来」。
	cipher *Cipher
}

func newMemStore() *memStore { return &memStore{rows: map[string]*Credential{}} }

func (m *memStore) Get(_ context.Context, branchID string) (*Credential, error) {
	r, ok := m.rows[branchID]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

func (m *memStore) Put(_ context.Context, row *Credential) error {
	cp := *row
	m.rows[row.BranchID] = &cp
	return nil
}

func (m *memStore) SetStatus(_ context.Context, branchID string, st CredentialStatus) error {
	r, ok := m.rows[branchID]
	if !ok {
		return errors.New("凭据行不存在")
	}
	r.Status = string(st)
	return nil
}

// newTestControl 造一个装配齐全的 Control（主密钥固定，便于断言密文可解）。
func newTestControl(t *testing.T, cubes *fakeCubes, accts *fakeAccounts, jobs *fakeJobs) (*Control, *memStore) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	ci, err := NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	st := newMemStore()
	st.cipher = ci
	return NewControl(cubes, accts, st, ci, jobs, nil), st
}

// ---- 开启 ----

// TestEnableRequiresCubeSourceConfigured ⭐
//
// 没绑定 cube 数据源就**不能**开启。
//
// ⚠️ 这一条是用户明确要求的（"开启时检查是否在 cube-router 设置绑定了数据源，
//	否则不予以开启并前端报错反馈用户"）。
//
// 放过它的后果：用户看到"开启成功"，但采集每次都失败（cube 报
// cube_source_not_configured）—— 而界面上没有任何东西说"你还没配数据源"。
func TestEnableRequiresCubeSourceConfigured(t *testing.T) {
	cubes := &fakeCubes{err: ErrCubeSourceNotConfigured}
	accts := &fakeAccounts{secret: "s3cret"}
	jobs := &fakeJobs{}
	ctl, _ := newTestControl(t, cubes, accts, jobs)

	_, err := ctl.Enable(context.Background(), "Bearer user", "00", "user-1")
	if err == nil {
		t.Fatal("没绑定数据源却开启成功了")
	}
	if !errors.Is(err, ErrCubeSourceNotConfigured) {
		t.Errorf("错误 = %v，期望 ErrCubeSourceNotConfigured（前端要靠它弹配置链接）", err)
	}
	if accts.created != 0 {
		t.Errorf("前置校验没过却建了号（%d 次）—— 留下一台无用的机器身份", accts.created)
	}
	if len(jobs.registered) != 0 {
		t.Errorf("前置校验没过却注册了 job %v", jobs.registered)
	}
}

// TestEnableRejectsDisabledCubeSourceSeparately
//
// "已绑定但被禁用"必须与"没绑定"给**不同的**哨兵。
//
// 合并的话，"去配置数据源"的链接对禁用的情况是纯误导 ——
// 用户配了也没用，那要管理员启用。
func TestEnableRejectsDisabledCubeSourceSeparately(t *testing.T) {
	cubes := &fakeCubes{name: "src", enabled: false}
	accts := &fakeAccounts{secret: "s3cret"}
	ctl, _ := newTestControl(t, cubes, accts, &fakeJobs{})

	_, err := ctl.Enable(context.Background(), "Bearer user", "00", "user-1")
	if err == nil {
		t.Fatal("数据源被禁用却开启成功了")
	}
	if !errors.Is(err, ErrCubeSourceDisabled) {
		t.Errorf("错误 = %v，期望 ErrCubeSourceDisabled", err)
	}
	if errors.Is(err, ErrCubeSourceNotConfigured) {
		t.Error("禁用被当成了「未配置」—— 前端会弹一个点了也没用的配置链接")
	}
}

// TestEnableCreatesAccountGrantsCubeReadAndStoresCiphertext ⭐
//
// 开启的正向路径必须做到四件事：
//
//	① 用**用户自己的 bearer** 建号（权限按真人判）
//	② 授的权限恰好是 cube:read（多授就是给机器身份多余的能力）
//	③ client_id 由门店推导
//	④ 落库的是**密文**，不是明文
func TestEnableCreatesAccountGrantsCubeReadAndStoresCiphertext(t *testing.T) {
	cubes := &fakeCubes{name: "src", enabled: true}
	accts := &fakeAccounts{secret: "s3cret-value"}
	jobs := &fakeJobs{}
	ctl, st := newTestControl(t, cubes, accts, jobs)

	state, err := ctl.Enable(context.Background(), "Bearer user-token", "00", "user-1")
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if accts.created != 1 {
		t.Fatalf("建号 %d 次，期望 1", accts.created)
	}
	if accts.gotBearer != "Bearer user-token" {
		t.Errorf("建号用的 bearer = %q，期望透传用户的（权限要按真人判）", accts.gotBearer)
	}
	if accts.gotClient != "sdc-00" {
		t.Errorf("client_id = %q，期望 sdc-00", accts.gotClient)
	}
	if accts.gotBranch != "00" {
		t.Errorf("门店 = %q，期望 00", accts.gotBranch)
	}
	if strings.Join(accts.gotPerms, "|") != "cube:read" {
		t.Errorf("授的权限 = %v，期望 [cube:read]（多授就是给机器多余能力）", accts.gotPerms)
	}

	row, err := st.Get(context.Background(), "00")
	if err != nil || row == nil {
		t.Fatalf("凭据没落库: %v", err)
	}
	if row.SecretCipher == "s3cret-value" {
		t.Fatal("落库的是明文 secret —— 主密钥就白设了")
	}
	// 解密回去必须等于明文 —— 这条断言比"密文里没有明文"更强：
	// 它同时证明"存的是密文"与"能解回来"。
	got, derr := st.cipher.Open(row.SecretCipher, "00")
	if derr != nil {
		t.Fatalf("落库的密文解不开: %v", derr)
	}
	if got != "s3cret-value" {
		t.Errorf("解出的明文 = %q，期望 s3cret-value", got)
	}
	if CredentialStatus(row.Status) != StatusEnabled {
		t.Errorf("落库状态 = %q，期望 enabled", row.Status)
	}
	if len(jobs.registered) != 1 || jobs.registered[0] != "00" {
		t.Errorf("注册 job 的门店 = %v，期望 [00]", jobs.registered)
	}
	if state == nil || state.Status != StatusEnabled {
		t.Errorf("返回状态 = %+v，期望 enabled", state)
	}
}

// TestReenableFromDisabled ⭐⭐
//
// "已关停 → 重新开启" 必须真把状态翻回 enabled。
//
// 2026-10-19 线上事故：上一版 Enable 在这一路径里只 `Put(ctx, row)`，
// 而 `row` 是从 Get 拿的、`row.Status` 仍是 disabled —— `Put` 是
// 全量覆盖 ⇒ 写回 PG 的还是 disabled。Flutter 拿到 200 + status="disabled"
// 显示"已关停"，toast 仍然"操作成功"，完全看不出问题。
//
// 进一步后果：Reconcile 按 enabled ∩ 在营 决定注册 job，
// 行 status 仍 disabled ⇒ job 不会重新注册 ⇒ 用户的"重新开启"
// 瞬间又回到起点，且没有任何告警。
//
// ⚠️ 这条是 2026-10-19 用户在 Flutter 里点"重新开启采集"后看到的 bug：
// toast 报"操作成功"，但状态没变化。
//
// ⚠️ 期望值用**字面量**（string(StatusEnabled)），不用任何被测常量推导。
func TestReenableFromDisabled(t *testing.T) {
	cubes := &fakeCubes{name: "src", enabled: true}
	accts := &fakeAccounts{secret: "s3cret"}
	jobs := &fakeJobs{}
	ctl, st := newTestControl(t, cubes, accts, jobs)
	ctx := context.Background()

	// 1) 首次开启 —— 走「建号 → 加密 → 落库 → 注册」全新路径。
	first, err := ctl.Enable(ctx, "Bearer u", "00", "u1")
	if err != nil {
		t.Fatalf("首次 Enable: %v", err)
	}
	if first.Status != "enabled" {
		t.Fatalf("首次 Enable 返回 status=%q, 期望 \"enabled\"", first.Status)
	}
	if accts.created != 1 {
		t.Fatalf("首次应 Create 一次账号, got %d", accts.created)
	}
	if len(jobs.registered) != 1 || jobs.registered[0] != "00" {
		t.Fatalf("首次应注册 00 的 job, got %v", jobs.registered)
	}

	// 2) 模拟关停：把行 status 翻成 disabled，并清掉 job。
	if err := st.SetStatus(ctx, "00", StatusDisabled); err != nil {
		t.Fatalf("SetStatus(disabled): %v", err)
	}
	jobs.registered = nil
	jobs.deleted = nil
	accts.enableSet = 0 // 关停自己也会调 SetDisabled(true)，清掉计数

	// 3) **重新开启** —— 这才是 bug 现场。
	again, err := ctl.Enable(ctx, "Bearer u", "00", "u1")
	if err != nil {
		t.Fatalf("重新 Enable: %v", err)
	}

	// 返回的 State 必须为 enabled。
	if again.Status != "enabled" {
		t.Fatalf("重新 Enable 返回 status=%q, 期望 \"enabled\"", again.Status)
	}

	// 关键的：落库行**本身**必须翻回 enabled。
	//
	// 拿这一步的 fromJson 取法被 fix 前是 disabled ⇒ UI 显示"已关停"
	// ⇒ toast 仍说"操作成功" ⇒ 用户没法察觉。
	row, err := st.Get(ctx, "00")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row == nil {
		t.Fatal("重新启用后行居然不存在")
	}
	if row.Status != string(StatusEnabled) {
		t.Fatalf("行 status = %q, 期望 %q（bug 现场：旧版只 Put 没翻状态）",
			row.Status, StatusEnabled)
	}

	// 副作用核对：不应再建号 / 轮换密钥（明文我们还有）。
	// 计数是**累积**的，所以"重新开启不该多调"= 与首次调用后相等。
	if accts.created != 1 {
		t.Errorf("重新启用不该再 Create 账号, got %d (期望 1，与首次开启后相同)", accts.created)
	}
	if accts.reset != 0 {
		t.Errorf("重新启用不该轮换密钥, got %d", accts.reset)
	}
	// auth 侧：SetDisabled(false) 必须被调一次。
	if accts.enableSet != 1 {
		t.Errorf("auth 侧取消禁用应被调一次, got %d", accts.enableSet)
	}
	// job 必须被注册（不然 Reconcile 看不到 enabled ⇒ 不会自己补）。
	if len(jobs.registered) != 1 || jobs.registered[0] != "00" {
		t.Errorf("重新启用应注册 00 的 job, got %v", jobs.registered)
	}
}

// TestEnableIsIdempotent ⭐
//
// 已是 enabled 时再点一次开启，必须**什么都不做**。
//
// 前端有重试按钮、网络会重发；每次重试都重建账号 ⇒ client_id 撞 409，
// 而用户看到的是"开启失败"，尽管第一次已经成功。
func TestEnableIsIdempotent(t *testing.T) {
	cubes := &fakeCubes{name: "src", enabled: true}
	accts := &fakeAccounts{secret: "s3cret"}
	jobs := &fakeJobs{}
	ctl, _ := newTestControl(t, cubes, accts, jobs)

	if _, err := ctl.Enable(context.Background(), "Bearer u", "00", "u1"); err != nil {
		t.Fatalf("首次开启: %v", err)
	}
	if _, err := ctl.Enable(context.Background(), "Bearer u", "00", "u1"); err != nil {
		t.Fatalf("重复开启: %v", err)
	}
	if accts.created != 1 {
		t.Errorf("重复开启建了 %d 次号，期望 1", accts.created)
	}
	if accts.reset != 0 {
		t.Errorf("重复开启轮换了 %d 次密钥，期望 0（没坏就不该动它）", accts.reset)
	}
}

// TestEnableRecoversFromAccountExistsByRotatingSecret ⭐
//
// **建号成功但落库失败**之后重试，必须能自愈。
//
// 这条路径存在的理由：明文只在 auth 响应里出现一次，落库失败 ⇒ 明文永久丢失；
// 而 client_id 是 UNIQUE，重建只会撞 409 ⇒ 门店永远开不了采集。
//
// → 命中"已存在"时转去**轮换密钥**，换一枚新明文重新落库。
func TestEnableRecoversFromAccountExistsByRotatingSecret(t *testing.T) {
	cubes := &fakeCubes{name: "src", enabled: true}
	// 第一次建号报"已存在"（模拟上次建号成功但没落库）
	accts := &fakeAccounts{secret: "fresh-secret", createErr: ErrServiceAccountExists}
	jobs := &fakeJobs{}
	ctl, st := newTestControl(t, cubes, accts, jobs)

	if _, err := ctl.Enable(context.Background(), "Bearer u", "00", "u1"); err != nil {
		t.Fatalf("命中已存在时必须自愈而不是报错: %v", err)
	}
	if accts.reset != 1 {
		t.Fatalf("轮换 %d 次，期望 1（不轮换就永远拿不到明文）", accts.reset)
	}
	row, err := st.Get(context.Background(), "00")
	if err != nil || row == nil {
		t.Fatalf("自愈后凭据没落库: %v", err)
	}
	if row.SecretCipher == "" {
		t.Error("落库的密文为空")
	}
}

// TestEnableDoesNotRecoverWhenRotateAlsoFails
//
// 轮换也失败时必须**明确报错**，不能落一个空凭据。
//
// 落空凭据的后果：采集每次都 401，而真因（轮换失败）被完全掩盖。
func TestEnableDoesNotRecoverWhenRotateAlsoFails(t *testing.T) {
	cubes := &fakeCubes{name: "src", enabled: true}
	accts := &fakeAccounts{
		createErr: ErrServiceAccountExists,
		resetErr:  errors.New("auth 拒绝"),
	}
	ctl, st := newTestControl(t, cubes, accts, &fakeJobs{})

	if _, err := ctl.Enable(context.Background(), "Bearer u", "00", "u1"); err == nil {
		t.Fatal("建号与轮换都失败却报成功了")
	}
	row, _ := st.Get(context.Background(), "00")
	if row != nil {
		t.Error("失败路径却落了凭据行 —— 会留下一台永远开不了采集的账号")
	}
}

// TestEnableRejectedBranchNeverTouchesAccounts
//
// 门店 id 推不出 client_id 时，必须在**建号之前**就拒。
//
// 顺序反了会留下一台归属不明的机器身份（client_id 推不出门店 ⇒
// 永远无法归属、无法禁用）。
func TestEnableRejectedBranchNeverTouchesAccounts(t *testing.T) {
	cubes := &fakeCubes{name: "src", enabled: true}
	accts := &fakeAccounts{secret: "s"}
	ctl, _ := newTestControl(t, cubes, accts, &fakeJobs{})

	if _, err := ctl.Enable(context.Background(), "Bearer u", "bad_id", "u1"); err == nil {
		t.Fatal("非法门店 id 却开启成功了")
	}
	if accts.created != 0 {
		t.Errorf("非法门店却建了 %d 次号", accts.created)
	}
}

// ---- 关停 ----

// TestDisableDisablesAccountAndDeletesJobsButKeepsCredential ⭐
//
// 关停 = 禁用服务账号 + 删定时任务，**凭据行保留**。
//
// 保留凭据是"重新开启无损"的前提：明文只存在一次，
// 删了就得重新分发密钥。
func TestDisableDisablesAccountAndDeletesJobsButKeepsCredential(t *testing.T) {
	cubes := &fakeCubes{name: "src", enabled: true}
	accts := &fakeAccounts{secret: "s3cret"}
	jobs := &fakeJobs{}
	ctl, st := newTestControl(t, cubes, accts, jobs)

	if _, err := ctl.Enable(context.Background(), "Bearer u", "00", "u1"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	state, err := ctl.Disable(context.Background(), "Bearer u", "00")
	if err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if accts.disabled != 1 {
		t.Errorf("禁用服务账号 %d 次，期望 1（只禁不删）", accts.disabled)
	}
	if len(jobs.deleted) != 1 || jobs.deleted[0] != "00" {
		t.Errorf("删除 job 的门店 = %v，期望 [00]", jobs.deleted)
	}
	row, _ := st.Get(context.Background(), "00")
	if row == nil {
		t.Fatal("凭据行被删了 —— 重新开启就必须重新分发密钥（明文回显不出来）")
	}
	if CredentialStatus(row.Status) != StatusDisabled {
		t.Errorf("状态 = %q，期望 disabled", row.Status)
	}
	// ⚠️ 密钥必须**还在**且还能解出来 ——
	// 这是"重新开启无损"的全部依据（明文只存在一次，删了就得重新分发）。
	// 只断言"行还在"不够：行在但密文被清空，重新开启照样采不了。
	secret, derr := st.cipher.Open(row.SecretCipher, "00")
	if derr != nil {
		t.Fatalf("关停后密文解不开（密钥被清了？）: %v", derr)
	}
	if secret != "s3cret" {
		t.Errorf("关停后解出的明文 = %q，期望 s3cret", secret)
	}
	if state == nil || state.Status != StatusDisabled {
		t.Errorf("返回状态 = %+v，期望 disabled", state)
	}
}

// TestDisableIsIdempotent
//
// 重复关停必须成功（幂等），不能报错。
//
// 前端有重试按钮；报 409 会让用户以为"关停没生效"。
func TestDisableIsIdempotent(t *testing.T) {
	cubes := &fakeCubes{name: "src", enabled: true}
	accts := &fakeAccounts{secret: "s"}
	ctl, _ := newTestControl(t, cubes, accts, &fakeJobs{})

	if _, err := ctl.Enable(context.Background(), "Bearer u", "00", "u1"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if _, err := ctl.Disable(context.Background(), "Bearer u", "00"); err != nil {
		t.Fatalf("首次关停: %v", err)
	}
	if _, err := ctl.Disable(context.Background(), "Bearer u", "00"); err != nil {
		t.Fatalf("重复关停必须幂等成功: %v", err)
	}
	if accts.disabled != 1 {
		t.Errorf("禁用 %d 次，期望 1（第二次已是目标状态，不该再动）", accts.disabled)
	}
}

// TestDisableOfNeverEnabledBranchSucceeds
//
// 从没开过的门店关停，必须成功（幂等），且**不建号不删 job**。
func TestDisableOfNeverEnabledBranchSucceeds(t *testing.T) {
	cubes := &fakeCubes{name: "src", enabled: true}
	accts := &fakeAccounts{}
	jobs := &fakeJobs{}
	ctl, _ := newTestControl(t, cubes, accts, jobs)

	state, err := ctl.Disable(context.Background(), "Bearer u", "77")
	if err != nil {
		t.Fatalf("关停一个从未开启的门店必须成功: %v", err)
	}
	if accts.disabled != 0 {
		t.Errorf("对不存在的账号调了 %d 次禁用", accts.disabled)
	}
	if len(jobs.deleted) != 0 {
		t.Errorf("删了 job %v —— 删不存在的 job 也会让 dapr 报错", jobs.deleted)
	}
	if state == nil || state.Status != StatusNotConfigured {
		t.Errorf("状态 = %+v，期望 not_configured", state)
	}
}

// TestDisableFailsLoudlyWhenAuthRefuses
//
// auth 侧禁用失败时必须**报错**且**不改本地状态**。
//
// 反过来（先改本地再禁 auth）的话，本地显示已关停、机器身份却还能换 token
// —— 采集照跑，而用户以为已经关了。
func TestDisableFailsLoudlyWhenAuthRefuses(t *testing.T) {
	cubes := &fakeCubes{name: "src", enabled: true}
	accts := &fakeAccounts{secret: "s"}
	jobs := &fakeJobs{}
	ctl, st := newTestControl(t, cubes, accts, jobs)

	if _, err := ctl.Enable(context.Background(), "Bearer u", "00", "u1"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	accts.disableErr = errors.New("auth 拒绝：缺少 service-account:disable")

	if _, err := ctl.Disable(context.Background(), "Bearer u", "00"); err == nil {
		t.Fatal("auth 拒绝禁用却报成功了")
	}
	row, _ := st.Get(context.Background(), "00")
	if row == nil || CredentialStatus(row.Status) != StatusEnabled {
		t.Errorf("本地状态 = %+v，期望仍是 enabled（auth 没禁成就不能本地说禁了）", row)
	}
	if len(jobs.deleted) != 0 {
		t.Errorf("auth 拒绝却删了 job %v", jobs.deleted)
	}
}
