# 增量补丁清单与断点续传

## 题面

客户端同学反馈，这周的灰度补丁把三个人卡在了百分之八十七：其中一个客户端在下载中途被切到了新发布的清单，已经确认的分块全部作废；另一个上报的块号比文件实际块数还大，进度条直接跳到满格，装完却报文件损坏；还有一个回滚包，文件从新版本改回旧内容之后，清单里的块边界和第一次发布时对不上，校验永远过不去。运维要求这次把清单到校验整条链路收干净。落点是模块 gsb/batch50-go-game-a-05 的 patch_manifest_planner.go，类型 PatchManifestPlanner，公开接口保持不变：NewPatchManifestPlanner(limits Limits) *PatchManifestPlanner、Build(fromVersion, toVersion string, files []FileEntry) (Manifest, error)、ManifestOf(manifestID string) (Manifest, bool)、Begin(manifestID, clientID string) (DownloadPlan, error)、AckChunk(manifestID, clientID, filePath string, chunk int) (Progress, error)、Verify(manifestID, clientID string) (VerifyResult, error)、Pause(manifestID, clientID string) error、Resume(manifestID, clientID string) (DownloadPlan, error)、ProgressOf(manifestID, clientID, filePath string) (Progress, bool)、SetClientDisk(clientID string, freeBytes int64) error、PinnedManifest(clientID string) (string, bool)、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的口径如下。第一，Build 的入参是新版本的完整文件表，基线取 fromVersion 对应的已发布清单：路径与哈希都没变记 keep，同名不同哈希记 replace，新出现记 add，只在旧版本里出现的路径记 remove。第二，清单号按版本对稳定生成，同一个版本对重复 Build 必须返回同一份清单、同一个序号，不能生成两份。第三，分块定长取 Limits.ChunkSize，只有最后一块可以短，块序号从 0 连续，Offset 与 Length 必须和文件大小对得上；keep 与 remove 不产生块。第四，Manifest 的 Chunks 是清单内所有块数之和，TotalBytes 是真正需要传输的字节数。第五，路径为空、大小为负、同一份文件表里路径重复、版本号为空或两端相同，分别报 ErrBadFile、ErrBadVersion；文件数超过 MaxFiles 报 ErrFileLimit，清单数超过 MaxManifests 报 ErrManifestLimit。第六，Begin 把清单钉到客户端，客户端号为空报 ErrBadClient，清单不存在报 ErrManifestNotFound；已经钉过另一份清单的客户端再 Begin 报 ErrVersionPinned，下载途中不允许换清单。第七，客户端设置了磁盘余量且小于 TotalBytes 时报 ErrDiskFull，被拒绝的 Begin 不能留下钉扎。第八，AckChunk 只接受属于该文件计划的块，重复确认同一块保持幂等，Progress 的 Confirmed 只增不减、Done 在确认满块时为真；Verify 的 FilesTotal 是清单里的文件数，keep 与 remove 视为已完成，Missing 按路径升序列出尚未完成的文件，全部完成时 OK 为真。第九，Pause 冻结下载，重复暂停报 ErrAlreadyPaused；Resume 只返回还没确认的块并带 Resume 标记，没暂停过报 ErrNotPaused，没有计划的客户端报 ErrNoPlan；ProgressOf 与 PinnedManifest 用第二个返回值表示是否存在，查不到的记录不能编造数据。

三处边界必须一起处理。同一个文件从 A 改成 B 又改回 A 时要复用旧内容块，回滚清单的块边界必须和首次发布时完全一致。客户端上报的块序号超出该文件块数时必须拒绝，进度位图一块都不能变，不能越界写坏。两个客户端共享同一份块缓存时，一方校验失败只能清自己那部分，不能让另一方已经确认的块失效。

硬约束两条。一是并发：同一客户端的块确认与暂停恢复请求必须串行结算，位图任何时刻都不能出现半更新的中间态，多路并发确认同一块只能记一次。二是重启：快照要带上清单、客户端钉扎、磁盘余量与块位图，恢复之后 Verify、ProgressOf、Resume 的结果必须与重启前一致；损坏载荷在 Restore 时报 ErrBadSnapshot，且不能动到现网位图。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-05`
- 实现文件：`patch_manifest_planner.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`PatchManifestPlanner`
