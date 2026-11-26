# 审计日志防篡改链：哈希链、锚点轮换与断链告警

## 题面

风控组的审计日志前一阵被人改过一次：一条账号解封记录的操作者被替换，前后记录看起来完全正常，值班同学直到例行抽检才发现某段摘要对不上。更麻烦的是，日志服务每天凌晨会做一次锚点轮换，轮换之后老段落的可验证性直接丢了，追查只能靠备份。另外还有一次夜间批量任务并发写入，第二天看链尾出现了两个后继，整段日志只能全部作废。落点在模块 gsb/batch50-go-safety-d-10 的 audit_chain_guard.go，类型 AuditChainGuard，公开接口保持不变：NewAuditChainGuard(cfg ChainConfig) *AuditChainGuard、Append(actor, action string, payload []byte) (Entry, error)、Entries() []Entry、Head() (Entry, bool)、Len() int、Verify() error、Rotate() (Anchor, error)、Anchors() []Anchor、LatestAnchor() (Anchor, bool)、Alarms() []Alarm、ClearAlarms() int、Redact(seq int64, reason string) (Entry, error)、Snapshot() ([]byte, error)、Restore(data []byte) error。

口径如下。第一，操作者或动作留空分别报 ErrBlankActor、ErrBlankAction；载荷超过 MaxPayload 报 ErrPayloadTooLarge；条目数到达 MaxEntries 再追加报 ErrChainFull，被拒绝的请求不得入链。第二，序号从 1 连续递增，每条的 PrevHash 等于前一条的 Hash，第一条的前链摘要是 64 个 0；Hash 为 64 位小写十六进制，摘要覆盖序号、操作者、动作、载荷与前链摘要。第三，空链与完好链的 Verify 都必须返回 nil，不能因为链为空就报错。第四，快照是 JSON，顶层为 entries、anchors、alarms 三个数组，条目字段为 seq、actor、action、payload、prev_hash、hash；恢复只做结构校验（字段齐备、序号从 1 连续、prev 逐条衔接）与锚点对账，不重算摘要，结构不合法报 ErrBadSnapshot 且不动现网。第五，被改过内容但结构仍完整的快照恢复之后，Verify 必须在第一条对不上的位置报 ErrChainBroken，并往告警里写一条 broken_chain，写明该序号；同一序号反复校验只保留一条告警。第六，断链一旦被 Verify 认定，后续 Append 一律报 ErrChainBroken，直到从快照恢复回一条完好链。

锚点相关的口径：Rotate 把当前链尾钉成锚点，锚点的 Seq 与 Head 必须等于链尾，编号从 1 递增；空链轮换报 ErrNoEntries，链尾已经被钉过再轮换报 ErrAnchorCurrent；AnchorEvery 大于 0 时每满这么多条自动轮换一次。轮换不能把老段落变成不可验证：新锚点必须保留与上一个锚点的交叉签名，使轮换前的分段仍然可验。恢复时每个锚点声明的链头摘要都必须能在恢复后的链里对上，任何一处对不上就整份拒绝并报 ErrAnchorMismatch。

抹除口径：Redact 不改原条目，而是在链尾追加一条 action 为 redact 的墓碑，载荷里写明被抹除的序号与原因；序号不存在、原因为空、同一序号重复抹除都报 ErrBadRedaction。ClearAlarms 清空告警并返回清除条数，Alarms 返回副本。

硬约束两条。一是并发：Append、Rotate、Redact 必须串行结算，并发追加时链尾只能有一个后继，不能分叉，也不能丢条目。二是重启：快照要带上条目、锚点与告警，恢复后 Len、Head、Anchors 与 Verify 结果必须与快照前一致；损坏的载荷报 ErrBadSnapshot 且不能动到现网链。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-d-10`
- 实现文件：`audit_chain_guard.go`
- 测试目录：`tests/`，通过导入路径引用本模块
- 包名：`task`；主要类型：`AuditChainGuard`、`ChainConfig`、`Entry`、`Anchor`、`Alarm`
