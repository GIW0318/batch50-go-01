# GPU 回读暂存环：围栏追踪、按用途分池与超时降级

## 题面

回读链路这周出了三次事故：拾取标识回读偶尔拿到上一次点击的数据，鼠标点地面却选中了上一帧的箱子；连续截图时第二张比第一张先到，拼出来的对比图整体错位；有台机器上 GPU 卡住之后，同一用途的后续回读全部挂死，另一用途的渲染统计也一起不出了。查下来是暂存槽没有围栏概念，完成前就被下一帧覆盖；结果按完成时刻交付，谁先好谁先给；超时以后既不降级也不回收，槽位越用越少。落点 gsb/batch50-go-game-b-15 的 gpu_readback_broker.go，类型 ReadbackBroker，公开接口保持：NewReadbackBroker(cfg BrokerConfig) *ReadbackBroker、SetClock(tick int64) error、Clock() int64、SetPoolSize(purpose string, slots int) error、Request(purpose string, size int) (Ticket, error)、Submit(t Ticket, data []byte) error、CompleteFence(t Ticket) error、Poll(t Ticket) (Result, bool, error)、Cancel(t Ticket) error、Latency(purpose string) Stats、Purposes() []string、Pending(purpose string) int、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的地方如下。一、用途名为空或只有空白报 ErrBadPurpose，请求字节数非正或超过 MaxBytes 报 ErrBadSize，这两类拒绝不改任何登记与计数。二、首次出现的新用途按 DefaultPoolSlots 建环，SetPoolSize 可改，槽数非正或超过 MaxSlotsPerPurpose 报 ErrBadPool，缩到低于当前占用报 ErrPoolBusy。三、环满时报 ErrPoolFull，不覆盖仍在写的槽；槽位只在围栏完成并且这一票已经交付、降级或取消之后才回收。四、请求占用的字节计入总占用，所有在飞字节之和不得超过 MaxBytes，超出报 ErrBytesLimit；环槽、默认槽数与字节上限留 0 或负分别按 8、4 与 1MiB 处理。五、Submit 的载荷长度必须等于请求字节数，否则报 ErrSizeMismatch；Submit 要复制传入的切片，调用方之后改自己的缓冲区不能影响回读结果；重复 Submit 报 ErrBadState。六、围栏没完成的票 Poll 返回空结果与 false，不报错；票号未登记报 ErrUnknownTicket；已交付的票再 Poll 报 ErrTicketConsumed。七、同一用途内按提交顺序交付，Seq 从 1 起逐票递增，不同用途各自计数；一个用途积压不影响别的用途取结果。八、逻辑时钟只能前进，倒退或为负报 ErrBadClock；从请求时刻到当前时刻超过 MaxLatencyTicks 的票，Poll 要返回降级结果，带降级标记与降级时刻，并释放等待；结果的时刻字段记生成该结果时的逻辑时钟。九、降级结果不进正常结果序列，重复 Poll 同一票号要稳定拿到同一份降级结果。十、Cancel 对等待中的票立即生效，之后 Poll 报 ErrTicketCancelled、Submit 报 ErrTicketCancelled，重复 Cancel 幂等；已交付或已降级的票不能取消，报 ErrBadState。Stats 里 Requests、Delivered、Degraded、Cancelled 分别累计，InFlight 只数仍在等结果的票，Pending 给同一口径的条数，BytesInFlight 是仍占着槽的字节合计。十一、被取消的票，其槽位要等围栏完成后才能回收，取消不能让仍被 GPU 写入的槽位立刻复用，取消后仍要能完成围栏。十二、同一用途里后提交但先完成的票不得越过先提交的票交付结果。

硬约束：Request、Submit、Poll 会被渲染线程与游戏线程并发调用，槽位分配、围栏推进与结果出队必须串行，同一票号不能被交付两次；快照带各用途环大小、票号水位、逻辑时钟与未完成票，重启恢复后未完成的票标记失效而不是继续等待，新票号接着水位往上发；快照损坏时 Restore 报 ErrBadSnapshot 且不动现网。验收：go test ./... 15 条全过，测试不许改、不许跳过、不许按固定输入写死；并发回读与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-15`
- 实现文件：`gpu_readback_broker.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`ReadbackBroker`
