# SSH 服务端转发准入与反向端口租约

## 题面

堡垒机上了端口转发审批之后，运维和合规同时来找：内网一台数据库还能被 direct-tcpip 直连进来，合规同学拿着云主机的元数据地址也照样连了出去；另一头几个租户互相抱怨，重启客户端后原来占着的 20022 端口被隔壁租户拿走了，反向隧道重建时提示端口被占。落点在模块 gsb/batch50-go-safety-b-01 的 ssh_forward_policy.go，类型 SshForwardPolicy，公开签名保持：NewSshForwardPolicy(cfg PolicyConfig) *SshForwardPolicy、RegisterSession(sessionID, tenant string, priority int) (Session, error)、CheckDirect(sessionID string, dest HostPort) (Decision, error)、AllocateRemote(sessionID string, req RemoteForward) (BoundPort, error)、Release(sessionID string) error、SessionState(sessionID string) (Session, error)、ChargeBytes(sessionID string, n int64) error、NormalizeHost(host string) (string, error)、ReclaimExpired(now time.Time) int、Snapshot() ([]byte, error) 与 Restore(data []byte) error。

需要落地的判定与配额：

1. 未登记的会话做转发一律 ErrSessionNotFound；会话号或租户为空报 ErrBlankSession；同号重复登记报 ErrSessionExists，且不留下第二条记录。
2. 每租户并发会话数超过上限报 ErrTenantSessionLimit，被拒的登记不计入会话数。
3. 目标先归一化，再做地址判定，最后匹配租户规则：回环、链路本地、云元数据地址与租户禁止网段直接拒绝，原因码分别是 loopback、metadata、cidr。
4. 主机不在允许清单给 not_listed，端口不在允许集合给 port，Decision 里的 Target 填归一化后的 host:port。
5. 允许项写成域名时，清单匹配通过还不算完，解析出来的每个地址都要再过一遍地址判定，落进禁止网段的必须在建连前拒绝。
6. 同一地址的 IPv4 映射写法、十进制与八进制变体要归一到同一判定，不能一种写法放行、另一种拒绝。
7. 会话通道数超过上限时 CheckDirect 返回拒绝决定并给 channel_cap 原因码，计数不再增加。
8. 反向转发只接受配置区间内的端口，越界报 ErrPortRange；同一端口不能被两个会话同时持有，后到者报 ErrPortBusy，原租约原样保留。
9. 端口池耗尽时按会话优先级决定排队还是当场拒绝；排队的请求不能预先占位，被拒的要明确回错。
10. Release 一次性回收该会话的通道与端口；对不存在的会话 Release 幂等成功，SessionState 仍报 ErrSessionNotFound。
11. ChargeBytes 累计超过单会话字节配额报 ErrByteQuota，本次不计账。
12. 租约到期由 ReclaimExpired 统一回收并返回条数，回收后的端口可被重新申请。

补一条现场口径：通道关闭不等同于会话释放，只有 Release 或租约到期才归还端口，避免半开连接把池子慢慢吃空。

硬约束：登记、申请端口、释放与字节记账都会被多个连接协程并发调用，端口分配与释放必须原子，同一端口在任何时刻只能有一个持有者，租户会话上限只放行配置条数；快照只反映全量旧状态或全量新状态，恢复后会话、租约与端口占用要和快照一致，进程被强杀重启后不遗留任何占用。

验收：go test ./... 的 15 条用例必须全过，不得修改测试、跳过或将期望值按输入写死；并发分配与会话异常断开自行构造验证；只做本地实现，交付源码与测试记录。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-01`
- 实现文件：`ssh_forward_policy.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SshForwardPolicy`
