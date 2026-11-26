# C2BeaconDetector 评分口径与验证记录

实现文件：`c2_beacon_detector.go`（模块 `gsb/batch50-go-safety-c-28`，包 `task`）。

## 窗口与规范化

- 目的地统一 `TrimSpace + ToLower`；空白目的地返回 `ErrBadDestination`。大小写不同的事件进入同一窗口。
- 事件窗口按 `(At, Src)` 排序。同一来源、同一时刻的事件视为重复，只保留一条；不同来源的同一时刻事件保留，供 CDN 租户分组使用。
- 每个目的地最多保留 `MaxEventsPerDst` 条事件，超出时淘汰时间最旧的事件；均间隔、抖动和稳定性均只基于留下的窗口计算。
- 目的地数达到 `MaxDestinations` 后，新目的地返回 `ErrDestinationLimit`；`Forget` 明确释放槽位。

## 评分公式

对每个来源分组（共享 CDN 先按 `Src` 分组）：

- 均间隔：`(窗口最后事件时间 - 首事件时间) / (样本数 - 1)`，以秒取整。
- 抖动：`(最大间隔 - 最小间隔) / 均间隔 × 100`，四舍五入为整数百分比。
- 置信度：`max(0, 100 - 抖动)`。
- 状态：置信度达到 `BeaconThreshold` 且负载、时长均稳定时为 `beacon`，否则为 `benign`。
- 样本数低于 `MinSamples` 时：`Ingest` 返回 `collecting`，`Score` 返回 `ErrInsufficientEvidence`；未知目的地评分返回 `ErrUnknownDestination`。

## 稳定性与重采样

- 负载大小按 `BytesOut + BytesIn` 计算，连接时长按 `Duration` 计算；相对离散度 `(max-min)/mean <= 25%` 视为稳定。
- 间隔稳定只是信标条件之一。巡检/心跳类正常业务若负载或时长明显波动，即使间隔完全固定也会判为 `benign`，原因码中记录 `payload_unstable` / `duration_unstable`。
- 对 5 个及以上样本、原始置信度低于阈值且负载/时长稳定的情况，按连续 2～4 个原始间隔重采样为更宽的变间隔跨度，比较重采样跨度的收敛抖动；只有当收敛后的置信度能跨过阈值时才采用重采样抖动，原因码记录 `resampled`，避免固定直方图一刀切。仍为 benign 的窗口保留原始间隔口径。

## 共享 CDN、白名单与历史

- 多租户目的地先按来源评分，再选取置信度最强的来源作为目的地状态；样本数仍报告整个窗口，原因码附加 `source_grouped`。
- `Allow` 只接受正 TTL，否则返回 `ErrBadTTL`。有效期内 `Ingest`/`Score` 返回 `allowed` 且置信度为 0；评分按墙钟时间检查有效期，过期立即恢复普通评分，无需等待 `Decay`。
- `Decay` 删除已过期白名单，并删除早于 `now - EvidenceHorizon` 的事件；删除条数累加到该目的地的 `Historical`。容量淘汰的事件同样从活动窗口清出并计入历史。
- `Explain` 返回样本数、按时间排序的间隔序列（秒）、原因码和历史条数；目的地无任何事件时返回 `ErrInsufficientEvidence`。

## 并发与快照

- 注册表与白名单由同一把 `RWMutex` 保护；每个目的地另有独立窗口锁。同一目的地串行更新，不同目的地可并行写入/评分。窗口切片整块替换，读方不会看到半更新样本数。
- `Snapshot` 在锁内复制窗口、白名单与历史计数并输出 JSON。`Restore` 先在锁外完整解码、规范化并校验版本/目的地上限/单窗口上限/重复事件/重复白名单，全部成功后才在写锁内整体替换；任一校验失败返回 `ErrBadSnapshot` 且不触碰现有状态。

## 跑测记录

环境说明：容器 home 下 Go 缓存不可写，测试使用 `GOCACHE=/tmp/go-build-uid30948 GOTMPDIR=/tmp`。

```text
$ go test ./...
ok  	gsb/batch50-go-safety-c-28/tests

$ go test -race ./...
ok  	gsb/batch50-go-safety-c-28/tests

$ go vet ./...
（无输出，通过）

$ go test ./... -count=30
ok  	gsb/batch50-go-safety-c-28/tests
```

另用临时自测文件（验证后已删除，未改动原始 15 条用例）构造并跑通以下场景：

- 4 个目的地、8 个写协程并发 `Ingest`，同时并发 `Snapshot`，`-race` 通过；恢复后窗口数与各目的地样本数一致。
- 16 个协程并发写入同一目的地同一时刻，最终样本数为 1。
- 固定间隔但负载/时长波动判 `benign`；固定负载/时长判 `beacon`。
- 高抖动但变间隔收敛的序列判 `beacon`，原因码含 `resampled`。
- 两个 CDN 租户各自固定间隔、整体交错时判 `beacon`，原因码含 `source_grouped`。
- 乱序、同刻重复、容量淘汰、历史计数、白名单即时过期、未知快照与超容量快照整体拒绝均通过。
