# gsb/batch50-go-safety-c-21

## 题面

链接巡检平台上线后，安全运营复核一份外链抓取任务时发现：用户填的收藏夹地址被服务端抓取，实际请求却落在内网管理端口；另一次请求经第三方短链跳转，第二跳打到本机回环地址。值班同学想查这次探测走了几跳、每跳地址属于什么网段、判定依据是什么，平台只回“已拦截”，其余全部丢失。

需要把能力补全到 gsb/batch50-go-safety-c-21 的 ssrf_probe_orchestrator.go，核心类型 SsrfProbeOrchestrator，入口 NewSsrfProbeOrchestrator(Options) *SsrfProbeOrchestrator。公开接口：Submit(Target) (string, error)；Next(string) (Step, error)；Report(string, Observation) error；Verdict(string) (Verdict, error)；Cancel(string) error；Probe(string) (ProbeState, bool)；Pending() []string；ClassifyAddress(string) string；Snapshot() ([]byte, error)；Restore([]byte) error。

探测生命周期：Submit 登记并排队，Pending 按标识排序给出仍占名额的探测；Next 按链序发步骤，首跳 Kind 为 primary、Hop 从 1 开始，URL 由 scheme、host、非默认端口与参数名拼成（http 80 与 https 443 不写端口）；Report 提交观测，301、302、303、307、308 且带 Location 时生成下一跳，Kind 为 redirect、Hop 加一、URL 取 Location 原值；其余状态码为终态，Verdict 置 Done、原因 reason_completed。Step 的 Requests 是已派发步骤数，HopsLeft 是还允许派发的跳数（含当前跳），MillisLeft 是总预算减已累计耗时。

需要补齐的行为：

1. scheme 只收 http 与 https；host 或参数为空（含纯空白）返回 ErrBlankTarget，非法 scheme 返回 ErrBadScheme，均不占名额。
2. 并发探测数超过 MaxConcurrentProbes 返回 ErrProbeLimit，取消立即释放名额。
3. 派发前按该跳主机解析出的每个地址判族，命中禁止族返回 ErrBlockedTarget，状态记 blocked，Verdict 的 Blocked 为真、Hop 为当跳、Reason 用对应族原因码，Evidence 至少含命中地址与族名。
4. 地址族：loopback（127/8、::1）对应 reason_blocked_loopback；private（10/8、172.16/12、192.168/16、fc00::/7）对应 reason_blocked_private；link_local（169.254/16、fe80::/10）、multicast（224/4、ff00::/8）、unspecified（0.0.0.0、::）各有原因码；无法解析归 invalid。ClassifyAddress 返回 public、loopback、private、link_local、multicast、unspecified、invalid。
5. 重定向目标必须是带 scheme 与主机的 http 或 https 绝对地址，否则 Report 返回 ErrBadRedirect，探测不可继续。
6. 未 Report 就再 Next 返回 ErrStepPending，不得跳步或并发取两步；Report 未知步骤返回 ErrUnknownStep，重复提交返回 ErrStepClosed。
7. 跳数预算按 MaxHops，超出不派发并返回 ErrBudgetExhausted，原因 reason_hop_budget，状态记 exhausted。
8. TotalMillis 累计每步 ElapsedMillis，达到上限后不派发下一跳，原因 reason_time_budget；请求条数上限 MaxRequestsPerProbe 以 reason_request_budget 收口。
9. 每个探测最多一个在途步骤，状态推进与预算扣减原子完成，两个调用方不得拿到同一步。
10. Snapshot 写出未完成探测的链位置、已消耗预算与已完成判定；Restore 到新实例后从原位置续跑，跳号与剩余额度不变，已判定探测保持原结论。
11. Restore 整体成功或整体失败：载荷损坏返回 ErrBadSnapshot，且不改动现有探测、在途步骤与预算。
12. Cancel 幂等；取消后 Next 返回 ErrProbeCancelled，Verdict 的 Cancelled 为真、原因 reason_cancelled。

现场还有三类易漏情况：重定向到同一主机的另一端口时，要按该跳实际解析结果判定，不能沿用首跳结论；两次解析之间地址变化时，要固定本次结果或二次复核，不能只看第一次；云元数据地址在部分网络被 NAT 映射成普通内网地址，只按字面匹配会漏判，需按解析结果归纳族。

硬约束：并发调用下 Submit、Next、Report、Cancel 不得重复发步、重复计数或让名额漂移；快照必须自包含且可重复恢复，恢复后的判定与续跑结果要和未重启时一致；跳数、请求数与总时长三项预算同时生效，任何一项耗尽都要留下原因码而不是静默截断。

验收口径：go test ./... 的 15 条用例必须全过；不得修改测试、跳过用例或按固定输入写死结果；并发与重启行为自行补充验证；只做本地实现，不接真实网络。交付改好的源码、测试输出与故障说明。

## 运行

```bash
go test ./...
```

## 模块

- 模块路径：`gsb/batch50-go-safety-c-21`
- 包名：`task`
- 实现文件：`ssrf_probe_orchestrator.go`
- 测试文件：`tests/test_ssrf_probe_orchestrator_test.go`
