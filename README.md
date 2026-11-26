# batch50-go-safety-c-15

- 模块路径：`gsb/batch50-go-safety-c-15`
- 包名：`task`
- 根文件：`livepatch_consistency_guard.go`
- 类型：`LivepatchConsistencyGuard`

## 题面

内核热补丁平台上线后，两批机器出现不一致：同一批补丁在一台机器上装成功、在另一台上装到一半重启，醒来后补丁栈里只剩前半截，业务在符号解析时直接崩；还有一次回滚了一个被后续补丁依赖的补丁，机器上留下悬空引用。现场代码在 gsb/batch50-go-safety-c-15 的 livepatch_consistency_guard.go，类型 LivepatchConsistencyGuard，需要把目标版本判定、确定性装载顺序与半装载回滚补全。

接口保持如下签名：NewLivepatchConsistencyGuard(limits Limits) *LivepatchConsistencyGuard、RegisterHost(host, kernel, buildID string) error、Register(p Patch) (PatchID, error)、Plan(host string) (Plan, error)、Apply(planID string) (Result, error)、Rollback(planID string, reason string) error、Status(host string) (HostState, error)、Hosts() []string、PatchInfo(id PatchID) (Patch, bool)、Snapshot() ([]byte, error)、Restore([]byte) error。

现场需要落实的行为与边界：

1. 登记主机必须带内核版本与构建标识，缺一项返回 ErrBlankHost；主机名重复返回 ErrHostExists；补丁 ID 为空返回 ErrBlankPatch，重复注册返回 ErrPatchExists。
2. 补丁只能依赖自身以外的补丁，自依赖返回 ErrCyclicPatch；声明的最低版本高于最高版本返回 ErrBadVersionRange。
3. 计划只包含尚未装载的补丁；前置补丁没注册或不在栈里返回 ErrPrereqMissing，目标内核版本落在区间之外返回 ErrKernelMismatch，两个互相声明冲突的补丁同时入选返回 ErrPatchConflict。版本按点分数字逐段比较，位数不同时缺位按零处理。
4. 装载顺序必须由依赖关系决定并与注册顺序无关；补丁 A 与 B 都依赖同一条基线时，不能按到达顺序排，必须给出稳定可复现的次序。
5. 装载在一个事务里完成：全部补丁按计划顺序进栈，任何一步失败整批回滚，栈深度与结果里列出的补丁数一致；同一计划只能装载一次，重复装载返回 ErrPlanConsumed。
6. 单主机栈深度有硬上限，超过返回 ErrStackLimit，且不写入任何一条；同一主机已有装载事务在途时新的装载返回 ErrHostBusy。
7. 回滚必须带原因，空原因返回 ErrBadRollbackReason；回滚按装载的逆序移除，状态里留下最近一次回滚原因。
8. 回滚一个被后续补丁依赖的补丁时，要连带回滚依赖它的补丁，不能留下悬空引用。
9. 未知计划返回 ErrUnknownPlan，从未装载过的计划回滚返回 ErrPlanNotApplied，未知主机返回 ErrUnknownHost。
10. 主机在装载中途重启，重启后的内核可能已经包含该补丁，要按构建标识判定为已生效，而不是重复装载一遍。
11. 重启后从快照恢复主机表、补丁栈与未完成事务；半装载的事务一律回滚到事务开始前的栈。
12. 补丁表与主机表都有条数上限，超限返回 ErrPatchLimit 或 ErrHostLimit，已登记记录不受影响。

两类硬约束必须满足：同一主机的装载与回滚串行执行，补丁栈变更要么整体提交要么整体不生效，并发装载不同主机互不阻塞；快照整体生效或整体不生效，恢复失败不得留下半装载的主机。时钟通过 Limits.Now 注入，为空时取系统时间。

验收方式：在题目目录执行 go test ./...，十五条用例必须全部通过；不得修改测试、跳过用例或按固定输入写死结果；并发装载与重启续跑要自己构造场景验证；只做本地实现。

## 运行

```bash
go test ./...
```

测试位于 `tests/test_livepatch_consistency_guard_test.go`，以 `impl` 别名导入本模块。
