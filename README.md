# ptrace 附加授权：Yama 范围、父子关系与一次性票据

## 题面

内部调试平台上线 ptrace 附加入口后连续出了三件事：一个普通用户拉起自己的调试器附加到了别人的生产进程，跟踪读到了不该看到的密钥；一次排查里同一张附加票据被脚本重试用了两次，重启后还继续跟踪着已经换过镜像的进程；值班同学在 Yama 收紧到只信父进程之后仍然被放行。受理窗口只对内部平台开放，票据由平台签发、节点上的守卫校验，任何一环放宽都等于把 root 级的跟踪能力发给普通账号。落点 gsb/batch50-go-safety-d-05 的 ptrace_attach_guard.go，类型 PtraceAttachGuard。

公开签名保持不变：NewPtraceAttachGuard(ScopePolicy) *PtraceAttachGuard；SetPolicy(ScopePolicy) error；Track(ProcessInfo) error；Process(pid int) (ProcessInfo, bool)；ScopeVerdict(tracerPID, traceePID int) (string, error)；RequestTicket(tracerPID, traceePID int, at int64) (Ticket, error)；Attach(AttachRequest) (AttachResult, error)；Detach(tracerPID, traceePID int) error；MarkExec(pid int, newImage string, at int64) error；Ticket(id string) (Ticket, bool)；Tickets() []Ticket；TracerOf(traceePID int) (int, bool)；Stats() GuardStats；Snapshot() ([]byte, error)；Restore([]byte) error。

调度口径：票据签发与判定统一按毫秒时间戳，IssuedAt 与 ExpiresAt 要能复盘是谁在什么时候申请了对谁的跟踪；Tickets 按票据 ID 升序返回；TracerOf 只回答当前是否在跟踪，不返回历史；Stats 里的 Issued、Denied、Attached、Detached 要与审计流水逐条对上，attach 被拒计入 Denied，没有签发的申请不计入 Issued。Track 对同一进程号是刷新已有条目，不新增记录；Detach 只解除这一对跟踪关系，不回退票据状态。

要处理的故障与边界：一、scope 不在 0 到 3 之间报 ErrBadScope；二、进程号非正或镜像为空报 ErrBadProcess，跟踪表超过 MaxTracked 报 ErrTrackedLimit；三、tracer 与 tracee 同号记 self_attach 并拒绝，任何 scope 下都不放行；四、scope 0 且 AllowSameUID 为真时同 UID 可附加，UID 不同记 uid_mismatch；五、scope 1 只允许直接父进程，其余记 not_parent；六、scope 2 只允许 UID 为 0 的 tracer，否则记 not_root；七、scope 3 一律记 scope_denied；八、未登记的进程一律报 ErrUnknownProcess；九、同一个 tracee 已有活跃票据时再申请报 ErrAlreadyTraced，一个被附加进程只能有一个 tracer；十、活跃票据数达到 MaxTickets 报 ErrTicketLimit；十一、票据只能用于一次附加，附加失败也不得复用到下一次尝试；十二、被附加进程 exec 之后原票据立即失效，不能拿旧票据继续跟踪；附加模式只认 attach 与 seize，其余报 ErrBadMode，未知票据报 ErrUnknownTicket，票据归属与请求方不一致记 ticket_owner_mismatch。

第十一条与第十二条是这次复盘点名的口径，别按票据可重试或只比对票据未过期来实现。

硬约束：多个调度器会并发为同一批目标申请票据，同一 tracee 只能有一个 tracer 胜出，活跃票据数不得超过 MaxTickets，计数不得丢失；附加成功与 Detach 并发时，TracerOf 只能看到已跟踪或未跟踪两种稳定结果。快照要带出跟踪表、票据水位、票据状态与计数，Restore 之后过期时刻仍按绝对时间判定、不重新计时，损坏载荷报 ErrBadSnapshot 且现网票据不动。票据与跟踪关系都有硬顶，不以淘汰他人方式腾位。

验收：go test ./... 的 15 条用例必须全过，不许改测试、不许跳过、不许按固定输入写死；并发申请与重启自行写程序验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-d-05`
- 实现文件：`ptrace_attach_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`PtraceAttachGuard`
