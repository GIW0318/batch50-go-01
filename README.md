# 职业进阶、技能点返还与转职回滚

## 题面

转职系统上周连出三起事故：有玩家等级和任务都够，点进阶后职业没变，界面只弹了一句前置不满足；有玩家转成剑士以后，身上还挂着只有旧职业能穿的板甲，属性照算；还有人背包塞满时点进阶，装备掉了、天赋点也扣了，职业却没切过去。落点是模块 gsb/batch50-go-game-a-19 的 class_advancement_guard.go，类型 ClassAdvancementGuard，公开接口保持：NewClassAdvancementGuard(limits Limits, now time.Time) *ClassAdvancementGuard、RegisterClass(spec ClassSpec) error、SetRequirement(class string, req Requirement) error、Enroll(player, class string) error、SetPlayerLevel(player string, level int) error、SetQuestFlag(player, flag string) error、SetSkillPoints(player string, points int) error、LearnSkill(player, skill string, points int) error、Equip(player, itemID string) error、SetBagFree(player string, n int) error、State(player string) (PlayerState, error)、Preview(player, toClass string) (Plan, error)、Advance(player, toClass string) (Result, error)、RefundSkills(player string) (int, error)、Revert(player string) error、CooldownLeft(player string) int64、AdvanceClock(now time.Time) error、History(player string) []Transition、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收掉的行为如下。一，职业登记：ID 为空报 ErrBadClass，重复 ID 报 ErrClassExists；给未登记职业设前置报 ErrUnknownClass，MinLevel 为负报 ErrBadRequirement。二，角色登记：空白角色号报 ErrBlankPlayer，未登记职业报 ErrUnknownClass；等级不为正报 ErrBadLevel，技能点数为负报 ErrBadPoints，背包空位为负报 ErrBadBag。三，前置求值：等级低于 MinLevel 记一条 level，前置职业不在角色履历里每个记一条 class:职业号，任务标记缺失每个记一条 quest:标记，缺失项按字典序排好一次给出；有缺失时 Advance 报 ErrRequirementNotMet，职业、天赋、装备、背包一律不动。四，Preview 只读：Plan 带起点职业、目标职业、是否通过、缺失项、可返还点数与需要卸下的装备，不写历史、不进冷却。五，进阶成功时职业切到目标职业，历史追加一条，带起点、终点、返还点数、卸下装备、时间与递增序号。六，技能点返只针对旧职业独有技能，投入的旧天赋要被清空，返还的点数加回可用点数；新职业也能用的技能点不得重复返还。七，装备处理：新职业不允许穿的装备要卸下放进背包，每件占一个格；背包空位不够时整次进阶回滚，职业、天赋、装备、背包保持原样并报 ErrBagFull。八，冷却：进阶成功后按 CooldownMillis 进入冷却，冷却期间换目标职业报 ErrCooldownActive，CooldownLeft 返回剩余毫秒。九，RefundSkills 返还角色全部已投点数并清空天赋表，不改职业。十，Revert 撤销最近一次进阶，把职业、天赋、可用点数、装备与背包还原到进阶前，一次进阶只能撤一次，没有可撤销记录报 ErrNoTransition。十一，AdvanceClock 只许单调前进，倒退报 ErrTimeWentBackwards。十二，History 按角色分账，长度受 MaxHistory 硬顶；快照要带职业表、前置条件、角色状态、历史与时钟，损坏载荷 Restore 报 ErrBadSnapshot 且不动现网，恢复后冷却与职业履历必须延续。

还有三处线上最容易漏。一是装备卸下时背包已满，进阶要整体回滚到进阶前的职业与天赋，不能留下半套已卸下的装备；二是技能点只返还未消耗的部分，花在共享技能上的点不得重复返还；三是冷却期间重复提交同一个目标职业按幂等处理，返回上一次的结果，而不是再报一次前置不满足。

硬约束两类。一是并发与原子性：同一角色同时只能进行一次进阶，多个会话同时提交必须串行，属性、装备与背包的读写要么是整次进阶前、要么是整次进阶后，不能读到只卸了一半装备的中间态。二是重启恢复：职业、天赋、返还记录与冷却要跨进程恢复，恢复后继续进阶的履历与中断前一致。验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-19`
- 实现文件：`class_advancement_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`ClassAdvancementGuard`
