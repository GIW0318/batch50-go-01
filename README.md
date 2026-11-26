# 跨设备认证中继的双通道绑定与一次性会话

## 题面

门店扫码登录在高峰期出乱子：桌面端生成挑战二维码，手机认证器扫完却提示通道已被占用；重试两次后两边断言各自有效，账号被登进两台设备；二维码被截图转发后，外部的人凭图片就能附加；已取消的中继过一会儿又回了一份断言；手机切网重连后，旧通道凭据还能继续转发。落点 gsb/batch50-go-safety-a-11 的 hybrid_transport_relay.go，类型 HybridTransportRelay，请把中继会话、双通道绑定、一次性凭据与重启恢复补齐。

接口保持如下签名：NewHybridTransportRelay(limits Limits) *HybridTransportRelay、Start(desktopID string, opts RelayOpts) (Relay, error)、Attach(relayID string, phoneID string, proof Proof) (Channel, error)、Forward(channelID string, challenge []byte) ([]byte, error)、Complete(relayID string, assertion []byte) (Result, error)、Cancel(relayID string, by string) error、Rebind(channelID string, proof Proof) (Channel, error)、RelayState(relayID string) string、ChannelInfo(channelID string) (Channel, bool)、Pending(desktopID string) int、Audit(relayID string) []Event、Sweep() int、Snapshot() ([]byte, error)、Restore([]byte) error。

需要落实的行为边界：

1. 中继标识形如 relay-1 并按建立顺序递增，配对令牌为 pair-<中继标识>；Start 记一条 started 审计并把序号记为 1，状态每迁移一次序号加一（附加 2、首转 3、完成 4），未知中继的 RelayState 返回空串。
2. TTL 未指定时取 Limits 默认值，超过上限按上限截断；字节上限同样截断并记录生效值，传入负值报 ErrBadLimit 且不建立中继。
3. 单桌面端未结束的中继数超过 MaxPendingPerDesktop 报 ErrRelayLimit；完成、取消、过期都不再计数。
4. 附加必须核对配对令牌，不匹配报 ErrAttachDenied 且保持原状态、不写审计；同一中继的第二次附加拒绝，通道建立后两端身份不可更换。
5. 通道标识形如 chan-1，状态为 open，一次性凭据形如 cred-<通道标识>；通道附加后中继才可转发，未知通道报 ErrChannelNotFound。
6. Forward 的回执为 ASCII 文本 channel=<通道标识>;seq=<n>;，n 从 1 开始按通道递增，被拒绝的调用不消耗序号，通道上记录已转发次数并写一条 forwarded 审计。
7. 挑战为空报 ErrEmptyChallenge，超过中继字节预算报 ErrChallengeTooLarge，两种情况都不改状态、不写审计。
8. Complete 只接受已转发过的中继，断言为空报 ErrEmptyAssertion；成功时 Result 状态为 completed、字节数取断言长度、完成时刻取当前时间，摘要为十六进制小写的 sha256(中继标识 + ":" + 断言原文) 并写进审计，原文不落库。
9. Cancel 的 by 只认 desktop 与 phone，其它值报 ErrBadActor 且不改状态；成功记一条 canceled 审计，原因为 reason_canceled_by_<by>，再次取消报 ErrRelayClosed。
10. Sweep 把到期中继转为 expired，记一条 expired 审计并带 reason_relay_timeout，返回条数；到期按不早于 ExpiresAt 判定，已过期的中继再附加报 ErrRelayExpired。
11. 快照要带上中继与通道表、状态、序号计数器和审计，恢复后序号继续递增；恢复失败报 ErrBadSnapshot 且不留半个状态。

另有三条边界要点名：二维码被截图后由第三方抢先附加时，不能仅凭二维码内容就通过，附加要绑定近场通道里拿到的对端证明；任一端取消与另一端提交断言可能同时到达，完成与取消按到达先后定序，只有一个成为最终结果，另一个报 ErrRelayClosed；手机转发中途切换网络需要重建通道时，Rebind 必须换发新的一次性凭据，旧凭据与旧通道序号都不得沿用。

两类硬约束：同一中继的附加、转发、完成、取消串行执行，并发到达时内部表不出现半更新；重启后未完成的中继按快照原样恢复，已完成的断言只保留审计摘要，恢复后不重新执行。

验收：在题目目录执行 go test ./...，十五条用例必须全部通过；不得修改测试、跳过用例或按固定输入写死结果；并发与重启恢复自行构造场景验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-a-11`
- 实现文件：`hybrid_transport_relay.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`HybridTransportRelay`

## 验收

15 条测试函数全部通过，无跳过、无按固定输入写死；并发与重启恢复自行验证。
