# 英雄熟练度累计与里程碑发放

## 题面

赛季中熟练度榜连续出现异常：一场排位拿了 5000 点经验，英雄直接跳到满级还多出溢出的经验；同一场战斗被重连补发两次，经验记了两遍；玩家升到 3 级后去找客服，说 2 级的里程碑奖励领不到，客服按等级查后台又看不到中间等级的发放记录；榜单前十有三个人分数一模一样，每次刷新顺序都在跳，运营已经暂停了熟练度榜的展示。问题在 gsb/batch50-go-game-a-23 的 hero_mastery_ledger.go，请把 HeroMasteryLedger 修好，签名保持：NewHeroMasteryLedger(limits Limits) *HeroMasteryLedger、Award(account, hero string, rawXP int, mode string) (Gain, error)、AwardBattle(account, hero, battleID string, rawXP int, mode string) (Gain, error)、Level(account, hero string) (MasteryLevel, error)、Progress(account, hero string) (Progress, error)、SetModeFactor(mode string, factor float64) error、ClaimMilestone(account, hero string, level int) (Reward, error)、Leaderboard(hero string, n int) []Entry、Snapshot() ([]byte, error)、Restore(data []byte) error。

等级门槛：1 级为起点，升到第 n 级（n≥2）需要累计 60×n×(n−1) 点经验，即 2 级 120、3 级 360、4 级 720。

要修的地方共十二处：1) 单场经验先乘模式系数，再受单场上限截断，超出部分不结转，Gain 里的 RawXP、Effective、Capped 要能对上；2) 模式系数未注册返回 ErrUnknownMode，系数非正或大于 3 返回 ErrBadFactor，空模式名返回 ErrBadMode，被拒绝的系数不能覆盖旧值；3) 原始经验非正返回 ErrBadXP，空账号或空英雄返回 ErrBadIdentity，报错时不能建出空记录；4) 经验累加后按门槛晋升到可达的最高等级，多余经验保留在 XP 里，等级只增不减，客服能按等级复算经验；5) 等级上限由 Limits 决定，触顶后经验不再增长，溢出量丢弃而不是留到赛季末结算；6) 单场经验折算后为小数时向下取整入账，余数不累积到下一场，分数展示与后台保持一致；7) 一次跨多级时中间等级的里程碑奖励要各自可领，不能只发最高等级的奖励；8) 里程碑奖励每级只发一次，重复领取返回 ErrAlreadyClaimed，未达等级返回 ErrLevelNotReached，等级小于 2 返回 ErrBadLevel；9) 里程碑奖励额度为等级乘 100，发放额度写进返回值和进度里的已领列表；10) 同一场战斗被重连补发重复上报时按战斗标识幂等，只记一次经验，AwardBattle 第二次返回 Duplicate 置真的结果，重连补偿不能把榜单推高；11) 榜单按经验降序、账号升序排列，n 小于等于 0 返回空，n 超过人数返回全部；12) 账号与英雄组合数超限返回 ErrLimit，未知组合返回 ErrUnknownPair，损坏快照返回 ErrBadSnapshot。

硬约束：Award 与 AwardBattle 会被战斗结算服多路并发调用，同一账号同一英雄的经验累计与晋升必须串行，不能出现两次同时跨界各发一份里程碑；已处理战斗标识的表也要设上限，避免长赛季无限增长；快照与写入互斥，恢复后经验、等级、模式系数与已领里程碑原样延续，已领奖励不得重发。榜单读取不能阻塞结算，读到的是某一时刻的完整排名。

验收：go test ./... 共 15 条必须全过；不得改测试、跳过或按固定输入写死；并发与重启自己验，跨级晋升与榜单排序各压一遍；只做本地实现，交付源码、跑测记录与等级口径说明。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-23`
- 实现文件：`hero_mastery_ledger.go`
- 测试文件：`tests/test_hero_mastery_ledger_test.go`
- 包名：`task`；测试目录同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`HeroMasteryLedger`
