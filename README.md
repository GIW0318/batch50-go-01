# 低等级区域属性压制与恢复

## 题面

跨服副本开放后玩家反馈两种怪象：60 级角色进 20 级区域，血量和攻击只掉了一点，照样一刀一个；另一些玩家出本之后属性没回满，反而比进本前更低，打个日常本都要喝药；还有玩家在副本门口来回走两步，属性一次比一次低，最后被压得只剩一点血。值班同学在场景服上复现，发现同一帧里离开与进入的日志顺序并不固定。问题在 gsb/batch50-go-game-a-25 的 level_sync_guard.go，请把 LevelSyncGuard 修好，签名保持：NewLevelSyncGuard(limits Limits) *LevelSyncGuard、SetZoneCap(zone string, level int) error、SetSyncRule(rule SyncRule) error、SetPlayerLevel(player string, level int) error、SetBaseStats(player string, stats Stats) error、SetGear(player string, level int, perLevel Stats) error、AddBuff(player string, delta Stats) error、Enter(player string, zone string) (BaseStats, error)、Leave(player string) error、Effective(player string) (Stats, error)、State(player string) (SyncState, error)、Snapshot() ([]byte, error)、Restore(data []byte) error。

压制口径：玩家等级高于区域上限时，每高 1 级削减 SyncRule.PercentPerLevel 个百分点，削减总量不超过 100 减 SyncRule.MinPercent，有效属性按剩余百分比取整；装备按 min(装备等级, 区域上限) 折算每级加成。

要修的地方共十二处：1) 未配置的区域进入时返回 ErrUnknownZone，区域上限非正返回 ErrBadCap，空区域号返回 ErrBadZone；2) 玩家等级低于或等于区域上限时不做压制，有效属性等于基准加装备加增益；3) 进入时返回的基准属性是进入瞬间的快照，不是压制后的数值，客服拿它核对原始面板；4) 离开后有效属性回到基准加装备加增益，State 的 Active 置假；5) 未进入就 Leave 返回 ErrNotSynced，重复 Leave 也不该悄悄成功；6) 装备等级高于区域上限时按钳制后的等效等级参与计算，装备本身不被修改，State 里同时给出原始等级与等效等级；7) 压制期间获得的新增益在离开时要保留，不能被进入时的基准快照覆盖掉；8) 重复进入同一区域不能重新快照基准，否则会把已压制的属性当成原始属性；9) 跨区域连续切换时恢复与压制在同一帧发生，有效属性只能按新区域计算一次，不能叠加两次压制；10) 同步规则百分比为负或大于 100、保底百分比越界返回 ErrBadRule，负属性返回 ErrBadStats，非正玩家等级返回 ErrBadLevel；11) 玩家表与区域表设上限，超限返回 ErrLimit，被拒绝的写入不能改动已有面板；12) 损坏快照恢复返回 ErrBadSnapshot，现有压制状态、基准快照与规则都不动。

硬约束：Enter、Leave、Effective 会被场景服与战斗服并发调用，同一玩家的状态迁移必须串行，不能出现半压制半恢复的读数；快照与写入互斥，恢复后区域上限、同步规则、基准快照与当前压制状态都要延续，重启后玩家不能凭空回满属性，也不能被多压一层。

验收：go test ./... 共 15 条必须全过；不得改测试、跳过或按固定输入写死；并发与重启自己验，跨区域切换至少压一遍；只做本地实现，交付源码、跑测记录与压制口径说明。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-25`
- 实现文件：`level_sync_guard.go`
- 测试文件：`tests/test_level_sync_guard_test.go`
- 包名：`task`；测试目录同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`LevelSyncGuard`
