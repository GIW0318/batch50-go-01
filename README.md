# 多沙箱行为报告归一与共识裁决

## 题面

客服转来一起样本定性纠纷：同一批样本在三家沙箱里跑出了互相打架的结论，运营照着最新一份报告处置，把已经确认的勒索行为放成了低危；另有一次，第四家沙箱的报告晚到两个小时，处置单上的结论被悄悄换掉，值班同学完全不知道依据变过。现场代码在 gsb/batch50-go-safety-c-31 的 sandbox_report_normalizer.go，类型 SandboxReportNormalizer，要把同一样本的多份沙箱报告归一成行为序列，给出可回溯的裁决，并在重启后接着做没做完的合并。

接口保持这些签名：NewSandboxReportNormalizer(limits Limits, dict []DictionaryEntry) *SandboxReportNormalizer、Ingest(report Report) (Normalized, error)、Expect(sampleID string, sandboxes []string) error、Merge(sampleID string) (Consensus, error)、Resolve(sampleID string, topic string) (Decision, error)、Explain(sampleID string, topic string) (Evidence, error)、Expire(sampleID string) error、Pending(sampleID string) int、State(sampleID string) (SampleState, bool)、Snapshot() ([]byte, error)、Restore([]byte) error。

要修的行为与边界：

1. 报告号、样本号、沙箱名任一为空，或结论不是 malicious/clean/unknown，分别按 ErrEmptyReport、ErrBadVerdict 拒收，且不占样本的在途名额；同一份报告重复投递只算一次，返回 ErrDuplicateReport，在途计数不增长。
2. 行为条目超过 MaxBehaviorsPerReport 时整份拒收 ErrBehaviorLimit，不能只留前几条，也不能因此建出样本。
3. 单样本报告数与同时跟踪的样本数分别由 ErrReportLimit、ErrSampleLimit 顶住，超限拒新、不动已有。
4. 行为词典按（沙箱，API）精确匹配优先、通配（*，API）其次映射成能力标签，未命中的归 unclassified；未在词典里登记的沙箱不得把整份行为丢掉，要落到通配项或 unclassified；同一标签内的多条行为合并计数，标签按名字升序。
5. Merge 只在 Expect 声明的沙箱到齐、或首个报告时间加 MergeWindow 已过时生成共识，否则 ErrPendingConsensus 并保留已到报告；共识未发布前 Resolve 返回 ErrNoConsensus。
6. 共识中的报告数、参与沙箱、各标签计数与输入一致，沙箱列表升序；窗口过期生成的共识要标出未到齐，不能当成完整结论。
7. 同一标签按参与沙箱的结论多数裁决，全票给 reason_unanimous，多数给 reason_majority；平票一律 ErrUnresolved，不允许按到达顺序猜。
8. 不同沙箱对同一 API 的命名不同，词典映射必须保留原始 API 名，Explain 出的证据要能回到具体报告与原始命名。
9. 迟到的报告不能让已发布的共识被静默覆盖：要么重算成新版本，要么在共识上标版本号，让调用方看得见依据变过。
10. 沙箱环境差异导致某个能力没被观察到，和明确判定为干净不是一回事，缺失与否定必须分开表达，Explain 要说明该能力来自哪些沙箱。
11. 未知样本的 Merge、Resolve、Explain 一律 ErrUnknownSample；Expire 之后该样本的在途报告与状态一起清空。
12. 同一样本的 Ingest 与 Merge 串行执行；重启恢复后未完成的合并沿用原来的截止时间，不重新计时；损坏快照 Restore 返回 ErrBadSnapshot 且不动现有状态。

硬约束：单样本在途报告数、行为条目数、样本数三项上限必须在并发投递下精确生效，不能靠事后裁剪；判定、共识发布与快照互斥，快照只能看到全量旧状态或全量新状态，截止时间与在途报告跨重启恢复。

验收：go test ./... 15 条全过；不得修改测试、跳过或用固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-31`
- 实现文件：`sandbox_report_normalizer.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SandboxReportNormalizer`；辅助类型 `Limits`、`DictionaryEntry`、`Report`、`Behavior`、`Consensus`、`Decision`、`Evidence`、`SampleState`
