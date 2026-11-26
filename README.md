# 设备丢失恢复：依赖序资源重建、旧句柄失效与连续失败后的降级固化

## 题面

发行前一天，三台机器的冒烟测试同时挂了。第一台：玩家用 Alt+Tab 切出去再切回来，游戏窗口全黑，日志里只有一行设备丢失，渲染线程还停在原地等一个永远不会回来的图形句柄；第二台：驱动升级后进主菜单能看到满屏细长条纹，像有人拿着旧显存里的内容在画；第三台更麻烦，连续崩了两次之后画面永远停在最低画质，阴影全平、后处理全关，重启也回不来。落点是模块 gsb/batch50-go-game-b-07 的 device_lost_recovery.go，类型 DeviceLostRecovery，公开接口保持：NewDeviceLostRecovery(limits Limits) *DeviceLostRecovery、Register(spec ResourceSpec) (Handle, error)、OnLost(reason string) error、Recreate(now time.Time) (RecoveryReport, error)、Resolve(handle Handle) (ResourceState, error)、Degrade(level int) error、DegradeLevel() int、Resources() []Handle、Status() string、Generation() int、QueueRender(req RenderRequest) error、DrainRender() []RenderRequest、HistoryValid() bool、Snapshot() ([]byte, error)、Restore(data []byte) error。

要改的地方如下。第一，资源种类只有管线、纹理、缓冲、渲染目标四种；名字为空白、种类不认识、字节数非正报 ErrBadResource，且不落表。第二，重名报 ErrDuplicateResource，表满 MaxResources 报 ErrResourceLimit，MaxResources 留 0 或负数按 128 生效。第三，依赖按名字声明：依赖名未登记报 ErrMissingDependency，依赖自己报 ErrBadResource；登记时只校验名字存在，环留到重建时判定。第四，登记成功后句柄从 1 起单调递增，当前代号为 1；Resolve 未知句柄报 ErrUnknownHandle，合法句柄返回名字、种类、字节、Valid 与代号。第五，Resources 按句柄升序返回当前可用句柄，Status 返回 ready 或 lost；设备丢失期间登记新资源报 ErrDeviceLost。第六，OnLost 原因为空白报 ErrInvalidReason 且状态不变，重复 OnLost 报 ErrAlreadyLost；丢失后旧句柄一律报 ErrStaleHandle，Resources 返回空，代号要等重建成功才加一。第七，没丢设备就 Recreate 报 ErrNotLost；重建顺序固定为管线、纹理、缓冲、渲染目标，同类按登记序，报告里带原因、新代号、重建句柄序列、级别与本次是第几次尝试。第八，重建发现依赖环报 ErrDependencyCycle 并消耗一次重建配额；同一轮丢失里尝试次数超过 MaxAttempts 报 ErrRecoveryBudget，MaxAttempts 留 0 或负数按 3 生效，失败的重建不得发布任何句柄。第九，重建次数与耗时都算预算：距第一次重建尝试超过 RebuildBudget 同样报 ErrRecoveryBudget。第十，Degrade 只接受 0 到 MaxLevel 之间的级别，越界报 ErrBadDegradeLevel 且级别不变，重复设置同一级别静默成功，MaxLevel 留 0 或负数按 3 生效。第十一，快照要带资源描述表、依赖、句柄水位、代号与降级级别；恢复后旧代号的句柄仍是 ErrStaleHandle，新句柄可正常解析，损坏载荷报 ErrBadSnapshot 且不动现网。

还有三处必须一起收掉。恢复期间渲染线程还会往队列里塞请求，这些请求要停在队列里等重建完成再按到达顺序发出，不能直接丢掉，否则玩家切回来看到的是一帧比一帧更空的画面。同一次运行里第二次设备丢失如果还在预算内，要直接按更低的画质级别重建，级别加一之后不再自动升回，避免在崩溃边缘反复试高画质。重建成功后 TAA 与后处理的历史缓冲整体作废，不能拿上一代的颜色做重投影，否则画面会拖出一条长长的残影。

硬约束两类。一是并发：多线程同时 Register、OnLost 与 Resolve 必须串行裁决，句柄不得重号，MaxResources 的边界不能因竞争被突破，读到的资源状态不能是半更新的中间态。二是重启恢复：资源表、代号与降级级别要能跨进程恢复，恢复后不得复活上一代句柄，损坏载荷必须整份拒绝且不影响现网。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-07`
- 实现文件：`device_lost_recovery.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`DeviceLostRecovery`
