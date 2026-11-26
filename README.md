# 扫描源行为聚类与活动生命周期

## 题面

安全运营季度复盘时发现，一起横向扫描整整跑了三天才被看见：攻击者轮换四十多个源地址，每个地址只慢悠悠探三个目标，按单源阈值看全都正常，告警一条没出。另一头，内部监控探针的例行巡检被误判成扫描，值班同学连着关了三次工单；等真扫描被合并处理时，同一段活动又生成了两个工单编号，资产方拿着两个号来问到底以哪个为准。现场代码在 gsb/batch50-go-safety-c-35 的 scan_source_clusterer.go，类型 ScanSourceClusterer，要把分散的探测事件按目标集合与时间节律聚类成扫描活动，识别轮换源地址的低速扫描并维护活动生命周期。

接口保持这些签名：NewScanSourceClusterer(limits Limits) *ScanSourceClusterer、Ingest(ev ProbeEvent) (string, error)、Cluster(activityID string) (string, error)、Merge(a, b string) (string, error)、Explain(clusterID string) (Features, error)、CoolDown(clusterID string, ttl time.Duration) error、Expire() (int, error)、Events(activityID string) ([]ProbeEvent, error)、Activities() []string、Archived() []string、Snapshot() ([]byte, error)、Restore([]byte) error。

要修的行为与边界：

1. 事件的源地址或目标为空，Ingest 返回 ErrEmptyEvent；同一事件编号重复投递返回 ErrDuplicateEvent，存量事件不重复计数。
2. 同一来源的事件收进同一活动，编号稳定；事件按发生时间升序保存，乱序到达也要排好，Events 返回副本而不是内部切片。
3. 跟踪活动数超过 MaxTrackedActivities 返回 ErrActivityLimit；单活动事件超过 MaxEventsPerActivity 返回 ErrEventLimit，超限拒新、已有事件保留。
4. Cluster 要求活动至少有 MinEventsToCluster 条事件，否则 ErrInsufficientEvidence；同一活动重复 Cluster 返回同一个簇编号。
5. Cluster、Merge、Explain、CoolDown 对未知对象给独立错误：活动不存在是 ErrUnknownActivity，簇不存在是 ErrUnknownCluster，不能一律返回 nil。
6. Merge 返回第一个活动所在簇的编号，被并活动的来源与目标集合都进入 Features；目标去重升序，事件数按合计。
7. Merge 自己和自己返回 ErrSameActivity，不能复制出一份新活动；成形前的活动参与合并按证据不足处理。
8. CoolDown 的 ttl 非正返回 ErrBadTTL，合法时记录 CoolingUntil；窗口过后 Expire 把簇归档而不是删除，Archived 查得到，Explain 仍能返回归档状态。
9. 轮换源地址后单个源看着都正常，聚类必须按目标集合与时间节律关联，不能只看源地址做阈值判断。
10. 正常监控探针的行为与扫描相似，要用目标范围与请求顺序区分，并保留白名单入口，不能靠调高阈值硬压。
11. 活动合并后原活动的告警与工单要重定向到保留编号，不允许同一段活动出现两个编号。
12. 同一来源的事件按时间序串行处理，聚类与合并原子；重启恢复后活动、事件、簇、冷却窗口与归档标记都还在，损坏的快照 Restore 返回 ErrBadSnapshot 且不得清掉现有活动与冷却窗口。

硬约束：跟踪活动数、单活动事件数、归档后保留的簇数三项上限在并发投递下精确生效，不能事后裁剪；聚类、合并、归档与快照互斥，快照只能看到全量旧状态或全量新状态。

验收：go test ./... 15 条全过；不得修改测试、跳过或用固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-35`
- 实现文件：`scan_source_clusterer.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`ScanSourceClusterer`；辅助类型 `Limits`、`ProbeEvent`、`Activity`、`Features`
