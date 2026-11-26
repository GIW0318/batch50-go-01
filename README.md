# BFD 会话检测协商与抖动抑制

## 题面

城域网两条隧道的链路检测同时显红，运维在两端抓包发现探测报文一直在收，状态却在 Down 与 Up 之间来回翻；更麻烦的是告警平台收到十几条重复中断，而真正断掉的那条只报了一次。还有一处对不上：对端把检测倍数调小以后，本端直接接受了，检测时间短到毫秒级，链路上一点抖动就整条隧道重建。上个月轮换过一次认证密钥，新密钥下发后旧密钥立刻失效，两侧还没对齐就中断了近十分钟，事后翻日志也说不清是谁把状态改下去的。落点在模块 gsb/batch50-go-safety-b-02 的 bfd_session_guard.go，类型 BfdSessionGuard，签名保持：NewBfdSessionGuard(cfg GuardConfig) *BfdSessionGuard、OpenSession(peerID string, cfg SessionConfig) (SessionID, error)、OnPacket(sessionID string, pkt ControlPacket) (Transition, error)、SetAuthChain(peerID string, chain []AuthKey) error、Tick(now time.Time) []Transition、Session(sessionID string) (Session, error)、NegotiatedDetectMicros(sessionID string) (int, error)、FlapCount(peerID string) int、Events() []Transition、Snapshot() ([]byte, error)、Restore(data []byte) error 与 CloseSession(sessionID string) error。

需要落地的判定：

1. 新会话从 Down 起步；peer 为空、发送或接收间隔为 0、检测倍数为 0 都报 ErrBadConfig；单 peer 会话数超上限报 ErrPeerSessionCap。
2. 检测时间取本端期望发送间隔与对端要求接收间隔的较大者再乘检测倍数，NegotiatedDetectMicros 返回微秒值。
3. 检测倍数低于策略下限时协商不成立，会话停在 Down，不能按对端要求放行。
4. 状态只允许 Down 到 Init、Init 到 Down 或 Up、Up 到 Down、任意状态到 AdminDown 这些迁移，其余报文只刷新存活时间。
5. 认证按 key ID 查链，key 缺失、摘要不符、超出有效窗口都报 ErrAuthKey，会话状态不变。
6. 认证失败的报文不刷新存活计时，也不计入丢包统计。
7. 只有累计到检测时间没有合法报文才判 Down，收到合法报文清零。
8. 从 Down 恢复必须经过 Init，单个报文不能直接跳到 Up。
9. 管理性关闭不产生抖动告警，恢复时也不走正常 Up 的告警链路。
10. 同一 peer 的翻转在抑制窗口内合并成一次抖动记录并保留原因，FlapCount 只加一次。
11. 密钥链允许两代并存，旧代在新代生效窗口内仍可校验，链长与重复 key ID 分别报 ErrChainTooLong、ErrBadChain。
12. 事件日志按 MaxEvents 只留最新若干条，超出淘汰最旧。

现场还要求每次状态变化都能复盘：迁移记录要带会话号、对端、前后状态、原因与发生时刻，重启后还能按原样读出来，值班同学据此判断是链路真的断了还是参数被人改坏。

硬约束：报文处理与定时器推进必须串行化到每会话单写者，状态迁移原子，同一时刻多路 Tick 不能重复判定同一次超时；快照恢复后会话状态、抖动计数与事件序列与快照一致，重启后已判定的超时不能被再报一次。

验收：go test ./... 的 15 条用例必须全过，不得修改测试、跳过或按固定输入写死；并发报文与定时器推进自行验证；只做本地实现，交付源码与测试记录。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-02`
- 实现文件：`bfd_session_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`BfdSessionGuard`
