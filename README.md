# 音频输出设备热切换

## 题面

投诉单 2026-1112-07：赛事直播客户端在玩家插拔耳机时，十几秒内画面上的音效时间轴和剩余时长对不上，暂停再继续会跳回插耳机前的位置；换到蓝牙音箱后同一段音频又比原来快了半拍，赛后回放的解说和画面完全错位。落点在 gsb/batch50-go-game-c-11 的 audio_device_switcher.go，类型名 AudioDeviceSwitcher，包名 task。

设备表按注册顺序保存，Devices() 必须原样返回全部条目及其可用状态，不得排序、去重或过滤。构造签名 NewAudioDeviceSwitcher(devices []Device, limits Limits) *AudioDeviceSwitcher；Limits 的 ProjectRate 为零时按 48000 处理，MaxDevices 为零时按 8，MaxSwitches 为零时按 32，Fade 为零表示不做淡入淡出。

需要落地的公开接口：SetDevice(id string) error、Switch(id string) (Plan, error)、Commit(plan Plan) error、Pending() (Plan, bool)、SetFade(d time.Duration) error、Advance(samples int64) error、Clock() DeviceClock、DropDevice(id string) error、Stats() SwitchStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的现象：

一、空闲时直接选设备要成功，选中后 Clock 的 Device 与 Rate 立刻反映该设备，切换计数仍为零。

二、已经开始出声或已有计划在途时直接选设备必须拒绝并报 ErrBusy，当前设备与播放位置都不变。

三、设备号不存在报 ErrUnknownDevice，设备不可用报 ErrDeviceUnavailable，切到当前设备报 ErrSameDevice，这三种拒绝都不能留下待提交的计划。

四、目标设备支持工程采样率时按工程采样率打开，只有不支持时才退到设备首选采样率，计划里的 Resample 要如实标注，并且不得因为这个变化改动播放速度。

五、计划的 AtSample 取发起切换时的已播样本数，FromDevice 与 FromRate 取当前设备，Index 从 1 起单调递增，Fade 取当次设置。

六、已提交的切换数达到 MaxSwitches 后继续切换报 ErrSwitchLimit；没有待提交计划时 Commit 报 ErrNoPlan，序号对不上的计划报 ErrBadPlan，重复提交同一计划也要报 ErrNoPlan。

七、切换先在旧设备上淡出，再在新设备上按新采样率重建并淡入；时钟只在提交完成后指向新设备，淡出余量与淡入进度都要能在 Clock 上读出来。

八、播放位置按已播样本数推进，双设备并行的时段也只能推进一次，不能因为两端同时出声把时间线翻倍。

九、Advance 的样本数必须为正，零与负数报 ErrBadAdvance 且位置不动；采样率换算用整数比例，播放位置与已播样本数要能各自对得上。

十、设备掉线时播放位置停在已播样本处，不允许因为设备消失把时间线退回切换起点；还有备用设备时按注册顺序就近接管。

十一、切换过程中再次发起切换，要以已经提交的计划为起点，未完成的淡出不能重复执行两遍。

十二、SetFade 只影响之后生成的计划，已经提交的计划时长不变。

硬约束：并发调用 Advance 时位置累加必须原子，任何一次读取只能看到完整的旧值或新值，不能出现半次推进；快照要一起覆盖设备选择、播放位置、淡出余量与切换计数，损坏快照 Restore 报 ErrBadSnapshot 且不动现网状态，正常恢复后设备选择、播放位置与淡出进度都延续，恢复后的计数接着往上走。

验收：go test ./... 十五条必须全过，不得修改测试、跳过或用固定输入写死；并发与重启恢复自行验证；只做本地实现，只用标准库。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-c-11`
- 实现文件：`audio_device_switcher.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`AudioDeviceSwitcher`
