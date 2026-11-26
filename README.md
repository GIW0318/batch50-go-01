# 商店回购台账：卖出批次计价、槽位淘汰与窗口关闭判定

## 题面

回购列表上线第一天就出账：玩家把同一把强化剑分两次卖给铁匠，买回时两笔被并成一条，强化等级和单价全对不上；窗口关掉之后还有人被扣了金币却没拿回东西；背包塞满的玩家买回失败，金币扣了、槽位也空了。运营把这批工单按时间排了一遍，发现台账根本没有时间、批次和容量的概念。

落点是模块 gsb/batch50-go-game-a-07 的 buyback_ledger.go，类型 BuybackLedger，公开接口保持不变：NewBuybackLedger(limits Limits) *BuybackLedger、SetMarketPrice(itemID string, unit int64) error、SetBagFree(playerID string, slots int) error、SetWallet(playerID string, gold int64) error、Wallet(playerID string) int64、BagFree(playerID string) int、Sell(playerID string, items []ItemRef, at time.Time) (SellReceipt, error)、Quote(playerID string, slot int) (BuybackQuote, error)、BuyBack(playerID string, slot int, at time.Time) (ItemRef, error)、Expire(now time.Time) (int, error)、List(playerID string) ([]BuybackEntry, error)、Spent(playerID string) int64、SlotsUsed(playerID string) (int, error)、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收掉的地方如下。第一，空列表与 nil 列表一律报 ErrEmptySell，条数超过 MaxItemsPerSell 报 ErrTooManyItems，单条数量小于等于零报 ErrInvalidCount。第二，绑定物品不能卖，报 ErrBoundItem；没有登记市价或市价非正的物品报 ErrNoPrice，登记市价本身非正报 ErrInvalidPrice。第三，每个物品按批次占一个槽位，槽位编号从零开始取最小空闲位，一批一条记录，不能把不同批次揉成一条。第四，槽位用满后按卖出时刻淘汰最旧的一批，被淘汰的批次随回执返回，槽位编号复用。第五，回购单价取卖出那一刻的市价乘数量，之后市价怎么变都不影响已有批次，强化等级原样保留在条目里。第六，Quote 对空槽报 ErrSlotEmpty、越界槽报 ErrInvalidSlot，有效槽把 Valid 置真并给出单价、卖出时刻与到期时刻。第七，BuyBack 先判槽位与窗口，再判金币，金币不足报 ErrInsufficientGold 且槽位与钱包都不动；成功后扣金币、记消费、释放槽位、占一格背包。第八，Expire 只清到期批次并返回条数，重复调用第二次返回零。第九，List 按槽位升序返回，槽位号与条目一一对应。第十，快照要带上市价、钱包、背包空格、消费额与全部批次，恢复后 Quote、Wallet、BagFree、Spent 与关服前一致，损坏载荷报 ErrBadSnapshot 且不动现有数据。

还有三处必须一起收掉。同一件物品在窗口内被分两次卖出时，两批必须各自占槽并保留各自的单价与强化等级，不能合并。窗口关闭瞬间到达的买回请求以锁内时间判定为准，已经过期的一律不得扣款。背包已满时整单买回失败并保留槽位，不能先扣款再退款。

硬约束有两类。一是原子性，BuyBack 的判窗口、扣金币、扣背包、释放槽位必须在同一把锁内完成，多路请求并发打同一名玩家的槽位时只能成功一次，另一方看到的是金币与槽位都未变的旧状态。二是重启与上限，快照必须原子替换全部状态；SlotsPerPlayer、MaxItemsPerSell、MaxUnitPrice 都是硬顶，超出报错而不是截断或静默丢弃，逐出只能发生在槽位已满且新批次本身合法的时候。

验收：`go test ./...` 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；并发买回与重启自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-07`
- 实现文件：`buyback_ledger.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`BuybackLedger`
