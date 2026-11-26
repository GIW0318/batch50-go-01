# 音频重采样批处理：采样率转换、抖动与削波保护

## 题面

上线前的语音包批量转码这周连续返工：同一段双声道素材转成目标采样率之后，左右声道出现了几毫秒的错位，玩家听感上人声偏到了右边；另一批高响度素材被削波保护压住之后，整段又被整体拉低了一次，响度表直接对不上验收口径；还有运维在队列里查不到任务跑到哪一步，重跑时前面已经完成的又被重新处理一遍。落点在模块 gsb/batch50-go-game-c-07 的 audio_resample_batch.go，类型 AudioResampleBatch，公开接口保持不变：NewAudioResampleBatch(cfg BatchConfig) *AudioResampleBatch、Submit(id string, pcm []int16, channels, rateIn, rateOut int) (JobSpec, error)、Plan(id string) (ResamplePlan, error)、Run(id string) (ResampleResult, error)、Result(id string) (ResampleResult, bool)、Cancel(id string) error、Queue() []string、Jobs() []JobSpec、Pending() int、Snapshot() ([]byte, error)、Restore(data []byte) error。

转码口径：任务号空白报 ErrBlankJob，重复报 ErrJobExists；声道数不在 1 到 MaxChannels 之间报 ErrBadChannels；输入或输出采样率不在受支持集合内报 ErrUnsupportedRate，两者相同报 ErrSameRate；样本数不能整除声道数报 ErrBadFrames；帧数超过 MaxInFrames 报 ErrFrameLimit；排队任务达到 MaxJobs 再提交报 ErrQueueFull。任务按提交顺序排队，Run 只能处理队首任务，跳队报 ErrJobOrder；已完成的任务再跑报 ErrJobDone；已取消的任务跑报 ErrJobCancelled；未知任务报 ErrUnknownJob。待处理任务可以取消，取消后从队列移除且重复取消幂等成功，已完成的任务取消报 ErrJobDone。

输出帧数为输入帧数乘输出采样率除以输入采样率后向上取整，Plan 要给出输入帧数、输出帧数、两个采样率与抖动预算。第 j 个输出帧先求整数位置：pos 取 j 乘输入采样率，i 取 pos 除以输出采样率的整数商，rem 取其余数；输出值按 a 乘输出采样率减 rem 之差、加上 b 乘 rem，再除以输出采样率后四舍五入，舍入按远离零处理，其中 a 是第 i 帧样本，b 是第 i 加 1 帧样本，越过末帧时 b 取 a。抖动按输出帧逐个分配，与插值是否精确无关，每帧最多一个单位，预算用完即停，DitherUsed 要如实回填。削波保护把超过正负 ClipCeiling 的样本压到边界，整帧里任意声道越界只计一次 ClippedFrames。

另外两处必须一起收掉。多声道素材的每一帧只能有一个采样位置和一次取整，所有声道共用同一个抖动偏移，不能每个声道各算各的，否则间隔会漂。削波保护触发之后只允许逐样本截断，不允许把整段再乘一个系数把峰值拉回上限，整体响度必须保持原来的相对关系。

硬约束两条。一是并发：提交、取消与处理必须串行结算，同一个任务号并发提交只能成功一次，其他请求报 ErrJobExists，队列里不能出现同一个任务两次。二是重启：快照要带上排队任务、已完成结果、队列顺序与序号，恢复后 Queue、Jobs、Pending 与结果必须与快照前一致；损坏的载荷报 ErrBadSnapshot 且不能动到现网任务。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-c-07`
- 实现文件：`audio_resample_batch.go`
- 测试目录：`tests/`，通过导入路径引用本模块
- 包名：`task`；主要类型：`AudioResampleBatch`、`BatchConfig`、`JobSpec`、`ResamplePlan`、`ResampleResult`
