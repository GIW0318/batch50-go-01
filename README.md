# UI 布局求解：测量排布两阶段、弹性余数与命中测试

## 题面

客户端这周的工单集中在一处：设置页在窗口大小变化后，一列按钮的宽度总和比容器多出一像素，右边滚动条时有时无；同一个窗口连缩两次，两个等宽按钮的宽度会互换，像是故意错位；面板被缩到看不见以后，点空白处偶尔还能点到早该不可见的按钮，点击落到空区域。现在这套布局是遍历节点时顺手写的，父子两遍混在一起，脏标记只在绘制阶段用，命中测试直接拿声明尺寸算。落点是模块 gsb/batch50-go-game-b-02 的 ui_layout_solver.go，类型 UiLayoutSolver，公开接口保持：NewUiLayoutSolver(cfg LayoutConfig) *UiLayoutSolver、Build(root NodeSpec) (LayoutTree, error)、SetViewport(w, h float64) error、Invalidate(nodeID string) error、Solve() (LayoutResult, error)、HitTest(x, y float64) (string, bool)、Node(nodeID string) (LayoutNode, bool)、Dirty() []string、SetSize(nodeID string, width, height SizeSpec) error、Stats() LayoutStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的地方如下。第一，根节点铺满视口，忽略它自己的尺寸声明；节点号为空、重复、方向不是 row 或 column、尺寸模式不是 fixed、percent、flex、百分比越界、内边距为负，都要在 Build 阶段报 ErrBadNode 或 ErrDuplicateNode，坏的声明整份不落地。第二，节点数超过 MaxNodes 报 ErrNodeLimit，深度超过 MaxDepth 报 ErrDepthLimit，两个上限留成 0 或负数时按 256 与 16 处理。第三，行方向按子节点宽度声明分配主轴、按高度声明分配交叉轴，列方向相反；交叉轴上没声明或声明 flex 的铺满父内容区。第四，内容区是盒子减去四边 Padding，子节点从内容区左上角起排，相邻子节点之间留 Gap。第五，定宽按像素算，百分比按父内容区同轴长度换算。第六，弹性按整数权重分剩余空间，每份先向下取整，所有弹性宽度之和必须正好等于剩余空间。第七，先测量子节点再分配剩余空间，嵌套面板的百分比与弹性一律以它自己的内容区为基准。第八，Solve 只重排脏节点及其子树，其余盒子原样保留；Invalidate 未登记节点报 ErrUnknownNode；脏集合按节点号升序给出，重排完清空。第九，视口变化必须把整棵树标脏重算，根宽度跟到新视口。第十，SetSize 只改声明，未登记节点报 ErrUnknownNode，负值或负权重报 ErrBadSize，改动在下一次 Solve 生效。第十一，命中测试返回命中点所在的最深节点；点在根内但不在任何子节点内返回根；点在根外返回空串与 false。第十二，快照要带声明树、视口与已解盒子，恢复后 Node 能读到原来的盒子，损坏载荷报 ErrBadSnapshot 且不动现网。

还有三处必须一起收掉：弹性除不尽的余数按稳定顺序补给排在前面的节点，同一份输入两次求解必须给出完全一样的结果，不能跟着 map 的遍历顺序走；被父级 Clip 裁掉的子节点仍要参与布局，但命中测试要按裁剪后的可见区域判定，不能只在绘制阶段裁剪；视口宽或高为 0 甚至负数时排布要退化到不 panic，视口恢复后脏区必须全量重算。

硬约束两类。一是并发：同时 Solve、Invalidate 与 SetSize 必须串行裁决，脏标记不能丢，返回的结果不能是算了一半的中间态。二是快照与重启：视口、声明树与已解盒子要能跨进程恢复，恢复后的第一次 Solve 必须整棵树重算，恢复失败不得影响现网数据。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-02`
- 实现文件：`ui_layout_solver.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`UiLayoutSolver`
