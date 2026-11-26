# 节点内核启动参数基线：求值、原子写入与重启回读核验

## 题面

值班收到两张单子：一批节点改完内核启动参数后重启进不了系统，另一批节点重启后参数悄悄退回旧值，值班同学反复改引导项，却没人说得清究竟哪一代才真正生效。更麻烦的是掉电重启过的那台机器，引导项一半新一半旧，值班同学也判断不出它到底算哪一代。现在要把节点启动参数的基线生成、变更、核验和回滚收进一个组件，落在 gsb/batch50-go-safety-c-06 的 boot_param_policy.go，类型 BootParamPolicy，包名 task。

公开接口保持：NewBootParamPolicy(PolicyLimits) *BootParamPolicy、RegisterNode(nodeID, role string) error、SetRole(nodeID, role string) error、Baseline(nodeID string) (ParamSet, error)、Plan(nodeID string, params ParamSet) (ChangePlan, error)、Apply(nodeID string, plan ChangePlan) error、Verify(nodeID string) (Drift, error)、Rollback(nodeID string) error、ReportRunning(nodeID, cmdline string) error、Generations(nodeID string) ([]Generation, error)、Current(nodeID string) (ParamSet, error)、Render(nodeID string, index int) (string, error)、BatchApply(nodeIDs []string, plans map[string]ChangePlan) (BatchResult, error)。

要修的故障与边界：

一、基线按角色分档，compute、storage、control 的审计、IOMMU、NUMA 均衡、panic 取值互不相同；未注册节点报 ErrUnknownNode；角色非法报 ErrBadRole，且已登记的角色不被改写。

二、基线按参数名升序排列；SetRole 之后基线重算，但已经生效的运行参数不动。

三、Plan 只给增量：新增参数按名字升序放进 Add，被删掉的键放进 Remove，Generation 取节点当前代次，Fingerprint 必须非空。

四、危险取值一律报 ErrForbiddenParam：救援 shell 置一、IOMMU 关闭、审计关闭、缓解措施关闭、模块黑名单为空或点名 nvme、ahci、控制台日志级别越过一到四。

五、单条参数值超过 MaxValueLen、条目数超过 MaxParams 报 ErrParamLimit，恰好等于上限必须放行。

六、Apply 只接受本节点的计划，拿错节点报 ErrBadPlan；同一代次被抢先提交后，后到的那份报 ErrStalePlan。

七、运行期可改的参数（控制台日志级别、panic、NUMA 均衡）写入后立即反映到 Current，其余参数进入待重启列表，由 Verify 的 Pending 逐条列出，不能假装已经生效。

八、Render 输出按参数名升序的引导条目，值里带空格必须加双引号，同一代次重复渲染结果一致。

九、ReportRunning 解析节点上报的命令行，Verify 拿它和最新一代比对，分别给出 Missing、Extra、Differs，逐键带上期望值与实际值；引导器默认条目与实际启动条目不一致时以实际启动为准；内核自动追加的镜像路径类参数不参与漂移判定。

十、Rollback 生成新的一代并回到上一代留存的内容，没有可回退的代次时报 ErrNoGeneration；已淘汰的代次不能凭空恢复。

十一、Generations 只保留 MaxGenerations 个代次，最旧的先淘汰，返回按代次升序。

十二、BatchApply 按 BatchSize 分批推进，一批全成才进下一批，某批失败立即停下，返回已完成的节点与失败节点并报 ErrBatchAborted，后续节点保持原样。

硬约束：Apply、Rollback 与 Generations 互斥，并发提交同一代次的计划只允许一个成功，其余报 ErrStalePlan；写引导配置与推进代次是一次原子提交，中途出错不得留下半代状态；节点数与上报命令行长度受上限约束，超限直接拒绝而不是截断。

验收：go test ./... 15 条全过，测试不得修改、跳过或按固定输入写死；并发与重启回读自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-06`
- 实现文件：`boot_param_policy.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`BootParamPolicy`
