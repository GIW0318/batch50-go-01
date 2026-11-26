# 监护人同意范围、复核到期与时间事件解绑

## 题面

未成年人模式的监护人绑定上线后，客服收到两类投诉：监护人把同意范围从看进度收窄到只看周报，孩子端仍能改设置；监护人账号因为安全事件被停用，关联却还留在生效状态；孩子过了十八岁生日，第二天照样能用监护入口。落点 gsb/batch50-go-safety-a-15 的 guardian_consent_link.go，类型 GuardianConsentLink，请把邀请、接受、范围收窄、复核到期与时间事件驱动的解绑这条链路补成可用状态。

接口保持如下签名：NewGuardianConsentLink(cfg Config) *GuardianConsentLink、Enroll(accountID string, born time.Time) error、Invite(guardianID string, wardID string, scope Scope) (Link, error)、Accept(linkID string, proof Proof) (Link, error)、Revoke(linkID string, by string, reason string) error、Review(linkID string, decision Decision) error、Due(now time.Time) []Link、Allowed(linkID string, action string) (bool, error)、Grant(linkID string, scope Scope) error、DisableGuardian(guardianID string, reason string) (int, error)、EnableGuardian(guardianID string) (int, error)、Sweep(now time.Time) int、Attempts(linkID string) int、State(linkID string) string、Snapshot() ([]byte, error)、Restore([]byte) error。

需要落实的行为与边界：

1. Enroll 登记账号与出生日期，账号为空报 ErrEmptyIdentity，重复登记报 ErrAccountExists，出生日期不在过去报 ErrBadBirthDate。
2. Invite 的两个账号都必须已登记，否则报 ErrAccountUnknown；监护人与被监护人不能是同一账号，报 ErrSelfLink；被监护人按当前时间已成年报 ErrWardAdult。
3. 单监护人与单被监护人的未关闭关联数都受上限约束，超限报 ErrLinkLimit。
4. 同意范围至少一个动作、不得超过 MaxScopeActions，去重并按字典序保存；范围截止时间必须晚于当前时刻，否则报 ErrBadScope。
5. 关联标识形如 link-1，接受码形如 code-<关联标识>，invited 状态的序号为 1。
6. Accept 只接受 invited 状态，错误接受码累加尝试次数并报 ErrProofRejected；达到 MaxProofAttempts 后关联作废为 invalid，再试报 ErrLinkInvalid。
7. 接受成功转 active，记录 AcceptedAt 与 ReviewAt（当前时刻加复核间隔）；重复接受报 ErrLinkClosed；范围截止已过报 ErrLinkExpired。
8. Allowed 只对 active、未过范围截止且动作在范围内的请求返回 true；未知关联报 ErrLinkNotFound，其余状态一律 false。
9. Revoke 的 by 只认 guardian、ward、system，原因不能为空，否则分别报 ErrBadActor 与 ErrBadReason；未知关联报 ErrLinkNotFound，已关闭报 ErrLinkClosed。
10. Review 通过则刷新 ReviewAt 并回到 active，不通过则降为 readonly；Due 返回复核到期的活动关联并按复核时间升序，Sweep 把超过宽限期仍未复核的关联降为 readonly 并返回条数。
11. 快照带上账号、关联状态、范围、尝试次数、复核时间与关联序号，恢复后原样保留；恢复失败报 ErrBadSnapshot 且不留半个状态。

另有三条边界要点名：被监护人在关联存续期间成年，解绑要由时间事件触发，不能依赖下一次登录；监护人账号本身被停用时，关联应挂起而不是删除，恢复后要重新复核；同意范围收窄后，之前已授予的访问要立即失效，而不是等下次操作才检查。

两类硬约束：同一对监护关系的邀请与接受串行执行，关联与权限授予原子生效，并发下不会出现半条关联或两条生效关联；范围与关联数都是硬边界，重启后按快照恢复复核到期时间，过期未复核的关联降级为只读。

验收：在题目目录执行 go test ./...，十五条用例必须全部通过；不得修改测试、跳过用例或按固定输入写死结果；并发与重启恢复自行构造场景验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-a-15`
- 实现文件：`guardian_consent_link.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`GuardianConsentLink`

## 验收

15 条测试函数全部通过，无跳过、无按固定输入写死；并发与重启恢复自行验证。
