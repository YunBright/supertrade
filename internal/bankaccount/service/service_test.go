package service

import (
	"context"
	"errors"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	_ "modernc.org/sqlite" // pure-Go driver (no CGO)

	"github.com/YunBright/supertrade/internal/bankaccount/model"
)

// newTestDB 构造 SQLite 内存 DB + AutoMigrate。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Dialector{DriverName: "sqlite", DSN: ":memory:"}, &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.BankAccount{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// TestCreate_RequiresAllFields 锁住必填校验。
//
// 四项缺一不可:supplier_id / 开户行 / 账号 / 户名。
// ⚠️ 户名缺失最危险 —— 审批人看到"某银行 6222****"但不知道付给谁。
func TestCreate_RequiresAllFields(t *testing.T) {
	svc := New(newTestDB(t))
	ctx := context.Background()
	base := AccountInput{
		SupplierID:  "29",
		BankName:    "中国银行南宁分行",
		AccountNo:   "6228480012345678",
		AccountName: "南宁某某贸易有限公司",
	}
	cases := map[string]func(in AccountInput) AccountInput{
		"缺 supplier_id":  func(in AccountInput) AccountInput { in.SupplierID = ""; return in },
		"缺开户行":         func(in AccountInput) AccountInput { in.BankName = ""; return in },
		"缺账号":           func(in AccountInput) AccountInput { in.AccountNo = ""; return in },
		"缺户名":           func(in AccountInput) AccountInput { in.AccountName = ""; return in },
		"supplier_id 空白": func(in AccountInput) AccountInput { in.SupplierID = "   "; return in },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Create(ctx, "00", "u1", mut(base))
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
}

// TestCreate_DuplicateAccountRejected 锁住同 (branch,supplier,account_no) 唯一。
func TestCreate_DuplicateAccountRejected(t *testing.T) {
	svc := New(newTestDB(t))
	ctx := context.Background()
	in := AccountInput{
		SupplierID:  "29",
		BankName:    "中国银行",
		AccountNo:   "6228480012345678",
		AccountName: "某某公司",
	}
	if _, err := svc.Create(ctx, "00", "u1", in); err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}
	_, err := svc.Create(ctx, "00", "u1", in)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("重复创建 err = %v, want ErrAlreadyExists", err)
	}
}

