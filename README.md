# 离线收益结算与领取记账

## 题面

活动结束后客服收到一批投诉：有人离线 12 小时只拿到 1 小时收益，有人挂了两张同类加成卡却被按相乘结算，还有人重复点“领取”把同一笔离线收益领了两遍，背包里凭空多出两堆材料。运营查了三天也没对上账，只能先按工单逐笔补偿，补偿名单和后台流水又对不上，玩家开始怀疑活动被暗改。问题在 gsb/batch50-go-game-a-22 的 offline_earnings_ledger.go，请把 OfflineEarningsLedger 的结算与领取链路修好，签名保持：NewOfflineEarningsLedger(limits Limits) *OfflineEarningsLedger、Record(player string, at time.Time) error、Settle(player string, now time.Time) (OfflineReport, error)、Preview(player string, now time.Time) (Reward, error)、Claim(player string) (Reward, error)、Pending(player string) (Reward, bool)、Bank(player string) int64、SetCap(d time.Duration) error、SetRate(player string, rate Rate) error、SetBagCapacity(n int) error、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的地方共十二处：1) 离线时长按最后在线时刻与结算时刻之差计算，超过 SetCap 设的上限要截断，报告里 Capped 置真，截断部分不能偷偷换成邮件补发；2) 产出按基础速率折算到秒，结果向下取整，报告里的 Seconds 与 Amount 要能对上，两者不能各算一套；3) 同一种加成 Kind 只取最高百分比，不能把同类倍率相乘，否则挂两张卡就能刷出双倍收益；4) 不同 Kind 的加成相乘，按整数百分比先乘后除，取整顺序要固定；5) Settle 只生成待领取账目，不动玩家余额，Claim 才真正入账，未领取的账目重启后还在，Preview 只算不落账，不生成待领取账目，也不改最后在线时刻；6) 重复 Claim 返回零值奖励和空错误，余额不再变化，也不能生成第二条账目；7) 没有待领取账目时 Claim 返回零值奖励，不报错，客服可以放心重试；8) 结算时刻早于最后在线时刻返回 ErrBadTime，等于最后在线时刻按零收益结算；9) 未领取又离线时，新离线时长从上次结算时刻起算，不能和未领取账目重复叠加，否则一次挂机能被结算两遍；10) 离线时长跨过加成道具过期时刻时，只对过期前的部分应用加成，过期后按基础速率；11) 领取时背包已满要把未入包的部分保留为待领取，不能丢弃也不能转成不可查的邮件，玩家背包满时不该白跑一趟；12) 空玩家号在 Record、SetRate、Preview、Claim 上返回 ErrBadPlayer，基础速率非正或加成为负返回 ErrBadRate，上限时长非正返回 ErrBadCap，未知玩家返回 ErrUnknownPlayer，玩家总数、单玩家加成条数超限返回 ErrLimit 且不给超限玩家建账，损坏快照返回 ErrBadSnapshot。

硬约束：Record、Settle、Claim 会被登录网关、活动服与商城服务并发调用，同一玩家的记账要串行，同一笔待领取账目只能被领取一次，两个客户端同时点领取只能有一个拿到货；快照与写入互斥，恢复后离线基准时刻、待领取账目与余额都要延续，重启不能重复发钱。速率与上限只在配置变更时生效，已经结算过的时长不按新速率回溯，结算与领取的先后顺序由服务端时间决定，客户端本地时间只作展示。

验收：go test ./... 共 15 条必须全过；不得改测试、跳过或对固定时间点写死；并发与重启自己验，并发领取用多协程压一遍；只做本地实现，交付源码、跑测记录与结算口径说明。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-22`
- 实现文件：`offline_earnings_ledger.go`
- 测试文件：`tests/test_offline_earnings_ledger_test.go`
- 包名：`task`；测试目录同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`OfflineEarningsLedger`
