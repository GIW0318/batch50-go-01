# 异步计算调度：队列时间线、显式跨队列栅栏与超限任务的跨帧顺延

## 题面

程序组这周报上来的三条现象都指向同一块调度逻辑。打开 GPU 时间线，计算队列与图型队列明明能并行，却整帧串行，帧时间直接翻倍；换台机器后，水面反射偶尔读到上一帧的深度缓冲，屏幕中间长出横向撕裂；室外场景每帧的计算任务偏多，超过时间片的那批像被直接扔了，下一帧再也看不到，体积雾一直糊着不散。落点是模块 gsb/batch50-go-game-b-06 的 async_compute_scheduler.go，类型 AsyncComputeScheduler，公开接口保持：NewAsyncComputeScheduler(limits Limits) *AsyncComputeScheduler、Submit(queue QueueKind, job JobSpec) (JobHandle, error)、Barrier(handle JobHandle, waits []JobHandle) error、BeginFrame(frame int) error、EndFrame() (FrameReport, error)、Cancel(handle JobHandle) error、Job(handle JobHandle) (JobState, bool)、Pending(queue QueueKind) []JobHandle、Depends(handle JobHandle) []JobHandle、Frame() int、Limits() Limits、Snapshot() ([]byte, error)、Restore(data []byte) error。

要改的地方如下。第一，队列只认图型与计算两种，传零值或别的值报 ErrBadQueue。第二，任务名为空白、耗时非正、读写集合里出现空串或同一资源重复出现，一律报 ErrBadJob 且不占句柄。第三，句柄从 1 起单调递增，零值永不合法；Job 要能读回名字、队列、读写集合、耗时与所属帧号，初始状态为 pending。第四，未开帧就提交或结束帧报 ErrNoFrame；帧号必须严格递增，重复或回退报 ErrBadFrame 且当前帧号不变。第五，存活任务数达到 MaxJobs 后新提交报 ErrJobLimit；MaxJobs 与 ComputeBudget 留成 0 或负数时按 256 与 1000 生效，Limits() 返回生效后的值。第六，栅栏只能声明跨队列依赖：同队列两个句柄报 ErrBarrierSameQueue，等自己或等待列表为空报 ErrBadBarrier，句柄未提交报 ErrUnknownJob；重复声明同一等待要合并，Depends 不得出现重复项。第七，EndFrame 先按提交序跑完图型任务，再在计算队列按提交序挑能塞进本帧时间片的任务；塞不下的整条顺延，状态置 deferred 并进入 FrameReport.Deferred，ComputeUse 记本帧实际用掉的计算耗时。第八，FrameReport 的图型与计算切片各自按执行序排列句柄，已执行任务状态变 done，被取消的进 Cancelled。第九，本帧只要有一条跨队列栅栏，两个队列就不得重叠，Serialized 为 true，否则为 false。第十，Cancel 把待执行或顺延任务置为 cancelled 并从待执行列表摘除，重复取消静默成功；取消已完成的任务报 ErrJobDone，未知句柄报 ErrUnknownJob。第十一，快照带帧号、任务表、等待关系与句柄水位；恢复后 Frame() 与快照一致，已执行任务仍能查到，再开帧的帧号必须大于快照帧号；损坏载荷报 ErrBadSnapshot 且不动现网。

另外三处必须一起收掉，漏一处线上就会复现。同一队列里两个都写同一资源的任务按提交顺序隐式有序，不必额外声明；跨队列时提交时间的先后不能拿来猜执行顺序，必须显式栅栏，否则就是那条横向撕裂。被顺延的任务在下一帧要和新提交的任务一起按优先级重排，不能因为早来一帧就永久沉底，否则低优先级任务一直饿着。取消一个正被别的任务等待的任务时，下游要一并取消并在 Reason 里写明被上游拖下水，不能静默跳过，否则时间线上会留下没人消费的中间纹理。

硬约束两类。一是并发：多线程同时 Submit、Barrier 与 Cancel 必须串行裁决，句柄不得重号，MaxJobs 的边界不能因竞争被突破，读到的状态不能是半更新的中间态。二是重启恢复：帧号、任务表与句柄水位要能跨进程恢复，恢复后第一次开帧不得回退，损坏载荷必须整份拒绝且不影响现网。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-06`
- 实现文件：`async_compute_scheduler.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`AsyncComputeScheduler`
