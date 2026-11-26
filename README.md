# Passkey 跨设备迁移与克隆检测

## 题面

客服接到一位财务主管的申诉：他换了新手机，用迁移链接把 passkey 搬过去，旧手机上的同一条凭据还能继续登录，两边签名计数各自往前跑，风控里没有一条克隆告警；另有一位用户迁移页放了半小时才点确认，系统仍然收下那张过期的挑战，直接绑到了新设备上。第二位用户的旧手机后来还在登录，客服只能在两台设备之间来回停用，谁也说不清当时绑的到底是哪一台。这个模块要同时管住迁移绑定和日常断言，落点是 gsb/batch50-go-safety-a-14 的 passkey_migration_guard.go，类型 PasskeyMigrationGuard，公开接口保持不变：NewPasskeyMigrationGuard(cfg GuardConfig) *PasskeyMigrationGuard、RegisterCredential(c Credential, at int64) error、BeginMigration(req MigrationRequest) (Challenge, error)、ApproveMigration(challengeID string, proof MigrationProof) (MigrationResult, error)、Authenticate(credID, deviceID string, counter uint32, at int64) (AuthResult, error)、BindDevice(credID, deviceID, pubKey string, at int64) error、FreezeCredential(credID, reason string) error、Unfreeze(credID, reason string, at int64) error、Credential(credID string) (CredentialState, bool)、Devices(credID string) []DeviceBinding、Events(credID string) []Event、SweepExpired(now int64) int、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的行为：一、注册时凭据 ID、用户、公钥、设备任一为空报 ErrBadRequest；同一 ID 换公钥再注册报 ErrCredentialConflict，原绑定保持不动。二、迁移请求对未知凭据报 ErrCredentialNotFound，对已冻结凭据报 ErrCredentialFrozen，设备数达到 MaxDevices 报 ErrDeviceLimit。三、挑战带一次性 nonce，过期时间为发起时刻加 ChallengeTTL，超过之后确认报 ErrChallengeExpired。四、挑战用过一次即作废，重放报 ErrChallengeReused；nonce 不符或签名为空报 ErrBadRequest，且不消耗挑战。五、迁移成功把新设备绑为 active、旧设备置 retired，并写一条 migration_approved 事件。六、已退役设备再认证报 ErrDeviceRetired，不得顺带冻结凭据。七、认证计数必须严格大于凭据水位，相等或更小报 ErrCounterRollback 并立即冻结该凭据，冻结后一律报 ErrCredentialFrozen。八、Unfreeze 必须带非空理由，解冻不清计数，冻结与解冻都留事件，事件里写明理由。九、事件按序号升序，超过 MaxEvents 只保留最新若干条。十、SweepExpired 清掉过期挑战并返回条数，已消费的挑战不得复活。十一、快照覆盖凭据、设备、水位、挑战、事件与序号，恢复后的序号必须接在现存状态之后；数据损坏报 ErrBadSnapshot 且不动现有状态。

最容易漏的三处：同一条凭据在两种设备上同时把计数各推一格时，只能有一个成功，另一个要按克隆处理并冻结；迁移请求过期后重新发起必须换新的挑战，不能复用旧的那张；备份设备恢复后水位只能前进，不能因为本地缓存的旧计数回退。

硬约束：认证、迁移确认、冻结与解冻共用同一份状态，并发下同一计数只能成功一次，序号不得重复或回退，认证失败的调用也不得改动水位；设备表、挑战表与事件表都受配置上限约束，过期清理不得影响仍然有效的挑战。

验收：go test ./... 15 条全过，不得改测试、跳过或按固定输入写死；并发认证与快照恢复自行验证；交付源码与跑测记录，只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-a-14`
- 实现文件：`passkey_migration_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`PasskeyMigrationGuard`
