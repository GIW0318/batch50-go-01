# 深度精度规划：近远平面收紧、反向 Z 切换与逐通道映射

## 题面

场景组这周报了两个问题：山谷关卡的远处山脊在某些角度会整片消失，把远平面往后推一点又出现一条闪动的锯齿；俯视角下靠近相机的地面会出现阶梯状的深度条纹，切成反向 Z 之后更糟，整屏像被一层东西挡住。查下来是近远平面按固定值写死，反向 Z 只改了比较方向没改清值，几个相机共用同一份近远平面，深度预通道与主通道又各算各的映射，问题就串起来了。落点是模块 gsb/batch50-go-game-b-04 的 depth_precision_planner.go，类型 DepthPrecisionPlanner，公开接口保持：NewDepthPrecisionPlanner(cfg DepthConfig) *DepthPrecisionPlanner、AddPass(passID string, rangeHint DepthRange) error、RemovePass(passID string) error、Passes() []string、SetBounds(objectID string, b Bounds) error、RemoveObject(objectID string) error、Objects() []string、Plan(camera CameraParams) ([]PassDepth, error)、Validate() []DepthIssue、SetReversedZ(on bool) error、ReversedZ() bool、Precision(atDepth float64) (float64, error)、Stats() DepthStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的地方如下。第一，AddPass 遇到空通道号、近平面小于 0、远近平面相同或反转，报 ErrBadPass；通道号重复报 ErrDuplicatePass。第二，Passes 与 Objects 都按名字升序；物体号为空、上下界相同或反转、含 NaN 报 ErrBadBounds，同一个物体再登记一次按覆盖处理；RemovePass 未登记报 ErrUnknownPass，RemoveObject 未登记报 ErrNotFound。第三，没有任何物体时，规划范围直接用 MinNear 与 MaxFar。第四，有物体时近平面取 MinNear 与最近表面距离中的较大者，远平面取 MaxFar 与最远表面距离中的较小者。第五，通道自带的范围与相机范围求交，近端取较大值、远端取较小值，通道范围两端都是 0 表示跟随相机。第六，物体整体落在相机后方、Bits 小于 1、或者求交后远端不大于近端，一律报 ErrBadCamera。第七，反向 Z 默认关闭，通道的比较函数是 less；打开后每个通道的比较函数变成 greater 且 Reversed 为真，关闭后恢复 less。第八，普通 Z 的清值是 1。第九，Precision 只能在 Plan 之后调用，没规划过报 ErrNoPlan；传入深度不是正数或超过远平面报 ErrBadDepth。第十，Precision 按 z 的平方乘 (远减近) 除以 (远近乘积乘 2 的 Bits 次幂) 给出最小可分辨深度差。第十一，Validate 在精度不够时按通道给出区间与 precision 代码，没规划过时返回空列表。第十二，快照要带通道表、物体包围盒、反向开关与上一次规划结果，损坏载荷在 Restore 时报 ErrBadSnapshot 且不动现网。

还有三处必须一起收掉：远平面收紧到刚好包住最远物体时，正好落在远平面上的表面仍然要可分辨、不能被裁掉，边界的等号要算在可见一侧；反向 Z 与普通 Z 来回切换时清值要分别给，普通 Z 清 1、反向 Z 清 0，不能沿用切换前的旧清值，否则整屏都会被挡；子相机与父相机的深度映射必须各自独立，叠加相机不得复用基础相机的近远平面，改一个不能带着另一个一起变。

硬约束两类。一是并发：多路同时 Plan、AddPass 与 SetBounds 必须串行裁决，规划结果不能是半更新的中间态，计数累加不能丢。二是快照与重启：通道表、包围盒、反向开关与上次范围要能跨进程恢复，恢复后的第一次 Precision 必须按恢复出来的范围算，损坏载荷整份拒绝且不影响现网。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-04`
- 实现文件：`depth_precision_planner.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`DepthPrecisionPlanner`
