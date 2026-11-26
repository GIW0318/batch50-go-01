# NAT64 会话映射与 DNS64 合成联动

## 题面

NAT64 网关上线三天，运维连续报来三类现象：同一内部主机的短连接偶尔拿到别人的外部端口，回包被投递到另一个租户；ICMP 端口不可达返回后，内网主机收不到改写过的差错报文；监控里 DNS64 合成记录数量对不上，权威区里还出现了本不该存在的合成 AAAA。回滚旧版本后现象消失，可新版本还压着两个租户的存量连接，谁也说不清端口到底分给了谁。请落在 gsb/batch50-go-safety-b-06 的 nat64_session_table.go，类型 Nat64SessionTable，签名保持 NewNat64SessionTable(Config) *Nat64SessionTable、RegisterTenant(tenantID, internalPrefix string) error、Reserve(tenantID string, n int) (Block, error)、Translate(src, dst Endpoint, proto uint8) (Mapped, error)、Release(mapped Mapped) error、Lookup(external Endpoint, proto uint8) (Mapped, bool)、Sweep(now time.Time) int、SynthAAAA(name string, aRecords, realAAAA []string) (DNSAnswer, error)、ReverseICMPError(external Endpoint, proto uint8) (Mapped, error)、PortStats(tenantID string) PortStats、Snapshot() ([]byte, error)、Restore([]byte) error。

要修的行为：
一、租户登记：内部前缀必须是合法 IPv6 CIDR，空租户号或 IPv4 前缀报 ErrBadTenant；同一租户换前缀报 ErrTenantExists；源地址不落在任何已登记前缀时报 ErrUnknownTenant。
二、块预留：n 必须落在 1 到配置块大小之间，否则 ErrBadBlockSize；每租户块数超限报 ErrBlockLimit；块端口固定从 1024 起，第二块换用池内下一个地址；块的 PortStart 与 PortEnd 必须与请求端口数一致。
三、地址校验：目的地址必须落在配置的 /96 前缀内并取出内嵌 IPv4，裸 IPv4 或其他 IPv6 报 ErrOutsidePrefix，非法地址报 ErrBadEndpoint。
四、会话复用：同一内部端点、目的端点与协议重复翻译时复用同一外部端点并累加引用计数，不同目的端口各自成会话。
五、端口耗尽：块内端口用完或池内地址用尽时报 ErrPortExhausted，不得挪用其他租户的块。
六、释放：每调用一次减一个引用，归零后 Lookup 立即查不到，再释放报 ErrUnknownMapping。
七、老化：TCP、UDP、ICMP 分别按各自空闲超时判定，Sweep 返回本次过期条数；端口过期后还要压住回收延迟才可再分配，压住期间新流报 ErrPortExhausted。
八、容量：会话总数超限报 ErrSessionLimit，失败不得留下半条会话。
九、DNS64：有真实 AAAA 时原样返回并置 Synth 为假；只有 A 记录时按前缀嵌入（IPv4 放在 /96 前缀的后 32 位）合成并置 Synth 为真；两者都没有报 ErrNoData；A 记录非法报 ErrBadEndpoint；有真实 AAAA 时不得再合成，避免同一名字出现两条解析路径。
十、快照：Snapshot 覆盖租户、块、会话与计数器，Restore 全量替换，损坏数据报 ErrBadSnapshot 且不得动现网；快照要带上回收中的端口状态，恢复后不能把还在压着的端口立刻发出去。

最容易漏的三处：ICMP 差错报文要反查已有会话并改写回内部地址，不能当成新流去建映射；端口块耗尽时排队等待与直接新建失败要分开计数，PortStats 的两个计数不能混用；DNS64 合成记录不得写进权威区，也不能返回给不支持该前缀的客户端。

硬约束：翻译与端口预留必须原子完成，并发下不得出现半分配、跨租户错配或同一外部端口发给两个流；快照与在线变更互斥，恢复后的现网状态只能是全量旧或全量新。块数、会话数、回收延迟与合成前缀都受配置上限约束。

验收：go test ./... 15 条全过，不得改测试、跳过或用固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-06`
- 实现文件：`nat64_session_table.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`Nat64SessionTable`
