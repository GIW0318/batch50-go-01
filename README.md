# 进程注入链路识别与阻断治理

## 题面

安全运营转来的工单：值班安全员发现同一条注入链被拆成了三条互不相关的告警——先是调试附加，几秒后远程写内存，再到远程建线程，每一条都只报"可疑"，没有任何一条被阻断；反过来，厂商自带的调试器每次附加都触发阻断，把正常的排障流程打断。更严重的一次，被阻断的目标进程早已退出，pid 被别的业务进程复用，旧阻断直接扣在新进程头上，业务侧报了故障。还有白名单过期后仍在生效、重启后阻断名单全丢的情况。

落点 gsb/batch50-go-safety-c-18 的 process_injection_sentinel.go，类型 InjectionSentinel。签名保持：NewInjectionSentinel(cfg Config) *InjectionSentinel、Ingest(ev Event) (Verdict, error)、Allow(rule AllowRule) (RuleID, error)、Rule(ruleID string) (AllowRule, bool)、Rules() []AllowRule、Suppress(ruleID string, ttl time.Duration) error、Release(pid int) error、Blocked() []int、Explain(pid int) (Trace, error)、Snapshot() ([]byte, error)、Restore([]byte) error。Config 的 MaxEventsPerTarget、MaxRules、MaxChainSteps、Window 分别约束单目标缓冲、规则表、链长与归并时间窗；Event 里的 TargetStart 标识进程实例。

要修的点：

1. Ingest 先校验 PID、TargetPID、TS 为正，Kind 属于 ptrace_attach、process_vm_write、remote_thread、exec 四类，否则报 ErrBadEvent，被拒事件不得留下任何状态。
2. 步骤权重 attach 30、remote write 40、remote thread 30、exec 10；同一目标在时间窗内的步骤按 TS 归并为一条链，同类步骤只计一次；总分达 30 告警、达 70 阻断。
3. 同一事件重复上报按 pid、目标、类型、时间戳幂等，Verdict 标 Duplicate 并沿用原判定，不重复入审计、不消耗序号。
4. 判定与阻断一次性提交：阻断落表后才返回 Verdict；已阻断目标的新事件一律回 prior_block，不再累加分数。
5. 每目标缓冲只保留最新 MaxEventsPerTarget 条事件，Explain 按 TS 升序返回事件与全部判定历史。
6. Allow 规则按父进程与签名匹配，命中返回 allow/allow_rule 且不计分；规则表超 MaxRules 报 ErrRuleLimit，字段缺失报 ErrBadRule，Rule 查不到时布尔为 false。
7. Suppress 只降级告警：窗口内命中规则的事件照常计分，但告警降为 allow 并标记 Suppressed；已经执行的阻断不因抑制而撤销；窗口非正报 ErrBadSuppression，未知规则报 ErrUnknownRule。
8. Release 撤销某个目标的阻断，目标本来没被阻断时报 ErrUnknownBlock。
9. 白名单按事件时间戳判定是否过期，过期后必须重新按链计分，不能继续沿用旧豁免。
10. 阻断要绑定进程实例标识，目标退出后 pid 被复用时新实例不得继承旧阻断，必须重新判定。
11. 同一注入链的步骤乱序到达时按目标与时间窗归并，不能把后续步骤当成独立攻击。
12. 快照要带规则表、抑制剩余窗口、阻断名单、每目标缓冲与序号；Restore 全成或全不成，损坏载荷报 ErrBadSnapshot 且不动现网；恢复后规则号继续递增，不得回退复用。

硬约束：

- 同一目标的事件处理串行，判定与阻断原子提交；8 个协程同时送链后续步骤只能产生一次阻断，其余走 prior_block。
- 单目标缓冲与规则表都有硬上限，超限按题面报错而不是静默丢弃。
- 白名单、抑制窗口与阻断名单跨重启保持，恢复过程任何一步失败都不得留下半套状态。

验收：go test ./... 15 条全过；不得改测试、跳过、按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-18`
- 实现文件：`process_injection_sentinel.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`InjectionSentinel`
