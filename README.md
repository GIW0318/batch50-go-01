# 缓存与内存带宽租户隔离：掩码不重叠与监控标识回收

## 题面

缓存侧的租户隔离接连出事：一个批处理租户把自己的缓存掩码扩到了别家的位上，两个租户互相踢缓存，命中率一起崩；另一台机器上监控标识用完之后，新租户的统计全落到老租户名下，值班看到的数字对不上。现在要把节点缓存与内存带宽资源组的租户治理收进一个组件，落在 gsb/batch50-go-safety-c-10 的 resctrl_tenant_guard.go，类型 ResctrlTenantGuard，包名 task。

公开接口保持：NewResctrlTenantGuard(GuardLimits) *ResctrlTenantGuard、RegisterNode(nodeID string, domains []Domain, mbpsCap int) error、Node(nodeID string) (NodeInfo, bool)、Create(tenantID string, spec ResctrlSpec) (string, error)、Delete(groupID string) error、Assign(pid int, groupID string) error、Detach(pid int) error、Rebalance(nodeID string) (int, error)、Stats(groupID string) (Counters, error)、Group(groupID string) (Group, bool)、Groups(nodeID string) []Group、SetOfflineCPUs(nodeID string, cpus []int) error、Sample(groupID string, raw Counters) (Counters, error)、Snapshot() ([]byte, error)、Restore([]byte) error。

要修的故障与边界：

一、注册节点要求至少一个缓存域、每个域 CPU 非空、带宽容量大于零，非法取值与重复注册都报 ErrBadSpec。

二、租户号为空报 ErrBadTenant，节点未注册报 ErrUnknownNode；不填类别时默认独占。

三、缓存掩码按低位优先的第一段连续空位分配，剩余连续空位不够申请宽度报 ErrMaskExhausted；显式掩码与已有组重叠报 ErrMaskOverlap，超出节点位宽或既没掩码也没宽度报 ErrBadSpec。

四、共享类租户落到同一个组，组名固定为节点号加 shared，后加入的租户按顺序记进 Tenants，不再单独分配掩码与监控标识。

五、组数达到 MaxGroups 时先回收最老且没有任务的组再建新组，确实没有可回收的才报 ErrGroupLimit。

六、Assign 把任务从一个组挪到另一个组，旧组不得残留；pid 非正报 ErrBadPID，组不存在报 ErrUnknownGroup；组内任务按 pid 升序保存。

七、Delete 之前必须先迁走任务，否则报 ErrGroupNotEmpty；删除后掩码与监控标识回到可用池，新建组优先拿回最小的空闲编号。

八、监控标识只有 MaxMonitorIDs 个，按最小的空闲编号分配，组数超过它时循环复用，每个组的统计仍要归属到自己的组。

九、内存带宽按权重换算阈值：组阈值等于本组权重除以节点上所有活跃组权重之和，再乘节点带宽容量；创建或删除组之后所有组的阈值都要重算。

十、Stats 返回该组的带宽阈值、监控标识与代次，组不存在报 ErrUnknownGroup。

十一、Rebalance 回收节点上所有空闲组并返回回收数量。

十二、快照保存节点拓扑、组、掩码、监控标识与任务归属，恢复要么全量成功要么不动现网，坏快照报 ErrBadSnapshot。

另外三条现场口径：任务 fork 之后子进程继承组，exec 之后若加入别的组要重新归属；离线 CPU 对应的缓存域掩码位要置零并重算可用掩码；监控计数会回绕与清零，统计必须做差分，不能把回绕当成负增长。

硬约束：掩码分配与任务归属变更是一次原子提交，任务不出现统计丢失或双记；超额申请按回收顺序让位，先回收空闲组额度再报错；单节点组数、监控标识数与掩码位数设上限，超限直接拒绝而不是降级分配。

验收：go test ./... 15 条全过，测试不得修改、跳过或按固定输入写死；并发创建与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-10`
- 实现文件：`resctrl_tenant_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`ResctrlTenantGuard`
