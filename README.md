# gsb/batch50-go-safety-c-25

## 题面

模糊测试平台每晚产出几十份崩溃样本，安全同学按栈顶函数名去重，把同一根因在不同编译优化下的样本拆成好几个簇，又把两个函数名相同但故障地址不同的样本并成一簇，分级结果来回跳；一次误操作合并两个簇后，原先挂在旧簇上的工单全部失联；还有人手工把低复现样本直接标成可利用，系统没有任何约束。复盘要求：去重落到故障地址与寄存器，分级只能升不能降，下调必须走复审。

需要在 gsb/batch50-go-safety-c-25 的 crash_triage_classifier.go 里补全 CrashTriageClassifier，入口 NewCrashTriageClassifier(Options) *CrashTriageClassifier。公开接口：Submit(Sample) (string, error)；Cluster(string) (string, error)；Grade(string) (Grade, error)；Recheck(string) (Grade, error)；Merge(string, string) error；Clusters() []ClusterView；Crashes(string) []string；History(string) []GradeChange；Snapshot() ([]byte, error)；Restore([]byte) error。

入库阶段：Submit 校验 InputID 非空白、至少一帧栈、故障地址非空白，否则 ErrBlankSample；指纹由栈帧的模块、函数与偏移、故障地址以及按键排序后的寄存器键值共同决定，输入编号、写入可控性与复现率不参与；指纹相同归入同一簇，Crashes 按提交顺序返回，新簇超过 MaxClusters 返回 ErrClusterLimit。单簇只保留最新 MaxSamplesPerCluster 份样本，更早样本连同编号一起淘汰，其后按编号查询返回 ErrUnknownCrash。

分级阶段：保留样本数少于 MinSamplesForGrade 时 Grade 返回 ErrInsufficientEvidence。计算规则：全部保留样本复现率都低于 MinReproducibility 时只给 tentative（1）；否则只要有样本写入可控为真就给 exploitable（3），其余给 probable（2）；等级用整数与标签同时表达，none 为 0。Grade 只允许上调：计算值高于当前值时写回并追加 reason_grade_raised；低于当前值时保持原等级并追加 reason_grade_kept_monotonic；相等时不追加历史，原因沿用保持。Recheck 是正式复审，可以下调，下调追加 reason_recheck_downgrade，不下调追加 reason_recheck_keep；每条历史带从 1 开始递增的序号。

合并阶段：Merge(a, b) 把 a 的样本按原顺序接到 b 之后，a 标记为已合并并退出活跃簇列表，b 的分级与历史保持不变；a 与 b 相同返回 ErrMergeSelf，任一簇未知返回 ErrUnknownCluster，再次合并已合并的簇返回 ErrClusterMerged；已合并簇不占 MaxClusters 名额。

现场还有三类容易漏判的情况：同一根因在不同编译优化下栈帧不同，指纹要含故障地址偏移与寄存器状态，不能只看函数名；样本复现率低时分级只能标为暂定，复审通过后才能上调为可利用；两个簇合并后，原先挂在源簇上的工单与告警要重定向到目标簇，不能失效。

硬约束：Submit、Grade、Merge 并发调用时簇归属、保留窗口与分级历史必须一致，不能出现同一编号落进两个簇；重启后恢复簇归属、保留窗口与完整分级历史，未完成的分级从已保留样本重算；簇数量与单簇保留数受上限约束，触顶给明确错误码。

验收口径：go test ./... 的 15 条用例必须全过；不得修改测试、跳过或按固定输入写死；并发与重启自行验证；只做本地实现。交付源码、测试输出与故障说明。

## 运行

```bash
go test ./...
```

## 模块

- 模块路径：`gsb/batch50-go-safety-c-25`
- 包名：`task`
- 实现文件：`crash_triage_classifier.go`
- 测试文件：`tests/test_crash_triage_classifier_test.go`
