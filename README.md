# 场景切换检测：直方图与边缘联合打分、双阈值滞回与切点合并

## 题面

点播转码这周出了三张事故单：一部四十五分钟的纪录片被切成三千八百段，一集电视剧的片头渐显被拆成二十几个片段，还有一部片子第 43 分钟之后所有切点整体错位半秒。运维从转码日志里能看到的只有每帧一个分数，切点是怎么从分数里挑出来的没人说得清，值班只能按帧反推。落点是模块 gsb/batch50-go-game-c-04 的 scene_cut_detector.go，类型 SceneCutDetector，公开接口保持不变：NewSceneCutDetector(cfg CutConfig) *SceneCutDetector、PushFrame(frame FrameStats, pts int64) (FrameVerdict, error)、SetThresholds(high, low float64) error、Thresholds() (float64, float64)、SetMinInterval(ms int) error、MinInterval() int、Flush() ([]CutPoint, error)、Reset() error、Cuts() []CutPoint、Score(index int) (float64, error)、FrameCount() int、Validate() error、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收清的地方共 11 条：其一，亮度直方图必须正好 HistBins 个桶、每桶非负且不含非数值、总和落在 0.99 到 1.01，否则报直方图错误，边缘密度必须落在 0 到 1 之间且不含非数值，否则报边缘错误，被拒的帧不推进帧计数；其二，直方图距离取两帧各桶绝对差之和的一半，边缘变化取两帧边缘密度差的绝对值除以两者较大值（下限 1e-9）；其三，综合得分等于直方图权重乘直方图距离加边缘权重乘边缘变化，权重之和在 1e-6 内必须等于 1，否则报权重错误；其四，首帧得分为 0，永远不是切点；其五，得分达到高阈值且与上一个已接受切点的 PTS 差（毫秒）不小于最短间隔时，本帧判为切点；其六，得分低于低阈值的帧直接忽略；其七，已接受的切点按帧序进入 Cuts，带帧号、PTS 与得分；其八，阈值必须满足低阈值为正、低阈值小于高阈值、高阈值不超过 1，非法报阈值错误且不改动现值，最短间隔不得为负；其九，Flush 返回同一份切点列表且可重复调用，之后仍可继续推帧；其十，打分历史只保留最近 MaxHistory 帧，被淘汰或尚未出现的帧号查分报帧号越界；其十一，配置校验覆盖直方图桶数至少 2、权重之和为 1、标称帧时长为正、历史上限为正。

另有几条必须一起收掉：淡入淡出期间得分持续中等，不能每帧报切点，要合并成转场结束处的一个切点；两个真实切点间隔小于最短间隔时保留得分高的那个、丢掉低的，丢掉的那个不能影响后续检测窗口；PTS 回绕或乱序时按解码序推进计数，不能把回绕当成负时长或重复时间戳，时间戳不可信时按标称帧时长推进，不按相邻时间戳差值反推。

硬约束：PushFrame 与 Cuts、Score、FrameCount 的并发调用不得看到半更新的分数窗口与切点表，帧计数与候选状态在同一次提交里推进；快照与恢复要么全量替换要么完全不动，损坏载荷报快照无效并保留现网状态。

验收：go test ./... 15 条全过，测试不得修改、跳过或按固定输入写死；并发与重启恢复自行验证；只做本地实现，不接外部依赖。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-c-04`
- 实现文件：`scene_cut_detector.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SceneCutDetector`、`CutConfig`、`FrameStats`、`FrameVerdict`、`CutPoint`
