# batch50-go-safety-c-14

- 模块路径：`gsb/batch50-go-safety-c-14`
- 包名：`task`
- 根文件：`hibernate_image_guard.go`
- 类型：`HibernateImageGuard`

## 题面

笔记本批量升级内核后，休眠唤醒开始出问题：一部分机器唤醒后蓝屏，日志显示内存镜像的块表只写了一半；另一部分机器唤醒后口令缓存还留在内存里，取证工具能直接读到旧的登录凭据。现场代码在 gsb/batch50-go-safety-c-14 的 hibernate_image_guard.go，类型 HibernateImageGuard，需要把分块加密写出、块表推进与唤醒校验补全。

接口保持如下签名：NewHibernateImageGuard(limits Limits) *HibernateImageGuard、Seal(image ImageRef, policy SealPolicy) (SealedImage, error)、AppendChunk(imgID string, data []byte) (int, error)、Commit(imgID string) error、Open(img SealedImage, key KeyRef) (ImageRef, error)、Close(imgID string) error、VerifyChunk(img SealedImage, idx int) (ChunkState, error)、Discard(imgID string) error、Chunks(imgID string) []ChunkState、Images() []SealedImage、RegisterSensitivePage(imgID, page string) error、ClearSensitivePage(imgID, page string) error、PendingSensitive(imgID string) []string、Snapshot() ([]byte, error)、Restore([]byte) error。

现场需要落实的行为与边界：

1. 封印策略必须带算法标识，为空返回 ErrPolicyMismatch；镜像引用页数不大于零返回 ErrBadImage；同一镜像 ID 重复封印返回 ErrImageExists。
2. 块只能按顺序追加，AppendChunk 返回本次写入的下标，块表水位随之前推；写满页数之前提交返回 ErrIncomplete，重复提交返回 ErrAlreadySealed，提交之后再追加返回 ErrImageSealed。
3. 单镜像块数与磁盘预留都有硬上限，超限分别返回 ErrChunkLimit、ErrDiskBudget，已写入的块保持可校验。
4. 打开镜像时算法与镜像头不一致返回 ErrPolicyMismatch，未提交的镜像返回 ErrNotSealed；未知镜像、未打开的镜像各有独立错误。
5. VerifyChunk 的下标越界返回 ErrChunkRange，块内容与块表记录的摘要不符返回 ErrChunkCorrupt 并把该块标成损坏，不能悄悄跳过。
6. 丢弃镜像后块表一并消失，重复丢弃返回 ErrUnknownImage；镜像表按 ID 稳定排序，便于审计对账。
7. 休眠过程中断电会让块表只写了一半，恢复时要能定位最后一个完整块，从该点续写或整体作废，不能把半块当成完整块拿去解密。
8. 唤醒后密钥与口令缓存等敏感页必须清零并登记，还有未清零的敏感页时不得把恢复标记为完成，Open 要拒绝。
9. 同一镜像被两个唤醒流程同时打开时只能有一个成功，另一个必须拿到明确的占用错误 ErrImageBusy。
10. 敏感页重复登记返回 ErrDuplicatePage，清理一个从未登记的页返回 ErrUnknownPage，待清页列表按登记顺序返回。
11. 重启后从快照恢复块表水位与未完成镜像，超过保留期的镜像自动作废，不再出现在镜像表里。
12. 所有读写都通过 Limits.Now 取时间，为空时用系统时间，保留期与宽限都按它计算。

两类硬约束必须满足：写镜像与丢弃互斥，块表推进要么整体生效要么不生效，并发追加不得出现水位跳号；快照整体生效或整体不生效，恢复失败不得留下半个镜像头。

验收方式：在题目目录执行 go test ./...，十五条用例必须全部通过；不得修改测试、跳过用例或按固定输入写死结果；并发打开与重启恢复要自己构造场景验证；只做本地实现。

## 运行

```bash
go test ./...
```

测试位于 `tests/test_hibernate_image_guard_test.go`，以 `impl` 别名导入本模块。
