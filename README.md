# 采样循环点规划

## 题面

音源库的弦乐长音换到循环模式后，尾音接缝处一直有一声轻微的咔哒，短的木管采样循环起来像被塞进一个盒子里；工程师手工把循环点挪到过零点，循环反而变短到几乎听不出音高；更奇怪的是重新跑一次分析之后，之前选好的循环点自己没了，已经导出的音色和工程里听到的不一样。请落到模块 gsb/batch50-go-game-c-10 的 sample_loop_planner.go，类型 LoopPointPlanner，公开接口按现状保留：NewLoopPointPlanner(opts Options) *LoopPointPlanner、Analyze(sample string, pcm []float32) ([]Candidate, error)、Select(sample string, at int) (LoopPoint, error)、SetCrossfade(sample string, d time.Duration) error、Validate(loop LoopPoint) error、Apply(loop LoopPoint, pcm []float32) ([]float32, error)、Discard(sample string) error、Candidates(sample string) []Candidate、Loop(sample string) (LoopPoint, bool)、Samples() []string、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的口径如下。第一，样本键空白报 ErrBadSample；PCM 长度小于两倍分析窗加一报 ErrShortPCM；分析窗缺省 32，最小循环长度缺省 64，候选上限缺省 256。第二，得分等于 1 减去尾部窗与头部窗逐点差的绝对值均值，保留三位小数；均值大于 0.25 的索引不算候选。第三，连续且得分相同的候选合并，只保留索引最小的一条；候选按得分降序、索引升序排列，超过上限按该顺序截断，Candidates 返回同一次分析的结果。第四，Select 取离 at 最近的候选（距离相同取索引小者），再吸附到最近过零点（距离相同取小索引）；过零点指该点样本为零或与前一点符号相反；没有候选报 ErrNoCandidate，样本未分析报 ErrUnknownSample；循环点终点取 PCM 末尾，长度等于终点减起点，低于最小循环长度报 ErrShortLoop 且不记录选中。第五，SetCrossfade 按采样率把时长折算成样本数，负值或超过最大交叉淡化样本数（缺省 4096）报 ErrBadCrossfade 且旧值保留，未分析样本报 ErrUnknownSample。第六，Validate 拒绝起点为负、终点不大于起点、长度字段与终点减起点不一致、交叉淡化大于长度的循环点，分别报 ErrBadLoop；长度低于下限报 ErrShortLoop。第七，Apply 先走 Validate，再检查终点不超过 PCM 长度；正常窗口输出长度等于循环长度，前 X 个样本按第 i 个取 (i+1)/(X+1) 的权重把头样本与尾部样本混合，其余样本原样输出。第八，交叉淡化窗覆盖整个循环长度时循环退化为单点播放并标记降级，不能产出零长度循环。第九，选中点落在两个过零点之间的平台区时取最近过零点，但不能为过零把循环长度推到下限之外。第十，重新分析同一样本只刷新候选，已选中的循环点要保留到显式重选。第十一，同一样本的多个层用同一前缀加 #序号 的键分别登记，任一层重选、改交叉淡化或丢弃都不得改动其它层。第十二，快照带候选、选中点、交叉淡化与 PCM，恢复后 Candidates、Loop 与 Apply 结果一致；损坏载荷报 ErrBadSnapshot 且不动现网。

硬约束两类。一是并发：多个线程同时分析、选点与写交叉淡化必须串行裁决，候选列表不能被读到半更新状态，同一层的候选与选中点必须成对一致。二是重启恢复：候选、选中点、交叉淡化与 PCM 跨进程恢复，损坏载荷整份拒绝；候选数量与分析窗长度受配置上限约束。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-c-10`
- 实现文件：`sample_loop_planner.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`LoopPointPlanner`
