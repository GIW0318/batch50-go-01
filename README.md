# 路径追踪采样序列：低差异序列逐像素扰动、重复样本幂等与累积整体重置

## 题面

美术在材质预览里连着报了三件事。第一件：关掉降噪只看原始累积，画面上一层很规整的网格状麻点，横平竖直，像采样点都落在同一批位置上；第二件：镜头停在原地不动，本该越来越干净，可只要把预算调紧一点，整张图就永远停在一遍采样的脏度；第三件：镜头从门口推到窗边之后，窗框附近糊着一圈上一机位的残影，要等好几秒才散，而画面中心早就干净了。落点是模块 gsb/batch50-go-game-b-09 的 path_sample_sequencer.go，类型 PathSampleSequencer，公开接口保持：NewPathSampleSequencer(cfg SampleConfig) *PathSampleSequencer、Config() SampleConfig、BeginFrame(frame int) error、Frame() int、SamplesFor(pixel, count int) ([]Sample, error)、NextSample(pixel int) (Sample, error)、Accumulate(pixel int, sample Sample, value Color) error、Reset(reason string) error、Convergence(pixel int) (ConvergenceState, error)、SampleCount(pixel int) int、BudgetRemaining() int、SetPriority(pixel, priority int) error、Stats() SequencerStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

要改的地方如下。第一，配置里宽、高、帧预算、每像素上限、收敛目标留 0 或负数时按 64、64、1024、512、128 生效，Config 返回生效值，Stats 的 Pixels 等于宽乘高。第二，BeginFrame 的帧号必须严格递增，重复或回退报 ErrBadFrame 且帧号不变，每帧可用预算按 FrameSamples 重置。第三，未开帧就取样、取单样本或累积，一律报 ErrNoFrame。第四，像素越界报 ErrBadPixel，数量非正报 ErrBadCount，被拒的调用不得消耗预算。第五，SamplesFor 按像素各自发放索引，从 1 起连续，下一次调用接着上次的水位往下发，NextSample 与 SamplesFor 共用同一水位。第六，样本坐标必须落在 [0,1) 且不含 NaN；相同配置的两个实例对同一像素同一索引给出完全相同的坐标；不同像素的偏移必须不同。第七，一帧内发放的样本总数不得超过 FrameSamples，请求量超过剩余预算时整批拒绝并报 ErrBudgetExhausted，BudgetRemaining 与 Stats 如实反映余量，不得部分发放。第八，累积要求样本属于该像素且索引为正，否则报 ErrBadSample；颜色任一分量是 NaN、Inf 或负值报 ErrBadColor；被拒的累积不改动样本数与均值。第九，均值按已累积样本数增量归一，SampleCount 与 Convergence 的 Samples 一致，达到 ConvergeSamples 后 Converged 为 true，从未累积的像素返回零值且不报错。第十，每像素累积数达到 MaxSamplesPerPixel 后继续累加报 ErrSampleCap。第十一，Reset 原因为空白报 ErrBadReason 且不清空累积。第十二，快照要带帧号、已发放索引水位、本帧已用预算与累积缓冲；恢复后 BudgetRemaining、SampleCount、均值与帧号必须对得上，继续 BeginFrame 的帧号必须大于快照帧号，损坏载荷报 ErrBadSnapshot 且不动现网。

另外三处必须一起收掉。相机一动，历史累积要整体作废，不能只把画面中心那块重置掉，边缘与角落也得跟着清，否则推镜之后会留下上一机位的鬼影。同一个像素用同一个样本索引重复提交时，累积必须幂等，不能因为重试或补帧把同一个样本加两遍，否则收敛曲线会整体偏掉。每帧预算不够分给所有脏像素时，要先给已经积累过样本、优先级高的像素续采，让它们先收敛，不能每帧把所有像素压回一个样本，那样画面永远脏。

硬约束两类。一是并发：多个工作线程同时取样与累积必须串行裁决，预算不能超发，样本计数与均值不能丢更新，读到的状态不能是半更新的中间态。二是重启恢复：帧号、索引水位与累积缓冲要能跨进程恢复，恢复后预算与均值必须对得上，损坏载荷整份拒绝且不影响现网。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-09`
- 实现文件：`path_sample_sequencer.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`PathSampleSequencer`
