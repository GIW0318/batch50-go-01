# 副本进度存档：检查点、队伍同步与回滚

## 题面

上周维护后，三个固定队的副本进度全乱了：有人在二王打完掉线，重连后回到了一王门口；有队伍在回滚奖励之后，玩家背包里的装备被直接扣走，客服只能一件件补；还有一支队伍在同一检查点上被记了两次进度，结算界面显示的通关时间和掉落对不上。运营复盘时发现，副本服务只按玩家自己的上报推进，既没有以副本为单位锁住检查点，也没有在回滚时把已发出的奖励先挂起。落点在模块 gsb/batch50-go-game-a-03 的 dungeon_checkpoint.go，类型 DungeonCheckpoint，公开接口保持不变：NewDungeonCheckpoint(cfg RunConfig) *DungeonCheckpoint、OpenRun(runID string, party []string) (Run, error)、CloseRun(runID string) error、Join(runID, playerID string, claimedCheckpoint int) (Member, error)、Leave(runID, playerID string) error、Advance(runID string, checkpoint int) (Run, error)、GrantReward(runID, playerID, itemID string, qty int) (Grant, error)、Rollback(runID string, checkpoint int) (Run, error)、Run(runID string) (Run, bool)、Members(runID string) []Member、Grants(runID string) []Grant、Reclaims(runID string) []Reclaim、ClaimReclaim(runID, playerID, itemID string) (Reclaim, error)、Snapshot() ([]byte, error)、Restore(data []byte) error。

口径如下。第一，开本时副本号空白报 ErrBlankRun，空队伍报 ErrEmptyParty，成员号空白报 ErrBlankMember，成员重复报 ErrDuplicateMember，人数超过 MaxParty 报 ErrPartyFull，副本数超过 MaxRuns 报 ErrRunLimit，重复开同一个副本报 ErrRunExists；副本从 0 号入口开始，全员在线且都记在 0 号检查点，成员序号按入队顺序从 0 递增。第二，离队只把成员标成离线并保留槽位，重复离队幂等成功，未知成员报 ErrUnknownMember，未知副本报 ErrUnknownRun。第三，推进检查点必须逐级加一，跳级报 ErrCheckpointGap，原地或倒退报 ErrCheckpointRegress，超过 Checkpoints 报 ErrCheckpointLimit，只要有一个人离线就报 ErrMemberOffline 且进度不动；成功后整队成员的检查点一起更新。

还有两处必须一起收掉。掉线成员重连时客户端会带一个自报的检查点，服务端不能采信这个值，重连后成员必须落回副本当前的检查点，不能让同队成员各自停在不同进度上。回滚撤销已经发出去的奖励时，不能直接去扣玩家背包，必须把每一笔被撤销的奖励挂进待回收队列，由回收器凭队列去处理；同一笔奖励只挂一次。

第四，发奖只允许发给在线成员，道具为空报 ErrBlankItem，数量非正报 ErrBadQuantity，未知成员报 ErrUnknownMember，离线成员报 ErrMemberOffline，奖励条数超过 MaxGrants 报 ErrGrantLimit，未知或已关闭的副本分别报 ErrUnknownRun、ErrRunClosed；同一成员在同一检查点重复领取同一道具要幂等，返回原来那条记录且不新增。第五，回滚目标必须严格小于当前检查点，否则报 ErrRollbackForward，负数报 ErrCheckpointRegress；回滚后副本与全队成员回到目标检查点，目标检查点之后的奖励从账本里撤掉。第六，待回收条目由 ClaimReclaim 领取，没有对应待回收条目报 ErrUnknownReclaim，未知副本报 ErrUnknownRun，同一笔奖励只能被领走一次。第七，关闭副本后推进、发奖、回滚一律报 ErrRunClosed，重复关闭幂等成功。

硬约束两条。一是并发：同一副本上的发奖、回滚与领取必须串行结算，多路并发提交同一笔奖励最后只能留下一条账本记录，不能出现重复发奖。二是重启：快照要带上每个副本的检查点、成员在线状态与检查点、奖励账本、待回收队列与序号，恢复后 Run、Members、Grants 必须与快照前一致，损坏的载荷报 ErrBadSnapshot 且不能动到现网副本。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-03`
- 实现文件：`dungeon_checkpoint.go`
- 测试目录：`tests/`，通过导入路径引用本模块
- 包名：`task`；主要类型：`DungeonCheckpoint`、`RunConfig`、`Run`、`Member`、`Grant`、`Reclaim`
