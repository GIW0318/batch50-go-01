# 订阅月卡账本：到期日顺延与退款回收

## 题面

月卡上线第二个赛季后，账单和玩家体感都对不上。玩家在月卡到期当天续费，原本还剩十几个小时的权益被抹成从当天零点重算，后台显示有效期反而短了一截；有的账号在凌晨零点前后各领到一次每日奖励，同一天拿了两份；还有玩家退款之后，剩余天数按比例折算出现半天，账本上留下一段负数有效期，客服解释不清到底退了多少。问题落在模块 gsb/batch50-go-game-a-13 的 subscription_pass_ledger.go，类型 SubscriptionPassLedger，公开接口按现状保留：NewSubscriptionPassLedger(limits Limits) *SubscriptionPassLedger、PassDays(sku string) (int, bool)、Purchase(accountID, sku string, at time.Time) (Entitlement, error)、ClaimDaily(accountID string, at time.Time) (Grant, error)、EntitlementOf(accountID string, at time.Time) (EntitlementState, error)、Refund(accountID, orderID string, at time.Time) (RefundResult, error)、Sweep(now time.Time) (int, error)、Ledger(accountID string) []LedgerEntry、Orders(accountID string) []Entitlement、Debt(accountID string) int、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的口径如下。第一，购买时账户号只有空白报 ErrBlankAccount，SKU 只认 pass-30 与 pass-90，其它报 ErrUnknownSKU；每账号订单数超过 MaxOrdersPerAccount 报 ErrOrderLimit，累计购买天数超过 MaxDays 报 ErrDayLimit，被拒的购买不留订单也不写账。第二，未过期的续购从原到期日接着排，不覆盖当天剩余时间，新订单的起点等于上一张卡的到期日；已过期则从购买时刻重新起算。第三，到期日当天续费时，新有效期接在原到期日之后，不能从当天零点重算而吃掉剩余小时。第四，每日奖励按 UTC 固定锚点一天一次，同一个 UTC 日重复领取报 ErrAlreadyClaimed，跨天未领不顺延、不补发；跨时区部署时锚点仍按 UTC，不能因本地时区多领一次。第五，没有生效中的月卡、或月卡已过期时领取报 ErrExpired。第六，发放记录要能查：每笔领取写一条账，记下 UTC 日键、邮件号与发放数量，邮件号不能为空。第七，退款只针对本账号的、未退过且仍在有效期内的订单，订单不存在或不属于该账号报 ErrUnknownOrder，重复退款报 ErrAlreadyRefunded。第八，退款按剩余天数整天回收，回收价值等于天数乘每日奖励，退款后有效期截到退款时刻。第九，剩余不足一天时按整天扣减并在账本记下欠款，不能退成负数有效期。第十，账号状态要能按时刻查询：是否生效、到期时刻、累计天数、当日是否已领、UTC 日键，未购买过的账号返回不生效的空状态而不是报错。第十一，Sweep 把已到期账号折叠一次并返回本次变化的账号数，同一个账号只计一次，重复扫描不再计数。第十二，账本按发生先后返回购买、领取、退款三类记录，订单列表按购买先后返回。

硬约束有三条。同一账号的购买、领取、退款可能来自多个协程，同一 UTC 日只能成功领取一次，订单天数与账本要么全见旧值要么全见新值，不能出现半更新。进程重启后订单、账本与当日领取标记都要从快照恢复，恢复后继续续购仍从原到期日接续、当日已领的不能再领；损坏的快照报 ErrBadSnapshot 且不动现网。订单数、累计天数与每日发放额都是硬顶，超限报错而不是截断或按比例折算。

验收口径：go test ./... 的 15 条用例必须全过，测试不得修改、跳过、按固定输入写死；并发领取与重启恢复自行验证；只做本地实现，交付改动后的源码与跑测记录。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-13`
- 实现文件：`subscription_pass_ledger.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SubscriptionPassLedger`
