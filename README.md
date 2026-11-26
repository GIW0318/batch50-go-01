# 容器设备准入规则的编译与整代更新

## 题面

容器平台这周的设备准入连出三起事故：一个作业把自己改成整机放行之后，旧程序还在生效，宿主机上的块设备照样能被读到；另一个作业提交的规则内容相同、只是书写顺序不同，两台节点编译出的摘要对不上，灰度只推了一半；还有一次设备热插拔之后主次设备号被新设备复用，本该被拒绝的节点按缓存里的老判定放了过去。请在 gsb/batch50-go-safety-c-03 的 device_cgroup_controller.go 里补完 DeviceCgroupController，负责编译规则、下发程序并按代次裁决。

对外接口保持：NewDeviceCgroupController(limits Limits) *DeviceCgroupController；Compile(policy DevicePolicy) (Program, error)；Attach(containerID string, prog Program) error；Update(containerID string, policy DevicePolicy) (Version, error)；Explain(containerID string, dev Device) (Verdict, Reason)；Program(containerID string) (Program, bool)；CurrentVersion(containerID string) Version；Detach(containerID string) error；Containers() []string；CompileCacheSize() int；Export() ([]byte, error) 与 Import([]byte) error。

要修的地方：一，编译产物号以 prog- 开头，记下修订号、规则条数与摘要，指令序列里拒绝指令必须排在放行指令之前；二，同一策略无论调用方按什么顺序书写规则，产出的摘要与产物号都要一致；三，设备类别只认 a、b、c，其他类别返回 ErrBadDeviceType，空规则表或修订号小于一的策略返回 ErrBadPolicy；四，单容器规则数超过 MaxRulesPerContainer 返回 ErrRuleLimit；五，Attach 只接受 Compile 产出的程序，零值程序返回 ErrBadProgram，容器数超过 MaxContainers 返回 ErrContainerLimit，挂上之后生效代次等于程序的修订号；六，未匹配任何规则时按默认拒绝裁决，原因码 default_deny，裁决里要带生效程序号与代次；七，拒绝规则压过放行规则，同一设备上两者同时命中时结论必须是拒绝，原因码 exact_deny；八，通配放行的规则要能命中同类别任意主次号的设备，原因码 wildcard_allow；九，Update 用策略里的修订号做乐观并发，修订号不大于当前生效代次的写入返回 ErrStaleRevision 且不改动现网；十，编译失败时上一代程序继续可用、容器不掉线，Detach 之后容器不再参与裁决、原因码 no_container，对未挂载容器执行 Update 返回 ErrNoContainer；十一，通配规则与精确规则同时命中同一台设备时不能依赖规则在切片里的先后，必须按显式优先级裁决，优先级相同时精确规则优先；十二，设备号热插拔后会被复用，判定要结合设备类别而不能只比对主次号，容器里的设备节点若是绑定挂载进来的，归属按宿主机侧 inode 判断而不是容器内路径。

硬约束：更新按版本号乐观并发，旧代次写入一律拒绝，回读到的生效版本必须是刚装上的那一代，中途失败要整代回退；单节点容器数与编译缓存条目都受上限约束，缓存超限按最久未用淘汰，不能让编译因此失败。已打开的描述符不受新规则影响，但新的打开必须按新代次判定；Export 与 Import 负责搬运容器表与程序，导入之后摘要与代次一致，损坏载荷返回 ErrBadSnapshot 且不动现网。

验收：go test ./... 十五条全过，测试不得修改、跳过或按固定输入写死；并发更新、热插拔复用与重启导出导入自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-03`
- 实现文件：`device_cgroup_controller.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`DeviceCgroupController`、`DevicePolicy`、`Rule`、`Program`、`Verdict`、`Reason`
