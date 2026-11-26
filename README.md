# 网关 Digest 质询的 nonce 代际与 nc 水位治理

## 题面

网关的 Digest 认证这两天把正常用户挡在门外：同一台手机切到移动网络后，第一笔请求就被判成重放，客服让用户重新登录才能恢复；安全组同时发现有人拿同一段抓包反复回放，服务端却每次都放行。翻日志能看到同一个 nonce 在两台设备上交替出现，nc 都从 1 开始，服务端把其中一路当成重复计数；另一头抓包重放的请求带着早就该作废的 nonce，照样拿到 200。落点是模块 gsb/batch50-go-safety-a-02 的 digest_auth_challenge.go，类型 DigestChallengeGuard，公开接口保持：NewDigestChallengeGuard(cfg Config) *DigestChallengeGuard、SetPassword(username, password string) error、Challenge(realm, username string) (Header, error)、Verify(req AuthRequest) (Principal, error)、RotateSecret() error、Usage(username string) NonceUsage、ValidateAlgorithm(algorithm string) error、NonceAge(nonce string) (time.Duration, error)、Sweep(now time.Time) int、ActiveNonces(username string) int、Snapshot() ([]byte, error)、Restore(data []byte) error。

摘要口径固定在代码注释之外先说清楚：HA1 是 sha256 十六进制(username:realm:password)，HA2 是 sha256 十六进制(method:uri)，response 是 sha256 十六进制(HA1:nonce:8 位小写十六进制 nc:cnonce:qop:HA2)，所有十六进制一律小写。要修的行为如下。realm 必须等于配置里的 realm，用户名不能是空白，否则报 ErrBadChallenge。nonce 由时间窗、服务端密钥与客户端指纹派生并登记，换密钥后旧代际的 nonce 立即作废。nc 必须严格大于该 nonce 已记录的水位，等于或小于都报 ErrReplay。qop 取 auth 或 auth-int，auth-int 必须连带把实体哈希算进 HA2，其余 qop 报 ErrQopRejected。算法只接受 SHA-256 与 SHA-512-256，收到 MD5、none 或其它值报 ErrAlgorithmRejected。用户名不存在与口令不对返回同一个 ErrAuthFailed，失败既不推进水位也不占用 nonce。nonce 超过时限报 ErrStaleNonce。Usage 要能读出签发数、活跃数、重放次数与最大水位。Sweep 回收过期 nonce 并返回条数。Header 里的 opaque 与算法、qop 要能直接拼进 WWW-Authenticate，每次 Challenge 各自登记一条独立记录，realm 不符时除了报错不能留下半条登记。

还有三条边界要一起收掉。同一用户多台设备各自从 nc=1 开始计数，不能互相把对方的 nc 判成重放。客户端时钟漂移导致 nonce 提前过期时，要回 stale 质询并换发一个全新的 nonce，旧 nc 不得被继承。qop=auth-int 的请求必须校验实体哈希，不能只看头部就放行。这三条在任何单条用例里都不显眼，线上正是它们把正常设备打成攻击者。

硬约束：同一 nonce 的并发重放只能成功一次，占位必须原子，失败的一方不得推进水位；nonce 水位与过期代际按用户设容量上限并按时间回收，容量顶到时宁可让最旧的一条先失效，也不能让表无限长大。单用户并发会话数与失败计数也各有上限。快照要带上 nonce 表、水位与密钥代际，损坏载荷在 Restore 时报 ErrBadSnapshot，恢复后旧 nc 仍然不能被复用；换钥后旧代际的 nonce 与它对应的水位一起作废，不能只换签发密钥而把水位留在原代际。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；并发重放与换钥自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-a-02`
- 实现文件：`digest_auth_challenge.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`DigestChallengeGuard`
