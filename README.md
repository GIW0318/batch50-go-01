# 动态分辨率控制环：帧时分档、滞回与逐视口目标重建

## 题面

动态分辨率上线后，美术和测试各报一类问题：切到战斗场景那一下，画面会先从清晰跳到模糊再跳回清晰，来回抖上两三秒；有人用手柄把游戏暂停在菜单里，背景一直在两个清晰度之间闪；更多的情况是某个分屏窗口明明已经掉到三十帧，缩放还挂在最高档不动。查下来是档位判定只看单帧、升降用同一套阈值，也没有冷却，重建请求发出去就当成已经切好，冻结与解冻之间计数还串着。落点是模块 gsb/batch50-go-game-b-12 的 dynamic_resolution_arbiter.go，类型 DynamicResolutionArbiter，公开接口保持：NewDynamicResolutionArbiter(cfg ArbiterConfig) *DynamicResolutionArbiter、AddViewport(viewport string, initial float64) error、RemoveViewport(viewport string) error、Viewports() []string、Scale(viewport string) float64、Ladder(viewport string) []float64、OverlayScale(viewport string) float64、SetRange(viewport, min, max float64) error、Report(viewport string, sample FrameSample) error、Freeze(viewport, reason string) error、Resume(viewport string) error、Frozen(viewport string) (bool, string)、PendingRebuild(viewport string) (RebuildRequest, bool)、CommitRebuild(viewport string, ok bool) error、Stats(viewport string) ViewportStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的地方如下。一、视口名为空或只有空白、初始档不在档位表里报 ErrBadViewport 且不建视口，重复视口报 ErrDuplicateViewport，移除或操作未登记视口报 ErrUnknownViewport；Scale、Ladder、Frozen、OverlayScale、Stats 这类不带错误返回的读取对未登记视口给 0 或空。二、档位表固定六档，从 0.50 到 1.00，Ladder 只返回落在当前范围里的档，升序；OverlayScale 对已登记视口恒为 1.0，界面与字幕层不跟着缩放。三、SetRange 的上下限都必须是档位表里的值且下限小于上限，否则报 ErrBadRange；范围收紧后当前档落在范围外要立刻贴到边界，改范围与冻结都作废未提交的重建请求。四、Report 的帧时必须是有限正数、InFlight 不能为负，否则报 ErrBadSample 且不计数。五、升档要求帧时不高于目标帧时的 0.9 倍并连续 UpStreak 个样本，降档要求不低于 1.15 倍并连续 DownStreak 个，两个阈值之间的死区把两个方向的连续计数一起清零。六、判定成立只生成一个待提交的重建请求，Scale 仍返回旧档，CommitRebuild 成功才切档并把切档与重建计数加一；没有待提交请求时报 ErrNoRebuild。七、CommitRebuild 传 false 表示重建失败，要丢掉请求停在原档位，不记切档，也不许把旧档改成中间值。八、同一方向两次切档之间至少隔 CooldownFrames 个样本，冷却没到不许再切，上下两个方向各算各的冷却。九、到顶不再升、到底不再降，既不生成请求也不刷新冷却。十、刚升档成功后的第一个样本帧时必然偏高，这个样本不参与降档判定，否则档位会在两档之间反复抖动。十一、冻结必须带非空原因，空原因报 ErrBadReason；冻结期间上报只记账不改档，解冻后按新样本重新累计，不能沿用冻结前的连续计数；解冻一个没冻结的视口报 ErrNotFrozen。十二、上一次重建的目标还被在飞帧引用时不能切换，提交要报 ErrRebuildBlocked，必须等上报的 InFlight 归零再切，旧目标不能提前释放。

硬约束：Report、CommitRebuild 与 Snapshot 会在渲染线程和 UI 线程并发进来，每个视口的档位、连续计数与统计要串行更新，快照只能是切换前或切换后的完整状态，不能半新半旧。快照要带各视口档位、范围、冻结原因、连续计数、待提交请求与统计，重启恢复后档位与连续计数延续，接着上报能按原进度完成升档；快照损坏时 Restore 报 ErrBadSnapshot 且不动现网。验收：go test ./... 15 条全过，测试不许改、不许跳过、不许按固定输入写死；分屏并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-12`
- 实现文件：`dynamic_resolution_arbiter.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`DynamicResolutionArbiter`
