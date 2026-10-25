# 结算付款审批：附件（结算单明细清单）链路

> 涉及仓库：`supertrade`（本文档所在，业务侧生成）/ `wechat-bot`（企微出口，上传+组装）/
> `deployer`（unit 模板）/ `wx-h5`（发起页）
> 最后更新：2026-10-25

---

## 1. 需求

企微「结算付款」模板里有一个 **File 型控件「附件」**（`item-1494251220825`，`require=0` 选填），
但后端从来不写它 ⇒ 审批单上那个控件一直是**空的**。

审批人在审批单上只能看到「结算单号(逗号分隔) + 付款金额 + 备注」，要判断
"这十几万到底付给了哪些供应商、每张多少"得自己去 sixun 查。

⇒ **附件放本次付款的结算单明细清单**（CSV）。

---

## 2. 企微模板控件表（`gettemplatedetail` 实测 2026-10-25）

| 控件 | id | 类型 | 必填 | 谁写 |
|---|---|---|---|---|
| 结算单号 | `Text-1791653939435` | Text | 是 | 本服务（`ApplyControlIDs[0]`） |
| 付款金额 | `Text-1791654387111` | Text | 是 | 本服务（`ApplyControlIDs[1]`） |
| 备注 | `item-1494251203122` | Textarea | 否 | 本服务（`ApplyControlIDs[2]`） |
| **附件** | **`item-1494251220825`** | **File** | 否 | **附件链路**（本文档） |
| AI审核结论 | `Textarea-1791692081606` | Textarea | 否 | 本服务（`AIConclusionControlID`） |

⚠️ **模板改了要同步两处**：`ApplyControlIDs`/`ApplyControlTypes`（前三个控件）与
`SETTLEMENT_AUDITOR_ATTACHMENT_CONTROL_ID`（附件控件）。企微的控件 id 随模板走。

### ⚠️ 读控件表要用 **POST + JSON body**

```bash
curl -X POST "https://qyapi.weixin.qq.com/cgi-bin/oa/gettemplatedetail?access_token=$TOK" \
     -H 'Content-Type: application/json' -d "{\"template_id\":\"$TPL\"}"
```

用 `GET ?template_id=...` ⇒ 企微**不读 query 参数** ⇒ 参数缺失 ⇒
**真实 id 与乱写 id 都返 301025**，看起来像"模板 id 非法/模板没了"，很容易误判成权限问题。
（2026-10-25 在这里绕了一大圈。判据：拿同一 token 调 `oa/getapprovalinfo`（POST+body）
按 template_id 过滤，能返回 sp_no 就说明模板和 id 都没问题。）

---

## 3. 为什么跨两个仓

企微的 File 型控件要的不是内容，而是 **media_id**：

```json
{"control":"File","id":"item-1494251220825",
 "value":{"files":[{"file_id":"<media_id>","file_name":"结算单明细-CP001.csv"}]}}
```

media_id **只能用持 OA token 的一侧上传素材才能拿到**（`media/upload?type=file`，multipart），
而 **OA secret 只有 wechat-bot 持有**（Dapr state `wechat-bot-manager||wecom-approval/config/00`）。

而"附件里放什么"是**业务知识**（结算单明细）⇒ 职责切分：

```
settlement-auditor（业务）                 wechat-bot（企微唯一出口）
  生成明细 CSV（附件内容）      ──wire──▶   base64 解码 → media/upload → 拿 media_id
  只带内容，不碰素材/控件                       → 往 apply_data.contents 追加 File 控件
```

### wire 契约（两边字段必须逐字对齐）

```go
// supertrade: internal/settlement-auditor/service/publisher.go
// wechat-bot: internal/wapproval/model.go
type ApprovalAttachment struct {
    ControlID    string `json:"control_id"`     // 企微模板控件 id
    ControlType  string `json:"control_type"`   // 默认 File
    FileName     string `json:"file_name"`      // 审批单上显示的名字
    ContentBase64 string `json:"content_base64"` // 文件内容
}
```

挂在 `ApplyEventPayload.attachment`（可选，缺省 = 不带附件）。

---

## 4. 业务侧实现（supertrade）

`internal/settlement-auditor/service/attachment.go`：

- `buildSettlementDetailCSV(rows []SettlementRow) []byte`
  - 列：结算单号 / 供应商(编号-名称) / 单据类型 / 账期 / 货款金额(元)，末行合计。
  - ⚠️ **带 UTF-8 BOM**（`EF BB BF`）：审批人拿 Excel 打开，不带 BOM 会被按 GBK 猜 ⇒ 整张表乱码。
  - 金额取 **cube 当前值**（与审批单上的付款金额同源，不会自相矛盾）。
  - 账期走 `normalizeCubeDate`（cube 有 ISO 与 MSSQL 美式两种串）。
