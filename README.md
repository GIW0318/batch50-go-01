# 过场动画多轨时间轴与跳过补发

## 题面

主线第三章的过场从上周开始连续出问题。玩家按跳过之后，本该在过场里触发的开门事件没有发生，任务链停在「等待门开启」，客服只能手工补进度；组队一起看片时，队里一个人按跳过，另外两个人的镜头和字幕也跟着跳到结尾；更麻烦的是过场播到一半被拉进战场，屏幕上下还挂着过场的黑边，角色动不了，只能重登。问题落在模块 gsb/batch50-go-game-a-11 的 cutscene_sequence_director.go，类型 CutsceneSequenceDirector，公开接口按现状保留：NewCutsceneSequenceDirector(limits Limits) *CutsceneSequenceDirector、Load(sequenceID string, tracks []Track) error、Play(sequenceID, viewerID string, at time.Time) (Playback, error)、Tick(now time.Time) ([]Cue, error)、Skip(viewerID string) (Playback, error)、Pause(viewerID string) error、Resume(viewerID string) error、Abort(sequenceID, reason string) error、Position(viewerID string) (Playback, bool)、Observers(sequenceID string) []string、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的口径如下。第一，序列号只有空白、一条轨道都没有、轨道种类不在 camera、dialogue、audio、event 四种之内、轨道号重复，都在装载阶段拒掉并分别报 ErrBlankSequenceID、ErrNoTracks、ErrBadTrackKind、ErrDuplicateTrack，被拒的序列不能留在表里。第二，cue 的时间超出所属轨道时长、为负数、或指向不存在的轨道，报 ErrBadCue，同一序列整条不装载。第三，轨道条数超过 MaxTracks 报 ErrTooManyTracks，cue 总数超过 MaxCues 报 ErrTooManyCues，之后按未知序列处理，不能只装载一半。第四，同一时刻到期的多条 cue 先按时间升序，时间相同按优先级从高到低，再按轨道号与 cue 号兜底；派发出去的 cue 要带上收件玩家。第五，同一条 cue 对同一名玩家只派发一次，越过最后一条 cue 后时钟状态转 finished。第六，Tick 按两次调用之间的真实毫秒差推进时钟，暂停期间不推进，恢复后从恢复时刻重新计时。第七，播放中的过场占用输入锁并记下接管前的镜头锚点，跳过、结束或中止时交还。第八，跳过把该玩家的时钟直接推到序列总时长并置 finished，期间不派发非必须 cue；标记为必须的事件仍要按时间顺序补发，不能整段丢弃导致任务链卡死。第九，同一段过场最多 MaxViewersPerShow 名玩家同时观看，超出报 ErrViewerLimit；不在观看名单里的玩家调 Skip 报 ErrViewerNotWatching。第十，多名玩家同看时其中一人跳过，不影响其他人的时钟与音画，每名玩家各自持有独立时钟与派发水位。第十一，过场中途被强制拉入战斗要先中止并回滚镜头与输入锁，Abort 必须带原因，空白原因报 ErrBadAbortReason，中止后该序列不再派发任何 cue，也不能带着黑边进战斗。第十二，快照要能描述装载的序列、每名玩家的时钟位置与已派发水位，恢复后接着往下走、已派发的 cue 不重发；损坏的快照报 ErrBadSnapshot 且原状态不被改动。

硬约束有三条。同一名玩家的 Tick、Skip、Pause 会从多个协程同时到达，同一条 cue 只能派发一次，派发水位与观众名单要么全见旧值要么全见新值，不得出现半更新。进程重启后要能从快照恢复上报的进度，装载的序列、时钟位置、派发水位一项都不能丢，已派发的 cue 不得重发。轨道数、cue 总数、同屏观众数都是硬顶，超限报错而不是截断、覆盖或静默淘汰。

验收口径：go test ./... 的 15 条用例必须全过，测试不得修改、跳过、按固定输入写死；并发派发与重启恢复自行验证；只做本地实现，交付改动后的源码与跑测记录。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-11`
- 实现文件：`cutscene_sequence_director.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`CutsceneSequenceDirector`
