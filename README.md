# 角色删除冷静期：登录即撤销、到期清理与昵称保留期的墓碑记录

## 题面

角色删除冷静期上线后客服被围住：有人点了删除又反悔，重新登录却提示角色已删除，只能等满冷静期再走申诉；有人冷静期内连续点两次删除，工单里出现两张不同的删除单；更糟的是一批到期角色清完之后，昵称立刻被别的账号注册走，历史资产和邮件跟着被冒领。运维把这批工单按操作时间排了一遍，发现申请、撤销、清理、改名四条链路各写各的，没有一个统一的状态。

落点是模块 gsb/batch50-go-game-a-08 的 character_retire_guard.go，类型 CharacterRetireGuard，公开接口保持不变：NewCharacterRetireGuard(limits Limits) *CharacterRetireGuard、Register(accountID, characterID, name string) (CharacterState, error)、Request(accountID, characterID string, at time.Time) (RetireTicket, error)、Cancel(accountID, ticketID string) (CharacterState, error)、Finalize(ticketID string, at time.Time) (RetireResult, error)、RestoreCharacter(accountID, characterID string) (CharacterState, error)、Login(accountID, characterID string, at time.Time) (CharacterState, error)、EnterMatch(characterID string) error、StateOf(characterID string) (CharacterState, error)、NameStatus(name string) (NameState, error)、Pending(accountID string) ([]RetireTicket, error)、Snapshot() ([]byte, error)、RestoreSnapshot(data []byte) error。

要收掉的地方如下。第一，账号、角色、昵称任一为空报 ErrBadArgument；角色编号重复报 ErrCharacterExists；昵称已被占用、已被保留或被待删角色占着报 ErrNameTaken。第二，对不存在的角色申请删除报 ErrCharacterNotFound，用别的账号申请报 ErrAccountMismatch。第三，同一角色重复申请返回同一张删除单，单号与到期时刻都不变，Pending 里只有一条。第四，同一账号的待删角色数量受 MaxPendingPerAccount 硬顶，超出报 ErrPendingLimit。第五，撤销要校验持单账号，账号不符报 ErrAccountMismatch，单号不存在报 ErrTicketNotFound；撤销成功后角色回到 active，Pending 里不再有它。第六，同一张单重复撤销幂等，第二次仍然返回 active 而不是报错。第七，冷静期未满调用 Finalize 报 ErrTooEarly，角色状态保持 pending。第八，冷静期满调用 Finalize 才真正清理，结果里 Purged 为真、带上角色编号、昵称与墓碑编号，角色状态变 purged。第九，同一张单第二次 Finalize 报 ErrTicketClosed，不允许重复清理。第十，待删角色不能进对局，EnterMatch 报 ErrCharacterRetired；撤销之后可以正常进入。第十一，快照要带上角色、删除单、昵称占用与墓碑，恢复后 StateOf、Pending 与关服前一致，损坏载荷报 ErrBadSnapshot 且不动现有数据。第十二，墓碑数量受 MaxTombstones 硬顶，超出时清理最旧的一条。

还有三处必须一起收掉。冷静期内玩家重新登录该角色视为撤销删除，直接回到 active，不能让玩家先吃一个报错再被要求手动撤销。清理之后昵称进入保留期，保留期内新角色不能注册同名，但原账号可以凭墓碑申诉恢复这个角色。到期清理与玩家撤销同时发生时以清理锁内的状态为准，撤销成功必须整体中止清理，不允许出现昵称已释放、角色数据还在的半清理状态。

硬约束有两类。一是并发与原子性，Request、Cancel、Finalize、Login 会被登录服和后台清理任务同时调用，同一角色同一时刻只能有一个终态，清理要么整份完成要么完全不做，-race 下不得有数据竞争。二是重启与上限，快照必须原子替换全部状态，恢复失败不得半套生效；MaxPendingPerAccount、MaxTombstones 都是硬顶，超限报错而不是截断。

验收：`go test ./...` 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；并发清理与重启自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-08`
- 实现文件：`character_retire_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`CharacterRetireGuard`
