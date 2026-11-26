# 相机栈合成：叠加相机清屏策略、稳定排序与帧内增删

## 题面

主界面组这周连着报障：进关卡时右上角的小地图会把主画面的颜色一起抹掉，只剩一块黑底；战斗界面的伤害数字偶尔压在小地图下面，同一个配置重开两次层级还不一样；有一次切场景，深度预通道把主相机的画面清掉了一半，整帧闪黑。查下来是叠加相机的清屏是按布尔位凑出来的，顺序放在一个 map 里存，合成时边遍历边渲染，中途增删相机就会出现半帧。落点是模块 gsb/batch50-go-game-b-05 的 camera_stack_compositor.go，类型 CameraStackCompositor，公开接口保持：NewCameraStackCompositor(limits Limits) *CameraStackCompositor、Push(base CameraSpec) error、Overlay(camera CameraSpec, order int) error、Remove(cameraID string) error、Compose() ([]RenderStep, error)、SetViewport(rect Rect) error、Viewport() Rect、SetOrder(cameraID string, order int) error、Base() (CameraSpec, bool)、Overlays() []CameraSpec、Cameras() []CameraSpec、Stats() ComposeStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的地方如下。第一，整个栈只有一台基础相机，重复 Push 报 ErrBaseExists，相机号为空报 ErrBadCamera。第二，Overlay 必须在 Push 之后调用，否则报 ErrNoBase；相机号与基础相机或已有叠加重复报 ErrDuplicateCamera；叠加数量达到 MaxOverlays 上限后报 ErrOverlayLimit，上限留成 0 或负数按 8 处理。第三，Compose 时没有基础相机报 ErrNoBase，步骤总数超过 MaxSteps 报 ErrStepLimit，上限留成 0 或负数按 16 处理。第四，合成顺序是基础相机永远排第一，叠加按 order 升序，IsUI 为真的相机一律排在最后并且不参与深度。第五，每一步的 Seq 从 1 开始逐个递增，同一份栈连算两次结果必须一致。第六，清屏策略按声明原样落到步骤上，只清颜色、只清深度、都不清、全清四种组合都合法，不能被合成为别的组合。第七，Overlay 传入的 order 覆盖相机自带的 Order 字段。第八，SetOrder 只能改叠加相机，指向基础相机报 ErrBaseRequired，未登记报 ErrNotFound。第九，Remove 基础相机报 ErrBaseRequired，未登记报 ErrNotFound，移除后下一次合成不再出现。第十，SetViewport 的宽或高不是正数报 ErrBadViewport，失败时保留原视口；相机自带视口是零矩形时用栈视口，否则用相机自己的。第十一，Cameras 返回基础相机在前、其余按合成顺序排列，Overlays 不包含基础相机。第十二，快照要带基础相机、叠加表、视口与各自的 order，损坏载荷在 Restore 时报 ErrBadSnapshot 且不动现网。

还有三处必须一起收掉：只清深度的叠加相机必须保留颜色缓冲，不能把基础相机已经画好的颜色清掉；order 相同的两台叠加相机要按注册顺序稳定输出，不能跟着 map 的遍历顺序变；合成过程中被移除的相机，本帧要么整段包含要么整段跳过，不允许出现渲染了一半的步骤。

硬约束两类。一是并发：多路同时 Overlay、Remove 与 Compose 必须串行裁决，任何一帧要么看到增删前的整份栈要么看到增删后的整份栈，计数累加不能丢。二是快照与重启：基础相机、叠加表、order 与视口要能跨进程恢复，恢复后第一次合成的顺序与恢复前一致，损坏载荷整份拒绝且不影响现网。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-05`
- 实现文件：`camera_stack_compositor.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`CameraStackCompositor`
