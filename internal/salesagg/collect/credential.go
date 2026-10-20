// credential.go — 门店采集的服务账号凭据：**推导、加密、存取**。
//
// # 为什么凭据存在 sales-agg 而不是只在 auth
//
// dapr 的 job 回调不带 Authorization 头（实测：job spec 无 headers 字段），
// 所以采集执行时必须现换机器 token。换 token 要 client_id + client_secret，
// 而 auth 侧只存 bcrypt 哈希、**永远回显不出明文**。
//
// → 明文必须有一份由我们自己保管。放哪是本文件要回答的核心问题，
//
//	  约束是 dapr scheduler 的投递语义（见 dapr 官方 scheduler.md）：
//
//		HA 形态是 3 副本 StatefulSet，无 leader election，全是对等节点；
//		触发投递到 app-id 匹配的**随机一个副本**，官方明说
//		"不保证 job 在创建它的那个副本上执行"。
//
// 由此有两条**确定会坏**的存法：
//
//	❌ 塞进 dapr job 的 data —— 明文进 scheduler 的 ETCD，多副本各存一份，
//	   备份/运维 cat 一下就全泄露。
//	❌ 存本机文件 —— 多副本下变成"有的节点有、有的没有"，
//	   症状是**随机 401**（一天两次，落在哪个副本不确定），
//	   而日志里只有一行 401，这种 bug 排起来最费时间。
//
// → 存在**运行时按 branch_id 现取的共享存储**（PG）里。
//
//	任何副本都能独立完成 token 交换，加副本不改代码。
//
// # 三层存储的职责边界
//
//	① auth PG  service_accounts + user_branches + user_branch_permissions
//	           —— 权威身份态，跨服务共享，本来就在那
//	② 本 PG   branch_collect_credentials（本文件）
//	           —— 本地启用态 + 可解密副本
//	③ systemd Environment  —— AES 主密钥，明文只在主机、不进版本库
package collect

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"gorm.io/gorm"
)

// ServiceAccountPrefix 是采集服务账号 client_id 的固定前缀。
//
// ⚠️ 必须与 auth 侧约定一致，且 client_id 全小写：
//
//	service.ValidClientID 的形态约束是
//	`^[a-z0-9](?:[a-z0-9-]{1,62})[a-z0-9]$`
//	而**门店 id 是含大写的**（如 `B001`）—— 所以必须 lower，
//	否则创建请求会被 auth 以"client_id 格式非法"拒掉，
//	而症状是"开启采集一直失败"，看不出是大小写问题。
const ServiceAccountPrefix = "sdc-"

// maxClientIDLen 与 auth 侧 ValidClientID 的上界一致（64）。
const maxClientIDLen = 64

// ErrInvalidBranchID 表示门店 id 无法推导出合法的 client_id。
var ErrInvalidBranchID = errors.New("门店 id 无法推导出合法的服务账号 client_id")

// ClientIDFor 由门店 id 推导服务账号的 client_id。
//
// # 为什么用纯函数推导，而不是另存一张 job → 账号 的映射表
//
// 映射表与实际注册进 dapr 的 job 名一旦对不上，症状是
// "门店显示已开启、但每天采集失败"，而且**两边都看起来有数据**，
// 排查时没有任何一处会报错。
//
// 推导式则让"对不上"在结构上不可能发生：库里存的 client_id
// 永远等于 ClientIDFor(branch_id)，要改就改门店 id。
//
// ⚠️ 非法字符**报错**而不是像 sanitizeID 那样替换成 `_`：
//
//	auth 侧的 client_id 不允许下划线，而 `_` 形态的 client_id 一旦创建成功，
//	就再也推不出门店 id 是什么 —— 变成一台无法归属的孤儿机器身份。
//	宁可当场拒绝开启，也不能留下不可逆的东西。
func ClientIDFor(branchID string) (string, error) {
	b := strings.TrimSpace(branchID)
	if b == "" {
		return "", fmt.Errorf("%w：门店 id 为空", ErrInvalidBranchID)
	}
	lower := strings.ToLower(b)
	for _, r := range lower {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			return "", fmt.Errorf(
				"%w：门店 id %q 含不允许的字符 %q（只允许字母/数字/连字符）",
				ErrInvalidBranchID, branchID, string(r))
		}
	}
	id := ServiceAccountPrefix + lower
	if len(id) > maxClientIDLen {
		return "", fmt.Errorf("%w：门店 id %q 过长，推导出 %d 字符（上限 %d）",
			ErrInvalidBranchID, branchID, len(id), maxClientIDLen)
	}
	// 末尾再核一次 auth 侧的形态约束（首尾不能是连字符）。
	// 门店 id 形如 "-" 时 lower 之后仍以连字符开头/结尾，
	// 而本地这层检查比 auth 的报错早得多，用户能拿到可读的原因。
	if strings.HasPrefix(id, ServiceAccountPrefix+"-") || strings.HasSuffix(id, "-") {
		return "", fmt.Errorf("%w：门店 id %q 首尾是连字符", ErrInvalidBranchID, branchID)
	}
	return id, nil
}