- `buildSettlementDetailAttachment(rows)` → `*ApprovalAttachment`
  - 控件 id 为空 / 没有结算单 ⇒ 返回 nil（**不带附件**，不猜 id）。

`pipeline.go` 的 `stepFinish`（pass 分支）把它挂进 `ApplyEventPayload.Attachment`。
`stepFinish` 现在多一个参数 `settlements []SettlementRow`（流水线 step 1 已经拿到 cube 行，
直接复用 ⇒ **不为附件多打一次 cube**）。

### 配置

```bash
# cmd/settlement-auditor/main.go 读取
SETTLEMENT_AUDITOR_ATTACHMENT_CONTROL_ID=item-1494251220825
SETTLEMENT_AUDITOR_ATTACHMENT_CONTROL_TYPE=File
```

⚠️ **id 不给默认值**：模板是企微后台里的那一张，控件 id 随模板走 ⇒ 猜错 id 只会让企微报"控件不存在"。
⚠️ **改 env 要同时改两处**：`deployer/systemd/supertrade-settlement-auditor.service`（模板）
**和**服务器上的 `~/.config/systemd/user/supertrade-settlement-auditor.service`（手工安装的），
改完 `systemctl --user daemon-reload` —— 否则 `restart` 用的还是旧 unit，新 env 不生效。
（deploy 脚本只推二进制，**不上传 unit**。）

---

## 5. 企微侧实现（wechat-bot）

`internal/wapproval/service.go`：

- `attachFileControl(...)`：`base64` 解码 → `client.UploadFile(...)` → `injectFileControl(...)`。
- ⚠️ **附件失败不阻断整单**：附件控件是**选填**的，而审批人判断要不要付钱靠的是
  单号/金额/备注 ⇒ 上传失败只记 warn，**继续提交**（控件留空），不把付款审批卡死。
  （base64 非法是上游的 bug，不是网络问题 ⇒ 直接报错，让上游发现。）

`internal/wapproval/service.go::injectFileControl`：

- 用 `map[string]any` 往返，**不要用结构体** —— 结构体反序列化会把上游给的未知字段吃掉。

`internal/wechat/wecom/oa.go::UploadFile` + `token.go::postMultipart`：

- `POST /cgi-bin/media/upload?type=file&access_token=...`，**multipart**，字段名 `media`。
- 临时素材有 3 天有效期，但审批单提交后企微会把附件固化进单据 ⇒ 审批人看到的附件不会消失。

---

## 6. 测试

| 仓 | 测试 | 锁什么 |
|---|---|---|
| supertrade | `service/attachment_test.go` | BOM 在、表头/列/合计、MSSQL 账期规整、无名称的供应商、控件 id 为空 ⇒ 不带附件 |
| wechat-bot | `internal/wechat/wecom/oa_test.go` | 上传成功拿 media_id、业务错误要报错、空文件名本地失败（不发请求） |
| wechat-bot | `internal/wapproval/attachment_test.go` | 注入是**追加**（不弄丢上游控件）、类型默认 File、files 形状、坏 apply_data 要报错 |
| wechat-bot | `internal/wapproval/attachment_test.go`（组合用例） | 命令带附件 → 上传 → 注入 → 提交 applyevent（用仓库自带的假企微，不碰生产） |

**线上自查**（不产生审批单）：

```bash
# 用 OA token 传一个测试文件，确认企微接受这个 multipart 形状
curl -X POST "https://qyapi.weixin.qq.com/cgi-bin/media/upload?access_token=$TOK&type=file" \
     -F "media=@/tmp/明细.csv;type=text/csv"
```

---

## 7. 部署

```powershell
# 1) 企微侧（先）：它是上传素材那一侧
powershell -NoProfile -ExecutionPolicy Bypass -File F:\go\src\github.com\YunBright\deployer\deploy-wechat-bot.ps1 -Service wechat-bot-manager

# 2) 业务侧
powershell -NoProfile -ExecutionPolicy Bypass -File F:\go\src\github.com\YunBright\deployer\deploy-supertrade.ps1 -Service settlement-auditor
```

顺序：**先 wechat-bot**。老的 wechat-bot 会忽略未知的 `attachment` 字段（Go 的 json.Unmarshal 行为），
⇒ 先发业务侧的话，附件会**静默丢失**（不报错、就是没有附件）。

改过 unit env 的话，部署后记得 `systemctl --user daemon-reload` + 重启，
并核对**进程**里的 env（`tr '\0' '\n' < /proc/<pid>/environ | grep ATTACHMENT`），别只看 unit 文件。

---

## 8. 还没闭环的一件事

**真实审批单上看到附件**，需要一笔**能过机器审核**的付款审批。
近两天提交都被机器驳回了 ⇒ applyevent 这条路没被验证过。
验证方式：发一笔小额付款审批 → 企微审批单上应出现「结算单明细-<首单号>.csv」。
