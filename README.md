# 本地分屏视口分配：重排、宽高比装箱与每视口预算

## 题面

本地分屏最近被 QA 连续退回：两个手柄一起进关，第二个人进来的一瞬间两个画面互换位置，玩家自己的准星跑到对面屏幕上；三个人联机时第三个视口的画面被压扁，圆形准星成了椭圆；窗口被拉到小尺寸后，四个视口里有两个只剩几个像素高，鼠标点上去完全对不上；还有一次热更新渲染预算，只改了一个人的配额，另外三个画面的帧率一起掉。现在的实现是每次有人进出就把布局重排一遍，谁先渲染谁拿新布局，预算按平均值一刀切。落点 gsb/batch50-go-game-b-13 的 split_viewport_allocator.go，类型 SplitViewportAllocator，公开接口保持：NewSplitViewportAllocator(cfg SplitConfig) *SplitViewportAllocator、SetWindow(w, h int) error、Window() (int, int)、Join(slot string, aspect float64) (LayoutPlan, error)、Leave(slot string) (LayoutPlan, error)、Layout() LayoutPlan、Pending() (LayoutPlan, bool)、Commit(plan LayoutPlan) error、SetAspect(viewport string, aspect float64) error、SetBudget(viewport string, us int64) error、Budgets() map[string]int64、Slots() []string、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的地方如下。一、槽位名为空或只有空白报 ErrBadSlot，宽高比非正或不是有限数报 ErrBadAspect，重复加入报 ErrSlotExists，加入数到 MaxSlots 报 ErrSlotLimit，上限留成 0 或负按 4 处理。二、单元格按人数均分：一人整窗，两人左右各半，三人左半整高、右半上下两格，四人四象限；宽高都用整除，右列与下排吃掉余数。三、视口宽高比与单元格不一致时居中留黑边，不许拉伸；内容宽高由浮点算出后四舍五入取整，居中偏移用剩余空间整除。四、任一单元格低于 MinCellW 或 MinCellH 的加入要报 ErrLayoutTooSmall，且不改变成员与版本，不许产出不可用布局。五、Join 与 Leave 只排队新版本，Layout 仍返回旧的生效版本，Commit 才原子切换；初始生效版本为 1、窗口为 0x0，下一次成功变更排出的版本从 2 起；有待提交版本时只认这一版，提交别的版本一律 ErrStalePlan，没有待提交版本时重复提交当前生效版本幂等成功。六、SetWindow 的宽高必须为正，否则 ErrBadWindow；改窗口后若任一单元格低于下限报 ErrLayoutTooSmall 且窗口保持不变；窗口变化同样走排队与提交。七、SetAspect 对未登记视口报 ErrUnknownViewport，非法比例报 ErrBadAspect，成功后排一个新版本，重建后的内容尺寸跟着变。八、加入时新视口的预算默认取 MinBudgetUs；SetBudget 的配额不能低于 MinBudgetUs，否则 ErrBadBudget；所有视口配额之和不得超过 TotalBudgetUs，超出报 ErrBudgetExceeded 且原配额不变；总预算与最小配额留成 0 或负按 33333 与 2000 处理。九、离开槽位要释放它的预算，后来的人重新分到配额；未登记的槽位离开报 ErrUnknownSlot。十、槽位按加入顺序站位，离开的位次留给下一次加入优先复用，避免相机与输入映射整体位移。十一、布局切换那一帧的输入区域与相机绑定必须取自同一版本，不能一半旧一半新。十二、总预算放不下新视口的最小配额时拒绝加入，不许按比例压到下限以下。

硬约束：加入、离开、改窗口与提交会被输入线程和渲染线程并发调用，版本推进与成员表必须串行，任何时刻只能有一个待提交版本；快照带窗口、成员、宽高比、预算与生效版本号，重启恢复后成员与版本延续，接着加入要排在新版本上，不能回退到旧版本；快照损坏时 Restore 报 ErrBadSnapshot 且不动现网。验收：go test ./... 15 条全过，测试不许改、不许跳过、不许按固定输入写死；四路并发加入与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-13`
- 实现文件：`split_viewport_allocator.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SplitViewportAllocator`
