# 原始内存与 IO 设备节点：打开授权、区间审计与描述符回收

## 题面

一台缓存异常的机器上，两个不相干的作业同时打开了 /dev/mem，其中一个还把地址区间写到了设备边界之外，事后既查不出是谁批的，也查不到工单号，更说不清描述符最后有没有回收。现在要把原始内存、端口与 MSR 设备节点的打开治理收进一个组件，落在 gsb/batch50-go-safety-c-09 的 devmem_access_guard.go，类型 DevmemAccessGuard，包名 task。

公开接口保持：NewDevmemAccessGuard(GuardLimits) *DevmemAccessGuard、AddDevice(dev Device) error、SetNow(tick int64) error、SetWindow(owner string, open, close int64) error、Authorize(caller Caller, dev Device, rw int) (Grant, error)、Open(grantID string) (File, error)、Close(fd int) error、Revoke(owner string) error、Reclaim(fd int) error、Audit() []Access、Grants(owner string) []Grant、Files(owner string) []File、Quarantined() []File、Snapshot() ([]byte, error)、Restore([]byte) error。Device 同一条记录两用：登记时填 Node、Kind、Base、Size、HostInode；申请时再填 Offset、Length 表示本次要的区间，偏移相对 Base。

要修的故障与边界：

一、登记设备要求节点名非空、Kind 属于 mem、port、msr、Size 大于零，重复登记与非法取值报 ErrBadRequest。

二、申请必须带工单号，空工单报 ErrNoTicket；调用方缺少该设备类别对应的能力（cap_rawio、cap_portio、cap_msr）报 ErrNoCapability。

三、维护窗口按调用方登记，未登记窗口或逻辑时钟落在窗口之外报 ErrWindowClosed，窗口起止必须严格递增，否则 ErrBadRequest。

四、申请区间必须落在设备登记的严格范围内，长度为零或为负、越界、负偏移一律报 ErrOutOfRange。

五、单次区间长度超过 MaxRangeLen 报 ErrRangeTooLong，恰好等于上限必须放行。

六、同一调用方未回收的授权数超过 MaxGrantsPerOwner 报 ErrGrantLimit。

七、每次授权、打开、拒绝、撤销、回收都要进审计，审计序号递增，记录调用方、控制组、设备、区间、读写模式与工单号。

八、审计队列满时必须阻断新的打开并报 ErrAuditFull，不许丢记录，也不许把这条授权悄悄消费掉。

九、一个授权只能打开一次，重复打开报 ErrGrantConsumed；未知授权报 ErrUnknownGrant。

十、撤销后该调用方所有授权失效，新的打开报 ErrGrantRevoked，已在途的描述符标成待回收并出现在 Quarantined 里，Reclaim 之后才真正消失；撤销之后重新授权要拿到更高的代次。

十一、Close 只释放还活着的描述符，重复关闭报 ErrUnknownFile；对既没有授权也没有描述符的调用方撤销报 ErrUnknownOwner。

十二、快照保存设备、授权、描述符与审计，恢复要么全量成功要么不动现网，坏快照报 ErrBadSnapshot。

另外三条现场口径：通过继承描述符可以绕过打开检查，必须按持有者与代次追踪，不能只看授权表；内核内存接口的读偏移没有区间语义，这类设备只能整体拒绝，不做部分授权；容器内设备节点是绑定挂载时，归属按宿主机 inode 判定，不按容器内路径。

硬约束：授权与打开要原子，撤销开始之后不允许再有新的打开成功，已经在途的描述符统一标成待回收；审计缓冲区只增不覆盖，满了就阻断打开；同时有效的授权数、单次区间长度与审计保留条数都设上限，超限直接拒绝而不是截断放行。

验收：go test ./... 15 条全过，测试不得修改、跳过或按固定输入写死；并发打开与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-09`
- 实现文件：`devmem_access_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`DevmemAccessGuard`
