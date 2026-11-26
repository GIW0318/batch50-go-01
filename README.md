# batch50-go-safety-c-11

- 模块路径：`gsb/batch50-go-safety-c-11`
- 包名：`task`
- 根文件：`wx_region_warden.go`
- 类型：`WxRegionWarden`

## 题面

某云厂商的宿主机安全巡检上线后，一批 Java 服务的即时编译线程连续被降权，进程在几分钟内全部崩溃；而另一批带 JIT 白名单的进程被完全放过，攻击者用匿名可执行内存落地载荷也没有任何告警。现场代码在 gsb/batch50-go-safety-c-11 的 wx_region_warden.go，类型 WxRegionWarden，需要把进程登记、映射巡检分级、JIT 豁免租约与快照恢复这条链路补全。

接口保持如下签名：NewWxRegionWarden(limits Limits) *WxRegionWarden、Register(pid int, profile Profile) (Handle, error)、Unregister(h Handle) error、Feed(h Handle, maps []Mapping) (int, error)、Scan(h Handle) (Report, error)、Classify(m Mapping) (Region, error)、GrantJitLease(h Handle, size uint64, ttl time.Duration) (Lease, error)、ReleaseLease(leaseID string) error、LeaseState(leaseID string) (Lease, error)、Disposition(h Handle, regionID string) (Action, string, error)、Restrict(h Handle, regionID string) (Action, string, error)、Regions(h Handle) []Region、Summary(tenant string) Summary、Snapshot() ([]byte, error)、Restore([]byte) error。

现场需要落实的行为与边界：

1. 只有同时可写且可执行的映射才进入报告，只写的堆段和只执行的文件段一律不处置。
2. 可疑匿名映射首次处置为告警档，原因码 reason_anon_wx；进程持有活跃 JIT 租约时降到观察档，原因码 reason_jit_lease_active。
3. 处置等级只能向更严的一侧移动，某个区域被降权后，下一轮即使看起来正常也不得回到观察档。
4. 租约只签给 profile 中 AllowJit 为真的进程，否则返回错误并带 reason_jit_denied；单进程租约条数与单条租约字节数都有硬上限，超限只拒新增，已有租约保留。
5. 同一租约重复释放返回错误，未知租约、未知区域、未知句柄各有独立错误值，不能一律返回 nil。
6. 降权遇到仍有效的租约、或区域不属于该进程时拒绝执行，并回 reason_restrict_denied 或 reason_region_not_owned，调用方要能从原因码看出为什么没降。
7. 映射表由 Feed 灌入，下一次 Scan 才生效；Start 不小于 End 的映射整批拒绝，不写入半份。
8. 租约到期后 LeaseState 必须报 expired；此时进程可能正在该区域执行，降权要推迟到安全点，期间只登记一次宽限 reason_lease_grace。
9. JIT 运行时常用两个别名映射同一物理页，一个只写、一个只执行，单看每条都不违规，必须按底层对象识别成同一区域，不能当成两块独立映射重复告警。
10. 匿名映射被 fork 后子进程会继承，父进程的降权不得误伤子进程中合法的 JIT 区域。
11. 重启后从快照恢复租约截止时间与各区域处置水位，未过期的租约继续有效；恢复失败要整体回退，不留半个状态。
12. 句柄由 pid、租户与注册序号组成，恢复后按 pid 与租户能重新对上进程，原句柄仍可用于查询。

两类硬约束必须满足：同一进程的 Feed、Scan 与租约变更串行执行，并发巡检不得看到半份映射表；快照整体生效或整体不生效，恢复过程失败不得留下部分记录。时钟通过 Limits.Now 注入，为空时取系统时间，租约到期与宽限都按它计算。

验收方式：在题目目录执行 go test ./...，十五条用例必须全部通过；不得修改测试、跳过用例或按固定输入写死结果；并发巡检与重启恢复要自己构造场景验证；只做本地实现。

## 运行

```bash
go test ./...
```

测试位于 `tests/test_wx_region_warden_test.go`，以 `impl` 别名导入本模块。
