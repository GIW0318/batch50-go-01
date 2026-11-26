# 弱口令泄露查询的 k-匿名前缀预算治理

## 题面

改密接口上线两周后，安全组发现注册页的泄露校验几乎从不拦人，登录页却频繁误报；同时上游服务的账单翻了三倍，网关日志里同一段前缀被并发重复请求了几百次。落点 gsb/batch50-go-safety-a-08 的 breach_range_oracle.go，类型 BreachRangeOracle，签名保持：NewBreachRangeOracle(Upstream, Limits) *BreachRangeOracle、Check(ctx context.Context, tenantID, password string) (Verdict, error)、CheckFor(ctx context.Context, tenantID, stage, password string) (Verdict, error)、Warm(prefix string) error、Budget(tenantID string) (Usage, error)、SetBudget(tenantID string, perWindow int) error、Health() Status、CachePrefixes() []string、Reset(tenantID string) error、Snapshot() ([]byte, error)、Restore([]byte) error。上游只认 5 位大写十六进制前缀，响应体首行是 PREFIX:<前缀>，之后每行是 <后缀>:<出现次数>；口令摘要按 SHA-1 取大写十六进制，前 5 位做前缀，其余 35 位做后缀。

要修的地方：租户号为空报 ErrBlankTenant，口令为空报 ErrBlankPassword，调用点不在 register、change、login 之内报 ErrUnknownStage，这三种情况都不许打上游；发出去的东西只能是 5 位前缀，后缀、明文、完整摘要不得出现在请求、缓存键和错误信息里；后缀列表为空时判未泄露、Band 取 none、Action 取 allow；命中次数 1 到 9 归 low、10 到 999 归 high、1000 及以上归 critical，critical 必须 deny，low 给 warn；同一口令第二次查询走缓存，不再打上游，Verdict 的 Source 标 cache 且计数与首次一致；同一前缀的并发查询合并成一次上游请求，等待者共享同一结果；SetBudget 传 0 或负数报 ErrBudgetLimit 且不改动额度，预算耗尽后仍返回结论、Band 取 budget_exhausted、Breached 为假且不再打上游，Budget 里 Used 与 Remaining 要算得对，没单独设过额度的租户按内置默认额度 100 次计；上游连续失败达到 CircuitOpenAfter 次后 Health 的 State 变 open，熔断期间不打上游、直接给 unavailable 且不报错，失败计数照旧保留；响应体超过 MaxResponseBytes 报 ErrResponseTooLarge 且不进缓存；单次查询超过 Limits 里的 Timeout 报 ErrUpstreamTimeout；缓存条目超过 MaxCacheEntries 时淘汰最久未用的那条，被重新使用的条目要刷新位置；Warm 只接受 5 位大写十六进制，其余报 ErrBadPrefix；快照要还原缓存内容、各租户额度用量与失败计数，损坏的快照报 ErrBadSnapshot 且不动现网，Reset 只清指定租户的用量。

还有三条边界要点名：前缀命中、但后缀列表里根本没有这个后缀时，要记为未泄露，不能因为前缀命中就判泄露；上游返回的前缀与本次请求不一致，或者某行条目格式非法时，整批结果作废并按不可用处理，不能挑着用；预算耗尽时注册与改密按拒绝处理，登录按放行加待复查处理，两条路径口径不同，不能一刀切。

硬约束两条：同前缀的并发查询只能产生一次上游请求，缓存写入与预算扣减必须原子，不能出现额度已扣但结果没写入的半状态；熔断、预算与缓存三者的判定要在一个临界区内完成，状态切换后任何读侧都看不到半更新。缓存条目数、单条响应大小、超时都必须按配置硬顶。

验收：go test ./... 15 条全过，不得改测试、跳过、按固定输入特判；并发合并与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-a-08`
- 实现文件：`breach_range_oracle.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`BreachRangeOracle`
