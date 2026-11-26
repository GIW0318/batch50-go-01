# 内核锁定等级与受控面联动治理

## 题面

工单来自一次夜间加固：运维把内核锁定等级从 integrity 抬到 confidentiality 之后，审计采集器的调试通道被一并关掉；事后想退回 integrity 把排障通道开回来，接口直接拒绝，水位卡在最高档。另一次更糟，变更做到一半某个受控面关闭超时，已经关掉的面回滚不了，等级却没有提交，控制台上显示的等级和实际生效状态对不上。主机重启后又整体掉回无锁定，锁定位形同虚设。

落点 gsb/batch50-go-safety-c-17 的 lockdown_mode_arbiter.go，类型 LockdownArbiter。签名保持：NewLockdownArbiter(cfg Config) *LockdownArbiter、Current() Level、Watermark() Level、RegisterSurface(s Surface) error、SetLevel(target Level, by Actor) (Transition, error)、SetWatermark(target Level, by Actor) error、Explain(surface string) (SurfaceState, error)、Surfaces() []SurfaceState、Snapshot() ([]byte, error)、Restore([]byte) error。Config 里的 MaxSurfaces 限制受控面数量，CloseBudget 限制单次变更的关闭代价。

要修的点：

1. 等级只有 none、integrity、confidentiality 三档，越界值报 ErrInvalidLevel；Actor 名称空白报 ErrBadActor。
2. SetLevel 目标等于当前等级时为空操作：Applied 为 false，不消耗序号，也不写任何面的关闭序号。
3. 目标高于当前等级时，按面名升序关闭 ClosedAt 不高于目标的面，Closed 列出本次关掉的面，Cost 汇总本次代价，Seq 递增；此前已关闭的面不得重复关闭，关闭序号保持首次的值。
4. 目标低于水位报 ErrLevelMonotonic；水位只能由 Signed 调用者抬升，抬升后当前等级不得低于水位；把水位往下调同样报 ErrLevelMonotonic。
5. RegisterSurface 校验名称与 Kind 非空、Cost 非负，否则报 ErrBadSurface；重名报 ErrSurfaceExists；ClosedAt 越界报 ErrInvalidLevel；超过 MaxSurfaces 报 ErrSurfaceLimit；注册时若该面在当前等级下已不允许打开，直接以关闭态入册。
6. Explain 未知面报 ErrUnknownSurface；Surfaces 按面名升序返回，关闭的面带正数关闭序号。
7. 单次变更按面名累计 Cost，超出 CloseBudget 的面记入 Skipped 并保持打开，等级照常提交；已经关闭的面不回退。
8. 降级（不低于水位）要重开 Reopenable 且 ClosedAt 高于目标等级的面，其余面保持关闭。
9. Critical 面一旦落在需要关闭的集合里，整次变更在预检阶段整体拒绝 ErrSurfaceBlocked，不能先关掉一部分再回滚。
10. 快照要带等级、水位与各面开关状态；Restore 全成或全不成，损坏载荷报 ErrBadSnapshot 且不动现网；恢复后等级不得低于水位。
11. 水位在快照里存主字段和审计副本两份，主字段读不出来时从副本恢复；两份都不可用时按最严等级复位，不能退成无锁定。
12. 并发下 SetLevel 与 SetWatermark 全局串行，12 个协程同时抬同一档只有一次 Applied，序号只加一。

硬约束：

- 同一次等级变更里，受控面关闭按序推进、结果一次性提交，不允许出现等级已变而面状态未知的中间态；超出代价预算的面如实记入 Skipped。
- 高等级水位与各面关闭状态必须跨重启保持，恢复过程任何一步失败都不得留下半套状态。
- 处于高等级时新注册的受控面要立即服从当前等级，不能因为入册时机绕开锁定。

验收：go test ./... 15 条全过；不得改测试、跳过、按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-17`
- 实现文件：`lockdown_mode_arbiter.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`LockdownArbiter`
