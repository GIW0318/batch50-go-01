# eBPF 加载权限闸门：指令白名单、map 访问与 attach 目标校验

## 题面

节点安全基线把 eBPF 加载改成先过闸门再进内核，灰度到第二批机器后出了两类事故：一个被篡改过的采集程序带着未列入白名单的指令照样加载成功，在宿主机上跑了一周才被发现；另一个租户反复重试加载同一个程序，把共享的加载配额吃满，旁边的日志采集全部起不来。灰度方案是每个采集代理在启动路径上同步调用闸门，加载失败就退出进程，所以误拒和超发都是线上事故；上线一周里还出现过两个租户互相顶掉对方加载名额，值班只能手工重启采集代理。落点是 gsb/batch50-go-safety-d-03 的 ebpf_load_gate.go，类型 EbpfLoadGate。

公开签名保持不变：NewEbpfLoadGate(GatePolicy) *EbpfLoadGate；RegisterMap(MapSpec) error；CheckMapAccess(tenant, name string, write bool) error；CheckInstructions(Program) error；Fingerprint(Program) string；Quota(tenant string) (used int, limit int)；Load(tenant string, prog Program) (Handle, error)；Attach(handleID string, target AttachTarget) error；Detach(handleID string) error；Unload(handleID string) error；Handle(id string) (Handle, bool)；Attached(tenant string) []Handle；Stats() GateStats；Snapshot() ([]byte, error)；Restore([]byte) error。

调度口径：Quota 返回的 used 只统计当前句柄表里的程序，Unload 之后名额要还回来；Handle 与 Attached 返回的是拷贝，调用方改它不能影响闸门内部状态；Stats 里的 Loaded、Rejected、Attached、Unloaded 用来和节点台账对账，只增不减。检查顺序按指令、类型、许可、map、配额推进，任一关不过都不能创建句柄。

要处理的故障与边界：一、租户为空报 ErrBlankTenant；二、程序名为空报 ErrBlankProgramName，指令为空报 ErrEmptyProgram；三、指令操作码不在 AllowedOps 里报 ErrOpNotAllowed；四、指令条数超过 MaxInsns 报 ErrProgramTooLarge，不截断；五、程序类型不在 AllowedProgTypes 里报 ErrProgTypeDenied；六、许可与 RequiredLicense 不一致报 ErrLicenseRequired；七、引用未登记 map 报 ErrUnknownMap，引用别租户的 map 报 ErrMapTenantMismatch，以写方式访问只读 map 报 ErrMapReadOnly；八、单个程序引用的 map 数超过 MaxMapsPerProg 报 ErrTooManyMaps；九、租户已加载数达到 MaxPrograms 报 ErrQuotaExceeded，不淘汰在跑的程序；十、同一程序重复加载必须按指纹幂等，不得重复占配额；十一、attach 目标名字为空报 ErrBadAttachTarget，单个程序挂载目标数超过 MaxAttachTargets 报 ErrAttachLimit；十二、仍挂着目标的程序卸载报 ErrStillAttached，没有目标的 Detach 报 ErrNotAttached，未知句柄一律报 ErrUnknownHandle；加载前先核对打算挂载的目标类型，类型与程序类型不一致必须在加载阶段就拒绝，不能先加载再卸载，也不许留下先记挂载再撤销的中间态。

其中第十条与最后一句是这次事故复盘定的口径，别按每次加载都新占配额或先放行再回滚来实现。

硬约束：多个采集代理会同时为同一租户加载程序，配额判定与句柄登记必须在同一临界区里完成，并发下放行条数不得超过 MaxPrograms；同一租户的 Attached 与 Load 并发时只能看到完整句柄，不得读到半个目标列表。快照要带出句柄表、map 登记与计数器，Restore 之后配额占用与挂载关系与重启前一致，损坏载荷报 ErrBadSnapshot 且现网句柄不动。每个租户的挂载目标总数与句柄数都有硬顶，不能靠淘汰别人来腾位置。

验收：go test ./... 的 15 条用例必须全过，不许改测试、不许跳过、不许按固定输入写死；并发加载与重启自行写程序验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-d-03`
- 实现文件：`ebpf_load_gate.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`EbpfLoadGate`
