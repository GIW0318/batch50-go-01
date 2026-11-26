# 动态链接前置注入面核查：预加载、搜索路径与缓存一致性

## 题面

安全巡检报上来两件事：镜像里多出一条全局预加载条目，镜像构建完之后还有人在库目录里塞了自编译的库；更糟的是安全同事手工把预加载文件清空后，作业重启又被同一批库注入回来，取证材料一点没留下。现在要把动态链接前置注入面的核查与修复收进一个组件，落在 gsb/batch50-go-safety-c-07 的 linker_injection_guard.go，类型 LinkerInjectionGuard，包名 task。

公开接口保持：NewLinkerInjectionGuard(GuardLimits) *LinkerInjectionGuard、AddRoot(root string) error、Scan(rootfs string) (Findings, error)、ScanAll(roots []string) (Findings, error)、Guard(ExecContext) error、Repair(rootfs string, f Finding) error、Watch(paths []string) error、Watched() []string、CheckEnv(env []string) Findings、CheckRunPath(rpath string) Findings、Block(binary, reason string) error、Blocked() []string、Audit() []AuditRecord。

要修的故障与边界：

一、预加载文件存在但只有空行与井号注释时不算问题，出现任何有效条目就报 KindPreloadFile，路径写 /etc/ld.so.preload，明细带上违规的库路径；静态链接的程序没有解释器，预加载语义照样作用于它，不能因为找不到解释器就跳过。

二、LD_PRELOAD、LD_AUDIT、LD_LIBRARY_PATH 只允许受控值，默认策略是 /usr/lib/gsb/libtrace.so、空、/usr/lib/gsb，其它取值报 KindEnvHijack 并在明细里点名变量。

三、运行时搜索路径里出现相对路径或单独的裸点报 KindRunPath，全绝对路径放行。

四、库目录对其他用户可写报 KindWritableLibDir。

五、缓存清单是纯文本，每行一个根内绝对路径加 sha256；清单与目录内实际库的摘要不一致或库缺失报 KindCacheMismatch。

六、单次扫描遍历到的普通文件数超过 MaxFiles、单个文件字节数超过 MaxFileSize 一律报 ErrScanLimit，不允许悄悄截断继续扫。

七、扫描结果按路径升序再按类型升序排列，ScanAll 合并多个根的结果并按同一顺序输出；通过内存文件或 /proc 下的描述符加载的库不在文件系统扫描范围内，这类来源只在启动快照里体现。

八、作业启动前先按快照校验：环境变量不合规报 ErrEnvHijack，快照里仍有未处理的高危发现报 ErrLaunchBlocked，被拦下的作业不得执行。

九、启动后必须复核关键路径摘要，作业执行期间预加载文件被改写报 ErrSnapshotChanged，不能只看启动前那一眼。

十、Repair 原子替换目标文件，原文件搬到 var/lib/gsb-quarantine 下留作取证；重复修复要幂等，不能把已经空了的文件再覆盖掉原始证据；目标文件不存在报 ErrFindingGone；容器内配置目录是只读挂载时修复要走覆盖层，不能直接写底层。

十一、Watch 一次登记多个路径，超过 MaxWatch 整体拒绝并报 ErrWatchLimit，空请求报 ErrNoWatch；Watched 按路径升序返回。

十二、Block 登记的二进制在解封前一律拒绝启动；Blocked 按名字升序返回。

硬约束：扫描与作业启动的竞态必须收住，启动前按快照校验、启动后复核关键路径，两次校验之间不得让作业看到中间态；修复写入走同目录临时文件加改名，掉电后要么是旧内容要么是新内容；单次扫描文件数、单文件大小、并发扫描数与观察路径数都设上限，超限直接拒绝而不是降级。

验收：go test ./... 15 条全过，测试不得修改、跳过或按固定输入写死；并发与重扫自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-07`
- 实现文件：`linker_injection_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`LinkerInjectionGuard`
