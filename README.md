# gsb/batch50-go-safety-c-22

## 题面

命令注入与模板注入的验证服务在灰度期出现两类误判：回显里明明带出了验证标记，判定却记成未命中；网络抖动导致请求整体变慢，又被当成时延命中，值班同学手工比对才发现目标其实已经限流。复盘时用例顺序、每通道原始响应和当时的判定依据都散在日志里，无法复现。

需要在 gsb/batch50-go-safety-c-22 的 injection_oracle_runner.go 里补全 InjectionOracleRunner，入口 NewInjectionOracleRunner(Options) *InjectionOracleRunner。公开接口：Plan(InjectionPoint) (CaseSet, error)；Run(string) (Attempt, error)；Submit(string, Response) error；Judge(string) (Outcome, error)；Abort(string, string) error；Case(string) (Case, bool)；Cases(string) []Case；Budget(string) (BudgetView, error)；Snapshot() ([]byte, error)；Restore([]byte) error。

计划阶段：Plan 为每个载荷生成一条用例，顺序与载荷一致，AttemptsPerCase 固定为 3；载荷列表为空返回 ErrBadPayload，Key 或 Param 为空白返回 ErrBlankPoint，同一 Key 重复计划返回 ErrPointExists，尝试总数超过 MaxAttemptsPerPoint 返回 ErrAttemptLimit，被拒绝时不留下半成品用例。条目状态从 planned 开始。

执行阶段：每条用例按 baseline、echo、time 三个通道依次派发，Ordinal 从 1 开始；一条尝试未提交前再次 Run 返回 ErrAttemptPending；同一注入点同一时刻只允许一条用例有在途尝试，否则 ErrCasePending；三条通道都提交后不再派发，Run 返回 ErrCaseClosed。Submit 对未知尝试返回 ErrUnknownAttempt，重复提交返回 ErrAttemptClosed，响应里的 AttemptID 与尝试不一致返回 ErrBadResponse。

判定阶段：Judge 要求三条通道都有响应，缺一条返回 ErrChannelsPending，成功后用例状态为 judged。回显通道把正文做一次规范化（压缩连续空白、还原常见实体转义）后再找载荷的 Marker，找到即 EchoMatched 为真。时延通道的 DeltaMillis 是观察耗时减基线耗时，ThresholdMillis 取 max(100, DelayMillis/2)，差值达到门槛即 LatencyHit。两通道都命中时 Channel 为 both、Reason 为 reason_both_channels_hit、Confidence 为 high；单通道命中时 Channel 为该通道名、Reason 为该通道原因码、Confidence 为 medium；都不命中时 Hit 为假、Channel 与 Confidence 为空、Reason 为 reason_no_channel_hit。Outcome 还要带 BaselineMillis 与 ObservedMillis，结论按用例缓存，重复 Judge 返回同一结论且不重复计数。

中止与预算：Abort 理由为空白返回 ErrAbortReason，成功后用例状态为 aborted，Run 与 Judge 都返回 ErrCaseAborted，重复 Abort 幂等。Budget 的 AttemptsLeft 等于上限减已提交响应数，MillisLeft 等于总时延预算减累计耗时；预算耗尽后 Run 返回 ErrDelayBudget。

现场还有三类必须处理的情况：时延通道要先测基线再比载荷，基线自身抖动大时要追加采样或复核基线，不能拿一次基线直接判负；回显可能被压缩或转义成看不到原样的形式，比对前必须规范化；目标在验证过程中触发限流时后续请求都会变慢，时延通道要能识别并暂停，不能把限流判成命中。

硬约束：同一注入点的用例与通道严格串行，状态推进与预算扣减必须原子；重启后从未判定的通道继续，已判定用例的原始响应丢弃、结论与预算保留；尝试数、总时延与并发注入点三个上限同时生效，触顶给对应错误码而不是静默截断。

验收口径：go test ./... 的 15 条用例必须全过；不得修改测试、跳过或按固定输入写死；并发与重启行为自行验证；只做本地实现。交付源码、测试输出与故障说明。

## 运行

```bash
go test ./...
```

## 模块

- 模块路径：`gsb/batch50-go-safety-c-22`
- 包名：`task`
- 实现文件：`injection_oracle_runner.go`
- 测试文件：`tests/test_injection_oracle_runner_test.go`
