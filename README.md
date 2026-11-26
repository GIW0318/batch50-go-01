# DNS Cookie 校验与密钥双代轮换

## 题面

解析入口加上 COOKIE 校验之后，某地市一批递归查询开始间歇性超时。抓包看到两类流量：一类客户端只带 8 字节客户端 cookie，服务端回了重试应答之后客户端不再重试，而是每几十秒重复同一请求，日志里的降级计数被刷到几十万；另一类是运营商 NAT 后面的客户端地址在几分钟内换了好几段，服务端把它们全算成攻击，干脆拉黑了整个地址池。还有人反馈密钥轮换那一小时里，老 cookie 明明还在宽限期内却被拒绝，另一些过了宽限期反而能过。落点在模块 gsb/batch50-go-safety-b-18 的 dns_cookie_validator.go，类型 DnsCookieValidator，签名保持：NewDnsCookieValidator(cfg CookieConfig) *DnsCookieValidator、Issue(client Cookie, addr netip.Addr) (Cookie, error)、Check(req CookieRequest) (Verdict, error)、Rotate(now time.Time) error、Generation() (int, time.Time)、DegradedCount(addr netip.Addr) int、Stats() CookieStats、Reset(now time.Time) error、Snapshot() ([]byte, error)、Restore(data []byte) error。

需要落地的判定：

1. 客户端 cookie 固定 8 字节，长度不符的签发报 ErrBadClientCookie；地址为零值报 ErrBadAddr；服务端 cookie 默认 16 字节，可配但只能在 8 到 32 之间。
2. 同一客户端 cookie 与同一地址派生出的服务端 cookie 必须逐字节一致；地址一变，结果必须不同。
3. 完全不带 COOKIE 的请求按放行处理，原因 cookie_absent，不计降级，这是首次查询的正常路径。
4. 只带客户端 cookie 的请求按配置放行但计入降级；需要完整 cookie 时返回 badcookie 并附新的服务端 cookie，重试窗口内同一地址只计一次降级，达到 MaxRetries 之后改为丢弃并给 retry_storm。
5. 客户端 cookie 长度不对时整条请求丢弃，原因 malformed_cookie，不做截断。
6. 服务端 cookie 与当前代派生结果不一致时按普通不匹配处理，返回 badcookie 并附新 cookie，计入坏 cookie 但不改结论；当前代命中给 accept、原因 current_key。
7. 服务端 cookie 长度必须与请求里的客户端 cookie 对应，长度不符按格式错误丢弃。
8. 密钥轮换：未到 RotationInterval 报 ErrRotateTooSoon 且不动密钥；到点后当前代降为上一代并生成新代，代次与轮换时刻可读。
9. 上一代在 GracePeriod 内继续有效，命中给 accept、原因 previous_key 并单独计数；宽限期一过必须拒绝，原因 stale_key。
10. 客户端地址经 NAT 变化导致的 cookie 失配按普通不匹配处理，不计入攻击计数。
11. COOKIE 选项总长超过协议长度上限时整条请求丢弃，而不是截断后校验。
12. 快照覆盖两代密钥、代次、计数与降级表，恢复后已发出的 cookie 仍能按代次校验；损坏载荷报 ErrBadSnapshot 且不动现网；Reset 只清计数与降级表，不动密钥。

硬约束两条：并发校验只能读同一份密钥快照，轮换与校验之间不允许出现半新半旧的密钥组合；降级表按地址计数，同一地址的窗口更新必须原子，并发的重复请求不能多计一次降级，也不能把第一次正常降级漏掉。

验收：go test ./... 的 15 条用例必须全过，不得修改测试、跳过或把期望值按固定输入写死；并发校验与重启恢复自行构造验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-18`
- 实现文件：`dns_cookie_validator.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`DnsCookieValidator`
- 只依赖标准库