// TestCreate_SameAccountDifferentBranchAllowed 跨门店同账号号互不冲突。
//
// 门店隔离是硬边界:门店 A 的账号不该在门店 B 查得到。
func TestCreate_SameAccountDifferentBranchAllowed(t *testing.T) {
	svc := New(newTestDB(t))
	ctx := context.Background()
	in := AccountInput{
		SupplierID:  "29",
		BankName:    "中国银行",
		AccountNo:   "6228480012345678",
		AccountName: "某某公司",
	}
	if _, err := svc.Create(ctx, "00", "u1", in); err != nil {
		t.Fatalf("门店00 创建失败: %v", err)
	}
	if _, err := svc.Create(ctx, "10", "u1", in); err != nil {
		t.Fatalf("门店10 创建同账号号应允许(不同门店各自维护),got %v", err)
	}
	// 门店 00 的列表里只能看到自己那一条。
	rows, err := svc.List(ctx, ListFilter{BranchID: "00"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("门店00 列表条数 = %d, want 1(跨门店数据串了)", len(rows))
	}
}

// TestSetDefault_OnlyOnePerSupplier 锁住"一个供应商最多一个默认账号"。
//
// 不变量:两个默认账号 ⇒ 前端下拉选中哪一个全靠运气。
func TestSetDefault_OnlyOnePerSupplier(t *testing.T) {
	svc := New(newTestDB(t))
	ctx := context.Background()
	a, err := svc.Create(ctx, "00", "u1", AccountInput{
		SupplierID: "29", BankName: "A行", AccountNo: "111", AccountName: "甲",
	})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := svc.Create(ctx, "00", "u1", AccountInput{
		SupplierID: "29", BankName: "B行", AccountNo: "222", AccountName: "乙",
	})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if err := svc.SetDefault(ctx, "00", a.ID, "u1"); err != nil {
		t.Fatalf("set default a: %v", err)
	}
	if err := svc.SetDefault(ctx, "00", b.ID, "u1"); err != nil {
		t.Fatalf("set default b: %v", err)
	}
	rows, err := svc.List(ctx, ListFilter{BranchID: "00", OnlyDefault: true})
	if err != nil {
		t.Fatalf("list defaults: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("默认账号数 = %d, want 1", len(rows))
	}
	if rows[0].ID != b.ID {
		t.Fatalf("默认账号 = %s, want %s(后设的应生效)", rows[0].ID, b.ID)
	}
}

// TestCreate_WithDefault_ClearsOthers 新建时就勾"设为默认"⇒ 其它同供应商账号的默认位被清掉。
func TestCreate_WithDefault_ClearsOthers(t *testing.T) {
	svc := New(newTestDB(t))
	ctx := context.Background()
	if _, err := svc.Create(ctx, "00", "u1", AccountInput{
		SupplierID: "29", BankName: "A行", AccountNo: "111", AccountName: "甲", IsDefault: true,
	}); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if _, err := svc.Create(ctx, "00", "u1", AccountInput{
		SupplierID: "29", BankName: "B行", AccountNo: "222", AccountName: "乙", IsDefault: true,
	}); err != nil {
		t.Fatalf("create b: %v", err)
	}
	rows, err := svc.List(ctx, ListFilter{BranchID: "00", OnlyDefault: true})
	if err != nil {
		t.Fatalf("list defaults: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("默认账号数 = %d, want 1", len(rows))
	}
}

// TestDelete_PromotesFallbackDefault 删掉默认账号 ⇒ 剩下的里 id 最小的一条被提为默认。
//
// 不变量:"该供应商始终有一个默认账号"(除非一个都不剩)。
func TestDelete_PromotesFallbackDefault(t *testing.T) {
	svc := New(newTestDB(t))
	ctx := context.Background()
	a, err := svc.Create(ctx, "00", "u1", AccountInput{
		SupplierID: "29", BankName: "A行", AccountNo: "111", AccountName: "甲", IsDefault: true,
	})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := svc.Create(ctx, "00", "u1", AccountInput{
		SupplierID: "29", BankName: "B行", AccountNo: "222", AccountName: "乙",
	})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if err := svc.Delete(ctx, "00", a.ID, "u1"); err != nil {
		t.Fatalf("delete a: %v", err)
	}
	rows, err := svc.List(ctx, ListFilter{BranchID: "00", OnlyDefault: true})
	if err != nil {
		t.Fatalf("list defaults: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("删默认后默认账号数 = %d, want 1(应自动提一个)", len(rows))
	}
	if rows[0].ID != b.ID {
		t.Fatalf("提为默认的 = %s, want %s", rows[0].ID, b.ID)
	}
}

// TestDelete_IsSoftDelete 软删后行仍在库里(审计/历史单据要对得上账)。
func TestDelete_IsSoftDelete(t *testing.T) {
	svc := New(newTestDB(t))
	ctx := context.Background()
	a, err := svc.Create(ctx, "00", "u1", AccountInput{
		SupplierID: "29", BankName: "A行", AccountNo: "111", AccountName: "甲",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Delete(ctx, "00", a.ID, "u1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, err := svc.List(ctx, ListFilter{BranchID: "00"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("软删后列表仍返回 %d 条, want 0", len(rows))
	}
	// 库里应仍有该行(deleted_at 非空)。
	var raw model.BankAccount
	if err := svc.db.Unscoped().Where("id = ?", a.ID).First(&raw).Error; err != nil {
		t.Fatalf("物理行不存在(应是软删): %v", err)
	}
	if raw.DeletedAt == nil {
		t.Fatal("deleted_at 为 nil —— 不是软删")
	}
}

// TestDelete_CrossBranchRejected 跨门店删别人的账号 ⇒ 404。
//
// 门店隔离:门店 10 不能删门店 00 的账号。
func TestDelete_CrossBranchRejected(t *testing.T) {
	svc := New(newTestDB(t))
	ctx := context.Background()
	a, err := svc.Create(ctx, "00", "u1", AccountInput{
		SupplierID: "29", BankName: "A行", AccountNo: "111", AccountName: "甲",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	err = svc.Delete(ctx, "10", a.ID, "u1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨门店删除 err = %v, want ErrNotFound", err)
	}
}

// TestList_FilterBySupplierAndDefault 锁住筛选语义。
func TestList_FilterBySupplierAndDefault(t *testing.T) {
	svc := New(newTestDB(t))
	ctx := context.Background()
	if _, err := svc.Create(ctx, "00", "u1", AccountInput{
		SupplierID: "29", BankName: "A行", AccountNo: "111", AccountName: "甲", IsDefault: true,
	}); err != nil {
		t.Fatalf("create 29: %v", err)
	}
	if _, err := svc.Create(ctx, "00", "u1", AccountInput{
		SupplierID: "29", BankName: "B行", AccountNo: "222", AccountName: "乙",
	}); err != nil {
		t.Fatalf("create 29b: %v", err)
	}
	if _, err := svc.Create(ctx, "00", "u1", AccountInput{
		SupplierID: "019", BankName: "C行", AccountNo: "333", AccountName: "丙",
	}); err != nil {
		t.Fatalf("create 019: %v", err)
	}

	// 按供应商过滤。
	rows, err := svc.List(ctx, ListFilter{BranchID: "00", SupplierID: "29"})
	if err != nil {
		t.Fatalf("list by supplier: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("供应商 29 的账号数 = %d, want 2", len(rows))
	}
	// 默认账号优先排序。
	if rows[0].AccountNo != "111" {
		t.Fatalf("第一条 = %s, want 111(默认账号应排最前)", rows[0].AccountNo)
	}
	// only_default。
	rows, err = svc.List(ctx, ListFilter{BranchID: "00", OnlyDefault: true})
	if err != nil {
		t.Fatalf("list defaults: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("默认账号数 = %d, want 1", len(rows))
	}
}

// TestUpdate_CanChangeSupplier 改挂供应商是允许的(账号可能挂错了)。
func TestUpdate_CanChangeSupplier(t *testing.T) {
	svc := New(newTestDB(t))
	ctx := context.Background()
	a, err := svc.Create(ctx, "00", "u1", AccountInput{
		SupplierID: "29", BankName: "A行", AccountNo: "111", AccountName: "甲",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	out, err := svc.Update(ctx, "00", a.ID, "u1", AccountInput{
		SupplierID: "019", BankName: "A行", AccountNo: "111", AccountName: "甲",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if out.SupplierID != "019" {
		t.Fatalf("supplier_id = %s, want 019", out.SupplierID)
	}
}

// TestUpdate_DuplicateRejected 改到已存在的 (supplier, account_no) ⇒ 拒。
func TestUpdate_DuplicateRejected(t *testing.T) {
	svc := New(newTestDB(t))
	ctx := context.Background()
	if _, err := svc.Create(ctx, "00", "u1", AccountInput{
		SupplierID: "29", BankName: "A行", AccountNo: "111", AccountName: "甲",
	}); err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := svc.Create(ctx, "00", "u1", AccountInput{
		SupplierID: "019", BankName: "B行", AccountNo: "222", AccountName: "乙",
	})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	_, err = svc.Update(ctx, "00", b.ID, "u1", AccountInput{
		SupplierID: "29", BankName: "B行", AccountNo: "111", AccountName: "乙",
	})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("改成重复账号 err = %v, want ErrAlreadyExists", err)
	}
}

// TestGet_CrossBranchNotFound 跨门店取详情 ⇒ 404。
func TestGet_CrossBranchNotFound(t *testing.T) {
	svc := New(newTestDB(t))
	ctx := context.Background()
	a, err := svc.Create(ctx, "00", "u1", AccountInput{
		SupplierID: "29", BankName: "A行", AccountNo: "111", AccountName: "甲",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Get(ctx, "10", a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨门店 get err = %v, want ErrNotFound", err)
	}
}
