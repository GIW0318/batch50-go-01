# 帧精确跳转协调

## 题面

投诉单 2026-1120-09：拖动进度条时画面偶尔闪过上一段的内容，连续拖几次还会停在拖动前的位置；长片跳到十分钟之后要等两秒以上，明明缓存过的段落又要重新解码。落点在 gsb/batch50-go-game-c-15 的 seek_coordinator.go，类型名 SeekCoordinator，包名 task。

构造签名 NewSeekCoordinator(limits Limits) *SeekCoordinator；Limits 的 Budget 为零时按 64 处理，CacheBytes 为零时按 1MB 处理，预算上限 4096。

需要落地的公开接口：Seek(target time.Duration) (RequestID, error)、Await(id RequestID) (Frame, error)、Cancel(id RequestID) error、SetBudget(frames int) error、CacheGOP(gop GOPInfo) error、Position() time.Duration、CacheSize() int、Stats() SeekStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的现象：

一、跳转请求要有唯一编号且单调递增，同一时刻只有最新一个请求能交付结果；新请求到达时，还没完成的旧请求标记为被取代，已经开始的那次解码可以跑完，但结果只记账不交付，调用方不能拿到过期帧。

二、目标时间必须非负，负数报 ErrBadTarget；没有任何缓存段落覆盖目标时报 ErrNotSeekable。

三、跳转锚定到目标之前最近的关键帧，再逐帧解码到目标帧；落点取目标所在的那一帧，帧号与时间戳都要对得上。

四、每帧解码数受预算约束，超预算时返回最近可达帧并把结果标为非精确；非精确落点返回的帧要成为后续跳转的锚定起点，不能回到请求之前的位置重新锚定。

五、预算取值必须在 1 到 4096 之间，越界报 ErrBadBudget。

六、缓存段按字节上限淘汰，先淘汰最早加入且没有被正在进行的解码占用的段；单个段本身就超过上限时报 ErrGOPTooLarge；缓存段与当前解码位置不连续时不能直接拼接，必须回到关键帧重新解码。

七、缓存段的键帧序号、时间戳、帧间隔、帧数、字节数必须都有效，否则报 ErrBadGOP，被拒绝的段不进缓存。

八、被取消的请求报 ErrRequestClosed 之后不能再交付；取消不存在的编号报 ErrUnknownRequest；等待不存在的编号也报 ErrUnknownRequest。

九、Position 只在帧真正交付之后前进，取消或被取代的请求不能改动播放位置。

十、Stats 要分别给出请求数、交付数、取消数、被取代数、解码帧数与淘汰段数。

十一、快照要覆盖播放位置、缓存段与请求编号水位；损坏快照 Restore 报 ErrBadSnapshot 且不动现网位置与缓存，恢复后编号继续递增、缓存索引可用。

十二、连续跳转时请求编号只增不减，已经交付过的编号不能再次交付，也不能被重新分配。

硬约束：并发发起跳转时编号必须唯一且计数不丢，任何一次读取都只能看到完整的旧状态或完整的新状态，不能读到半个请求；缓存段的字节记账必须与真实内容一致，淘汰之后 CacheSize 要立刻反映占用；重启恢复后播放位置与缓存索引延续，不能把缓存过的段落重新当冷启动处理。

验收：go test ./... 十五条必须全过，不得修改测试、跳过或用固定输入写死；并发与重启恢复自行验证；只做本地实现，只用标准库。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-c-15`
- 实现文件：`seek_coordinator.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SeekCoordinator`
