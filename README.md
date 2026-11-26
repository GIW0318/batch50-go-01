# 无缝播放的编码延迟裁剪

## 题面

母带组交付的有声书连续三集在章节接缝处冒出一小段重复的音节，片头还夹着一声被切断的爆音；整轨连播版本报给客户的总时长比曲目表多出四十多毫秒，进度条拖到结尾会停在一个取不到样本的位置。请落到模块 gsb/batch50-go-game-c-06 的 gapless_trim_planner.go，类型 GaplessTrimPlanner，公开接口按现状保留：NewGaplessTrimPlanner() *GaplessTrimPlanner、Open(track string, info EncoderInfo) (TrackView, error)、Trim(track string, from, to time.Duration) (Clip, error)、Join(clips []Clip) (Clip, error)、SampleAt(track string, pos time.Duration) (int64, error)、Duration(track string) (time.Duration, error)、SetPaddingPolicy(p Policy) error、Close(track string) error、View(track string) (TrackView, bool)、Clips() []Clip、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的口径如下。第一，轨道名为空白报 ErrBlankTrack；采样率、帧数、帧长非正，priming 或 padding 为负，或两者之和吃掉全部编码样本，报 ErrBadEncoder，且不登记轨道。第二，Encoded 等于帧数乘帧长，Playable 等于 Encoded 减 priming 再减 padding，两者都按样本计；Duration 按采样率向下取整换算；同一轨道重新 Open 用新编码信息覆盖并作废旧区间。第三，Trim 的 from 不得为负、to 必须大于 from 且不得超过轨道时长，越界报 ErrBadRange，轨道不存在报 ErrUnknownTrack，被拒区间不得进入 Clips。第四，每段裁剪的起点样本号按 priming 加上 from 对应的样本数，段内样本数按 to 减 from 的样本数，Duration 是这段可播样本的时长，padding 不得混进段内计数。第五，Trim 还要扣掉策略里额外丢弃的头尾样本，扣完不足 MinSamples 报 ErrShortClip，缺省下限按一个样本。第六，SetPaddingPolicy 的 HeadFade、TailFade、MinSamples 任一为负报 ErrBadPolicy 且旧策略不变，Policy 返回生效值。第七，SampleAt 返回真实解码流里的样本号，位置零落在 priming 之后第一个可播样本，同一毫秒内不得跨样本，相邻样本边界严格加一；位置为负或达到轨道时长报 ErrBadRange，未知轨道报 ErrUnknownTrack。第八，Join 空列表报 ErrNoClips，采样率不一致报 ErrRateMismatch 且不改变已有裁剪记录。第九，拼接按传入顺序累计，前段末尾与后段开头共用的边界样本只能计入一次，既不重复也不丢样。第十，同一轨道按多个区间反复裁剪时，每一段都要各自带上 priming 偏移，不能只有第一段扣。第十一，Close 后轨道与它的裁剪记录一起消失，未登记轨道 Close 报 ErrUnknownTrack，关闭后再操作也报 ErrUnknownTrack。第十二，快照覆盖轨道表、全部裁剪与当前策略，恢复后 Duration、SampleAt、Clips 与快照前一致，继续 Trim 不受影响；损坏载荷报 ErrBadSnapshot 且现网状态不动。

另外，任何一次失败的 Open、Trim 或 SetPaddingPolicy 都不得留下半条记录，重试同一请求要得到与首次成功时相同的记账结果；Join 只产出队列段，不得改动已经登记的单段裁剪。

硬约束两类。一是并发：多个线程同时 Open、Trim 与 Snapshot 必须串行裁决，Clips 不得出现重复区间，读到的轨道视图不能是半更新的。二是重启恢复：轨道、裁剪记录与策略要能跨进程恢复，损坏载荷整份拒绝且不影响现网。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-c-06`
- 实现文件：`gapless_trim_planner.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`GaplessTrimPlanner`
