# Unix 域套接字租户隔离与描述符预算

## 题面

沙箱作业的控制面最近连着出了几起串号：租户 A 的作业把监听套接字挂到抽象命名空间后，租户 B 的一个临时进程照着同一条路径连了上去，控制指令被整段读走，事后从清单里完全看不出是谁连的；另一台节点上作业已经被杀，同名套接字还能连上，重启之后残留的名字要人工逐个清理，清理漏掉的作业再也起不来。还有一个作业靠反复传文件描述符把宿主机上的普通文件句柄塞进了沙箱。请在 gsb/batch50-go-safety-c-01 的 unix_socket_namespace_guard.go 里补完 UnixSocketNamespaceGuard，按租户与作业隔离套接字、约束连接方身份并对描述符传递记账。

对外接口保持：NewUnixSocketNamespaceGuard(Options) *UnixSocketNamespaceGuard；Listen(tenantID, jobID, name string) (Endpoint, error)；Dial(tenantID, name string, policy PeerPolicy) (Conn, error)；Release(jobID string) error；Audit() []Binding；BindingsFor(tenantID string) []Binding；PeerIdentity(connID string) (PeerCred, error)；AdmitFD(connID, kind string, size int, now time.Time) error；FDBudgetLeft(connID string) int；ExpireLeases(now time.Time) []string；Snapshot() ([]byte, error) 与 Restore([]byte) error。

要修的地方：一，抽象命名空间的名字必须编码成 @gsb/<tenant>/<job>/<name>，空租户、空作业、空名字、含斜杠或超长一律返回 ErrBadName；二，同一名字并发监听只能有一个成功，其余返回 ErrNameTaken，名下不得出现两条绑定；三，单租户监听数与连接数分别受 TenantSocketLimit、TenantConnLimit 约束，超限返回 ErrSocketLimit 与 ErrConnLimit，换成别的租户不受影响；四，Dial 必须核对绑定所属租户，跨租户返回 ErrTenantMismatch，未知名字返回 ErrNoBinding，作业内短名不得当成全局名解析；五，RequireSameCgroup 打开时读到的 cgroup 必须带 gsb/<tenant> 段，否则返回 ErrPeerUntrusted 且该连接不计入绑定；对端进程在凭据读出之前就已退出时同样按不可信处理，不得沿用上一条连接缓存下来的身份；六，描述符只放行 AllowedKinds 里的类型，其余返回 ErrFDKindDenied；七，单连接的条数与字节预算各自封顶，超出返回 ErrFDBudget，FDBudgetLeft 要反映真实余量；从父进程继承来的描述符在 exec 之后仍能继续传递，必须按打开代次与持有者分开计数，不能让一个作业靠子进程把预算翻倍；八，Release 之后绑定消失、租户配额回收、同名可以重新监听，重复释放保持成功；九，Audit 的序号单调递增，行内容稳定可复现，在册绑定的状态为 bound；十，PeerIdentity 回读连接建立时记录的 pid、uid、gid 与租户，连接号不存在返回 ErrNoBinding；十一，租约到期的绑定由 ExpireLeases 回收并返回名字；十二，快照损坏时 Restore 返回 ErrBadSnapshot，现网绑定一条都不能少。

硬约束：名字注册与释放必须原子，同名并发监听只放行一个，租户的套接字与连接计数在同一把锁下裁决。抽象套接字没有文件节点，进程崩溃后没有东西可以清理，只能靠租约过期把名字和配额收回来，ExpireLeases 要能独立完成这件事；快照必须能描述在册绑定与剩余配额，恢复后同名不得出现重复计数。

验收：go test ./... 十五条全过，测试不得修改、跳过或按固定输入写死；并发监听、exec 继承描述符与崩溃后租约回收自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-01`
- 实现文件：`unix_socket_namespace_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`UnixSocketNamespaceGuard`、`Endpoint`、`Conn`、`Binding`、`PeerCred`、`PeerPolicy`
