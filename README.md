# gsb/batch50-go-safety-c-23

## 题面

某电商的搜索接口被外部通报存在盲注风险，安全同学用布尔与时间两种手法复测时出现矛盾结论：同一参数有时判成可注入，隔几分钟再测又判成正常。复盘发现页面每次响应都带随机请求号和时间戳，相似度比较被这些片段带偏；时间通道用了写死的固定阈值，链路一慢就大面积误判；推断到一半服务重启，已确认字符和消耗预算全部丢失，只能从头再来。

需要在 gsb/batch50-go-safety-c-23 的 blind_sqli_prober.go 里补全 BlindSqliProber，入口 NewBlindSqliProber(Options) *BlindSqliProber。公开接口：Baseline(ParamPoint, int) (Baseline, error)；Probe(ParamPoint, Payload) (Diff, error)；Infer(string) (Inference, error)；Budget(string) (BudgetView, error)；Stop(string) error；Case(string) (CaseView, bool)；Points() []string；Snapshot() ([]byte, error)；Restore([]byte) error。

基线阶段：Baseline 通过构造时注入的取样函数恰好采样 samples 次，规范化后的正文全部一致才算稳定，签名取首份规范化正文；Baseline 带 CaseID、PointID、Samples、MeanMillis、JitterMillis 与 Stable。采样数低于 MinBaselineSamples 返回 ErrBadSampleCount，取样失败返回 ErrFetchFailed，正文不一致返回 ErrBaselineUnstable，同一参数重复建基线返回 ErrPointExists；被拒绝的请求不得调用取样函数，也不得登记用例。判定稳定后才把参数记进 Points。

探测阶段：Probe 只在参数已有稳定基线时工作，否则 ErrNoBaseline；Expression 为空白、Kind 不认识或 ProbeChar 不在字母表内返回 ErrBadPayload；每次探测恰好调用一次取样函数。规范化正文与签名一致时 Similarity 为 100，否则按最长公共前缀占比取整，低于 ChangeSimilarityFloor（零值按 90）时 Changed 为真。DeltaMillis 是观察耗时减基线均值，ThresholdMillis 取 max(100, DelayMillis/2, JitterMillis*3)，差值达到门槛时 LatencyHit 为真，Diff 带当前推断位置。

推断阶段：布尔通道以 Changed 为命中，时间通道以 LatencyHit 为命中，任一命中即确认该 ProbeChar：Position 加一、Extracted 追加该字符、候选集重置为整个字母表；未命中则把该 ProbeChar 从当前位候选集排除，候选按字母表顺序返回。ProbeChar 为空且命中表示已到字符串结尾，Complete 置真；确认位数达到 MaxPositions 也置真。Budget 的 RequestsLeft 等于请求上限减已探测次数，PositionsLeft 等于 MaxPositions 减已确认字符数，MillisLeft 等于总时延预算减累计耗时；次数触顶后 Probe 返回 ErrBudgetExhausted。

停止与恢复：Stop 幂等，冻结后 Probe 与 Infer 都返回 ErrCaseStopped，已采集证据与预算读数保持可查。Snapshot 必须带上基线签名、推断位置、候选与已确认字符、已消耗预算；Restore 到新实例后 Infer 与 Budget 的结果与重启前一致，载荷损坏返回 ErrBadSnapshot 且不得改动现有状态。

现场还有三类容易漏判的情况：页面带随机令牌或时间戳时，相似度必须先剥离易变片段再比较；时间通道门槛要随基线方差自适应，固定阈值在慢链路上会大量误判；推断出的字符可能有大小写或编码等价的多种结果，收敛时要保留候选集而不是只留一个。

硬约束：同一参数的探测严格串行并保持最小间隔，基线采样与探测结果一起落盘；单参数请求数、并发参数数与总时延三个上限同时生效；重启后恢复推断位置与已消耗预算，已停止用例保持停止。

验收口径：go test ./... 的 15 条用例必须全过；不得修改测试、跳过或按固定输入写死；并发与重启自行验证；只做本地实现。交付源码、测试输出与故障说明。

## 运行

```bash
go test ./...
```

## 模块

- 模块路径：`gsb/batch50-go-safety-c-23`
- 包名：`task`
- 实现文件：`blind_sqli_prober.go`
- 测试文件：`tests/test_blind_sqli_prober_test.go`
