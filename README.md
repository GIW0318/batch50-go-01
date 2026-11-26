# seccomp 过滤器接管：允许列表合并、NO_NEW_PRIVS 与逃逸拦截

## 题面

容器运行时把 seccomp 策略改成由配置中心下发后，安全组复盘发现两件事：一份只收紧了两条 syscall 的增量策略被合进去之后，原本拒绝的 mount 和 bpf 重新放行了；另一份策略在 12 线程的进程上只有调用线程受限，其余线程照旧能调 ptrace，逃逸检测形同虚设。配置中心在灰度窗口里还会把同一份策略从三路同时推送，最终生效的必须是同一份，审计台账里也要能说清这次合并去掉了哪条、新加了哪条。落点 gsb/batch50-go-safety-d-06 的 seccomp_profile_guard.go，类型 SeccompProfileGuard。

公开签名保持不变：NewSeccompProfileGuard(GuardPolicy) *SeccompProfileGuard；Policy() GuardPolicy；Validate(Profile) error；Merge(base, next Profile) (MergeResult, error)；Tighten(Profile, []SyscallRule) (Profile, error)；SortRules([]SyscallRule) []SyscallRule；RuleFor(Profile, string) (SyscallRule, bool)；CheckEscape(Profile) []string；Apply(Profile) (ApplyReport, error)；Applied() (Profile, bool)；Stats() GuardStats；Snapshot() ([]byte, error)；Restore([]byte) error。

职责划分：Validate 只做结构与策略校验，NO_NEW_PRIVS 与线程覆盖放到 Apply 阶段把关，调用方先 Validate 再 Apply；Apply 的报告要能让值班一眼看出装到了多少线程、拦掉多少条规则、还有没有漏放行的受保护 syscall；Policy 返回填过默认值之后的配置，默认的受保护 syscall 名单按名字升序。

要处理的故障与边界：一、配置名为空报 ErrBlankProfile，规则里 syscall 名为空报 ErrBlankSyscall；二、默认动作写 allow 报 ErrPermissiveDefault，默认必须是拒绝类动作；三、动作不在 allow、errno、kill、trace、log 之内报 ErrBadAction；四、受保护 syscall（默认含 ptrace、bpf、userfaultfd、kexec_load）被写成 allow 报 ErrProtectedSyscall；五、同一 syscall 出现两条规则报 ErrDuplicateRule，规则数超过 MaxRules 报 ErrRuleLimit，不截断；六、合并只允许收紧：与基线相比会放行新 syscall，或把原有的拒绝改成放行，整次合并都要拒绝并报 ErrWidening，结果里列出被放宽的 syscall，不得静默合并；七、合并结果里 Added 记录 next 新出现、base 里没有的规则名，Removed 记录 base 放行而合并后不再放行的 syscall，两份都按名字升序；八、Tighten 只替换给定 syscall 的动作，返回新 profile，不改调用方传进来的那份；九、SortRules 按名字升序返回新切片，不改调用方切片，同一份策略重复排序顺序稳定；十、CheckEscape 列出被放行的受保护 syscall，按名字升序；十一、Apply 前必须 NO_NEW_PRIVS，缺了报 ErrNoNewPrivs 且不得生效；十二、线程数为 0 报 ErrBadThreadCount，超过 MaxThreads 报 ErrThreadLimit；线程数大于 1 而策略没有声明全线程生效时，必须拒绝并报 ErrNotAllThreads，只有调用线程受限不算生效。

第六条与第十二条后半句是本轮复盘的结论，别按尽量合并或主线程生效即可来实现。

硬约束：Apply 会被多个策略下发协程并发调用，任意时刻 Applied 只能看到完整的一份策略，计数器不丢；同一份策略重复 Apply 为幂等，Applied 不重复计数。快照要带出已生效策略与计数器，Restore 之后 Applied 与重启前一致，损坏载荷报 ErrBadSnapshot 且现网生效策略不动。规则数与线程数都有硬顶，超限拒绝而不是截断或裁剪。

验收：go test ./... 的 15 条用例必须全过，不许改测试、不许跳过、不许按固定输入写死；并发下发与重启自行写程序验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-d-06`
- 实现文件：`seccomp_profile_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SeccompProfileGuard`
