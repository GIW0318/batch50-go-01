# 装备属性洗练：锁定计价、加权随机池与预览提交

## 题面

洗练系统换了计价之后，客服这周收到三张单子：有玩家点了预览没提交，第二天再点确认，属性换了但扣的是昨天的价；有玩家把两条属性锁住重随，结果出来锁的那两条数值和顺序都变了，只有词条名字还留着；还有人一直洗到跟当前完全一样的结果，系统提示没有变化、一分钱没扣，历史里也没留痕，运营对不上账。落点是模块 gsb/batch50-go-game-a-20 的 attribute_reroll_guard.go，类型 AttributeRerollGuard，公开接口保持：NewAttributeRerollGuard(limits Limits, now time.Time, seed int64) *AttributeRerollGuard、SetPool(pool Pool) error、RegisterItem(spec ItemSpec) error、GrantShards(itemID string, n int) error、Shards(itemID string) int、Entries(itemID string) ([]Entry, error)、Lock(itemID string, idx int) (Cost, error)、Unlock(itemID string, idx int) error、UnlockAll(itemID string) error、Locked(itemID string) []int、Preview(itemID string, locked []int) (Roll, error)、Apply(itemID string, roll Roll) (Result, error)、History(itemID string) []RollRecord、AdvanceClock(now time.Time) error、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收掉的行为如下。一，随机池按品质档登记：档位为空、条目为空、条目名为空、权重非正、最小值大于最大值都报 ErrBadPool。二，装备登记：ID 或属性条目为空报 ErrBadItem，档位没有对应随机池报 ErrUnknownPool，重复 ID 报 ErrItemExists。三，预览：锁定下标为负、超过属性条数或同一下标重复报 ErrBadLock，锁定数超过 MaxLocks 报 ErrLockLimit，未登记装备报 ErrUnknownItem。四，计价：一次重随的基础价是 BaseRollCost，每多锁一条加 LockSurcharge，预览给出的 Cost 必须与实际扣费一致。五，预览不落库：不替换当前属性、不扣碎片、不写历史，只发一个带递增序号与到期时刻的待提交结果。六，结果生成：除锁定下标以外的每条属性都从对应档位的池里按权重抽取，数值落在条目的最小到最大值之间，同一结果里不得出现重名词条。七，提交只认最近一次预览的序号，序号对不上或已被用掉报 ErrStaleRoll，碎片不足报 ErrInsufficientShards 且不动属性与余额，扣费、替换属性与写历史在同一步完成。八，锁定：Lock 返回下次重随的预计价格与锁定条数，同一下标重复锁报 ErrBadLock，超过上限报 ErrLockLimit；Unlock 未锁定的下标报 ErrNotLocked，UnlockAll 一次清空。九，碎片发放数量必须为正，否则报 ErrBadShards，余额查询只读。十，历史按装备分账，每条带提交的属性、实付价格、毫秒时间与递增序号，长度受 MaxHistory 硬顶，超出淘汰最旧。十一，AdvanceClock 只许单调前进，倒退报 ErrTimeWentBackwards 且不改时钟，预览有效期取 PreviewTTLMillis。十二，快照要带随机池、装备属性、碎片余额、锁定集合、历史与时钟，损坏载荷 Restore 报 ErrBadSnapshot 且不动现网，恢复后新历史的序号必须大于快照里的任一条。

还有三处线上最容易漏。一是预览过期后直接提交要拒绝并让玩家重新预览，不能按过期结果替换属性却按新价格扣费；二是锁定的条目在结果里要逐项保持，包括数值与顺序，不能只保持词条类型；三是重随出与当前完全相同的属性组合时也要正常扣费并记账，不能按无变化跳过。

硬约束两类。一是并发与原子性：同一件装备的预览、提交与锁定必须串行裁决，读属性的一方看到的属性、碎片余额与历史属于同一时刻，不能读到扣了费没换属性的中间态。二是重启恢复：属性、余额、锁定集合、历史与预览序号要跨进程恢复，恢复后旧预览不能再用。验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-20`
- 实现文件：`attribute_reroll_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`AttributeRerollGuard`
