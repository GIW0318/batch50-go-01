# 2D 瓦片地图分块合成：图层叠加、脏块重建与动画相位

## 题面

瓦片地图最近被关卡组退回三次：把背景层的一块石头换成火把之后，石头还在原地显示，火把只闪了一下就不动了；打开一张大图时前两秒所有块都是上一关的残留内容，切场景之后要过好几帧才对；改了一格地砖，整张图连同远处的块一起重建，进关直接卡住；还有动画火把在不同块里相位不一样，左边的火把亮着，右边的火把灭了。现在的实现是逐格直接提交绘制，脏标记只在整图级别使用，图层顺序调完不重算，动画帧按块各算一套。落点 gsb/batch50-go-game-b-14 的 tilemap_chunk_compositor.go，类型 TilemapChunkCompositor，公开接口保持：NewTilemapChunkCompositor(cfg TilemapConfig) *TilemapChunkCompositor、ChunkIDOf(x, y int) ChunkID、AddLayer(layer int) error、SetAnimatedFrames(tile TileID, frames int) error、SetTile(layer, x, y int, tile TileID) error、Rebuild(chunk ChunkID) (int, error)、Flush() (int, error)、DirtyChunks() []ChunkID、Visible(cam Camera) []ChunkID、Animate(chunk ChunkID, tick uint64) error、SetLayerOrder(order []int) error、LayerOrder() []int、DrawList(chunk ChunkID) ([]TileInstance, error)、Budget() (used, limit int)、LayerBudget(layer int) (used, limit int, degraded bool)、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的地方如下。一、图层号不能为负，重复登记报 ErrDuplicateLayer，图层数到 MaxLayers 报 ErrLayerLimit，上限留 0 或负按 8 处理；登记顺序就是初始图层序。二、SetTile 的图层必须已登记，否则 ErrUnknownLayer；坐标必须落在 Cols 与 Rows 之内，否则 ErrOutOfRange，越界的写入不改格子也不标脏。三、同一块里多层瓦片按图层序叠加，每个元格只出一个实例，取最上层的不透明图块；实例按元格先纵后横升序排列，透明标记与空图块都不绘制，也不遮蔽下层。四、改一格只把所在块标脏，别的块保持原状态；Rebuild 立刻重算指定块并清掉它的脏标记，返回实际编译出的实例数；不在图内的块号报 ErrUnknownChunk。五、脏块按块号先纵后横升序批量重建，每帧重建块数受 MaxRebuildsPerFrame 限制，上限留 0 或负按 4 处理，超出的脏块留到下一帧。六、Animate 推进全局动画相位，所有块的动画图块用同一个 tick 取模，相位变了要把带动画图块的块标脏重算；块号非法报 ErrUnknownChunk。七、动画注册的帧数小于 2、图块为空或是透明标记都报 ErrBadAnimation。八、SetLayerOrder 必须是已登记图层的全排列，缺项、重复、未登记都报 ErrBadOrder；改图层序要把所有非空块标脏重算。九、Visible 返回与相机矩形相交的块，按先纵后横升序；相机宽或高非正返回空。十、每层实例数有上限 MaxLayerInstances，超出时该层标记降级并按元格序保留前若干个实例；全局上限 MaxInstances 同样是硬顶，Budget 返回已编译实例数与上限。十一、图层顺序调整后，原本被上层遮蔽的区域要重新可见，重排不能让遮蔽结果停留在旧顺序上。十二、某格被替换成透明图块时该块要按内容重新判定是否还有可绘制实例，不能只凭脏标记保留旧实例。

硬约束：SetTile 会被流式加载线程写入，Rebuild 与 Animate 在渲染线程推进，脏集合与已编译缓存的更新要串行，重算过程中读到的块要么是旧内容要么是新内容；重建预算耗尽时脏块维持旧内容显示，下一帧必须优先于新产生的脏块被重建。快照带格子内容、图层序、动画注册、当前相位与脏集合，重启恢复后脏集合与相位延续，接着 Flush 能补上没重算的块；快照损坏时 Restore 报 ErrBadSnapshot 且不动现网。验收：go test ./... 15 条全过，测试不许改、不许跳过、不许按固定输入写死；流式写入与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-14`
- 实现文件：`tilemap_chunk_compositor.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`TilemapChunkCompositor`
