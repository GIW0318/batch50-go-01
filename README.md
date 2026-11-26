# GraphQL 准入成本核算与租户窗口预算

## 题面

开放平台 GraphQL 端点被一个 first: 100000 的查询打穿，单请求把下游数据库拖垮；同一查询换个别名就能绕过限流；introspection 查询单独放量，导致租户额度怎么算都对不上。安全团队复盘时发现限流只看查询字符串长度，别名一换就绕过。请落在 gsb/batch50-go-safety-b-08 的 graphql_cost_guard.go，类型 GraphqlCostGuard，签名保持 NewGraphqlCostGuard(Limits) *GraphqlCostGuard、Analyze(query string, vars map[string]any) (Plan, error)、Admit(tenantID string, plan Plan) (Decision, error)、Charge(tenantID string, cost int) error、Refund(tenantID string, cost int) error、Usage(tenantID string) Window、Tick(now time.Time) int、CacheStats() CacheStats、SetIntrospection(tenantID string, enabled bool) error、Snapshot() ([]byte, error)、Restore([]byte) error。

成本口径：每个字段节点记 FieldCost 一分；带 first、last、limit 参数的字段，其子树成本乘以该参数值，乘数上限为 MaxListMultiplier，字段自身另记一分，例如 { users(first: 5) { id name } } 在 FieldCost 为 1 时是 1+5×2=11；深度按根选择集下的字段为第一层计算。

要修的行为：
一、解析：支持简写查询与带 operation 关键字的文档；括号不配对、空文档或非法字符报 ErrBadQuery；引用未定义的变量按解析失败处理。
二、限制：深度超过 MaxDepth 报 ErrDepthLimit；展开后的字段节点数超过 MaxFields 报 ErrFieldLimit。
三、片段：命名片段与内联片段都要展开计入；循环引用报 ErrFragmentCycle；引用不存在的片段报 ErrUnknownFragment。
四、别名：每个别名各自算一个字段节点，不得因为字段名相同就合并计数。
五、指纹：Plan.QueryHash 为 64 位小写十六进制，同一文档同一变量取值指纹相同，文档或变量取值变化时指纹必须变化。
六、缓存：同一指纹二次分析置 Cached 为真并累加命中，返回的 Plan 必须是副本，调用方改写不得污染缓存；缓存容量受 CacheSize 约束，超出后旧条目可被淘汰。
七、准入：租户额度不足时 Allowed 为假、Reason 为 budget_exhausted，且不得扣费；单查询成本超过 MaxCost 时 Reason 为 cost_limit；被拒绝的请求不得改变 Usage，调用方重试仍按同一额度判断。
八、扣费与回滚：Charge 与 Admit 都以最近一次 Tick 的时刻记账，cost 非正报 ErrBadCost；Refund 退款不得超过已用额度，否则报 ErrRefundExceeds。
九、窗口：Tick 回收超过 WindowLength 的扣费并返回条数，Usage 给出 Used、Limit 与 Remaining。
十、租户：空租户号报 ErrBadTenant。
十一、成本字段：Plan.Cost、Fields 与 Depth 必须来自同一次展开，不能按字面文本估算。

最容易漏的三处：变量把分页参数放大到远超字面默认值时按变量实际值计费；同一别名重复选择同一字段不得重复计费，也不得漏掉父级乘数；introspection 查询单独计费并可按租户关闭，但与普通查询共享同一窗口额度。

硬约束：同一指纹的解析结果不可变且并发安全；Admit 的额度判断与扣减必须原子，并发下总扣费不得超过租户额度，任何失败不得留下部分扣费；Tick 与 Admit 并发时不得重复回收同一笔扣费。单文档节点数、单查询成本、租户窗口额度与缓存条数都受配置上限约束。

验收：go test ./... 15 条全过，不得改测试、跳过或按固定输入写死；并发准入自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-08`
- 实现文件：`graphql_cost_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`GraphqlCostGuard`
