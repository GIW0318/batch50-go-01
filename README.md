# SCRAM 服务端握手的 nonce、通道绑定与迭代下限

## 题面

数据库代理换成 SCRAM 认证后，客户端连接成功率掉到六成：一半是驱动在完成消息里回的 nonce 和服务端拼出来的对不上，一半是开了 TLS 的客户端被要求重新握手；安全评审还指出，老账号的迭代次数只有五百多也能通过，爆破尝试在日志里连续出现却没有被拦。落点 gsb/batch50-go-safety-a-10 的 scram_server_guard.go，类型 ScramServerGuard，签名保持：NewScramServerGuard(tenantID string, store CredentialStore, p ScramPolicy) *ScramServerGuard、Begin(clientFirst []byte) (serverFirst []byte, stateID string, err error)、BeginFrom(source string, clientFirst []byte) (serverFirst []byte, stateID string, err error)、Continue(stateID string, clientFinal []byte) (Principal, error)、Abort(stateID string) error、SetPolicy(p ScramPolicy) error、Policy() ScramPolicy、Pending() int、Sweep() int、SetClock(func() int64)、SetChannelBinding(digest []byte)、Failures(username, source string) int、Snapshot() ([]byte, error)、Restore([]byte) error。

报文口径按 SCRAM-SHA-256：客户端首包是 GS2 头加 n=<账号>,r=<客户端 nonce>，GS2 头取 n,,、y,, 或 p=tls-unique,,，带授权身份时写成 n,a=<authzid>,,；服务端首包是 r=<客户端 nonce 拼服务端 nonce>,s=<标准 base64 盐>,i=<迭代次数>；客户端末包是 c=<标准 base64 的 GS2 头加绑定数据>,r=<合并 nonce>,p=<标准 base64 的 ClientProof>。AuthMessage 是客户端首包的 bare 部分、服务端首包、去掉 p= 的客户端末包三段用逗号连接；SaltedPassword 用 PBKDF2-HMAC-SHA256 迭代 i 次取 32 字节，ClientKey 是对它做 HMAC 的 Client Key，StoredKey 是 ClientKey 的 SHA-256，ServerKey 是 HMAC 的 Server Key，服务端用 StoredKey 验证明文证明，回包 v=<标准 base64 的服务端签名>。口令明文一律不落内存表。

要修的地方：首包缺 GS2 头、标志不在 n、y、p 之内、缺 n= 或 r=、nonce 短于 8 字节都报 ErrBadClientFirst 且不建状态；账号在存储里查不到报 ErrUnknownUser；存储里的迭代次数低于策略 MinIterations 报 ErrIterationsTooLow；在途握手超过 MaxInflight 报 ErrInflightLimit，Abort 释放一个并让 Abort 不存在或已结束的 stateID 报 ErrUnknownState，同一个 stateID 只能推进一次；末包的 r 必须与首包发出去的合并 nonce 完全一致，不一致报 ErrNonceMismatch 并丢弃状态；c 必须等于标准 base64 的 GS2 头拼当前连接绑定摘要，摘要不一致报 ErrChannelBindingMismatch；证明不对报 ErrAuthenticationFailed；成功时返回 Username、TenantID、AuthzID、Iterations 和以 v= 开头的 ServerFinal；authzid 非空时必须是同租户内已存在的账号，并原样写进 Principal；TTL 到期由 Sweep 清扫并返回条数，清扫后 Continue 报 ErrUnknownState；SetPolicy 不接受非正的 MinIterations，非法时报 ErrBadPolicy 且原策略不变；同账号同来源的失败累计到 MaxFailures 后 BeginFrom 报 ErrLocked，换来源不受影响，Failures 能查到分组计数；快照要带上策略、进行中的握手状态与失败计数，恢复后能继续推进，损坏的快照报 ErrBadSnapshot 且保留现网。

还有三条边界要点名：客户端声明支持通道绑定（y 标志）却没在末包里带绑定数据时必须拒绝，不能静默降级成无绑定；服务端 nonce 每次都要新生成，复用会让跨连接重放成立；authzid 指向不存在账号与口令错误的应答必须不可区分。

硬约束两条：每个握手状态都有 TTL 与并发上限，超限与超时都按拒绝或清扫处理，不能无限堆积；策略变更对进行中的握手按开始那一刻的快照生效，改策略不能把已开始的握手打断。

验收：go test ./... 15 条全过，不得改测试、跳过、按固定输入特判；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-a-10`
- 实现文件：`scram_server_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`ScramServerGuard`
