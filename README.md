# 轨道冻结与解冻

## 题面

编曲工程加载到弦乐组就掉帧：实时渲染四层弦乐时处理器直接顶满，切到别的轨道又恢复；工程师给弦乐组做了冻结，可一解冻缓存就整块重算，来回切换十几次内存涨到两倍还多；更糟的是冻结以后改了上游滤波器的截止频率，声音还停在旧参数上，切换瞬间还会爆一下。请落到模块 gsb/batch50-go-game-c-08 的 track_freeze_manager.go，类型 TrackFreezeManager，公开接口按现状保留：NewTrackFreezeManager(limits Limits) *TrackFreezeManager、SetCacheKey(track string, parts []string) error、Freeze(track string) (FreezeID, error)、Unfreeze(track string) error、Status(track string) (State, error)、Invalidate(track string) error、Collect() []string、Budget() (bytes int64, limit int64)、Advance(blocks int) (int64, error)、Cached(key string) (CacheEntry, bool)、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的口径如下。第一，轨道名空白报 ErrBadTrack；缓存键部件为空、含空白项或去空白后重复报 ErrBadKey，合法键由去重排序后的部件用斜杠连接，登记后状态为实时态且字节为零。第二，未登记的轨道做冻结、解冻、失效或查询分别报 ErrUnknownTrack；冻结期间（含已排队切换）不接受缓存键变更，报 ErrFrozen。第三，Freeze 只登记待切换，返回非空编号，状态仍是实时态但 Pending 为真；Advance 跨过块边界后才变冻结态，Block 等于新块号，Bytes 等于单条缓存开销；已经冻结或已排队冻结的轨道再次 Freeze 报 ErrAlreadyFrozen，编号与状态不变。第四，Unfreeze 对实时态报 ErrNotFrozen；冻结态调用后进入待切换，跨过块边界才回到实时态且 Bytes 归零。第五，Invalidate 只对冻结态有效，对实时态报 ErrNotFrozen；冻结态失效后缓存键作废并排队回到实时路径。第六，Advance 的块数必须为正，零或负数报 ErrBadBlocks 且块号不变；块号从零起单调递增。第七，单条缓存开销与总量上限缺省按 64 KiB 与 1 MiB 生效，Budget 返回生效值；超限时 Freeze 报 ErrBudget 且轨道保持实时态。第八，Collect 按最久未使用顺序回收可回收条目并返回回收的键。第九，解冻后缓存不立即删除而是标记可回收，重新冻结同一缓存键要能直接复用而不是重算。第十，冻结期间修改轨道参数会令缓存键失效，但已经开始的块要播完才切回实时路径。第十一，缓存淘汰只能淘汰可回收条目，仍被冻结轨道引用的条目不得被淘汰。第十二，快照要带轨道状态、缓存键、待切换标记、块号与缓存条目，恢复后冻结状态与预算延续；损坏载荷报 ErrBadSnapshot 且不动现网。

硬约束两类。一是并发：同一轨道的冻结请求必须串行裁决，只能有一个请求拿到待切换名额，其余报已冻结；读状态不得看到半更新的键或字节。二是重启恢复与资源上限：冻结状态、缓存键与块号跨进程恢复，损坏载荷整份拒绝；缓存总量是硬边界，任何时刻不得靠淘汰在用的条目来腾位置。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-c-08`
- 实现文件：`track_freeze_manager.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`TrackFreezeManager`
