# 光照贴图 UV 展开与图集打包

## 题面

烘焙组反馈：同一面墙在光照图里一半清晰一半发糊，墙角处还出现一条黑缝；重新烘一张图表后整张图集的 UV 都挪了位，已经烘好的物件跟着错位，只能整批重烘。还有一个更怪的现象：同一个网格连续展开两次，图集尺寸和图表顺序都不一样，下游缓存全部失效。这套工具从近景道具一直用到整栋建筑的静态网格，密度、边距和图表编号一旦不稳，下游的采样就会一片一片返工。落点在 gsb/batch50-go-game-b-18 的 lightmap_uv_unwrapper.go，类型 LightmapUnwrapper，要把光照贴图 UV 的图表切分、密度均衡与图集打包补齐。

公开接口保持：NewLightmapUnwrapper(maxCharts, maxAtlasSize int) (*LightmapUnwrapper, error)、SetSmoothAngle(deg float64) error、SetPadding(px int) error、Padding() int、SetChartLimit(n int) error、Unwrap(mesh Mesh, texelsPerUnit float64) (Atlas, error)、Validate(atlas Atlas) []Issue、Rebake(chart ChartID) error、Charts() []Chart、ChartDensity(chart ChartID) (float64, error)、Stats() UnwrapStats、Snapshot() ([]byte, error)、Restore([]byte) error。

要修的点：

1. 图表上限与图集尺寸在构造时就要校验，非正分别报 ErrBadLimit、ErrBadAtlasSize；
2. 边距为负报 ErrBadPadding 且不改原值，Padding 如实回报当前边距；
3. 图表上限调小必须落在构造上限之内，非正或超过上限报 ErrBadLimit；
4. 纹素密度非正报 ErrBadDensity，空网格或无三角形的网格报 ErrEmptyMesh，被拒的展开不得覆盖上一次的图集；
5. 按平滑角切分图表，二面角超过平滑角的边成为图表边界，低于阈值的相邻面合并，平滑角超出零到一百八十度报 ErrBadSmoothAngle；
6. 同一图表内纹素密度必须一致，密度按世界尺度算出来，缩放只改 UV 不改密度标记会让后续采样用错尺度，未触发重缩放时 ChartDensity 返回的正是请求的每单位纹素数，未知图表报 ErrUnknownChart；
7. 图表数超过上限报 ErrChartLimit，不允许静默截断，也不能把超出的三角形塞进已满的图表；
8. Validate 要报出图表矩形相交与越出图集边界两类问题，问题里带图表标识与类型，干净图集不得报任何问题；
9. 重打包只影响被改动的图表，其余图表的 UV 位移与密度保持不变，重打包次数记进 Stats；
10. 图表打包不得重叠，边距按像素对齐，图集宽高受构造参数硬顶，装不下时先扩到上限再放弃并报错；
11. 打包结果与设置经 Snapshot/Restore 原样延续，损坏载荷报 ErrBadSnapshot 且不动现有图集。

三类硬约束：两个图表打包后边界相接但边距不足，要判为冲突并重排，不能接受贴边；某图表重新缩放后纹素密度要同步到烘焙采样，不能只改 UV 不改密度标记；零面积或退化三角形跳过时要单独记账，且不能占用图表编号，后续图表的编号不能因此出现空洞。

验收：go test ./... 的 15 条必须全过，不得改测试、跳过或按固定输入写死；重启与重打包场景自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-18`
- 实现文件：`lightmap_uv_unwrapper.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`LightmapUnwrapper`

## 关键类型

- `Mesh`、`Tri`、`Vec2`、`Vec3`、`Rect`、`Chart`、`ChartID`、`Atlas`、`Issue`、`UnwrapStats`
- 问题类型：`IssueOverlap`、`IssueOutOfBounds`、`IssueDensity`
- 错误集：`ErrBadLimit`、`ErrBadAtlasSize`、`ErrEmptyMesh`、`ErrBadDensity`、`ErrBadPadding`、`ErrBadSmoothAngle`、`ErrChartLimit`、`ErrUnknownChart`、`ErrBadSnapshot`
