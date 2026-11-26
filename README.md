# 登录排队放行：分池位置推进、票据 TTL 重连与优先通道防饿死

## 题面

新赛季开服当晚排队系统被骂上热搜：明明前面只剩几十个人，位置十分钟不动；玩家断线重连一次，回来直接排到队尾；客服白名单账号一连放行几十个，普通队列整段卡死；维护把三个服缩成一个服之后，已经拿到放行凭证的玩家仍然进得去，实际在线人数超过容量两成。运维把当晚的排队记录拉出来，发现分池、心跳、放行、回收四条链路各写各的，票据没有唯一身份，容量下降也没有任何回撤。

落点是模块 gsb/batch50-go-game-a-09 的 login_admission_queue.go，类型 LoginAdmissionQueue，公开接口保持不变：NewLoginAdmissionQueue(cfg Config) *LoginAdmissionQueue、SetCapacity(region string, capacity int) error、MarkPriority(accountID string) error、Enqueue(accountID, region string, at time.Time) (Ticket, error)、Heartbeat(ticketID string, at time.Time) (Position, error)、Admit(now time.Time) ([]Grant, error)、Redeem(ticketID, nonce string, at time.Time) error、Leave(ticketID string) error、Status(ticketID string) (TicketState, error)、FindTicket(accountID, region string) (Ticket, bool)、Grants(region string) []Grant、EvictExpired(now time.Time) (int, error)、Occupied(region string) int、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收掉的地方如下。第一，账号或区服为空报 ErrBadArgument，区服没有配过容量报 ErrUnknownRegion，容量为负报 ErrInvalidCapacity，容量可以调到零。第二，同一区服内的票据序号从 1 开始严格递增，Status 给出的排队位置等于同池内序号更小的等待票据条数。第三，Admit 按区服名升序逐池放行，池内按顺序取票，单批放行条数不超过 BatchSize，已发出但未兑换的凭证要占住座位。第四，座位已满或凭证已占满时该池不再放行，Admit 返回空列表而不是报错。第五，凭证一次性使用，Redeem 成功后票据变 admitted 并占用一个座位，同一张凭证第二次兑换报 ErrGrantUsed；Grants 返回该区服尚未兑换且未过期的凭证。第六，凭证号对不上报 ErrGrantNotFound，票据号不存在报 ErrTicketNotFound。第七，排队中放弃后票据置 left，重复放弃报 ErrTicketClosed。第八，票据带 TTL，心跳缺失超过 TicketTTL 视为过期，过期的 Heartbeat 报 ErrTicketExpired 并把票据置 expired。第九，EvictExpired 一次清掉所有过期的等待票据并返回条数，重复调用第二次返回零。第十，快照要带上各池容量、票据、凭证与优先名单，恢复后 Status、Grants、Occupied 与关服前一致；损坏载荷报 ErrBadSnapshot 且不动现有数据。第十一，优先通道只认 MarkPriority 登记过的账号，未登记的账号不得插队。第十二，放行凭证带过期时刻，凭证过期后兑换报 ErrGrantExpired。

还有三处必须一起收掉。排队中的玩家断线重连时，必须在 TTL 内凭账号找回原票据与原位置，不能重新排到队尾。优先通道连续放行时普通队列的前进速率要有下限，久未前进的票据要被临时提升，不能出现白名单长期占满座位。区服容量下降时只撤回还没兑换的凭证，已经进游戏的玩家不受影响。

硬约束有两类。一是原子性与顺序，Enqueue、Admit、Redeem、Leave 会被多路登录网关同时调用，同一张凭证只能被兑换一次，同一张票在同一时刻只能有一个终态，-race 下不得有数据竞争。二是重启与上限，快照必须原子替换全部状态；BatchSize、PriorityQuota 与各池容量都是硬顶，超限放行要分批而不是一次发完，票据与凭证的保留量不随在线时长无限增长。

验收：`go test ./...` 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；掉线重连、优先通道与缩容自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-09`
- 实现文件：`login_admission_queue.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`LoginAdmissionQueue`