// Cipher 用 AES-256-GCM 加解密 client_secret。
//
// # 为什么用 GCM 而不是 AES-CBC
//
// GCM 自带完整性校验：密文被改动过会**解不开**并报错。
// CBC 改一个字节会静默解出垃圾 —— 而垃圾恰好还是一个"合法的 secret 字符串"，
// 于是错误会一路走到 token 交换才暴露成 401，症状与真因（数据被改）完全无关。
//
// # 为什么要绑 AAD
//
// AAD（附加认证数据）不加密但参与认证，Seal/Open 时传入**门店 id**。
// 于是 A 门店的密文搬不到 B 门店解 —— 密文被复制粘贴串店时会直接失败，
// 而不是解出一个能用的 secret 却打错门店的数据。
// 门店隔离由 X-Branch-ID → cube-router 完成（见 source.go 的 withBranch），
// 而这一层防止的是**凭据本身**被跨门店搬运。
type Cipher struct {
	aead cipher.AEAD
}

// masterKeyLen 是 AES-256 的密钥字节数。
const masterKeyLen = 32

// NewCipher 由主密钥构造 Cipher。
//
// 主密钥来自环境变量（systemd Environment，与 POSTGRES_DSN 同一处不进版本库）。
// 长度必须**正好** 32 字节：短密钥降级到 AES-128 是绝不能接受的
// 静默降级 —— 而 Go 的 aes.NewCipher 在 24 字节时会安静地用 AES-192。
// 所以这里显式只接受 32，其余一律报错。
func NewCipher(masterKey []byte) (*Cipher, error) {
	if len(masterKey) != masterKeyLen {
		return nil, fmt.Errorf(
			"采集凭据主密钥长度必须是 %d 字节（base64 解码后），收到 %d —— 拒绝降级到更弱的密钥",
			masterKeyLen, len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("构造 AES: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("构造 GCM: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// CipherFromEnvKey 按 base64 解码主密钥并构造 Cipher。
func CipherFromEnvKey(b64 string) (*Cipher, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("采集凭据主密钥不是合法 base64: %w", err)
	}
	return NewCipher(key)
}

// Seal 加密 client_secret，输出 base64 密文。
//
// ⚠️ nonce 由 crypto/rand 生成且**永不复用**：
//
//	同一主密钥下复用 nonce 会让 GCM 的机密性彻底失效
//	（可由两个明文密文对直接异或还原），且**不会报错**。
func (c *Cipher) Seal(plaintext, aad string) (string, error) {
	if c == nil || c.aead == nil {
		return "", errors.New("采集凭据加密器未装配（主密钥缺失？）")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("生成 nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), []byte(aad))
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open 解密 client_secret。aad 必须与 Seal 时**完全一致**。
//
// ⚠️ 错误**不回显任何密文/明文片段**：这个错误会经采集端点的
// 503 响应回到调用方，而调用方可能是定时任务日志被翻出来的地方。
func (c *Cipher) Open(ciphertextB64, aad string) (string, error) {
	if c == nil || c.aead == nil {
		return "", errors.New("采集凭据解密器未装配（主密钥缺失？）")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(ciphertextB64))
	if err != nil {
		return "", fmt.Errorf("采集凭据密文不是合法 base64: %w", err)
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("采集凭据密文长度不对（短于 nonce）")
	}
	plain, err := c.aead.Open(nil, raw[:ns], raw[ns:], []byte(aad))
	if err != nil {
		return "", fmt.Errorf("采集凭据解密失败（密文被篡改，或主密钥/门店不匹配）: %w", err)
	}
	return string(plain), nil
}

// CredentialStatus 是门店采集的启用状态。
//
// ⚠️ 三个取值不是布尔：中间那个 **not_configured** 有独立含义，
//
//	"从来没配过" 与 "配了但凭证被清空" 在数据上完全一样，
//	只有这个显式状态能区分。而"没配过"时**必须保留全部门店**，
//	绝不能退化成"全部排除" —— 那会让所有真供应商集体消失。
type CredentialStatus string

const (
	// StatusNotConfigured = 从未配置（行不存在或状态如此）。
	StatusNotConfigured CredentialStatus = "not_configured"
	// StatusEnabled = 已开启，job 已注册，采集时用它换 token。
	StatusEnabled CredentialStatus = "enabled"
	// StatusDisabled = 用户关停采集。**凭据保留**，重新开启不需要重新分发 secret。
	StatusDisabled CredentialStatus = "disabled"
)

// Credential 是某门店采集服务账号的本地凭据行。
//
// # ⚠️ SecretCipher 存的是**密文**，不是明文
//
//	明文只在两个时刻存在：auth 的创建响应里、以及本行的写入瞬间。
//	存密文是为了让"拿到 PG 备份 / pg_dump"不等于"拿到采集凭据" ——
//	主密钥在 systemd Environment 里，与本表不在同一个备份域。
type Credential struct {
	BranchID string `gorm:"column:branch_id;type:varchar(64);primaryKey"`
	// ClientID 冗余存一份，便于审计与人工排查（不必先知道分支规则）。
	// 它**必须**等于 ClientIDFor(BranchID)，由 Store 在写入时强制。
	ClientID string `gorm:"column:client_id;type:varchar(64);not null"`
	// SecretCipher 是 base64(AES-256-GCM(secret, aad=branch_id))。
	SecretCipher string `gorm:"column:secret_cipher;type:text;not null"`
	Status       string `gorm:"column:status;type:varchar(32);not null"`
	// CreatedBy 记代建人（用户 id，来自他的 JWT），用于审计与"谁开的"。
	CreatedBy string `gorm:"column:created_by;type:varchar(64)"`
	EnabledAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 显式给出表名。
//
// ⚠️ GORM 会按结构体名推导表名（Credential → credentials）。
// 这里必须显式，否则建出来的表叫 credentials，与迁移脚本里的名字对不上。
func (Credential) TableName() string { return "branch_collect_credentials" }

// Secret 返回该门店的明文 client_secret。
func (c *Cipher) Secret(row *Credential) (string, error) {
	if row == nil {
		return "", errors.New("凭据行为空")
	}
	return c.Open(row.SecretCipher, row.BranchID)
}

// CredentialStore 存取门店采集凭据。
//
// # ⚠️ 为什么是"读时现取"而不是启动时全量加载进内存
//
//	内存缓存在多副本下会变成"各副本缓存不同步"，而凭据是**刚被换过**的
//	（重新开启 = 不换 secret，但换状态；首次开启 = 新建）。缓存让状态翻转
//	要等进程重启才生效，而用户刚点的"开启"在界面上已经生效了。
//	一天两次换 token 的成本，远低于"用户说开了但采集还是 401"这种不一致。
type CredentialStore struct {
	db *gorm.DB
}

// NewCredentialStore 构造。db 为 nil 时所有方法返回明确错误（不 panic）。
func NewCredentialStore(db *gorm.DB) *CredentialStore {
	return &CredentialStore{db: db}
}

// Models 返回需要在**写者**侧 AutoMigrate 的模型。
func CredentialModels() []any { return []any{Credential{}} }

// Put 写入（或覆盖）某门店的凭据。
//
// ⚠️ 全量覆盖而非增量补丁：
//
//	两份 App 并发编辑时，"基于旧版本的增量"会静默丢条目；
//	全量覆盖的冲突至少是显式的（后写者赢，且前者的意图已不可追回）。
func (s *CredentialStore) Put(ctx context.Context, row *Credential) error {
	if s == nil || s.db == nil {
		return errors.New("采集凭据存储未装配（没有 PG 连接）")
	}
	if row == nil || strings.TrimSpace(row.BranchID) == "" {
		return errors.New("凭据行缺少门店 id")
	}
	want, err := ClientIDFor(row.BranchID)
	if err != nil {
		return err
	}
	// 强制一致：不接受调用方传一个与门店 id 不匹配的 client_id。
	// 那种不一致一旦落库，运行时会拿着 A 的账号去打 B 的门店，
	// 而症状是 403 —— 会被误判成"权限没授对"。
	if cid := strings.TrimSpace(row.ClientID); cid != "" && cid != want {
		return fmt.Errorf("凭据行的 client_id %q 与门店 %q 推导出的 %q 不一致", cid, row.BranchID, want)
	}
	row.ClientID = want
	if row.Status == "" {
		row.Status = string(StatusEnabled)
	}
	return s.db.WithContext(ctx).Save(row).Error
}

// Get 取某门店凭据；不存在返回 (nil, nil)。
func (s *CredentialStore) Get(ctx context.Context, branchID string) (*Credential, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("采集凭据存储未装配（没有 PG 连接）")
	}
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return nil, errors.New("门店 id 不能为空")
	}
	var row Credential
	err := s.db.WithContext(ctx).Where("branch_id = ?", branchID).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

// SetStatus 切换某门店的启用状态（关停采集 = disabled，不删行）。
func (s *CredentialStore) SetStatus(ctx context.Context, branchID string, st CredentialStatus) error {
	if s == nil || s.db == nil {
		return errors.New("采集凭据存储未装配（没有 PG 连接）")
	}
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		return errors.New("门店 id 不能为空")
	}
	return s.db.WithContext(ctx).Model(&Credential{}).Where("branch_id = ?", branchID).
		Updates(map[string]any{"status": string(st), "updated_at": gorm.Expr("NOW()")}).Error
}

// ListEnabled 列出所有已开启采集的门店，按门店 id ASC。
//
// ⚠️ 排序是为了让 Reconcile 的注册顺序稳定：数据未变时日志与 diff
// 也逐字不变，而"顺序变了"会被误读成"门店集合变了"。
func (s *CredentialStore) ListEnabled(ctx context.Context) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("采集凭据存储未装配（没有 PG 连接）")
	}
	var rows []Credential
	if err := s.db.WithContext(ctx).
		Where("status = ?", string(StatusEnabled)).
		Order("branch_id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if id := strings.TrimSpace(r.BranchID); id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

// ListAll 列出全部凭据行（**含未开启的**），按门店 id ASC。
//
// # 为什么需要它
//
//	健康检查要回答"哪些门店的密文解不开" —— 而 ListEnabled 只给 enabled 的，
//	缺了"未开启但密文已死"这一类（它们在重新开启时才会爆）。
//	⇒ 这里给全量，由调用方决定要不要按状态过滤。
//
// ⚠️ 返回的是**行拷贝**，调用方改它不会影响库。
func (s *CredentialStore) ListAll(ctx context.Context) ([]*Credential, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("采集凭据存储未装配（没有 PG 连接）")
	}
	var rows []Credential
	if err := s.db.WithContext(ctx).Order("branch_id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*Credential, 0, len(rows))
	for i := range rows {
		cp := rows[i]
		out = append(out, &cp)
	}
	return out, nil
}
