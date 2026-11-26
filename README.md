# 有向距离场阴影：自适应步进、锥形软影半影与体素分辨率变化的偏置重算

## 题面

美术把夜间关卡截图甩过来，三处问题一眼可见。第一处：角色站在石柱旁边，脸和肩上有几块本不该有光的亮斑，像有人从墙里点了一盏灯；第二处：把月光调成一个大面光源以后，柱子投在地上的影子边缘硬得像刀切，一点过渡都没有；第三处：地形体素精度从一米提到半米重烘之后，所有朝向阳光的平面都浮出一层细密横纹，凑近看像布纹。落点是模块 gsb/batch50-go-game-b-10 的 sdf_shadow_raymarcher.go，类型 SdfShadowRaymarcher，公开接口保持：NewSdfShadowRaymarcher(limits Limits) *SdfShadowRaymarcher、LoadField(fieldID string, voxels []Voxel) error、SetLight(lightID string, light LightSpec) error、Trace(origin, dir Vec3, maxDist float64) (ShadowResult, error)、SetStepBudget(n int) error、StepBudget() int、Bias(distance float64) (float64, error)、Fields() []string、Lights() []string、Active() (string, string)、SetActiveField(fieldID string) error、SetActiveLight(lightID string) error、Limits() Limits、Stats() TraceStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

要改的地方如下。第一，场号为空、体素列表为空报 ErrBadField；体素最小角在某轴大于最大角、坐标含 NaN、密度小于 0 或大于 1 报 ErrBadVoxel，被拒的场不得落表。第二，体素数超过 MaxVoxels 报 ErrVoxelLimit，场数超过 MaxFields 报 ErrFieldLimit；MaxFields、MaxLights、MaxVoxels、MaxSteps 留 0 或负数时按 8、8、4096、64 生效，BaseBias 留 0 或负数按 0.001 生效，Limits 返回生效值。第三，场与光源都按号升序列表，第一个载入的场与第一个登记的光源自动成为活动项，之后载入或登记不得夺走选择。第四，切换活动项时未知号报 ErrUnknownField 或 ErrUnknownLight，被拒的切换保持原选择。第五，光源号为空、半径含 NaN 或为负、强度非正或含 Inf，一律报 ErrBadLight，光源数超过 MaxLights 报 ErrLightLimit。第六，Trace 在没有活动场时报 ErrNoField，没有活动光源时报 ErrNoLight。第七，Trace 的起点或方向含 NaN 或 Inf、方向为零向量、maxDist 非正，报 ErrBadRay，被拒的射线不计入统计。第八，步进按当前点到最近遮挡体素表面的距离推进，密度不低于 0.5 的体素算遮挡，进入命中半径即判为遮挡；步数用尽仍未走完 maxDist 时按不确定处理，不得当成无遮挡。第九，未命中时 Depth 记为 maxDist、Certain 为 true、半影为 0；命中时 Depth 记账步距离、Certain 为 true，半影按锥形估计夹到 0 到 1 之间。第十，SetStepBudget 只接受正整数，否则报 ErrBadStepBudget 且预算不变。第十一，Bias 距离为负或 NaN 报 ErrBadDistance；无距离时等于 BaseBias，距离越大偏置越大，并随体素边长缩放。第十二，快照要带场、光源、活动选择、步数预算与统计计数；恢复后 Fields、Lights、Active、StepBudget 与统计必须对得上，损坏载荷报 ErrBadSnapshot 且不动现网。

还有三处必须一起收掉。步数预算被调紧或射线在长距离里反复小步推进时，用尽预算的结果只能标成不确定，交给上层做保守着色，绝不能直接返回无遮挡，否则墙后的亮斑就是这么漏进来的。把面光源半径调大，影子边缘的半影要跟着变宽，但完全处在阴影里的那一块不能因此变浅、更不能凭空亮起来。SDF 场换分辨率重烘之后，自阴影偏置必须按新的体素边长整份重算，不能沿用旧值，否则表面会出现一圈圈条纹。

硬约束两类。一是并发：多条光线并行步进时读取的场与光源必须是一致快照，统计计数不能丢更新，命中结果不得出现半更新的中间态。二是重启恢复：场、光源、选择与步数预算要能跨进程恢复，恢复后同一射线的命中结果必须一致，损坏载荷整份拒绝且不影响现网。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-10`
- 实现文件：`sdf_shadow_raymarcher.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SdfShadowRaymarcher`
