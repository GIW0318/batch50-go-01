# OpenID4VP 可验证凭证出示的定义匹配与状态核验

## 题面

钱包团队反馈：用户在一家租户申请开具学历证明，出示成功后同一份凭证又被拿去另一家租户的申请里使用，我们的验证端照样给出通过；还有一台安卓设备把钱包声明的格式改成我们不认识的私有格式，验证端跳过这张凭证直接宣布整体通过。审计同事把两次出示的记录调出来，发现第二次用的 nonce 根本不是这个 request 签发的，验证端却没拿它和记录做过比对。落点是模块 gsb/batch50-go-safety-a-03 的 vc_presentation_verifier.go，类型 PresentationVerifier，公开接口保持：NewPresentationVerifier(cfg Config) *PresentationVerifier、CreateRequest(tenantID string, def PresentationDefinition) (Request, error)、Verify(requestID, vpToken, submission string) (Result, error)、State(requestID string) (RequestState, error)、Expire(now time.Time) int、PutStatusList(list StatusList) error、StatusListEpoch(uri string) (int64, bool)、SupportedFormats() []string、PendingRequests(tenantID string) []RequestState、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的行为如下。定义里没有 descriptor 或者条数超过 MaxCredentialsPerRequest，报 ErrBadDefinition；descriptor 声明的格式不在 Formats 白名单里，报 ErrUnsupportedFormat。同一租户未完成的 request 数超过 MaxPendingPerTenant 时报 ErrRequestLimit，不能挤掉更早的申请，租户之间互不影响。vp_token 与 submission 都是 JSON：vp_token 带 credentials 数组，每项有 id、format、holder、claims 以及可选的 status 与 kb；submission 的 descriptor_map 把 descriptor id 映射到 $.credentials[N] 这样的路径。Verify 按映射取到凭证后依次检查：kb 的 nonce 必须等于 request 签发时的 nonce，否则 ReasonNonceMismatch；kb 的 aud 必须等于配置里的 Audience，否则 ReasonAudienceMismatch；kb 的 iss 必须等于凭证的 holder，否则 ReasonHolderMismatch；kb 的 iat 要落在请求有效期内；descriptor 里每个 FieldConstraint 的路径都要在 claims 里取到值并命中 Values 之一，否则 ReasonConstraintFailed。全部通过时 Status 为 verified、Reason 为 ok、Matched 列出命中的 descriptor id、Subject 取 holder。

状态清单也要接上。凭证带 status 时按 uri、index、epoch 查已登记的清单：index 落在 Revoked 里报 ReasonRevoked；清单纪元与凭证声明的纪元不一致报 ReasonStatusEpoch；没有登记过该 uri 的清单不能当成有效。清单纪元只升不降，收到比缓存更旧的纪元时整份忽略并保留原来的快照。结论一旦给出就不能改写，同一个 request 的并发提交只允许一个成功，其余报 ErrRequestClosed，State 里的 Submitted 只记一次；已经过期的 request 再提交同样报 ErrRequestClosed。

还有三条边界。一张凭证同时满足多个 descriptor 时要按 submission 的映射归属，不能默认塞进第一个。状态清单服务不可达时要保守拒绝，不能把未知状态当作有效。钱包声明的格式不被验证端支持时必须显式拒绝，不能跳过该凭证继续判定通过。

硬约束：状态迁移必须原子，并发提交只出一个结论；状态清单结果按 URI、索引与纪元缓存，纪元变化时整份失效。单租户未完成 request 数、单请求凭证数与过期清扫周期都有上限，过期清扫只能处理已到期的记录并把清扫条数返回给调用方。快照要带上 request 表、结论与清单缓存，损坏载荷在 Restore 时报 ErrBadSnapshot，恢复后已给出的结论仍然不可改写。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；并发提交与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-a-03`
- 实现文件：`vc_presentation_verifier.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`PresentationVerifier`
