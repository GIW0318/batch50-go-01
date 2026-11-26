# 农田生长推进、浇水加速与离线补算

## 题面

值班这周连着接了三张农田的单子：有玩家下线一天再上线，三块地里的作物齐刷刷卡在第二阶段不动；有玩家把水壶点满，界面提示浇水成功，作物进度一点没变；还有一片地整晚没人管，第二天来收，产量按满额发了，枯萎像是根本没算进去。落点是模块 gsb/batch50-go-game-a-18 的 crop_growth_ledger.go，类型 CropGrowthLedger，公开接口保持：NewCropGrowthLedger(limits Limits, now time.Time) *CropGrowthLedger、RegisterSeed(spec SeedSpec) error、Plant(plot string, seed Seed) (CropID, error)、Water(plot string) error、Harvest(plot string) (Yield, error)、Stage(plot string) (StageInfo, error)、Uproot(plot string) error、Advance(now time.Time) ([]PlotEvent, error)、RefillWater(units int) error、WaterLeft() int、PlotCount() int、Events() []PlotEvent、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收掉的行为如下。一，种子登记：ID 为空、Stages 小于 1、StageMillis 非正、MaxWaters 或 DryLimit 为负、WiltPercent 越出 0 到 100 报 ErrBadSeed；StageMillis 超过 MaxStageMillis 报 ErrStageTooLong；重复 ID 报 ErrSeedExists。二，播种：地块号为空报 ErrBlankPlot，未登记种子报 ErrUnknownSeed，地块已有作物报 ErrPlotOccupied，地块数到达 MaxPlots 报 ErrPlotLimit；新作物从第 0 阶段起步，进度与水分次数都清零。三，生长只看真实时间：Advance 按与上一次推进的时间差累加，每个阶段满 StageMillis 进一档，跨档的余量带到下一阶段，不许多算也不许丢；最后一档走满进成熟态并停住，再推进不改阶段与进度。四，浇水只加速当前阶段：Water 按 WaterBonusMillis 加到当前阶段进度，且不超过本阶段长度；每次浇水用掉一份水源，本阶段的水分次数在进入下一阶段时清零。五，水源是共享池：RefillWater 只接受正数，否则报 ErrBadWater 且不改池子；池子空了 Water 报 ErrNoWater，作物进度一动不动。六，成熟或枯萎之后才能收获：收获清空地块并返回 Yield，正常产量取 BaseYield，枯萎后按 WiltPercent 折算；空地块 Harvest 报 ErrNoCrop，还在长的报 ErrNotReady，重复收获报 ErrNoCrop。七，Uproot 直接铲掉作物并清空地块，空地块报 ErrNoCrop，事件记 uprooted。八，阶段投影要带地块、作物号、种子、当前阶段、总阶段、本阶段进度与时长、已用与剩余水分次数、连续缺水档数、成熟与枯萎标记。九，Stage 与 PlotCount 只读不改，PlotCount 只数当前有作物的地块。十，事件按 planted、watered、stage_advanced、matured、wilted、harvested、uprooted 分类，带地块、作物号、阶段、毫秒时间与递增序号，总量受 MaxEvents 硬顶，超出淘汰最旧。十一，Advance 时间倒退报 ErrTimeWentBackwards 且整份不动，时钟只许单调前进。十二，快照要带种子表、各地块作物、阶段进度、缺水档数、水源余量与事件序号，损坏载荷 Restore 报 ErrBadSnapshot 且不动现网，恢复后作物号必须继续递增。

还有三处线上最容易漏。一是离线补算一次跨过多个阶段时，每个阶段都要各自判定是否枯萎，不能只看补算完的最终状态；二是浇水次数用尽后的浇水请求要当场拒绝且不消耗水源，也不能把这笔次数记账到下一阶段；三是收获与枯萎落在同一个补算窗口时按枯萎后的产量结算，不能沿用窗口开始时的快照。

硬约束两类。一是并发与原子性：同一地块的浇水、收获与推进必须串行裁决，读阶段的一方看到的进度、水分次数与枯萎标记属于同一时刻，不能读到浇了一半的中间态。二是重启恢复：阶段进度、连续缺水档数、水源余量与作物号要跨进程恢复，恢复后以快照里的时间为补算基准继续推进，时间倒退按报错处理。验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-18`
- 实现文件：`crop_growth_ledger.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`CropGrowthLedger`
