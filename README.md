# 出站周期信标检测

## 题面

值班同事在出口流量报表里看到一台内网主机每六十秒连一次陌生域名，抓包发现每次上行字节和连接时长也几乎一样，但同一批告警里另一台跑监控采集的主机同样规律，误报把值班台淹没了。安全组要求把出站周期信标的判定做成可解释的评分流程，落点 gsb/batch50-go-safety-c-28，实现文件 c2_beacon_detector.go，类型 C2BeaconDetector，签名固定：NewC2BeaconDetector(limits Limits) *C2BeaconDetector、Ingest(ev FlowEvent) (Verdict, error)、Score(dst string) (BeaconScore, error)、Explain(dst string) (Evidence, error)、Allow(dst string, ttl time.Duration) error、Decay(now time.Time) error、Snapshot() ([]byte, error)、Restore(data []byte) error，另配 Allowlist、Destinations、Samples、Forget 四个辅助方法。

要修的点：一，目的地标识要去首尾空格并转小写，空目的地报 ErrBadDestination，大小写不同的事件必须落进同一个窗口；二，事件允许乱序到达，评分一律按时间顺序重排后再算间隔，同一时刻的重复事件只保留一条；三，单目的地保留的事件数封顶，超出淘汰最旧，均值按留下的窗口算；四，跟踪的目的地数量有上限，超限报 ErrDestinationLimit，只有释放后才接新的；五，事件不足最小样本数时 Ingest 的状态为 collecting、评分报 ErrInsufficientEvidence，未跟踪的目的地评分报 ErrUnknownDestination，不能拿零分冒充正常；六，均间隔取窗口首尾差除以间隔数，抖动按最大最小间隔之差占均值的百分比算，置信度等于一百减抖动、低于零按零算，低于阈值判 benign；七，心跳类正常业务也有周期，必须把负载大小与连接时长的稳定性一起计入判断，只看间隔会把巡检流量全打成信标；八，抖动大的信标在固定窗口里看不出周期，要按变间隔重采样再看收敛，别用固定直方图一刀切；九，目的地是共享 CDN 时多个租户的流量混在一起，评分要先按源分组再聚合，否则会互相污染；十，白名单带有效期，非正有效期报 ErrBadTTL，有效期内该目的地只收事件不告警，过期后立即恢复评分，而 Decay 负责把过期条目与超过保留期限的证据清掉，清掉的证据计入历史条数；十一，Explain 要给出样本数、间隔序列、原因码与历史条数，没有事件时报 ErrInsufficientEvidence；十二，进程重启后窗口、白名单与历史计数都要恢复，恢复失败的快照整体拒绝且不动现有状态。

硬约束：同一目的地的评分更新串行执行，窗口推进要么看到完整旧窗口要么看到完整新窗口，多个目的地之间互不阻塞；窗口与白名单的写入在同一把锁内完成，读方不得观察到半更新的样本数；单目的地事件数、跟踪目的地数与证据保留时长都有硬上限，长时间运行内存不随事件总量增长。

验收：go test ./... 的 15 条用例全过，测试不得修改、跳过或按固定输入写死；并发写入与重启恢复要自己构造场景验证；只做本地实现，交付源码、跑测记录与评分口径说明。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-28`
- 实现文件：`c2_beacon_detector.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`C2BeaconDetector`
