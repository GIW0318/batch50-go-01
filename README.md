# 噪声底跟踪：频带最小值统计、谐波冻结与增益下限抬升

## 题面

客服这周收了三条录音投诉：会议室空调停掉以后，一句刚开口的话被吞掉半截，要等两个字之后声音才回来；放背景音乐的那一路，人声没被压下去，长笛声反倒越来越闷，十分钟后整段像隔着棉被；调音师把降噪的最低抑制从 -12 dB 提到 -6 dB，已经压到底的频带过了三四秒才慢慢松开，监听里像有人在抽气。这三件事都落在频带噪声底的估计与抑制增益上。落点是模块 gsb/batch50-go-game-c-02 的 noise_floor_tracker.go，类型 NoiseFloorTracker，公开接口保持不变：NewNoiseFloorTracker(cfg NoiseConfig) *NoiseFloorTracker、PushFrame(frame []float32, at time.Time) (FrameDecision, error)、NoiseFloor(band int) (float64, error)、Gain(band int) (float64, error)、SpeechProbability(band int) (float64, error)、SetGainFloor(db float64) error、Reset(reason string) error、Validate() error、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收清的地方共 11 条：其一，一帧按 Bands 等分成若干频带，频带电平取该段样本均方值的十倍对数（dB），均方不足 1e-20 按 1e-20 计；其二，帧长度必须等于 FrameSize 且能被 Bands 整除，否则报帧长度错误，帧计数不推进；其三，噪声底以最近 Window 帧电平的最小值加 BiasDB 为目标，每帧向目标靠拢，上升受 CreepDBPerSec 限制、下降受 DecayDBPerSec 限制，单帧变化量不得超过速率乘帧时长；其四，新建的跟踪器每条频带从 InitialFloorDB 起步、增益为 0，第一帧不得把底直接抬到该帧电平；其五，语音概率按（电平减底再减 SpeechMarginDB）除以 6 截断到 0 至 1，任一频带概率超过 0.5 时整帧判为语音；其六，抑制增益目标为电平减（底加 SuppressMarginDB），上限 0 dB、下限为当前增益下限，下降用 DelayMS 一阶平滑、上升用 ReleaseMS 一阶平滑，起落不对称；其七，增益任何时候都不得低于当前下限；其八，SetGainFloor 只接受 -60 至 0 dB，越界与 NaN 报错且不改动现值，GainFloor 回读；其九，Reset 必须给非空原因，否则报错且状态不动，成功后帧计数归零、各频带回到初始底、增益归 0；其十，频带下标越界（含负数）报下标错误，NoiseFloor、Gain、SpeechProbability 一致；其十一，配置校验覆盖 Bands 为正、FrameSize 能被 Bands 整除、SampleRate 在 8000 至 192000、Window 在 1 至 1000、DelayMS 与 ReleaseMS 为正、GainFloorDB 在 -60 至 0、BiasDB 在 0 至 20、CreepDBPerSec 与 DecayDBPerSec 在 0 至 60、InitialFloorDB 在 -120 至 -10，配置非法时 PushFrame 报配置错误。

另有几条必须一起收掉：持续音乐段不能被当成噪声底吸收，频带峰值因数（峰值比均方根）低于 6 dB 且电平高出噪声底 3 dB 时判为有谐波结构，本帧暂停噪声底下降；增益下限提高后，已经压到旧下限的频带要按新下限立即恢复，不能等下一帧；长时间静音后突然出现语音时，噪声底不跟随抬升，靠最小值窗口与上升限速保证首字不被吞。

硬约束：PushFrame 与 NoiseFloor、Gain、SpeechProbability 的并发调用不得看到半更新状态，帧计数、窗口与各频带状态在同一次提交里推进；快照与恢复要么全量替换要么完全不动，损坏或频带布局不一致的载荷报快照无效并保留现网状态；时间戳为零或没有前进时按 FrameSize 除以 SampleRate 的标称帧时长推进。

验收：go test ./... 15 条全过，测试不得修改、跳过或按固定输入写死；并发与重启恢复自行验证；只做本地实现，不接外部依赖。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-c-02`
- 实现文件：`noise_floor_tracker.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`NoiseFloorTracker`、`NoiseConfig`、`FrameDecision`
