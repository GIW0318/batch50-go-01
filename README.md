# 曲面细分因子规划：共享边统一、内部因子与帧预算降档

## 题面

美术组这周连着提了三张单子：同一块曲面在远近切换时边缘会裂开一条细缝；把相机拉近看石壁，位移贴图上的凹凸还是平的；把相机快速拉远，整片地形像被人按了一下，一帧之内从细密网格跳成粗网格，画面抖得人眼发花。现在这套细分因子是在渲染线程里按每个面各算各的，谁算完谁提交，两块面挨着的边经常拿到不同档位，接缝就是这么来的。落点是模块 gsb/batch50-go-game-b-01 的 tessellation_factor_planner.go，类型 TessellationFactorPlanner，公开接口保持：NewTessellationFactorPlanner(limits Limits) *TessellationFactorPlanner、RegisterPatch(patchID string, edges []Edge) error、SetDisplacement(patchID string, maxHeight float64) error、SetScreenRatio(patchID string, ratio float64) error、Plan(view ViewParams) ([]PatchFactors, error)、PendingPlan() (string, bool)、Commit(planID string) error、Cancel(planID string) error、Factor(patchID string) (PatchFactors, bool)、Patches() []string、PatchEdges(patchID string) ([]Edge, bool)、Stats() PlanStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的地方如下。第一，补丁号为空、边表为空、边长非正或为 NaN 一律报 ErrBadPatch，且不落表。第二，同一个补丁号重复登记报 ErrDuplicatePatch，补丁数达到 MaxPatches 后新登记报 ErrPatchLimit；MaxPatches 或 MaxEdgeFactor 留成 0 或负数时按默认 64 处理。第三，连续因子按 边长 × PixelsPerUnit ÷ (CameraDistance × PixelErrorBudget) 再乘 (1 + 位移高度) 计算，向上取到 2 的幂，最低 1 档，最高不超过 MaxEdgeFactor。第四，内部因子取该补丁边缘因子最大值的一半，最低 1 档；Level 记最大边缘因子的指数。第五，屏幕占比必须落在 0 到 1 之间，越界报 ErrBadRatio，给未登记的补丁设置位移高度或占比报 ErrUnknownPatch。第六，Plan 的 CameraDistance、PixelsPerUnit、PixelErrorBudget 任一非正或为 NaN 报 ErrBadView；补丁表为空报 ErrNoPatches。第七，输出按补丁号升序，同一个 view 连算两次给出的因子必须完全相同。第八，一帧的因子总量按每个补丁的边缘因子加内部因子求和，超过 MaxFactorSum 时按屏幕占比由低到高挑补丁，把它当前最高的一档整档减半，所有补丁里同档的边一起减半，避免留下接缝，降到落回预算为止；全部降到 1 档仍超预算报 ErrBudgetExceeded，这一帧不产出任何因子。第九，同一时刻只留一个待提交计划，再 Plan 一次会把上一份作废，拿作废的计划号 Commit 报 ErrUnknownPlan；对已生效的计划号重复 Commit 幂等成功且不重复计数。第十，Cancel 只能取消待提交计划，取消后计划号失效。第十一，位移高度为负或 NaN 报 ErrBadDisplacement，原高度保持不变。第十二，快照要带上补丁表、位移高度、屏幕占比、已提交因子与帧号，损坏载荷在 Restore 时报 ErrBadSnapshot 且不动当前状态。

还有三处必须一起收掉，漏一处线上就会复现。两块补丁共享同一条边时，各自算出的连续因子往往不同，提交前要把这条边统一到较大的那一档，不能让两块面各写各的。位移贴图缺失的补丁按平面处理，因子不能沿用上一帧被位移抬高的档位。相机快速拉远时向下的档位变化要留一档滞回，不能整帧突变，否则玩家看到的就是闪烁。

硬约束两类。一是并发：多个渲染线程同时 Plan 与 Commit 必须串行裁决，任何时刻只有一份生效因子，计数累加不能丢，读到的因子也不能是半更新的中间态。二是重启恢复：提交结果、帧号与降档轮次要能跨进程恢复，恢复后继续 Plan 的帧号必须大于快照里的帧号，损坏载荷必须整份拒绝且不影响现网。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-01`
- 实现文件：`tessellation_factor_planner.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`TessellationFactorPlanner`
