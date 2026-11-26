# 共享材质属性动画驱动与实例拆分

## 题面

美术联调时发现，同一份共享材质被场景里十几个物件引用，播自发光呼吸动画时远处物件先亮、近处物件晚半帧，同一帧截图的明暗对不上；把自发光曲线解绑后那块物件整片变黑，本该回到共享材质的原值；还有几个物件的动画从往复切到单次后直接跳回第一帧，尾巴被吃掉。这批材质只有一份，实例按需拆出，美术改一次曲线就得等整帧渲染完才敢切场景，联调效率被拖得很低。落点在 gsb/batch50-go-game-b-16 的 material_animator.go，类型 MaterialAnimator，要把运行时材质属性动画的驱动、实例拆分与采样记账补齐。

公开接口保持：NewMaterialAnimator(maxCurves, maxKeys, maxInstances int) *MaterialAnimator、AddMaterial(name string, base map[string]float64) error、SetCurve(id CurveID, keys []Key) error、Bind(material, prop string, curve CurveID) (BindingID, error)、Unbind(id BindingID) error、Advance(dt time.Duration) error、Value(material, prop string) (float64, error)、Materialize(material string) (InstanceID, error)、SetLoop(material string, mode LoopMode) error、Loop(material string) (LoopMode, error)、SetPaused(material string, paused bool) error、Bindings(material string) []BindingID、Stats() AnimStats、Snapshot() ([]byte, error)、Restore([]byte) error。

要落地的行为：

1. 空材质名报 ErrBadMaterial，重复注册报 ErrDuplicateMaterial，未注册材质报 ErrUnknownMaterial；
2. 属性只认自发光、透明度、U 偏移、V 偏移四个名字，其它一律 ErrUnknownProperty；
3. 绑定时曲线不存在报 ErrUnknownCurve，同一材质同一属性重复绑定返回同一 BindingID，不额外占绑定；
4. 关键帧时间必须非负且非递减，否则报 ErrBadKeys；曲线条数与单曲线关键帧数受构造参数硬顶，超限报 ErrCurveLimit、ErrKeyLimit，且被拒的曲线不得残留；
5. 采样按相邻关键帧线性插值，相位为零取首帧值，首帧之前保持首帧值，越过末帧按循环模式处理，循环重复回绕到相位起点；
6. 新建材质默认循环重复播放，Advance 传负时长报 ErrBadDelta 且相位与帧计数都不动；
7. 单次模式下相位到末尾就停在末尾值；
8. 解绑后该属性回到共享材质基色，解绑不存在的绑定报 ErrUnknownBinding；
9. 带动画的属性经 Materialize 拆出独立实例，同一材质重复取实例返回同一 InstanceID，实例数超限报 ErrInstanceLimit；
10. 暂停按材质记账，暂停期间该材质相位不推进，其它材质照常推进，对未注册材质暂停报错；
11. 绑定关系、曲线内容、实例分配与相位要经 Snapshot/Restore 原样恢复，恢复后相位继续累进，损坏载荷报 ErrBadSnapshot 且不动现有状态；
12. Stats 要如实反映材质、曲线、关键帧、绑定、实例与帧计数，任何被拒绝的操作都不能让计数虚增。

两条硬约束：同一材质被多个绘制批次引用时，属性更新必须在帧边界一次性生效，不能一个批次读到新值、另一个批次还读到旧值；解绑最后一个动画属性后实例要并回共享材质，但并回必须等引用该实例的在飞帧结束，期间新来的绘制仍读旧实例值。另外循环模式从循环切到单次时，若当前相位已越过末尾，要停在末尾值而不是回绕到开头。

验收：go test ./... 的 15 条必须全过，不得改测试、跳过或按固定输入写死；并发与重启场景自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-16`
- 实现文件：`material_animator.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`MaterialAnimator`

## 关键类型

- `CurveID`、`BindingID`、`InstanceID`、`LoopMode`、`Key`、`AnimStats`
- 属性常量：`PropEmissive`、`PropOpacity`、`PropUVOffsetU`、`PropUVOffsetV`
- 循环模式：`LoopOnce`、`LoopRepeat`、`LoopPingPong`
