# 出站 SMTP 的 DANE 判定与 MX 降级

## 题面

海外投递最近接连出事：同一封报关邮件发往客户域，出口网关要么把 TLS 降级成明文发出去，要么在证书已经换过之后一直重试，最后整批退信。运维抓包后整理出两条线索：一是解析器返回的应答没有认证位，网关却照着 TLSA 记录强推加密，对端直接拒收；二是首选 MX 连不上之后，网关把收件域的 TLSA 套到了另一台 MX 上，握手全部失败。落点在模块 gsb/batch50-go-safety-b-16 的 smtp_dane_tlsa_validator.go，类型 SmtpDaneTlsaValidator，签名保持：NewSmtpDaneTlsaValidator(cfg DaneConfig) *SmtpDaneTlsaValidator、Policy(domain string) (DanePolicy, error)、Verify(mx string, chain []Cert) (Verdict, error)、Fallback(mx string, cause string) (Verdict, error)、Refresh(domain string) error、Cached(domain string) (DanePolicy, bool)、MXOrder(domain string) ([]MxHost, error)、Classify(answer TlsaAnswer) bool、Stats() DaneStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

需要落地的判定：

1. 收件域为空或全空白报 ErrBlankDomain，且不触发任何解析；解析失败报 ErrLookup，不留半个缓存条目。
2. MX 列表为空报 ErrNoMx；超过 MaxMxPerDomain 条报 ErrMxLimit，整份策略不落缓存；列表按优先级升序排，同优先级按主机名升序，同一主机大小写不同视为重复只留一条。
3. 每个 MX 的 TLSA 在 _25._tcp 下单独查询，认证位与记录集一起进入策略；策略在 PolicyTTL 内复用，过期后 Cached 返回 false 并重新查询，同一域并发查询只发一次。
4. Refresh 强制重查并整体替换；重查失败返回 ErrLookup 且保留旧策略继续可用，不得清空。
5. Verify 链为空先报 ErrEmptyChain；MX 不属于任何已缓存策略报 ErrUnknownMx。
6. 认证位为真且记录集非空时该 MX 为强制加密：没有一条记录能匹配就硬失败，模式 reject、原因 tlsa_mismatch，不得回退明文；匹配成功给 dane_secure，并回报记录总数与命中条数。
7. 记录集里混有不可用记录时整组判定：选择子、匹配类型或用途越界，或数据为空，整组不可用，模式 reject、原因 tlsa_record_invalid，不许跳过坏记录挑一条能过的判通过。
8. 证书链上任何一张已过 NotAfter 即 reject、原因 cert_expired，匹配再准也不放行。
9. 选择子 0 用整份证书编码比对，选择子 1 用公钥信息比对，两者不可互换；匹配类型 0 按原样比对，1 与 2 分别按 SHA-256 与 SHA-512 比对。
10. Fallback 把失败的 MX 记成已尝试，返回下一个未尝试的 MX（模式 fallback_next_mx，Next 填主机名）；列表走完给 reject、原因 mx_list_exhausted，Next 留空；原因为空报 ErrBadCause。
11. 降级到次优先 MX 时按该 MX 自己的域名重查策略，不沿用收件域的 TLSA；认证位为假时该 MX 的 TLSA 视为不可信，只能按明文机会加密处理并注明原因 ad_flag_absent，不得据此强推加密。
12. 快照要覆盖策略缓存、每个 MX 的尝试标记与计数器，恢复要么全量生效要么不动现网，损坏载荷报 ErrBadSnapshot 且旧策略仍然可查。

硬约束两条：同一域的并发 Policy 与 Refresh 只能产生一次有效解析，所有调用方拿到的策略内容一致，缓存与尝试标记的更新必须原子；策略与失败标记的读取不能被并发的 Refresh 看到半新半旧的组合。

验收：go test ./... 的 15 条用例必须全过，不得修改测试、跳过或把期望值按固定输入写死；并发查询与重启恢复自行构造验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-16`
- 实现文件：`smtp_dane_tlsa_validator.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SmtpDaneTlsaValidator`
- 只依赖标准库
