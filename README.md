# 坐骑召唤、强制落骑与耐久记账

## 题面

这周客服转上来三类单子：有玩家在副本门口按了召唤，引导条走到一半被人打断，坐骑照样出现在脚下；有玩家骑着坐骑跨进竞技场，人还挂在上面被判违规；还有一批人反映坐骑被打碎后重新召唤，新坐骑落地就是残血，耐久上限怎么也回不去。召唤引导、场景禁骑和坐骑耐久三处各自记账，谁也看不见谁。落点是模块 gsb/batch50-go-game-a-16 的 mount_controller.go，类型 MountController，公开接口保持：NewMountController(limits MountLimits, now time.Time) *MountController、RegisterMount(spec MountSpec) error、Summon(player, mountID string) (SummonID, error)、MountUp(player, mountID string) error、Dismount(player, reason string) error、State(player string) (MountState, error)、SetZoneRule(zone string, rule ZoneRule) error、EnterZone(player, zone string) ([]MountEvent, error)、Advance(now time.Time) ([]MountEvent, error)、Damage(mountID string, amount int) (Durability, error)、Interrupt(player, cause string) ([]MountEvent, error)、Events(player string) []MountEvent、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收掉的行为如下。一，召唤必须先进入引导，Summon 只登记召唤并返回新的 SummonID，阶段停在 summoning，不直接上骑，引导余量取坐骑登记时的 GuideMillis。二，同一玩家对同一坐骑在引导中重复召唤按幂等处理，返回同一个 SummonID，不新增召唤也不追加事件。三，引导没走完就调 MountUp 报 ErrGuideIncomplete，只有 Advance 把引导余量推到 0 之后阶段才转 ready，这时 MountUp 才进 mounted。四，同一玩家同一时刻只能有一只活动坐骑，已骑乘再召唤报 ErrAlreadyMounted，引导另一只坐骑时报 ErrSummonPending。五，坐骑与玩家绑定，Owner 不符报 ErrMountNotOwned，未登记报 ErrUnknownMount，玩家号为空报 ErrBlankPlayer。六，Dismount 的原因不能为空，否则报 ErrBadReason；未骑乘时调 Dismount 报 ErrNotMounted；正常落骑回到 idle 且不进入冷却。七，禁骑场景：Summon 命中 ForbidSummon 报 ErrZoneForbidsSummon；引导中的玩家被带进禁骑场景要把引导整份作废并发 summon_voided；已骑乘玩家跨进 ForbidMount 场景要被强制落骑，事件原因记 zone_forbidden。八，Damage 扣减当前活动坐骑实例的耐久，amount 非正报 ErrBadAmount 且不改数值，耐久归零要落骑、发 mount_broken 并把玩家推进冷却，冷却时长取坐骑的 CooldownMillis，冷却中召唤报 ErrCooldownActive，Advance 走完冷却回 idle 并补一条 cooldown_end。九，Interrupt 必须带原因：引导中作废引导，骑乘中按该原因落骑，缺原因报 ErrBadReason。十，事件按玩家分账，每人只保留最新 MaxEvents 条，序号严格递增，Events 返回副本。十一，坐骑登记数受 MaxMounts 硬顶，空 ID、耐久非正、引导或冷却为负一律报 ErrBadMountSpec，重复 ID 报 ErrMountExists。十二，快照要带坐骑表、场景规则、召唤实例、引导与冷却余量、事件序号，损坏载荷 Restore 报 ErrBadSnapshot 且不动现网，恢复后新召唤的序号必须大于快照里的任一条。

还有三处线上最容易漏。一是引导走满的同一帧玩家被击倒，这次召唤要整份作废，不能先上骑再落骑，更不能顺手扣掉一截耐久；二是已经骑乘的玩家被带进禁骑场景，强制落骑不消耗召唤冷却，冷却额度不能被一次场景切换吃掉；三是坐骑耐久归零后重新召唤要按新的坐骑实例记账，旧实例扣掉的耐久不能被新召唤继承。

硬约束两类。一是并发与原子性：多个会话同时替同一玩家召唤、上骑、落骑必须串行，同一玩家不能出现两只活动坐骑，Damage 与 Dismount 并发时读到的耐久不能是半更新值。二是重启恢复：召唤实例、引导余量、冷却余量与事件日志要跨进程恢复，恢复后 Advance 以快照时钟为基准推进，时钟倒退报 ErrTimeWentBackwards 而不是把引导清零。验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-16`
- 实现文件：`mount_controller.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`MountController`
