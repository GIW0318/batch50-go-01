# 主机端口绑定与租约治理

## 题面

平台侧转来的工单：一台网关主机上，普通业务账号把 443 端口占走了，特权端口审批形同虚设；另一个服务的预留已经过期三小时，端口还被挂在占用表里，别人申请一直被拒；等到巡检脚本统一清理时，正持有该端口的进程被直接解绑，线上连接整批闪断。共享端口组也出过事：最后一个成员退出与新绑定请求同时到达，中间出现了几秒钟"没人持有却显示被占用"的窗口，新请求被拒。主机重启后预留表与组成员全部丢失，配额被重复占用。

落点 gsb/batch50-go-safety-c-20 的 port_bind_warden.go，类型 PortBindWarden。签名保持：NewPortBindWarden(cfg Config) *PortBindWarden、Reserve(port int, owner string, ttl time.Duration) (Lease, error)、Bind(leaseID string, opts BindOpts) (Binding, error)、JoinGroup(group string, bindingID string) error、Release(bindingID string) error、Explain(port int) (PortState, error)、Checkpoint() (ReclaimReport, error)、Lease(leaseID string) (Lease, bool)、Binding(bindingID string) (Binding, bool)、Snapshot() ([]byte, error)、Restore([]byte) error。Config 的 PrivilegedOwners、ExternallyBusy、MaxLeases、MaxPerOwner、MaxGroupMembers 分别约束特权属主、内核已占用端口、池容量、单属主额度与组成员上限，Now 为可注入时钟。

要修的点：

1. 端口必须在 1 到 65535 之间，属主名称非空，租期为正，否则报 ErrBadPort、ErrBadOwner、ErrBadTTL。
2. 端口分类如实上报：1024 以下为 privileged，32768 以下为 reserved，其余为 dynamic。
3. 特权端口只有 PrivilegedOwners 里的属主能预留，未授权报 ErrPortNotAllowed；Bind 时声明 Privileged 的请求还要复核属主身份。
4. 同一端口已被其他属主预留报 ErrPortInUse；同一属主再次预留视为续租，租约号不变，到期时间按本次请求重算。
5. 单属主租约数超过 MaxPerOwner 报 ErrOwnerLimit，池内总数超过 MaxLeases 报 ErrLeaseLimit，被拒请求不占额度。
6. Bind 要求租约存在且未过期，分别报 ErrLeaseNotFound、ErrLeaseExpired，绑定地址空白报 ErrBadBinding；Config.ExternallyBusy 里的端口视为已被内核占走，绑定报 ErrPortInUse。
7. 动态端口范围内核可能已把端口分给别的进程，预留成功不代表绑定一定成功，这条失败路径要能如实返回而不是重试。
8. 组成员只能通过 JoinGroup 加入：组名空白报 ErrBadGroup，绑定不存在报 ErrUnknownBinding，成员数超过 MaxGroupMembers 报 ErrGroupLimit，重复加入幂等，已在其他组的绑定不得静默换组。
9. 组成员变更原子生效，不允许出现无人持有却被占用的中间窗口；最后一个成员退出时组随之解散，端口立即回到未占用。
10. 预留到期后不立即解绑，统一由 Checkpoint 回收；仍有活跃绑定的租约先记一次宽限，下一个检查点再处理，不得直接断开。
11. Release 未知绑定报 ErrUnknownBinding；释放后端口状态、组成员与组归属同步清理，租约本身保留到到期。
12. 快照要带租约到期时间、绑定、组归属与序号；Restore 全成或全不成，损坏载荷报 ErrBadSnapshot 且不动现网；恢复后租约号与绑定号继续递增。

硬约束：

- 同端口的预留与绑定串行裁决，16 个协程抢同一端口只能有一个赢家，其余一律 ErrPortInUse。
- 池容量、单属主额度与组成员数都是硬上限，超限报错而不是挤掉已有记录。
- 预留、绑定与组成员跨重启保持，恢复过程任何一步失败都不得留下半套状态。

验收：go test ./... 15 条全过；不得改测试、跳过、按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-20`
- 实现文件：`port_bind_warden.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`PortBindWarden`
