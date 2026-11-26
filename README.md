# 装备分解产出与批量回滚治理

## 题面

装备分解上线后材料产出对不上：分解一把紫装给的矿石比表里少，同品质两把一起分解还会被按总量合并取整；更糟的是批量分解时夹了一件绑定装备，前几件已经化成材料，最后一件报错，背包里多出半批东西，玩家拿这批材料换了强化石，运营只能连夜回档；还有玩家把同一把武器在不同分线各点一次分解，两份材料都进了包，客服现在只能逐单手工核对。问题在 gsb/batch50-go-game-a-24 的 salvage_yield_ledger.go，请把 SalvageYieldLedger 修好，签名保持：NewSalvageYieldLedger(limits Limits) *SalvageYieldLedger、Preview(items []ItemRef) ([]Yield, error)、Salvage(player string, items []ItemRef) (BatchResult, error)、SetTable(tier string, table Table) error、YieldOf(item ItemRef) ([]Yield, error)、History(player string) []BatchRecord、SetBatchLimit(n int) error、SetBagCapacity(n int) error、Bag(player string) map[string]int、AlreadySalvaged(id string) bool、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的地方共十二处：1) 产出按品质档与强化等级查表，找不到精确档位时取不超过该强化等级的最高档，缺失档位不能让玩家白拆；2) 同档多件按逐件结算后相加，不能先求和再取整，两把加五的紫装要按两件各算一次；3) 品质档未配置返回 ErrUnknownTier，空装备号返回 ErrBadItem，报错不做任何记账；4) 绑定装备返回 ErrBoundItem，任务物品返回 ErrQuestItem，两者都不能产出材料，误拆一件就得回档整批；5) 批量分解中夹带一件绑定物时整批拒绝，不能先分解前几件再报错；6) 产出材料超出背包容量要整件回滚，不能把超出的材料静默丢弃，也不能只回滚超出那一件；7) 同一装备再次提交分解要按已分解返回空产出，不能二次产出材料；8) Preview 只算不落账，不写历史、不占背包、不标记已分解，客服可以用它预览报价；9) 批次件数超过 SetBatchLimit 设的上限返回 ErrBatchTooLarge，空批次返回 ErrEmptyBatch，超限批次不能先记账再报错；10) 产出列表按材料名升序返回，多件同材料累加，同一个材料不能出现两行；11) 历史按批次记录玩家、装备号和材料产出，序号单调递增，不同玩家的背包与历史互不干扰，客服按历史能查到每一次产出；12) 空玩家号返回 ErrBadPlayer，上限参数非正返回 ErrBadLimit，表定义重复档位、负档位、非正数量、空品质档返回 ErrBadTable，损坏快照返回 ErrBadSnapshot。

硬约束：同一玩家的 Salvage 会被分解台与批量脚本并发调用，单玩家批次必须串行，同一装备在并发下也只能被标记已分解一次；背包写入与历史追加要么一起成功要么一起回滚，读到的不能是半批材料。快照与写入互斥，恢复后已分解集合、背包与历史都要延续，重启不能把已分解的装备放回可分解状态。

验收：go test ./... 共 15 条必须全过；不得改测试、跳过或按固定输入写死；并发与重启自己验，批量回滚至少压一遍；只做本地实现，交付源码、跑测记录与产出表口径说明。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-24`
- 实现文件：`salvage_yield_ledger.go`
- 测试文件：`tests/test_salvage_yield_ledger_test.go`
- 包名：`task`；测试目录同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SalvageYieldLedger`
