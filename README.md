# 装备宝石镶嵌、套装计数与属性汇总

## 题面

这周装备线上出了三笔工单：一件胸甲连着两次往同一个孔位镶同一颗宝石，宝石扣掉了孔位还是空的；有玩家把套装宝石取出来换洗，属性面板上的套装加成一直挂着不掉；还有人在两个客户端同时往同一个孔位塞宝石，两边都提示成功，最后孔位里只有一颗，另一颗不知去向。落点是模块 gsb/batch50-go-game-a-17 的 socket_inlay_guard.go，类型 SocketInlayGuard，公开接口保持：NewSocketInlayGuard(limits Limits) *SocketInlayGuard、RegisterItem(spec ItemSpec) error、RegisterGem(spec GemSpec) error、SetSocketRule(tier string, rule SocketRule) error、GrantGem(player, gem string, n int) error、GemCount(player, gem string) int、Inlay(player, itemID string, socket int, gem string) (Result, error)、Extract(player, itemID string, socket int) (Gem, error)、Sockets(itemID string) ([]Socket, error)、Summary(player string) (Stats, error)、History(itemID string) []InlayRecord、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收掉的行为如下。一，装备登记：ID 或 Owner 为空、孔位颜色表为空报 ErrBadItem；孔位数超过所属品质的 MaxSockets 报 ErrSocketLimit；重复 ID 报 ErrItemExists；登记数越过 MaxItems 报 ErrItemLimit。二，宝石登记：ID 或颜色为空、套装阈值非正报 ErrBadGem，重复 ID 报 ErrGemExists，未登记的宝石一律 ErrUnknownGem。三，镶嵌先校验：孔位索引越界报 ErrSocketIndex，索引落在品质上限之外报 ErrSocketLocked，宝石颜色与孔位颜色不符报 ErrColorMismatch，孔位已有宝石报 ErrSocketOccupied，袋里没有这颗宝石报 ErrGemNotFound，不是装备主人报 ErrNotOwner；整次拒绝不得扣宝石、不得写历史。四，镶嵌成功时袋里对应宝石减一，孔位记下宝石，返回的 Result 带装备号、孔位、宝石、套装号、该套装当前孔位计数、是否触发套装、提交后的属性汇总与单调递增的序号。五，套装计数按孔位实时重算：同一套装号在各孔位上的宝石数达到 SetThreshold 即生效，每生效一套在属性上追加 SetBonusAttack、SetBonusDefense、SetBonusCrit。六，属性汇总等于全部孔位宝石属性之和加套装加成，一次提交，不允许只加半套。七，取出按品质规则：ExtractPolicy 为 return 时宝石回袋，为 destroy 时宝石销毁不回袋，两种规则都要清空孔位并写一条历史。八，同一孔位重复取出返回零值 Gem 并报 ErrSocketEmpty，不写历史。九，历史按装备分账，每条带序号、玩家、装备号、孔位、动作、宝石号与套装号，长度受 MaxHistory 硬顶，超出淘汰最旧。十，Summary 按玩家汇总其名下全部装备，没有装备的玩家返回零值且不报错。十一，快照要带装备表、宝石表、品质规则、各孔位状态、玩家宝石袋与历史，损坏载荷 Restore 报 ErrBadSnapshot 且不动现网，恢复后新历史的序号必须大于快照里的任一条。十二，GrantGem 的数量必须为正，否则报 ErrBadAmount 且不改袋。

还有三处线上反复出现。一是同一件装备被两个会话并发镶嵌同一个孔位时只能有一个成功，失败的请求不得扣掉宝石；二是套装计数在宝石取出后要立刻下降，属性汇总不能停在旧档位；三是取出规则为销毁时历史仍要保留宝石来源，已销毁的宝石不能再计入套装。

硬约束两类。一是并发与原子性：同一件装备的镶嵌与取出必须串行裁决，读方看到的孔位与属性汇总要么是整次操作前、要么是整次操作后，不能读到只扣了宝石没进孔位的中间态。二是重启恢复：孔位状态、宝石袋、套装计数与历史要跨进程恢复，恢复后继续镶嵌的结果与中断前一致。验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-17`
- 实现文件：`socket_inlay_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SocketInlayGuard`
