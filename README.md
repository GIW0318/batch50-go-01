# 旧 U2F 标识迁移与签名计数器治理

## 题面

改版上线后，一批老 U2F 安全密钥集体登不上。投诉分成两类：早先绑在旧域名下的钥匙一律提示标识不匹配；另一类是在两家客户处登记过同一把钥匙的用户，登录后计数器校验偶发失败。运维在灰度环境复现出一条线索：同一把钥匙先按旧标识注册、再按新入口注册，两次拿到的凭据标识对不上，之后所有断言都落在一条错位的记录上。落点 gsb/batch50-go-safety-a-06 的 webauthn_appid_migration.go，类型 AppIdMigrationGuard，签名保持：NewAppIdMigrationGuard(Limits) *AppIdMigrationGuard、Register(userID string, att Attestation) (Credential, error)、Authenticate(userID string, assertion Assertion) (Result, error)、BindLegacy(appid string, keyHandle, credID []byte) error、Migrate(userID string) (int, error)、Credentials(userID string) []Credential、Mapping(appid string, keyHandle []byte) ([]byte, bool)、SetLimits(Limits)、Snapshot() ([]byte, error)、Restore([]byte) error、Stats() Stats。

要修的地方：账号或凭据标识为空报 ErrBlankIdentity，且不落任何记录，重试也不会留下半个凭据；单用户凭据数超过 MaxCredentialsPerUser 报 ErrCredentialLimit，超限那次不覆盖已有记录；Register 携带的 appid 必须是 BindLegacy 登记过的旧标识，没登记过报 ErrAppIDMismatch；同一 appid 下同一 keyHandle 只允许指向一个凭据，绑到另一个凭据报 ErrMappingConflict 且旧映射保留，不被后来者改写；映射总数超过 MaxAppIDMappings 报 ErrMappingLimit 且不写入；断言带回来的 appid 与凭据登记值不一致报 ErrAppIDMismatch；凭据不存在报 ErrUnknownCredential，不能顺手新建；签名计数器必须严格大于已记录值，相等或回退报 ErrCounterRollback，同时把该凭据标成可疑，此后即使计数器前进也报 ErrCredentialRevoked，只有重新登记才能恢复；成功的断言把计数器推到新值，Credentials 与 Stats 读到的水位要一致；Migrate 只把该用户的旧标识凭据改成新标识并返回条数，第二次调用返回 0，待转条数超过 MaxMigrationBatch 报 ErrBatchLimit 且一条都不改；快照要能还原凭据、映射、计数器水位与迁移计数，损坏的快照报 ErrBadSnapshot 且不动现网，恢复之后新登记的凭据序号要继续往上走。

还有三条边界必须照顾到：不认识计数器扩展的认证器每次恒报 0，这类凭据不能按回退当成克隆处理，否则每次登录都会被标可疑；旧认证器没带 appid 扩展时走降级校验，但只对仍登记在旧标识下的凭据成立，迁走之后不再降级；同一把钥匙被两个租户各登记一次时，计数器水位与映射必须按租户各自隔离，不能共享，否则一边登录会把另一边的水位顶掉。

硬约束两条：同一凭据的并发断言按计数器串行推进，只能有一个请求成功，不允许两个请求读到同一旧值再各自写回；迁移期间新旧标识都能读，但只写新表，被拒的批次整体不生效，中途失败不留半迁移状态。单用户凭据数、appid 映射数、单批迁移条数都必须按配置硬顶，不静默放行。

验收：go test ./... 15 条全过，不得改测试、跳过、按固定输入特判；并发推进与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-a-06`
- 实现文件：`webauthn_appid_migration.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`AppIdMigrationGuard`
