# 新手引导推进与步骤奖励治理

## 题面

线上新手引导在第 3 步卡死：玩家关掉客户端再回来，引导从第 1 步开始重放，登录奖励又多发了一份；另一批玩家点了“跳过引导”之后，后续步骤的奖励照样到手，运营对账对不平；还有玩家引导做到一半掉线，重新登录后引导箭头全部归零，只能从头再点一遍。上线三天收到三百多张相关工单，补偿发了两次才把投诉压住。问题落在 gsb/batch50-go-game-a-21 的 tutorial_progress_guard.go，请把 TutorialProgressGuard 这条推进链路修好，公开签名保持不变：NewTutorialProgressGuard(limits Limits) *TutorialProgressGuard、Begin(player, flowID string) error、Complete(player, step string) (Progress, error)、Skip(player, flowID string) error、Replay(player string) error、Current(player string) (Step, error)、Grant(player, step string) error、SetFlow(flowID string, steps []StepDef) error、Flow(flowID string) []StepDef、State(player string) (Progress, bool)、Rewards(player string) []RewardRecord、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的地方共十二处：1) 步骤只能按流程定义顺序完成，跳步返回 ErrOutOfOrder，当前步骤原地不动，进度序号也不得前移，否则玩家会反复卡在同一步；2) 未知流程返回 ErrUnknownFlow，未知步骤返回 ErrUnknownStep，未知玩家返回 ErrUnknownPlayer，空玩家号返回 ErrBadPlayer，报错请求不能创建玩家记录；3) 同一步重复 Complete 按幂等处理，返回与首次一致的进度和序号，不重复推进，也不额外记账；4) 每步奖励只发一次，重复 Grant 返回 ErrAlreadyRewarded，对未完成步骤 Grant 返回 ErrStepNotReached，被拒绝的请求不能写奖励账；5) 整链跳过要落成已跳过标记，之后任何 Complete 都返回 ErrFlowSkipped，且不再产生奖励；6) 流程定义里步骤 ID 重复返回 ErrDuplicateStep，步骤数超过上限返回 ErrLimit，空流程定义返回 ErrBadFlow，被拒绝的定义不能覆盖旧版本；7) 玩家在步骤中途下线，重新登录要回到同一步，步骤内已触发的子状态不能丢，否则玩家会认为进度被清空；8) 流程升级后新增步骤要插在当前步骤之后，不能把已完成玩家打回到新增步骤之前的旧步骤；9) 跳过后玩家手动重播只能重播表现，不能再次发放步骤奖励；10) 单玩家奖励记录条数与引导玩家总数各设上限，超限返回 ErrLimit，超限请求不能留下半条记录；11) 快照要带上流程定义、每个人所在步骤、步骤内子状态与已发奖励，恢复后序号继续递增而不是回绕，客服补发时以快照为准；12) 损坏快照恢复要返回 ErrBadSnapshot，现有进度、流程定义与奖励账一条都不能动。

硬约束：同一玩家的 Begin、Complete、Grant 会被网关多路并发调用，单玩家操作必须串行裁决，同一步奖励在并发下也只能落一次账，不能出现同一步发两份；同一玩家的重播请求也走这把锁，重播期间不允许 Complete 插进来；快照与写入互斥，读到的只能是全量旧状态或全量新状态，不能读到写了一半的玩家表。重启后进度、子状态与已发奖励原样延续，任何已发奖励不得重放，客服凭奖励账能说清每一步发了多少。

验收：go test ./... 共 15 条必须全过；不得改测试、跳过或用固定输入写死结果；并发与重启自己验；只做本地实现，交付源码与跑测记录。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-21`
- 实现文件：`tutorial_progress_guard.go`
- 测试文件：`tests/test_tutorial_progress_guard_test.go`
- 包名：`task`；测试目录同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`TutorialProgressGuard`
