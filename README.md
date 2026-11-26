# BGP 前缀劫持监测

## 题面

上周三凌晨，边缘出口的劫持告警平台一夜推了两百多条，值班同学按流程封了三个上游邻居，天亮复盘发现大半是同一家 CDN 在两个 AS 里做的正常多源宣告，真正那条伪造源前缀反而停在待确认没人跟进；另有两条告警在点了撤销之后，同一前缀又被重新宣告，系统却沿用旧窗口往下跑。被误封的那三个小时里，两个省级出口的业务全部绕行，事后补了两份事故说明。这套判定现在要收口成可回归的实现，落点是模块 gsb/batch50-go-safety-b-10 的 bgp_hijack_watch.go，类型 BgpHijackWatch，公开接口保持不变：NewBgpHijackWatch(cfg WatchConfig) *BgpHijackWatch、SetROA(roa ROA) error、Announce(a Announcement) (Verdict, error)、Withdraw(peer, prefix string) error、Routes(prefix string) []Route、AlertState(prefix string) (Alert, bool)、Dismiss(prefix string) error、Sweep(now int64) []Alert、ROAs() []ROA、Stats() Stats、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的行为：一、前缀必须是合法 CIDR 且主机位为零，否则 ErrBadPrefix；邻居为空报 ErrBadPeer；两者都不建表。二、AS 路径不得为空，末段必须等于宣告 origin，否则 ErrBadPath。三、同一前缀的多个邻居分别登记，Routes 按邻居名升序返回；同一邻居重复宣告只更新末次时间，不新增行。四、每个前缀的邻居数受 MaxPeersPerPrefix 限制，超出报 ErrPeerLimit，已有邻居不受影响。五、RPKI 判定按 ROA 覆盖长度与 origin 匹配，过期 ROA 视为不存在，命中不了任何 ROA 记 roa_not_found。六、origin 与 ROA 不符记 origin_mismatch，路径长度超过 MaxASPathLen 或含私有 ASN 记 path_anomaly，信号按字母序去重后写进 Verdict 与 Alert。七、三类信号分值 40、30、10，总分到 60 判告警、到 30 判待确认，其余放行，单信号不得直接升级为告警。八、告警表受 MaxAlerts 硬顶，超限淘汰最旧一条并计入 Evicted，Seq 全局严格递增。九、Dismiss 撤销在线告警并计入 Dismissed，对不存在的告警幂等返回 nil；Sweep 把超过 HoldWindow 没有新宣告的告警关闭并返回。十、Withdraw 对未知前缀幂等返回 nil，撤空后再宣告且间隔小于 FlapWindow 计一次 Flap。十一、快照覆盖 ROA、路由、告警与 Seq；数据损坏报 ErrBadSnapshot 且不动现有状态；用 Seq 落后于现存状态的快照恢复报 ErrStaleSnapshot。

最容易漏的三处：同一条前缀上两个 origin 各自都有 ROA 覆盖时属于合法多源宣告，不得报成冲突；RPKI 校验失败本身只能降级为待确认，不能单独当成劫持；告警撤销后同一前缀再次出现必须从新窗口重新计时，不得沿用旧窗口。

硬约束：Announce、Withdraw、Sweep、Dismiss 共用同一份状态，并发调用只能看到调用前或调用后的完整状态，Seq 不得重复或回退，任何时刻不允许出现前缀已有告警但告警表查不到记录的中间态；邻居表、告警表与 ROA 表都受配置上限约束，淘汰只按最旧，不在请求里扩容。

验收：go test ./... 15 条全过，不得改测试、跳过或按固定输入写死；并发与快照恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-10`
- 实现文件：`bgp_hijack_watch.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`BgpHijackWatch`
