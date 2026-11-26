# OAuth 授权端点的 JAR 签名请求对象校验与 jti 核销

## 题面

授权端点上线请求对象校验之后，一部分老客户端登不进去，另一部分本该被拒的请求却拿到了授权码：有人把 request 里的 scope 换成更大的值，签名没有跟着变，端点照样放行；同一份 request 提交两次也能过，因为 jti 从来没被核销。值班同学把两次提交的 jti 打印出来，发现完全一样，服务端却把它当成两个不同的请求各自走完了流程。落点是模块 gsb/batch50-go-safety-a-04 的 jar_request_object.go，类型 RequestObjectGuard，公开接口保持：NewRequestObjectGuard(cfg Config) *RequestObjectGuard、RegisterClient(c Client) error、Client(clientID string) (Client, bool)、RotateKey(clientID, kid string, pub ed25519.PublicKey, grace time.Duration) error、Parse(raw string) (RequestObject, error)、Verify(ro RequestObject, client Client) error、Merge(query url.Values, ro RequestObject) (AuthRequest, error)、Consume(jti string) error、Consumed(jti string) bool、StoreRequestURI(uri, raw string, ttl time.Duration) error、ParseRequestURI(uri string) (RequestObject, error)、InFlight(clientID string) int、Sweep(now time.Time) int、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的行为如下。Parse 只接受三段式紧凑 JWS，段数不对、base64 解不开、JSON 坏掉都报 ErrMalformed，超过 MaxObjectBytes 报 ErrObjectTooLarge，SigningInput 必须是前两段拼起来的那一段。Verify 先看算法：只接受客户端注册过的 alg，none 与 HS 开头的算法一律报 ErrAlgNotAllowed，哪怕客户端把 HS256 登记进白名单也不行。接着按 kid 找客户端公钥，找不到报 ErrUnknownKey；验签失败报 ErrBadSignature；iss 必须等于 client_id，否则报 ErrIssuerMismatch；aud 必须包含配置里的 Endpoint，否则报 ErrAudienceMismatch；iat 早于 MaxTokenAge 或 exp 已过都报 ErrExpired；请求对象缺少 jti 或者 jti 是空串时报 ErrMalformed，不能拿空串去核销。

Merge 的优先级要写死：请求对象里出现的参数以请求对象为准，只有它省略的参数才从 query 继承，两边同名但取值不同报 ErrParamConflict，取值相同才算通过；query 里同时带 request 与 request_uri 报 ErrRequestConflict。已知参数 client_id、response_type、redirect_uri、scope、state、nonce 落进结构体字段，其余进 Extra，键名保持原样。客户端注册表里同一个 client_id 重复登记按覆盖处理，旧的公钥与算法白名单不能残留。Consumed 只做查询不改状态，Sweep 负责把过了重放窗口的墓碑清掉。

还有三条边界。请求对象里参数被显式写成空值时算作出现，不能因为取值为空就回头去 query 里补一份。客户端密钥轮换后旧密钥在宽限期内仍可验签，但同一个 jti 不得因为换钥而重新可用。request_uri 指向的内容一旦被消费就要删除，重复解析同一个 URI 必须返回过期而不是重新展开。这三处不写清，客户端换一台设备或换一次密钥就会踩到，排查时又很难复现。

硬约束：jti 核销要并发安全，重放窗口内保留墓碑，同一个 jti 第二次提交必然失败；request_uri 只能消费一次并且带 TTL。单客户端在途请求数、请求对象字节数与参数个数都有上限，在途请求按客户端计数，超过上限时新请求直接拒绝而不是排队。快照要带上客户端注册表与 jti 墓碑，损坏载荷在 Restore 时报 ErrBadSnapshot，恢复后已核销的 jti 仍然不可用。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；并发核销与换钥自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-a-04`
- 实现文件：`jar_request_object.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`RequestObjectGuard`
