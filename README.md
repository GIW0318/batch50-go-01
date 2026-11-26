# batch50-go-safety-c-42

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-42`
- 根包名：`task`
- 实现文件：`race_window_prober.go`
- 测试目录：`tests/test_race_window_prober_test.go`
- 类型：`RaceWindowProber`

## 故障单

商品秒杀接口被刷之后，先出现在客服台的是一堆重复发货单，运营说不清到底是脚本并发领取，还是幂等根本没生效；安全同学手工用两路并发复现了十几次，结论一会儿有一会儿没有，没人敢写进报告。这批活动还在跑，验证每拖一天就多出一批脏数据，运营要求当天给出可复现的结论。要把窗口探测做成可复用的验证流程，模块路径 gsb/batch50-go-safety-c-42，实现文件 race_window_prober.go，类型 RaceWindowProber，签名固定：NewRaceWindowProber(cfg Config) *RaceWindowProber、SetTransport(t Transport) error、SetClock(now func() time.Time)、Plan(op Operation) (Probe, error)、Fire(probeID string) ([]Outcome, error)、Verify(probeID string) (Verdict, error)、Cancel(probeID string) error、State(probeID string) (Probe, bool)、Probes() []string、Attempts(key string) int、Pending() int、Snapshot() ([]byte, error)、Restore(data []byte) error。

十二处要落地：一，Plan 校验业务键与地址非空、请求数不少于二，违者报 ErrBadOperation；请求数超 MaxRequests 报 ErrRequestLimit，并发度小于一、大于请求数或超 MaxConcurrency 报 ErrConcurrencyLimit，重试数超 MaxRetries 报 ErrRetryLimit；探测总数封顶报 ErrProbeLimit。二，探测标识按 probe-1 起严格递增，同一进程内不复用；探测一旦计划完成，请求数、并发度与重试数不得再改。三，Fire 前必须挂上传输，没有传输报 ErrNoTransport，未计划的标识报 ErrUnknownProbe，已经发过的再发报 ErrAlreadyFired，被取消的报 ErrProbeCancelled。四，一次 Fire 发出的请求数等于计划数，序号从一连续，同一序号只出现一次，并发度不得超过计划值。五，单个请求失败不影响 Fire 本身返回，失败原因记在对应结果里，结果表按序号升序返回，调用方可以逐条复盘。六，同一业务键同时只允许一批在飞，第二批报 ErrProbeInFlight；不同业务键可以并行推进，互不阻塞。七，Fire 前后各取一次服务端副作用计数，判定只看这段差值。八，差值大于一判 confirmed，重复次数为差值减一；差值等于一且无失败请求判 safe；有请求失败或差值为零判 uncertain，原因码写清窗口不完整。九，重复校验同一探测返回同一结论标识，结论标识按 verdict-1 起递增。十，取消只对未发出的探测生效，重复取消幂等返回成功；Pending 统计已发出未校验的探测数，Attempts 统计某业务键实际发出的请求数，两者在快照恢复后不得回退。十一，服务端幂等键生效时返回相同结果不算竞态成功；探测自身的重试不得改变业务状态，否则结论作废。十二，同一目标在冷却期内不得重复探测，避免把业务数据写脏；快照要能恢复探测状态、结果与结论，损坏快照整体拒绝。

硬约束：同一业务键的探测串行、不同业务键并行，发车与判定之间不得出现半批可见的状态；进程重启后凭快照恢复已发出的探测与结论，已校验的不再重复发车，计数继续累加，恢复后重新挂传输仍可继续校验。

验收：go test ./... 的 15 条用例全过，测试不得修改、跳过或按固定输入写死；同键并发与重启恢复自行验证；只做本地实现，交付源码和跑测记录。

## 运行

```bash
go test ./...
```

测试位于 `tests` 目录，包名同样为 `task`，通过 `impl "gsb/batch50-go-safety-c-42"` 引用根包，共 15 条用例。
