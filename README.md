# DNS 隧道外泄检测

## 题面

安全运营在月报里发现某台内网主机一天发了四万多条 TXT 查询，后缀看着像公司自建域，值班同学把整个域加进白名单之后，同一台主机换个前缀继续往外吐数据，告警再没响过；另一头，一条只有九个字符、熵值很高的短查询被单独判成隧道，业务侧的健康检查被误封了半天。事后翻流量才发现，那条隧道把文件切成小段塞进查询名，TXT 只是回执，白名单域名恰好是它借用的跳板。风控同学复盘时说，一条查询像不像外泄，本来要看名字形态、负载大小和频次三样，凑齐两样才敢升级，凑齐三样才敢拦。现在要把查询侧的判定收口，落点是模块 gsb/batch50-go-safety-b-11 的 dns_tunnel_inspector.go，类型 DnsTunnelInspector，公开接口保持不变：NewDnsTunnelInspector(cfg InspectorConfig) *DnsTunnelInspector、AllowName(name string) error、Observe(q Query) (Verdict, error)、Whitelisted(name string) bool、ClientState(clientID string) (ClientVerdict, bool)、Flush(clientID string) (ClientVerdict, error)、Sweep(now int64) []ClientVerdict、Explain(q Query) []string、Stats() Stats、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的行为：一、客户端 ID 或查询名为空、名字含空标签或标签超过 63 字符，报 ErrBadQuery。二、只接受 A、AAAA、TXT、CNAME、MX 五种类型，其余报 ErrBadQType。三、熵按最左标签计算，超过 EntropyThreshold 记 high_entropy。四、TXT 负载超过 MaxTXTLength 或整名超过 MaxNameLength 记 long_payload。五、同一客户端在 Window 内查询数超过 MaxQueriesPerWindow 记 high_rate，越过阈值后继续累计，直到旧查询滑出窗口。六、三档分值 35、35、30，满 80 判 block，满 40 判 observe，其余 allow，信号按字母序去重后写进结论。七、白名单按整名登记，只放行完全一致的 FQDN，命中查询不进窗口也不计分；空白、含空标签或以点开头的条目报 ErrBadWhitelist。八、客户端窗口按 At 累计，FirstSeen 与 LastSeen 如实记录，TXTBytes 累加，同一条查询重复观察不得重复计数。九、客户端表受 MaxClients 硬顶，新客户端超限报 ErrClientLimit，已跟踪客户端照常判定。十、Sweep 清理超过 Window 没有新查询的客户端并返回最终结论，Flush 单独取走一个客户端。十一、快照覆盖白名单、客户端窗口、统计与序号，恢复后序号必须接在现存状态之后；数据损坏报 ErrBadSnapshot 且不动现有状态。

最容易漏的两处：白名单条目的子域不得整域豁免，登记 corp.example 不能让 a1b2.corp.example 一起放行；高熵但很短的查询不能单独判成隧道，必须结合负载与频率一起看。

硬约束：Observe、AllowName、Flush、Sweep 共用同一份状态，并发调用下计数与序号不得丢也不得重，结论只允许反映调用前或调用后的完整窗口；客户端表与每客户端保留的查询时间戳都受上限约束，超限只淘汰最旧，不在请求里扩容。

验收：go test ./... 15 条全过，不得改测试、跳过或按固定输入写死；并发查询与快照恢复自行验证；只做本地实现，不发起真实解析也不连外部服务。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-11`
- 实现文件：`dns_tunnel_inspector.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`DnsTunnelInspector`
