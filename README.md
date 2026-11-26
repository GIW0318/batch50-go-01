# 自动增益控制：非对称包络、门限冻结与立体声联调

## 题面

采集链路换上新固件以后，客服记录了三段投诉录屏：主播刚开口的第一个字总是偏小，半秒之后才追上来；连着几声咳嗽以后，底噪被抬得比人声还明显；接上双通道声卡之后，左声道一直贴着限幅器发抖，右边却还很轻。运维把三段录屏对齐到同一处：自动增益的包络与门限。值班同学把音频导进示波器又看了一遍，咳嗽那段的包络在门限下方来回抖，开口那段的增益在门限边上反复抬落，双声道的两条曲线一条已经压平、一条还在往上走。落点是模块 gsb/batch50-go-game-c-03 的 auto_gain_controller.go，类型 AutoGainController，公开接口保持不变：NewAutoGainController(cfg AgcConfig) *AutoGainController、Process(frame []float32) (GainState, error)、ProcessStereo(left, right []float32) (GainState, error)、SetTargetDBFS(db float64) error、SetMaxGain(db float64) error、SetGateThreshold(db float64) error、GateState() GateState、GainDB() float64、Frames() int64、Validate() error、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收清的地方共 11 条：其一，帧电平取峰值绝对值折算的 dBFS，全零帧按 -120 dBFS；其二，目标增益等于目标电平减帧电平，限制在 MinGainDB 到 MaxGainDB 之间；其三，增益向目标推进用一阶平滑，下降用 AttackMS、上升用 ReleaseMS，帧时长按帧长除以采样率折算；其四，增益下降必须明显快于上升，同一量级误差下单帧下降量至少是上升量的三倍；其五，帧电平低于门限时冻结增益、只累加门限帧计数，不推进包络；其六，门限帧计数在第一个非门限帧清零；其七，输出电平等于帧电平加增益；其八，预测输出超过 LimiterDBFS 时先把增益压到上限减帧电平并标记为限幅，输出不得高于上限；其九，SetMaxGain 收紧时当前增益立刻被钳到新上限，不用等下一帧，各设置接口收到非数值一律报错且不改动现值；其十，配置校验覆盖采样率、目标电平、增益窗口、门限、两个时间常数与限幅上限，配置非法时 Process 报配置错误；其十一，空帧报空帧错误，立体声左右长度不一致报长度不一致错误，被拒的帧不推进帧计数。

另有几条必须一起收掉：门限以下冻结期间的电平变化不能攒成欠账，开口瞬间不能一次性补上；目标电平高于 0 dBFS 时钳到安全值并报配置错误，不能真把输出推到削波，削波一次整段素材就废了；左右能量差很大时联调要按能量较高的声道算，避免一个声道长期贴限幅。

硬约束：Process、ProcessStereo 与 GateState、GainDB、Frames 的并发调用不得看到半更新的增益，帧计数与包络在同一次提交里推进，读侧只会看到上一次提交完成的整组值；快照与恢复要么全量替换要么完全不动，损坏载荷报快照无效并保留现网增益与计数；GainState 的 ChannelGains 按声道给出实际施加的增益，联调时两个值必须相同且与 GainDB 一致。

验收：go test ./... 15 条全过，测试不得修改、跳过或按固定输入写死；并发与重启恢复自行验证；只做本地实现，不接外部依赖。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-c-03`
- 实现文件：`auto_gain_controller.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`AutoGainController`、`AgcConfig`、`GainState`、`GateState`
