# 去隔行策略规划

## 题面

投诉单 2026-1116-31：老片重制上线后，运动镜头边缘全是锯齿，静止镜头又被抹得发虚；3:2 节奏的片源在每一段的固定位置抖一下，同一集重新处理一遍结果还不一样，交付时长比原来短了几帧。落点在 gsb/batch50-go-game-c-13 的 deinterlace_planner.go，类型名 DeinterlacePlanner，包名 task。

构造签名 NewDeinterlacePlanner(cfg Config) *DeinterlacePlanner；Config 的 MotionThreshold 为零时按 0.25 处理，ConfirmFrames 为零时按 2 处理；节奏只认 none、3:2、2:2 三种取值。

需要落地的公开接口：Probe(frames []Frame) (FieldOrder, error)、Plan(frame Frame) (Method, error)、Apply(plan Plan, frame Frame) (Frame, error)、SetMotionThreshold(v float64) error、SetCadence(c Cadence) error、Order() FieldOrder、Cadence() Cadence、Reset() error、Stats() DeintStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的现象：

一、场序判定只看相邻两帧：把行号分成偶数行与奇数行两组，分别求当前帧与上一帧同号行的平均绝对差，奇数行的差明显大于偶数行判为上场优先，反之判为下场优先；两者相差不足两成时必须判为不确定，且不报错。

二、Probe 至少要两帧且帧几何一致，传空、只传一帧或宽高不一致报 ErrBadFrames；帧自身宽高为零、或者亮度平面长度与宽高不符报 ErrBadFrame。

三、场序不确定时整帧保守走场混合，不能直接整帧合并，也不能猜一个场序凑合。

四、每帧先算梳状度量：取奇数行与上下行均值之差的绝对值，按最大亮度归一化；低于阈值走整帧合并，高于阈值走运动自适应插值。

五、逐帧方法切换要带滞回：只有超过阈值两成以上才从合并翻到插值，回落到低于阈值两成才翻回合并，避免在阈值附近来回抖。

六、阈值滞回只作用于逐帧方法切换，不能改变节奏决定的场重复位置；3:2 节奏里每五个位置的第 2、第 4 个必须重复，其余位置照常判定。

七、场序判定在镜头切换后要重新确认，不能把上一镜头的场序沿用到下一镜头；重新确认期间按场混合处理。

八、静止区某一帧被误判成运动时只影响当前帧，不得把整段升级成全程插值。

九、节奏与阈值的切换只对之后的帧生效，已经规划过的那一帧不重算；未知节奏报 ErrBadCadence，阈值不在零到一之间报 ErrBadThreshold。

十、Apply 按计划的方法重建帧：整帧合并原样返回，插值把奇数行替换成上下行的均值，末行缺少下一行时用上一行补齐；未知方法报 ErrBadPlan。

十一、Stats 要分别给出规划帧数、三种方法的次数、重复次数、方法翻转次数与当前场序。

十二、快照要覆盖场序、节奏位置与统计；损坏快照 Restore 报 ErrBadSnapshot 且不动现网状态，恢复后节奏位置延续，下一帧接着原来的相位走。

硬约束：并发规划同一帧序列时状态只能串行推进，规划帧数与各项统计必须与真实调用一致；重启恢复后节奏相位不能从头开始，否则整段片源的重复位置会整体错位。

验收：go test ./... 十五条必须全过，不得修改测试、跳过或用固定输入写死；并发与重启恢复自行验证；只做本地实现，只用标准库。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-c-13`
- 实现文件：`deinterlace_planner.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`DeinterlacePlanner`
