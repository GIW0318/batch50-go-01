# batch50-go-safety-c-44

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-44`
- 根包名：`task`
- 实现文件：`asset_fingerprint_clusterer.go`
- 测试目录：`tests/test_asset_fingerprint_clusterer_test.go`
- 类型：`AssetFingerprintClusterer`

## 故障单

资产台账每季度都要人工合并一次，扫出来两千多个端点里至少四百个其实是同一台机器换了域名和端口；上周更离谱，运维按台账滚动重启，同一实例被当成两台分别停了一次，业务抖了十分钟。台账里每条记录还要能说清是和哪几条证据并到一起的，审计不接受「算法自动合并」这种解释。要把端点归并做成可解释的流程，模块路径 gsb/batch50-go-safety-c-44，实现文件 asset_fingerprint_clusterer.go，类型 AssetFingerprintClusterer，签名固定：NewAssetFingerprintClusterer(cfg Config) *AssetFingerprintClusterer、Add(obs Observation) (string, error)、Merge(a, b string) (string, error)、Split(clusterID string) ([]string, error)、Explain(clusterID string) ([]Signal, error)、Cluster(clusterID string) (Cluster, bool)、Clusters() []string、Members(clusterID string) []string、Confidence(clusterID string) int、Evict(now time.Time) int、Snapshot() ([]byte, error)、Restore(data []byte) error。

十一处要落地：一，观测缺少标识或主机名直接报 ErrEmptyObservation，同一观测标识重复提交返回原簇，不新增成员。二，归并按信号加权：证书主体公钥六十分、备用名集合二十五分、站点图标哈希二十分、响应头顺序十五分、服务串十分、跳转终点十分，命中权重之和达到阈值才并入，否则新开一簇，阈值取配置值，不得在代码里写死。三，新簇置信度一百分，按信号并入的簇取命中权重之和并封顶一百分，重复观测不改变它。四，备用名相同但证书主体公钥不同视为不同实例，不能因为名字重叠就并簇。五，命中已知默认错误页的图标哈希要降权，不能拿一个批量部署的默认图标当强证据；共享内容分发证书也不作强证据。六，人工拆分按成员拆成一簇一个，拆分结果各自独立，之后同一观测不得被自动并回原簇。七，链式合并的置信度逐跳衰减，按两个簇里较低者乘十分之九取整，防传递闭包把不相干的实例并成一坨。八，合并两个已存在的簇生成新标识，旧标识立即失效，再拿旧标识合并报 ErrUnknownCluster；与自己合并幂等返回原标识。九，Explain 按权重降序列出证据，写清命中细节并指出来自哪两个观测，未知簇报 ErrUnknownCluster。十，成员数、簇数与观测总数都是硬上限，超限报 ErrClusterLimit、ErrMemberLimit 与 ErrObservationLimit；超过存活时间没有新观测的簇由 Evict 淘汰并返回条数，仍在被引用的簇不得消失。十一，快照恢复后簇标识、成员列表、置信度与证据查询逐项一致，损坏快照整体拒绝且不动现有状态。

硬约束：同一簇的写入必须串行，多个扫描器并发提交时只能形成一份成员表，成员列表返回时按标识升序；上限判定与写入必须在同一临界区内完成，不能先写后判。

验收：go test ./... 的 15 条用例全过，测试不得修改、跳过或按固定输入写死；并发提交与重启恢复自行验证；只做本地实现，交付源码与跑测记录。

## 运行

```bash
go test ./...
```

测试位于 `tests` 目录，包名同样为 `task`，通过 `impl "gsb/batch50-go-safety-c-44"` 引用根包，共 15 条用例。
