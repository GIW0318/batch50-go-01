# 平面反射：镜像相机、斜近平面裁剪与多平面共享图集的更新频率预算

## 题面

关卡组在湖区做验收，三张单子一起压过来。第一张：镜头抬到水面上方斜看过去，倒影里的山脊被削掉一截，靠近水线的石头整块消失，只剩一片亮边；第二张：玩家潜到水下再抬头，整个湖面变成一块倒过来的天空，糊得看不清方向；第三张：镜厅里八面镜子加两面水墙同时开反射，显存直接冲顶，帧率掉到个位数，而只有正对玩家的两面镜子反射还在变，其余几面像被冻住了。落点是模块 gsb/batch50-go-game-b-08 的 planar_reflection_guard.go，类型 PlanarReflectionGuard，公开接口保持：NewPlanarReflectionGuard(limits Limits) *PlanarReflectionGuard、RegisterPlane(planeID string, plane PlaneSpec) error、BuildMirrorCamera(planeID string, camera CameraParams) (MirrorCamera, error)、SetUpdateRate(planeID string, everyN int) error、Commit(planeID string) (ReflectionState, error)、Invalidate(planeID string) error、State(planeID string) (ReflectionState, bool)、Planes() []string、SetResolution(planeID string, width, height int) error、BeginFrame(frame int) error、Frame() int、Atlas() AtlasLayout、Stats() ReflectionStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

要改的地方如下。第一，平面号空白、法线为零向量或含 NaN、宽或高非正，一律报 ErrBadPlane 且不落表；重名报 ErrDuplicatePlane，表满 MaxPlanes 报 ErrPlaneLimit，MaxPlanes 留 0 或负数按 16 生效。第二，AtlasWidth 与 AtlasHeight 留 0 或负数按 4096 生效；单面分辨率的像素数超过整张图集，或所有平面视口面积之和超过图集面积，报 ErrAtlasBudget。第三，平面首次登记后视口占用图集从左上角起排，更新频率默认 1，代数从 0 开始，Enabled 为 true，Planes 按平面号升序。第四，镜像相机的位置与朝向按平面做镜像反射：位置与朝向都消去两倍的法向分量，Up 同样镜像，Near 与 Far 沿用原相机。第五，斜近平面按平面方程输出为 (N.x, N.y, N.z, -N·P)，N 取归一化法线，保留 x*X+y*Y+z*Z+w 不为负的一侧。第六，相机位置为零向量、朝向为零向量、Up 为零向量、FOV 非正、Near 非正、Far 不大于 Near，都报 ErrBadCamera；平面号未登记报 ErrUnknownPlane。第七，SetUpdateRate 的 everyN 非正报 ErrBadUpdateRate 且原值不变，未登记平面报 ErrUnknownPlane。第八，Commit 必须在 BeginFrame 之后调用，没开帧报 ErrNoFrame；BeginFrame 的帧号必须严格递增，重复或回退报 ErrBadFrame 且当前帧号不变。第九，Commit 按帧号差判断是否到期：距上次真正渲染不足 everyN 帧时 Updated 为 false，代数与 Frame 不动，Skipped 加一；到期时代数加一、Frame 记当前帧、Updated 为 true，Updates 加一；同一帧重复 Commit 只有第一次真正渲染。第十，Invalidate 让该平面下一次 Commit 无视频率立刻渲染，代数在真正渲染时才加一，未登记平面报 ErrUnknownPlane。第十一，SetResolution 的宽高非正报 ErrBadResolution，改后该平面视口按新宽高生效，越界报 ErrAtlasBudget 且保持原分辨率。第十二，快照要带平面表、法线与平面点、分辨率、更新频率、代数与帧号；恢复后 Frame 与快照一致，继续 BeginFrame 的帧号必须大于快照帧号，损坏载荷报 ErrBadSnapshot 且不动现网。

还有三处必须一起收掉。斜近平面裁剪的判定边界要把正好落在平面上的几何留在保留侧，等号不能丢，否则水线附近的石头会被裁出月牙形缺口。相机跑到平面下方时该平面的反射要整面停用，返回 Enabled 为 false 并在 Reason 里写清原因，不能继续用镜像相机渲出一张倒置的画面。某个平面改分辨率时要按新面积把整张图集里的视口重新排一遍，不能让相邻平面继续压在旧坐标上，否则两面镜子的反射会互相串图。

硬约束两类。一是并发：多线程同时 Commit 与 Invalidate 必须串行裁决，同一帧同一平面最多一次真正渲染，代数不得跳号，Update 与 Skipped 计数不能丢。二是重启恢复：平面表、分辨率、频率、代数与帧号要能跨进程恢复，恢复后帧号不得回退，损坏载荷必须整份拒绝且不影响现网。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-08`
- 实现文件：`planar_reflection_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`PlanarReflectionGuard`
