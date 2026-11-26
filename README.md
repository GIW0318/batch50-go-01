# 文件能力集治理：白名单、继承位与提权回收

## 题面

运维上周给边缘节点批量下发了一轮 capability 变更，事后复盘出了两起事故：同一台机器上的调试二进制既留着 setuid 位又挂着 cap_net_raw，安全组判定为双重提权；另一台机器上带继承位的采集程序 exec 之后，子进程看到的能力比变更单里批准的还多。值班同学只能人工逐台回滚，回滚完又有三台机器在重启后恢复成了旧配置。变更单本身没有错，落点在模块 gsb/batch50-go-safety-d-07 的 file_capability_guard.go，类型 FileCapabilityGuard，公开接口保持不变：NewFileCapabilityGuard(policy CapabilityPolicy) *FileCapabilityGuard、RegisterFile(path string, caps []string, flags FileFlags) error、RemoveFile(path string) error、Grant(path, capability string) error、Revoke(path, capability string) error、SetFlags(path string, flags FileFlags) error、File(path string) (FileRecord, bool)、Files() []FileRecord、EffectiveCaps(path string) []string、ReloadPolicy(policy CapabilityPolicy) error、Reconcile() []Violation、Repair(path string) (RepairReport, error)、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的口径如下。第一，路径为空白报 ErrBlankPath，重复登记同一路径报 ErrDuplicateFile，登记数超过 MaxFiles 报 ErrFileLimit，被拒绝的请求不得留下半条记录。第二，能力不在 Allowed 白名单内报 ErrCapNotAllowed，单个路径超过 MaxPerFile 报 ErrCapLimit，同一请求里重复写同一个能力只保留一份。第三，登记时 setuid 位与能力同时出现直接报 ErrSetUIDWithCaps，不允许两种提权手段并存。第四，Grant 追加能力：已存在的能力重复授予幂等成功且不改变 Gen，未知路径报 ErrUnknownFile，越界报 ErrCapLimit；Revoke 撤销不存在的能力报 ErrCapAbsent。第五，SetFlags 写入完全相同的标志幂等成功且不涨 Gen，把 setuid 位开到仍有能力的路径上报 ErrSetUIDWithCaps。第六，每条记录的 Caps 与 Files 的返回顺序都按升序稳定输出，Gen 从 1 起、每次真实改动加一，File 查不到时返回 false。

还有两处必须一起收掉。后台的 Grant 给一个仍带 setuid 位的路径追加能力时，必须顺手把 setuid 位清掉，不能让两种提权路径在同一文件上共存。带继承位的文件在计算可执行能力时，只能保留 InheritAllowed 里的那部分，白名单之外或继承白名单之外的能力不能跟着 exec 传到子进程；Reconcile 要把这种继承位超范围的情况报成 unbounded_inherit，Repair 只能收窄继承位，不能反过来放宽白名单。

第七，ReloadPolicy 收窄白名单时不得改写已登记记录，Allowed 为空、上限非正、InheritAllowed 不是 Allowed 子集都报 ErrBadPolicy 且原策略继续生效。第八，Reconcile 按路径、类型、能力升序报告 unlisted_cap 与 unbounded_inherit，干净时返回空。第九，Repair 删除白名单外能力、收窄继承位、清掉与能力并存的 setuid 位，返回被移除的能力清单，未知路径报 ErrUnknownFile，修完之后 Reconcile 必须为空。

硬约束两条。一是并发：同一个路径上的 Grant、Revoke、SetFlags 必须串行结算，并发授予同一批能力最后只能留下一份排序好的集合，不能出现重复能力或丢更新。二是重启：快照要带上白名单、每条记录的路径、能力、标志与 Gen，恢复后 Files、EffectiveCaps 与 Reconcile 结果必须与快照前一致；损坏的载荷报 ErrBadSnapshot，且不能动到现网表。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-d-07`
- 实现文件：`file_capability_guard.go`
- 测试目录：`tests/`，通过导入路径引用本模块
- 包名：`task`；主要类型：`FileCapabilityGuard`、`CapabilityPolicy`、`FileRecord`、`FileFlags`、`Violation`、`RepairReport`
