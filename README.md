# 分线频道分配：软硬上限分流、整队同线与切换冷却

## 题面

跨服战场开放后，同一张地图的玩家被塞进一条线，另一条线却几乎空着；队长带着四个人切图，三个人进了新线、一个人留在旧线，队伍语音当场断掉；世界首领开启期间还有人靠反复切线躲机制。值班同学把当晚的分线记录拉出来比对，发现分流、整队、切线三件事各写各的，谁也没有真正管住容量。

问题落在模块 gsb/batch50-go-game-a-06 的 channel_line_assigner.go，类型 ChannelLineAssigner，公开接口保持不变：NewChannelLineAssigner(limits Limits) *ChannelLineAssigner、OpenLine(mapID string, capacity int) (LineState, error)、CloseLine(mapID, lineID string, at time.Time) ([]string, error)、ReserveParty(mapID, partyID string, members int) (string, error)、Enter(mapID, playerID, partyID string, at time.Time) (Assignment, error)、Leave(mapID, playerID string) error、Switch(mapID, playerID, targetLine string, at time.Time) (Assignment, error)、Rebalance(mapID string, at time.Time) ([]Move, error)、LockSwitching(mapID string, locked bool) error、IsLocked(mapID string) (bool, error)、Locate(mapID, playerID string) (Assignment, bool)、Lines(mapID string) ([]LineState, error)、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收掉的地方如下。第一，容量小于等于零直接报 ErrInvalidCapacity，线路编号按 line-1、line-2 顺序生成，软上限由容量乘 SoftRatio 向下取整且至少为一。第二，单张地图的线路数量受 MaxLinesPerMap 硬顶，超出报 ErrLineLimitReached。第三，单人进入优先落到占用最低且未过软上限的线，占用相同时按编号升序；所有线都过软上限但仍有余量时，允许压进最空的一条，不得把已经在线的人挤走。第四，所有线都到硬上限时报 ErrLineFull，不踢人也不临时开新线。第五，同一玩家重复进入只占一个座位，返回同一条线，占用不增加。第六，离开释放座位，对不在本图的玩家报 ErrPlayerNotOnMap。第七，切线要过冷却，冷却未到报 ErrSwitchCooling，目标线不存在报 ErrLineNotFound，目标线与当前线相同报 ErrSwitchSameLine，成功切线的冷却截止时间等于本次切换时刻加 SwitchCooldown。第八，地图锁定切线期间 Switch 一律报 ErrSwitchLocked，锁定状态要能被 IsLocked 读到。第九，关闭线路把在途玩家清出来，返回的玩家编号按字典序升序，线路状态变 closed 且占用归零。第十，Rebalance 只搬单人玩家，把超过软上限的线往低于软上限的线匀，单次搬动条数不超过 MaxMovesPerRebalance，Move 结果按玩家编号升序。第十一，快照要带上线路、占用、队伍绑定、锁定与冷却截止时间，恢复后 Locate 与 Lines 的结果和关服前一致；损坏载荷报 ErrBadSnapshot 且不动现有数据。第十二，队伍预占按整队人数一次占够，名额记在线路的 Reserved 上，队员逐个进入时消耗预留而不额外扩容。

还有三处必须一起收掉。整队预占时，如果没有任何一条线的剩余硬容量装得下全队，就整队拒绝并报 ErrPartyNoRoom，绝不能把队员拆到两条线上。单人被自动分流之后，同队好友后进来要优先跟随队长所在线，但不得挤掉已经在线的人。目标线在切线途中被关闭时，玩家回原线，这一次不计入切换冷却。

硬约束有两类。一是并发，Enter、Leave、Switch、CloseLine、Rebalance 会被多路网关协程同时调用，同一个座位只能被占一次，任意时刻的占用总数都不允许超过硬上限，-race 下不得有数据竞争。二是重启与配额，快照必须原子替换全部状态，恢复失败不得半套生效；MaxLinesPerMap、MaxPartySize、MaxMovesPerRebalance 三个上限都是硬顶，超限报错而不是截断或静默丢弃。

验收：`go test ./...` 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；并发与重启自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-06`
- 实现文件：`channel_line_assigner.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`ChannelLineAssigner`
