# RADIUS 动态授权下发与退避重传

## 题面

宽带割接之后一批用户的限速策略改不动：运维在网管上下发 CoA，有的 NAS 回了确认但用户速率没变，有的直接回错误原因；同一会话重复点下发，系统里堆出好几个作业号；断线指令和改速指令混在一起，日志里分不清成功的是哪一种。更麻烦的是某台 NAS 掉电后作业一直转圈，前台看到的还是处理中，最后会话既没断、速率也没改。落点在模块 gsb/batch50-go-safety-b-17 的 radius_coa_dispatcher.go，类型 RadiusCoaDispatcher，签名保持：NewRadiusCoaDispatcher(cfg DispatcherConfig) *RadiusCoaDispatcher、Submit(req CoaRequest) (Job, error)、Complete(jobID string, reply Reply) (Result, error)、Outcome(jobID string) (Result, error)、Retry(jobID string) error、Cancel(jobID string) error、Pending() []Job、Tick(now time.Time) []Job、SessionState(sessionID string) (SessionRecord, error)、Stats() DispatcherStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

需要落地的判定：

1. 类型为空或会话为空报 ErrBadRequest，类型只认 coa 与 disconnect，其余报 ErrUnsupportedKind，被拒的提交不占队列也不留记录。
2. 同一会话已有在途作业时重复提交幂等返回同一作业号且不新建；作业收尾（确认、拒绝、超时、取消）之后才允许新作业。
3. 待处理作业数达到 MaxPending 报 ErrQueueFull，被拒的提交不改变已排队作业。
4. Tick 只派发已到 NextAt 的作业，重传间隔从 BaseBackoff 起逐次翻倍并以 MaxBackoff 封顶，每次派发累加尝试次数并重算下次时间。
5. 尝试次数超过 MaxAttempts 或累计耗时超出 TotalBudget 时作业只能落到 timeout，不得标成功，也不得自动改投另一台 NAS。
6. 应答码与请求类型必须配对：CoA 只接受 41 与 44，Disconnect 只接受 40 与 45，错配报 ErrProtocolMismatch 且作业状态与排队表都不变。
7. 确认应答把作业落到 acked，记录应答码、尝试次数与耗时；对已收尾作业再次投递应答报 ErrJobClosed。
8. 拒绝应答按错误原因分类：401、402、403、404、405、406 属永久拒绝，不再重试；506 与 507 属可重试，作业回到排队并按退避重新排期，尝试次数保留。
9. 取消只对排队或在途作业生效，取消后 Outcome 固定 canceled；对不存在的作业操作报 ErrUnknownJob。
10. Retry 只接受超时或可重试拒绝的作业，其余报 ErrNotRetryable；重排之后作业重新进入排队表。
11. 断线成功把会话送进终止态，改速成功会话仍为在线，两者不能共用一条迁移路径；作业已取消之后才到达的应答只做记账，不改变已记录的结论。
12. 快照覆盖作业、会话、结果与序号，恢复后新作业号继续往上走且不与旧号冲突，损坏载荷报 ErrBadSnapshot 且现网表不动。

硬约束两条：同一会话同一时刻只允许一个在途作业，Submit、Complete 与 Cancel 对同一会话的读写必须原子，并发的 Tick 不能把同一次重传派发两遍，退避与总预算计数也不得被并行调用重复推进；快照恢复要么全量生效要么不动，恢复后的排队顺序与尝试次数要和快照一致。

验收：go test ./... 的 15 条用例必须全过，不得修改测试、跳过或把期望值按固定输入写死；并发下发与重启恢复自行构造验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-17`
- 实现文件：`radius_coa_dispatcher.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`RadiusCoaDispatcher`
- 只依赖标准库
