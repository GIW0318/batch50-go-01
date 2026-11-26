# 时间戳令牌核验与签署时刻回验

## 题面

制品仓库上个月做合规抽检，签名旁边的时间戳令牌被复核脚本整批判成无效：脚本拿当前时间回验证书链，签发时证书明明有效，只因现在过期就全被拒；同一批里有个令牌的随机数对不上，脚本没有单独拦下而是当成普通无效继续往下走，统计口径全乱；还有个机构证书只带时间戳用途，脚本按服务端证书的用途去查，直接判了不可信，业务方拿着时间戳机构的说明来对账，两边扯了三天。请把令牌核验做成可复用的组件，落点 gsb/batch50-go-safety-c-38，实现文件 timestamp_authority_verifier.go，类型 TimestampAuthorityVerifier，签名固定：NewTimestampAuthorityVerifier(limits Limits) *TimestampAuthorityVerifier、Verify(token []byte, imprint Imprint) (Verdict, error)、Batch(tokens [][]byte, imprint Imprint) ([]Verdict, error)、Trust(tsa string, chain []Cert) error、Stats() Stats、Snapshot() ([]byte, error)、Restore(data []byte) error，另配 AllowPolicy、Revoke、Trusted、VerdictFor、Reset 五个辅助方法。

要修的点：一，令牌按 JSON 读入，带 tsa、serial、genTime、digest、nonce、policy，字段缺失或不是合法 JSON 报 ErrBadToken 且不产生结论；二，imprint 带算法、摘要与随机数，算法只认 sha256，摘要必须是六十四位十六进制，否则报 ErrImprintMismatch，令牌没有随机数报 ErrNonceMissing，随机数与请求不一致报 ErrNonceMismatch，这两种都不计入核验数；三，摘要不一致是结论不是错误，结论为无效、原因码 imprint；四，证书链按签署时刻回验，令牌里的 genTime 落在叶证书有效期内即可，当前是否过期不影响结论；五，签署时刻早于生效或晚于失效，原因码 cert-window；六，吊销按时刻比较，吊销时刻不晚于签署时刻才算吊销并给原因码 revoked，序列号为空报 ErrBadRevocation；七，信任锚登记时机构名不能为空、链不能为空、叶证书主题要等于机构名、每张证书的签发者要等于下一张的主题，任何一条不满足报 ErrBadTrust 且不登记；八，机构没登记时结论为不可信，原因码 untrusted；九，机构证书带仅用于时间戳的扩展时不能按普通服务端证书用途校验，签署时刻的精度与本地时钟偏差要分开记，不能用本地时间替代签署时刻；十，AllowPolicy 登记可接受的策略标识，令牌里的策略与信任配置不符时按不可信处理；十一，Batch 逐条核验并保持入参顺序，单条坏令牌只影响自己，规模超过 MaxBatch 报 ErrTooManyTokens；十二，同一令牌与同一 imprint 重复核验必须幂等并命中缓存，第二次返回 Cached 为真且 Verified 不重复计数，Trusted 按机构名升序返回，Rejected 记无效结论数，快照要么完整包含信任锚、吊销表、缓存与计数，要么整体拒绝损坏载荷，恢复后缓存仍能命中。

硬约束：同一实例的登记、核验、吊销与快照串行推进，批内并发不得出现同一令牌被计两次或计数丢失；快照要么完整可用，要么整体拒绝且不动现有信任锚与吊销表。验收：go test ./... 的 15 条用例全过，测试不得修改、跳过或按固定输入写死；批内并发与重启恢复自行验证，只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-38`
- 实现文件：`timestamp_authority_verifier.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`TimestampAuthorityVerifier`
