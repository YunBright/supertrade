package seed

// 2026-10-24:大宗品项(冷库里"不是零售、攒着统一结算"的东西)的初始装配。
//
// # 大宗品项 = **一个部位**(2026-10-24 门店口径)
//
// 门店原话:「添加大宗品项时,名称,编码,单位,POS货号都不需要,针对部位而已,
// 大宗台账的目标只是协助每日毛利计算和部位毛利分析,越简单越好。」
//
// ⇒ 装配数据只需要 **部位**;编码(= 部位 wire 值)、名称(= 部位中文名)、
// 单位(= kg)全部由服务端自动填(见 service.CreateBulkProduct)。
//
// # 为什么是 `fat`
//
// 猪油/板油是**分割副产品**:每头猪出几公斤,卖不完的**进冷库**压着,
// 攒到酒店(统购方)一次性收走时**才结账、才知道单价**。
//
// ⇒ 它必须挂一个部位(`fat`):统收结算的收入要落回产出它的部位,
// 否则那块 kg 的毛利永远是 -100%(有成本没收入)。
//
// 2026-10-23 实测(两店):
//   佳乐 00032 猪板油 —— 13/14 天在售,日均约 11 斤(零售档)
//   佳乐 03808 水油   —— 每 9~11 天整一笔 400~870kg @1.70~1.80(大宗档)
//   富百家 01445 猪油 —— 14/14 天在售,日均约 7.5 kg
//
// ⇒ 两店的 fat 都登记;**绑 POS 货号已取消**(2026-10-24):
// 大宗结算**手工登记**,不再由 POS 自动采信。
//
// # ⚠️ 2026-10-25:又加回来了 —— 但**语义不同**
//
// `03808 水油`是**每 9~11 天整一笔 400~870kg**的 POS 行(佳乐)。
// 2026-10-25 门店拍板：**这条线一律走手工统收结算** ⇒
//
//	POS 卖 03808 ⇒ **不按零售算**(不进鲜肉域的收入、不吃 FIFO)
//	⇒ 改由门店在「统收结算」里手工登记(领用冷库重量 + 记收入)
//
// ⇒ 而"绑 POS 货号"这个字段仍然需要 —— 它是**「这条货号不走零售」的标记**
// (**门店不填**,由装配写入;大宗品项本身仍然是"只选部位")。
//
// ⇒ 富百家的 `01445 猪油`是**每天零售 ~9.35kg** ⇒ 没有大宗线 ⇒ **不绑**。

// bulkProductSeed 一条大宗品项(只有部位 + 备注 + 可选的"大宗线 POS 货号")。
type bulkProductSeed struct {
	BranchID string
	CutType  string
	Note     string
	// PosCubeProductID 是**大宗线的 POS 货号**（空 = 该部位没有大宗线）。
	PosCubeProductID string
}

// bulkProductSeeds 大宗品项装配数据。
//
// ⚠️ **一个部位只能有一个大宗品项**(唯一约束 (branch_id, cut_type))。
var bulkProductSeeds = []bulkProductSeed{
	{
		BranchID: "01", CutType: "fat",
		PosCubeProductID: "03808",
		Note: "猪油/板油:卖不完的进冷库压着,攒到酒店统收时手工登记结算" +
			"(03808 水油的 POS 销售不自动进鲜肉域,一律在「统收结算」手工登记)",
	},
	{
		BranchID: "00", CutType: "fat",
		Note: "猪油/板油:卖不完的进冷库压着,攒到酒店统收时手工登记结算" +
			"(01445 猪油是每天零售的货,没有大宗线)",
	},
}

// BulkProductSeedEntry 一条大宗品项装配记录(对外)。
type BulkProductSeedEntry struct {
	BranchID string
	CutType  string
	Note     string
	// PosCubeProductID 是大宗线的 POS 货号（空 = 没有大宗线）。
	PosCubeProductID string
}

// BulkProductSeeds 大宗品项装配数据(对外)。
func BulkProductSeeds() []BulkProductSeedEntry {
	out := make([]BulkProductSeedEntry, 0, len(bulkProductSeeds))
	for _, s := range bulkProductSeeds {
		out = append(out, BulkProductSeedEntry{
			BranchID: s.BranchID, CutType: s.CutType, Note: s.Note,
			PosCubeProductID: s.PosCubeProductID,
		})
	}
	return out
}
