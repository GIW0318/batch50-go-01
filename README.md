# 认证会话与 TLS 通道绑定的导出值校验

## 题面

网关切到 TLS 终止代理之后，认证会话成片报绑定不一致：直连客户端提交的导出值和后端算出来的对不上，走代理的干脆被降级成不绑定；同一会话在日志里一会儿已校验、一会儿已过期；运维重启一次，旧连接还在握手，新进程却认为会话早就校验过了。落点 gsb/batch50-go-safety-a-13 的 channel_binding_verifier.go，类型 ChannelBindingVerifier，请把会话登记、导出值采集、绑定校验、代理来源与重启失效这条链路补成可用状态。

接口保持如下签名：NewChannelBindingVerifier(limits Limits) *ChannelBindingVerifier、Open(sessionID string, principal string, channel ChannelRef) (Binding, error)、Bind(sessionID string, exporter []byte) (Binding, error)、Verify(sessionID string, proof []byte) (Verdict, error)、Downgrade(sessionID string, reason string) error、Reverify(sessionID string) error、Expire(sessionID string) error、Explain(sessionID string) (Trace, error)、State(sessionID string) string、BindingOf(sessionID string) (Binding, bool)、Pending() int、Snapshot() ([]byte, error)、Restore([]byte) error。

需要落实的行为与边界：

1. Open 登记会话，会话标识、主体与通道标识都不能为空，来源只认 direct 与 proxy，否则分别报 ErrEmptyIdentity 与 ErrBadSource；同一会话重复登记报 ErrSessionExists。
2. 在途绑定数超过 MaxInflight 报 ErrInflightLimit；未绑定、已绑定、已校验的会话都算在途，过期与降级的会话不再计数。
3. 导出值只在通道建立时采集一次，Bind 保存十六进制小写的 sha256；空值报 ErrBadExporter，超过 MaxExporterBytes 报 ErrExporterTooLarge，二次绑定报 ErrAlreadyBound 且不改原摘要。
4. 绑定有效期从 Bind 时刻起算，Binding 上记录 BoundAt 与 ExpiresAt；到期后 Verify 与 Bind 都报 ErrBindingExpired。
5. Verify 对提交值重新求摘要比对，不一致报 ErrBindingMismatch 并累加 Mismatches，会话仍可重试；一致时状态转 verified、累加 Verifies，回执带当前摘要、reason_match 与当前时刻，原摘要不变。
6. 会话状态只有 unbound、bound、verified、downgraded、expired 五种；未知会话报 ErrSessionNotFound，State 返回空串，Explain 同样报 ErrSessionNotFound。
7. Downgrade 用于导出值不可得的情形，原因不能为空否则 ErrBadReason，重复降级报 ErrAlreadyDowngraded，降级后 Verify 报 ErrNotBound。
8. Expire 主动结束绑定，重复调用报 ErrAlreadyExpired；已过期会话再 Bind 报 ErrBindingExpired，Pending 不再计数。
9. 每次成功的 open、bind、verify、downgrade、reverify、expire 按发生顺序记进轨迹，Explain 原样返回；会话序号从 Open 的 1 开始，每成功一步加一。
10. 快照保留会话标识、主体、通道来源、轨迹与校验计数，但恢复后绑定一律作废为 unbound、摘要与绑定时刻清空，必须用新通道重新 Bind；恢复失败报 ErrBadSnapshot 且不留半个状态。
11. 通道来源为 proxy 时，转发头里的代理身份必须命中 TrustedProxies，否则报 ErrUntrustedProxy，不能静默降级为不绑定。

另有三条边界要点名：反向代理终止 TLS 时后端拿不到导出值，要识别代理提供的可信转发头并据此建立绑定，而不是直接把会话降级成不绑定；通道重协商或密钥更新后导出值会变化，已绑定会话要按策略重新校验而不是立即失效；客户端复用同一通道发起多个会话时，各会话的绑定要独立记录，不能共用一个标记。

两类硬约束：同一会话的绑定、校验、降级、过期串行执行，导出值只采集一次，并发下不会出现两次绑定或半条轨迹；会话表、计数与轨迹的读取要原子，Explain 看不到半更新；重启后绑定一律失效，快照只能恢复会话与计数，旧摘要不得复活。

验收：在题目目录执行 go test ./...，十五条用例必须全部通过；不得修改测试、跳过用例或按固定输入写死结果；并发与重启恢复自行构造场景验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-a-13`
- 实现文件：`channel_binding_verifier.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`ChannelBindingVerifier`

## 验收

15 条测试函数全部通过，无跳过、无按固定输入写死；并发与重启恢复自行验证。
