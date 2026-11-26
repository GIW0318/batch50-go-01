# 崩溃输入最小化

## 题面

线上崩溃样本从三百多字节缩到十几个字节之后复现不出来，另一个样本缩到最后干脆变成了另一种崩溃，研发照着最小样本修了半天才发现修错了地方。安全组要求把最小化流程做成带等价校验的作业，落点 gsb/batch50-go-safety-c-29，实现文件 crash_minimizer.go，类型 CrashMinimizer，签名固定：NewCrashMinimizer(limits Limits) *CrashMinimizer、Start(sample Sample, cluster ClusterID) (Job, error)、Candidate(jobID string) (Input, error)、Result(jobID string, crashed bool, cluster ClusterID) error、Best(jobID string) (Input, error)、Cancel(jobID string) error、Progress(jobID string) (Progress, error)、Status(jobID string) (string, error)、Snapshot() ([]byte, error)、Restore(data []byte) error，另配 Jobs。

要修的点：一，样本没有数据或没有编号就拒绝，不能开空作业，未知作业统一报 ErrUnknownJob；二，同时进行的作业数量封顶，超出报 ErrJobLimit；三，每次取候选先丢掉当前最好输入的后半段，后半段被否掉再丢前半段，只有回填为同一崩溃簇的那一次才替换最好输入；四，回填必须对应一个在途候选，没有候选时回填报 ErrNoCandidate，同一候选不能回填两次，在途候选没有回填之前再次取候选要返回同一份输入且不额外消耗预算；五，回填说还能崩但崩溃簇变了，必须回退这一步并报 ErrDifferentCrash，不能因为还能崩就当成有效缩减；六，样本带长度前缀或校验和时，删减要连带修正这些结构字段，否则会得到不可解析的样本，长度对不上要按坏样本处理；七，有些崩溃依赖执行次数或环境，单次重放不崩不代表不可复现，关键删减要用小样本多次确认，确认不过就回退；八，重放次数有硬上限，超出报 ErrBudgetExhausted，作业状态转为 budget_exhausted，最好输入原样保留；九，两半都被否掉后作业转为 converged，之后取候选报 ErrConverged，不再要求回填；十，取消后不能再取候选也不能回填，报 ErrJobCancelled，重复取消仍然成功，最好输入保持可读；十一，进度要如实给出重放、接受、拒绝次数与最好输入长度，作业状态只在 running、converged、budget_exhausted、cancelled 四态之间流转；十二，进程重启后恢复最好输入与各项计数，在途候选不跨重启保留，未确认的候选不得直接当成最好结果，损坏的快照整体拒绝且不动现有作业。

硬约束：同一作业的候选生成与结果回填串行执行，同一时刻只允许一个在途候选，别的作业不被卡住的作业拖住；单作业重放次数与单次运行时长都有上限，无限循环视为不合格；快照只记录已确认的状态，恢复后继续缩减要得到与中断前一致的输入序列，不得把重启前没有回填的候选当成已确认结果。

验收：go test ./... 的 15 条用例全过，测试不得修改、跳过或按固定输入写死；并发回填与重启续跑自行验证；只做本地实现，交付源码、跑测记录与一页等价判定说明。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-29`
- 实现文件：`crash_minimizer.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`CrashMinimizer`
