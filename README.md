# 误删道具找回：凭证保留期与一次性补发

## 题面

客服后台的误删找回工单最近两头冒烟。一边是玩家把误删的武器要回去了，两天后又来报背包里有两把同款，查下来是同一张处置凭证被两位客服先后批过，邮件发了两次；另一边是玩家的处置凭证早就过期，单据却还在待审队列里，批下去之后道具凭空复活，运营对不上账。还有玩家删了角色再来申请，补发邮件投不进去，单据一直挂在待处理里没人能收尾。问题落在模块 gsb/batch50-go-game-a-12 的 item_restore_desk.go，类型 ItemRestoreDesk，公开接口按现状保留：NewItemRestoreDesk(limits Limits) *ItemRestoreDesk、RecordDisposal(characterID string, items []DisposedItem, at time.Time) error、Submit(characterID string, receiptIDs []string, at time.Time) (RestoreCase, error)、Approve(caseID, operator string) (Grant, error)、Reject(caseID, reason string) error、Status(caseID string) (CaseState, error)、Receipt(receiptID string) (DisposedItem, bool)、Grants(characterID string) []Grant、OpenCases(characterID string) []string、MarkReacquired(characterID, instanceID string) error、MarkCharacterGone(characterID string) error、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的口径如下。第一，删除或出售时按件写处置凭证，凭证从处置时刻起算保留 RetentionDays 天，到期即失效；凭证号为空时由台席补号，凭证号重复报 ErrDuplicateReceipt。第二，角色号空白报 ErrBlankCharacter，一件物品都没报 ErrNoItems，数量非正或实例标识为空报 ErrBadItem，被拒的批次一件都不写。第三，提交时凭证不存在报 ErrUnknownReceipt，超出保留期报 ErrReceiptExpired，已被补发过报 ErrReceiptUsed，角色已删除报 ErrCharacterGone，一张单子的凭证数超过 MaxReceiptsPerCase 报 ErrTooManyReceipts。第四，同一周期按 UTC 自然日计，开单数超过 MaxCasesPerPeriod 报 ErrCaseQuota，单据总价值超过 MaxValuePerPeriod 报 ErrValueQuota，超限时不开单也不占额度。第五，一批凭证里有的过期、有的仍然有效时整单拒绝并列清失效项，不能只补发有效的那部分。第六，丢弃之后玩家又在副本里捡回同种物品时，要按实例标识认出已重新获得并拒绝，不能只看物品名。第七，审批人为空报 ErrOperatorRequired，单据不存在报 ErrCaseNotFound，单据不处于待审状态报 ErrCaseClosed。第八，审批通过只生成一份补发：记下补发号、单据号、角色号、邮件号与实例清单，价值等于凭证之和，同时把该单凭证全部标记为已使用。第九，补发统一走邮件并按凭证幂等，同一凭证被两名客服同时审批只生成一次补发，后到的审批返回已处理，不得发出第二封邮件。第十，驳回必须带原因，空白原因报 ErrReasonRequired；驳回不动凭证的可用性，玩家可以拿同一批凭证重新开单，已驳回的单据不能再被审批。第十一，待审列表按开单先后返回，已通过或已驳回的单据不再出现。第十二，快照要能描述凭证、单据与补发记录，恢复后待审单据照常审批、已用凭证不回到可用；损坏的快照报 ErrBadSnapshot 且不动现网。

硬约束有三条。同一凭证可能被两个客服同时审批，补发只能产生一次，开单、审批与额度记账要么全见旧值要么全见新值，不能出现半更新。进程重启后凭证保留期、已用标记、单据状态与补发记录都要从快照恢复，已补发过的凭证不得二次补发。单张单据的凭证数、每周期开单数与总价值都是硬顶，超限报错而不是截断、先补一部分或事后追认。

验收口径：go test ./... 的 15 条用例必须全过，测试不得修改、跳过、按固定输入写死；并发审批与重启恢复自行验证；只做本地实现，交付改动后的源码与跑测记录。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-12`
- 实现文件：`item_restore_desk.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`ItemRestoreDesk`
