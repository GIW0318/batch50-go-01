# 变形网格法线重建调度

## 题面

角色做完形变动画后，肩膀和膝盖处出现一圈黑边，接缝两侧的两个重复顶点位置一样、UV 不同，却被打包成同一个顶点算了一根平均法线；美术调完平滑角后，已经重建过的躯干区域仍是旧法线，只有当前脏点跟着变；低端机上每帧重建预算不够，被顺延的顶点第二天还排不上，一直在等。现场只能靠调大预算绕过，帧率一掉老毛病又回来。落点在 gsb/batch50-go-game-b-19 的 normal_rebuild_scheduler.go，类型 NormalRebuildScheduler，要把蒙皮与形变后网格法线的重建与失效调度补齐。

公开接口保持：NewNormalRebuildScheduler() *NormalRebuildScheduler、RegisterMesh(name string, mesh RebuildMesh) error、MarkDirty(mesh string, verts []int) error、Rebuild(mesh string, budget int) (int, error)、Normals(mesh string) ([]Vec3, error)、SetSmoothAngle(deg float64) error、SmoothAngle() float64、SetSeamPolicy(p SeamPolicy) error、DirtyCount(mesh string) int、Stats() NormalStats、Snapshot() ([]byte, error)、Restore([]byte) error。

要修的点：

1. 网格名为空报 ErrBadName，重复注册报 ErrMeshExists，没有三角形或三角形索引越界报 ErrBadMesh；
2. 脏顶点下标越界或为负报 ErrBadVertex，且不得留下部分脏标记，未知网格报 ErrUnknownMesh；
3. MarkDirty 对同一顶点重复出现要去重，脏集合按顶点计数，空列表视为无操作；
4. 平滑角只能在零到一百八十度之间，越界报 ErrBadAngle 且保持原值；
5. 法线按相邻面法线以面积加权累加，面积取三角形两边叉积的模长，二面角不超过平滑角的边参与累加，超过的视为硬边；
6. 一个顶点参与多个互不相连的平滑组时，以三角形列表里最先出现的面为基准，只有与基准面夹角不超过平滑角的面参与累加，保证同一输入的结果可复现；
7. Rebuild 的预算必须为正，非正报 ErrBadBudget，返回本次实际重建的顶点数，任何情况下不超过预算，剩余脏点留到下次；
8. Rebuild 只动脏顶点及其一环邻居，未脏且不在一环内的顶点保留上一帧法线，新注册网格的法线全为零向量；
9. Rebuild 之后脏计数与 Stats 里的脏、顺延、已重建计数要互相对得上，顺延数是本次因预算没做完、留到下一帧的顶点个数，被拒绝的调用不计入；
10. Normals 返回副本，调用方改动不影响内部状态，未知网格报 ErrUnknownMesh；
11. 脏集合、已算好的法线、平滑角与接缝策略经 Snapshot/Restore 原样延续，恢复后重建接着做，损坏载荷报 ErrBadSnapshot。

三类硬约束：接缝两侧重复顶点位置相同但 UV 不同时，重建不能按位置去重合并累加，否则法线会被隔壁面拉歪；平滑角调整后已重建区域要整体失效，不能只对当前脏顶点生效；预算耗尽被顺延的顶点在下一帧必须优先重建，不能因为不断新增脏顶点而被饿死。

验收：go test ./... 的 15 条必须全过，不得改测试、跳过或按固定输入写死；并发与重启场景自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-19`
- 实现文件：`normal_rebuild_scheduler.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`NormalRebuildScheduler`

## 关键类型

- `Vec3`、`Vec2`、`Tri`、`RebuildMesh`、`SeamPolicy`、`NormalStats`
- 接缝策略：`SeamSplit`、`SeamMerge`
- 错误集：`ErrBadName`、`ErrMeshExists`、`ErrBadMesh`、`ErrUnknownMesh`、`ErrBadVertex`、`ErrBadBudget`、`ErrBadAngle`、`ErrBadSeam`、`ErrBadSnapshot`
