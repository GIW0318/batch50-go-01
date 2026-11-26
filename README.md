# 离线导出渲染

## 题面

发行前的批量导出窗口一开就出岔子：同一首工程连导两遍，波形首尾各差几十个采样，做盲听对比时能听出梳状染色；带侧链压缩的工程导出后整体比实时预览晚了一截，人工对齐也找不回来；更麻烦的是渲染机重启后，导到一半的任务从零开始，前面十几分钟白跑。请落到模块 gsb/batch50-go-game-c-09 的 offline_bounce_renderer.go，类型 OfflineBounceRenderer，公开接口按现状保留：NewOfflineBounceRenderer(opts Options) *OfflineBounceRenderer、Start(job BounceJob) (JobID, error)、Advance(job JobID, blocks int) (Progress, error)、Cancel(job JobID) error、Status(job JobID) (Progress, error)、SetTail(d time.Duration) error、SetLatencyComp(ms float64) error、Jobs() []JobID、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的口径如下。第一，工程名空白、采样率或块长非正、内容块数非正、轨道余响表为空或含负数，一律报 ErrBadJob 且不建任务。第二，同一工程已有未完成任务时重复提交报 ErrJobActive；任务数达到上限（缺省 4）报 ErrJobLimit。第三，任务号从 1 起单调递增，Jobs 按任务号升序；Progress 的 Content 是内容块数，Tail 是尾音块数，Done 是已完成块数，Samples 等于 Done 乘块长；尾音块数取尾音时长折算块数与最长余响两者的较小值，尾音为零时为 0。第四，Advance 每次最多推进 MaxBlocksPerCall（缺省 64）块，超过、为零或为负报 ErrBadBlocks 且进度不变；推进量超过剩余块数按剩余量截断，跑完置 done 并把 Silent 置真。第五，SetTail 为负或超过尾音上限（缺省 10 秒）报 ErrBadTail 且保留旧值，尾音只对之后新建的任务生效。第六，SetLatencyComp 为负或超过 500 毫秒报 ErrBadLatency 且保留旧值。第七，Status 对未知任务号报 ErrUnknownJob，任务结束后仍可查询；Cancel 对运行中的任务置 cancelled 并保留已完成块数，已结束或已取消的任务再 Cancel 报 ErrJobDone，取消后 Advance 同样报 ErrJobDone。第八，同一工程同样参数两次导出必须得到逐样本一致的结果，Checksum 在分批推进与一次推完之间必须相同。第九，尾音阶段只推进仍有余响的轨道，已经静音的轨道不能继续占用块预算。第十，两次导出要在补偿延迟之后逐样本对齐比较，补偿量不能计入结果时长。第十一，取消后重新提交同一工程可以复用已完成块，但参数一旦变更旧块必须作废。第十二，快照带任务进度、尾音与补偿设置、任务号水位；恢复后 Status 与快照一致，继续推进结果与未中断时一致；损坏载荷报 ErrBadSnapshot 且不动现网。

另外，任何一次失败的 Start、Advance 或 SetTail 都不得改动任务表与既有进度，重试必须得到与首次成功相同的记账结果。

硬约束两类。一是并发与配额：多个线程同时提交任务必须串行裁决，任务上限不得被突破，同一工程只能有一个活动任务。二是确定性重启恢复：同一输入逐样本一致，进度与设置跨进程恢复，损坏载荷整份拒绝。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-c-09`
- 实现文件：`offline_bounce_renderer.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`OfflineBounceRenderer`
