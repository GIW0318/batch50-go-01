# batch50-go-safety-c-13

- 模块路径：`gsb/batch50-go-safety-c-13`
- 包名：`task`
- 根文件：`dma_remap_authorizer.go`
- 类型：`DmaRemapAuthorizer`

## 题面

一批显卡直通到虚拟机后，宿主机在热插拔时整机异常重启：同一条 PCIe 桥下的网卡被单独绑给了另一个虚拟机，IOMMU 组被拆开，网卡和显卡互相踩内存；值班同学拔掉设备再插回，旧授权被自动复用，新拓扑上的设备直接拿到了不属于它的 DMA 权限。现场代码在 gsb/batch50-go-safety-c-13 的 dma_remap_authorizer.go，类型 DmaRemapAuthorizer，需要把组完整性判定、临时授权窗口与撤销隔离补全。

接口保持如下签名：NewDmaRemapAuthorizer(limits Limits) *DmaRemapAuthorizer、RegisterDevice(dev DeviceID, group string, functions []Function) error、Detach(dev DeviceID) error、Attach(dev DeviceID, group string) error、Enroll(dev DeviceID, domain string) (Grant, error)、Authorize(grantID string, window time.Duration) (Lease, error)、Revoke(dev DeviceID, reason string) error、GroupStatus(group string) (GroupView, error)、DomainUsage(domain string) Usage、ActiveLeases(domain string) []Lease、Snapshot() ([]byte, error)、Restore([]byte) error。

现场需要落实的行为与边界：

1. 注册设备必须带上 IOMMU 组与至少一个功能，组为空返回 ErrBlankGroup，功能为空返回 ErrNoFunctions，同一设备重复注册返回 ErrDeviceExists。
2. 授权以组为单位：Enroll 一个设备要把同组所有设备一起绑定到目标域，返回的授权里能列出整组设备。
3. 组内只要有一个功能被策略排除，整组拒绝绑定并返回 ErrGroupIncomplete，不能只跳过那个功能把剩下的绑下去。
4. 组内某个设备已经绑到别的域时，再来绑定同组设备要返回 ErrGroupConflict，不允许一个组跨两个域。
5. 单域可绑定设备数、单次窗口时长与单域窗口总时长都有硬上限，超限分别返回 ErrDomainLimit、ErrWindowTooLong、ErrWindowBudget，已生效的授权不受影响。
6. 授权窗口到期后 ActiveLeases 不再列出它；撤销时仍在窗口内的授权一并作废，GroupView 里能看到撤销原因。
7. 撤销必须带原因，空原因返回 ErrBadRevokeReason；撤销未知设备返回 ErrUnknownDevice，撤销后同组设备全部解绑。
8. 设备在窗口内被拔出再插回，功能号相同但拓扑路径变了，旧授权不能自动复用，必须重新登记和授权。
9. 复合设备在同一组里暴露多个功能，其中某个被排除时整组不绑定，原因信息要能落到 GroupView 上，便于值班复核。
10. 同一物理设备经不同 PCI 域枚举到相同厂商标识时，去重键必须包含拓扑路径，不能把两块不同位置的卡当成一块。
11. GroupStatus 对未知组返回 ErrUnknownGroup，Enroll 对未登记设备返回 ErrUnknownDevice，Detach 之后该设备不能再被绑定。
12. 重启后从快照恢复授权关系与窗口剩余，超过宽限期仍未到期的窗口直接失效，不能让陈旧窗口在唤醒后继续生效。

两类硬约束必须满足：同一组的授权与撤销按组串行，组状态迁移与设备绑定要么一起生效要么都不生效，并发登记不同组时不得互相阻塞；快照整体生效或整体不生效，恢复失败不得留下半绑定的组。时钟通过 Limits.Now 注入，为空时取系统时间。

验收方式：在题目目录执行 go test ./...，十五条用例必须全部通过；不得修改测试、跳过用例或按固定输入写死结果；并发授权与重启恢复要自己构造场景验证；只做本地实现。

## 运行

```bash
go test ./...
```

测试位于 `tests/test_dma_remap_authorizer_test.go`，以 `impl` 别名导入本模块。
