# 二进制格式注册表治理：解释器绑定、magic 掩码冲突与注册原子性

## 题面

值班室接到一条诡异的投诉：同一台打包机上，脚本格式和另一种自解压格式互相顶替，内核把一个本该交给解压器的文件塞给了脚本解释器，进程起来就崩；更麻烦的是容器里把某个格式关掉之后，宿主机上的注册也跟着消失了，两边互相影响，排障时谁都说不清当前到底是哪条注册在生效。现在要把二进制格式注册表的治理收进一个组件，落在 gsb/batch50-go-safety-d-01 的 binfmt_registry_guard.go，类型 BinfmtRegistryGuard，包名 task。

公开接口保持：NewBinfmtRegistryGuard(limits Limits) *BinfmtRegistryGuard、Register(e Entry) (string, error)、Unregister(id string) error、UnregisterIn(ns, id string) error、Resolve(magic []byte) (Entry, bool, error)、ResolveIn(ns string, magic []byte) (Entry, bool, error)、SetEnabled(id string, on bool) error、SetEnabledIn(ns, id string, on bool) error、SetPriority(id string, priority int) error、Entry(id string) (Entry, bool)、Entries() []Entry、NamespaceEntries(ns string) []Entry、Purge(ns string) int、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的故障与边界：

一、magic 与掩码必须等长且非空，长度不超过 MaxMagicLen，掩码之外的位必须为零，违反一律报 ErrBadEntry。

二、解释器必须是绝对路径、不含 ..、落在 AllowedDirs 允许的目录内，空路径也不接受，违反报 ErrBadInterpreter。

三、解释器脚本自身的起始行若落在本条 magic 的匹配范围内，就是自环，必须拒绝并报 ErrInterpreterLoop，不能等运行起来才发现递归。

四、标志位只认 P、O、C、F，未知标志、重复标志、O 与 F 同时出现都报 ErrBadFlags。

五、同一作用域内优先级相同且 magic 与掩码完全相同的重复注册报 ErrMaskConflict，不能静默覆盖；优先级不同允许并存。

六、注册表总量达到 MaxEntries 报 ErrEntryLimit；条目编号与序号由注册表递增分配，删除后编号不复用，条目按注册顺序返回。

七、Resolve 按输入前缀匹配，禁用条目不参与；多条命中时先比优先级，再比掩码有效位，最后按注册先后；掩码全零的通配条目只能兜底，不能吞掉更具体的注册。

八、SetEnabled 与 SetPriority 对未知编号报 ErrUnknownEntry；改优先级若造成同优先级重复要报 ErrMaskConflict 并保持原值。

九、命名空间覆盖条目只在本空间可见，宿主视图看不到；ResolveIn 优先本空间条目，未知命名空间回落到宿主。命名空间里的启用开关只改本空间覆盖，宿主条目的启用状态不受影响，宿主开关也不回写覆盖。

十、Purge 清掉命名空间的全部覆盖并返回条数，宿主注册不受影响；命名空间名非法报 ErrBadNamespace。

十一、Resolve 输入为空或超过上限报 ErrBadEntry，未命中返回零值与 false 而不是报错。

十二、快照保存宿主条目、覆盖条目与计数器，恢复要么全量成功要么不动现网，坏快照报 ErrBadSnapshot，恢复后编号继续递增。

硬约束：并发注册串行分配编号，同一条目不会被两次登记；快照与注册互斥，读到的不是半新半旧；条目总量与 magic 长度设上限，超限直接拒绝而不是降级。

验收：go test ./... 15 条全过，测试不得修改、跳过或按固定输入写死；并发注册与重启恢复自行构造场景验证；只做本地实现，交付源码与跑测结果。

## 运行

模块路径 `gsb/batch50-go-safety-d-01`，包名 `task`，实现文件 `binfmt_registry_guard.go`，测试在 `tests/test_binfmt_registry_guard_test.go`。

```bash
go test ./...
```
