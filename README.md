# 可验证凭证出示的范围核对与 nonce 防重放

## 题面

内容平台的年龄门槛上线后，钱包出示的凭证三成被判不合格：只给字段哈希的出示被判成缺字段，签发者中间证书如今过期、签发当时明明有效的整条链被否，同一份出示换个请求重放居然也能通过；客服拿着审核日志也说不清是哪一步拒的。落点 gsb/batch50-go-safety-a-12 的 credential_presentation_verifier.go，类型 CredentialPresentationVerifier，请把请求受理、出示校验、披露范围核对、nonce 防重放与快照恢复这条链路补成可用状态。

接口保持如下签名：NewCredentialPresentationVerifier(opts Options) *CredentialPresentationVerifier、Request(spec PresentationSpec) (RequestID, error)、Verify(requestID string, presentation Presentation) (Result, error)、Explain(resultID string) (Trace, error)、Revoke(credentialID string, reason string) error、Nonce(requestID string) (Nonce, error)、Attempts(requestID string) int、Pending() int、Revoked(credentialID string) bool、ResultAt(resultID string) (Result, bool)、Sweep() int、Snapshot() ([]byte, error)、Restore([]byte) error。

需要落实的行为与边界：

1. 请求标识形如 req-1，按创建顺序递增；在途请求数超过 MaxRequests 报 ErrRequestLimit；校验方与用途不能为空、必填字段至少一项，否则报 ErrBadSpec。
2. 请求允许的签发者必须都在信任注册表里，缺一个报 ErrUnknownIssuer；已关闭、到期与已出结果的请求不再计入在途。
3. nonce 为 32 位小写十六进制，同一请求稳定不变、不同请求不相同；未知请求报 ErrRequestNotFound，到期请求不再出具 nonce 并报 ErrRequestExpired。
4. 出示必须带本请求的标识与 nonce，不一致分别报 ErrRequestMismatch 与 ErrNonceMismatch；失败尝试逐次累加，达到 MaxAttempts 后请求关闭，再试报 ErrTooManyAttempts。
5. 必填字段必须出现在明文披露或哈希披露里，缺一项报 ErrScopeMismatch；披露字段总数超过 MaxDisclosed 报 ErrTooManyDisclosed；未达上限的失败尝试不阻止后续通过。
6. 已撤销凭证的出示给出 rejected 结果、原因 reason_revoked，不返回错误；撤销未知凭证报 ErrUnknownCredential，重复撤销报 ErrAlreadyRevoked，原因为空报 ErrBadReason。
7. 签发者不在请求允许列表报 ErrIssuerNotAllowed；证书链在签发时刻不完整、链上证书当时无效或已吊销报 ErrIssuerUntrusted。
8. 成功出示返回 accepted 结果，标识形如 res-1、时间取当前时刻并带上签发者标识；Disclosed 列出实际披露字段并按字典序排列；Explain 按 request、nonce、revocation、issuer、scope 五步给出轨迹与原因。
9. 请求到期后不再接受出示，报 ErrRequestExpired 并关闭请求；Sweep 清扫到期请求并返回条数；未知结果报 ErrResultNotFound。
10. 快照带上在途请求、已用 nonce、失败计数、结果与撤销表，恢复后 nonce、计数与序号原样保留；恢复失败报 ErrBadSnapshot 且不留半个状态。
11. 每条结论都留痕，结果里带请求标识、原因、披露字段与时间，可按结果标识回查。

另有三条边界要点名：选择性披露可能只给字段哈希而不给原值，范围校验要认可这种可验证但不透明的字段；信任链里的中间证书如今过期但签发当时有效，要按签发时间而不是当前时间判定；同一凭证在不同请求里的出示要各自绑定 nonce，跨请求复用必须拒绝。

两类硬约束：同一请求的出示校验串行执行，nonce 一次性，重放只能看到唯一结论；存活时长与尝试次数封顶，重启后按快照恢复在途请求与已用 nonce，不能出现请求凭空复活。

验收：在题目目录执行 go test ./...，十五条用例必须全部通过；不得修改测试、跳过用例或按固定输入写死结果；并发与重启恢复自行构造场景验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-a-12`
- 实现文件：`credential_presentation_verifier.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`CredentialPresentationVerifier`

## 验收

15 条测试函数全部通过，无跳过、无按固定输入写死；并发与重启恢复自行验证。
