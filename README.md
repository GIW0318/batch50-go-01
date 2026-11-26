# 进程文件系统暴露面治理

## 题面

安全合规复查时发现两件互相矛盾的事：一台宿主机上的监控代理读不到业务容器的进程列表，采集断了一整天；同一台机器上，一个普通租户账号却能列出别的租户的进程和线程目录，连可执行文件路径都看得清清楚楚。运维按流程改过一次隐藏级别，重新挂载之后设置被悄悄退回默认值，回读也没人核对。再翻豁免名单：一条按进程名匹配的豁免被复用到了另一个同名但内容不同的二进制上，另一条豁免在进程退出后还在继续放行。

要把这块能力补到 gsb/batch50-go-safety-d-09 的 procfs_exposure_guard.go，核心类型 ProcfsExposureGuard，入口 NewProcfsExposureGuard(Options) *ProcfsExposureGuard。公开接口：SetPolicy(ns string, policy Policy) error、Policy(ns string) (Policy, bool)、Apply(ns string) error、MountedLevel(ns string) (int, bool)、Visible(ns, viewer string, target Process) (bool, error)、Exempt(ns string, entry Exemption) error、Exemptions(ns string) []Exemption、Exit(ns string, pid int) error、Verify(ns string) ([]Diff, error)、Namespaces() []string、Snapshot() ([]byte, error)、Restore([]byte) error。

1. 命名空间名为空白返回 ErrBadNamespace；隐藏级别只收 0、1、2，其余返回 ErrBadLevel，被拒绝的策略不入库。
2. 隐藏级别只能逐级收紧：从 2 调到 1 属于放宽，必须显式带 Relax 才允许，否则返回 ErrRelaxRequired 且原策略不变。
3. 策略写入后不会自动生效，Apply 才算挂载完成；没有策略时 Apply 返回 ErrNoPolicy，策略存在但从未 Apply 时可见性查询返回 ErrNotApplied。
4. Apply 之后要能回读到实际挂载的级别；策略改了没重新 Apply 时，Verify 报一条 hide_level 差异，want 是新级别、got 是当前挂载级别，重新 Apply 后差异清空。
5. 隐藏级别 0 对所有进程可见；级别 1 只隐藏其他属主的进程，属主与查看者一致时可见；级别 2 完全隐藏，只有豁免能放行。
6. 自身进程与线程目录始终可见，不受隐藏级别影响。
7. 豁免按可执行文件摘要绑定：同名但内容不同的进程不得豁免，摘要与查看者两项都对上才放行。
8. 豁免必须带到期时间，到期时间不晚于当前时间返回 ErrBadExemption；查看者、路径、摘要任一为空同样拒绝；对没有策略的命名空间登记豁免返回 ErrNoPolicy。
9. 过期的豁免不再放行；进程退出时用 Exit 按进程号清掉它名下的豁免，重复 Exit 幂等。
10. 豁免只在登记它的命名空间内有效，不能被别的命名空间借用。
11. 子命名空间默认继承父命名空间的策略，一旦自己显式设过策略就不再跟随父变更。
12. 策略变更与可见性查询必须读同一份原子快照，改策略期间查询不能看到半新半旧的豁免表；快照含策略、挂载级别与豁免，损坏载荷返回 ErrBadSnapshot 且不动现网。

硬约束两条：一是并发与原子性，策略变更、豁免增删与可见性查询共用一份原子快照，并发下不得出现半新半旧的豁免表，同一策略并发 Apply 的结果必须收敛到同一个挂载级别；二是恢复与时限，快照自包含、可重复恢复，豁免到期与进程退出两个时点都必须真正生效，恢复后的挂载级别不能退回默认值。

验收口径：go test ./... 的 15 条用例全过，不得修改测试、跳过用例或按固定输入写死结果；并发与重启行为自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-d-09`
- 实现文件：`procfs_exposure_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`ProcfsExposureGuard`
