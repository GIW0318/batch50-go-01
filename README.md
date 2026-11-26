# 作业检查点的凭据剥离与跨节点恢复治理

## 题面

平台上一次作业回滚之后出了两件事：一个被恢复的作业在源节点和目标节点各跑了一份，两边同时往同一个结果目录写，产物互相覆盖，事后分不清哪一份是新的；另一个镜像里带着明文数据库口令和一份私钥，被审计直接从对象存储里捞了出来。还有一次目标节点内核版本偏老，恢复跑到一半才报错，作业停在半启动状态，残留的挂载与网络命名空间只能人工逐个清理。请在 gsb/batch50-go-safety-c-02 的 checkpoint_restore_guard.go 里补完 CheckpointRestoreGuard，管住导出、查看、恢复与销毁四件事。

对外接口保持：NewCheckpointRestoreGuard(Config) *CheckpointRestoreGuard；StartJob(id JobID, spec JobSpec) error；RegisterNode(id NodeID, caps NodeCaps) error；Checkpoint(jobID JobID, opts Options) (SnapshotID, error)；Inspect(id SnapshotID) (Manifest, error)；Restore(id SnapshotID, target NodeID) (JobID, error)；Destroy(id SnapshotID) error；Expire(now time.Time) int；Snapshots(tenant string) []SnapshotID；Instances(jobID JobID) int；InstanceCount() int；Usage(tenant string) Usage；Audit() []AuditEntry；Export() ([]byte, error) 与 Import([]byte) error。

要修的地方：一，StartJob 的租户与作业号为空或全是空格时返回 ErrBadJob，被拒的作业不得计入实例数；二，快照号以 snap- 开头，清单记下源节点、租户、创建时间、镜像体积（取作业的 DiskBytes）与摘要，状态为 sealed；三，环境变量键名含 PASSWORD、SECRET、TOKEN、CREDENTIAL、PRIVATE_KEY 的一律剥离，Options.Redact 追加的键名同样剥离，Redacted 按名列出，清单序列化之后不得出现这些值；四，未知作业返回 ErrNoJob，未知快照返回 ErrNoSnapshot，被拒的导出不占配额；五，单租户快照数与总容量分别受 MaxSnapshotsPerTenant 与 MaxTotalBytes 约束，超限返回 ErrSnapshotLimit、ErrCapacityExceeded，邻居租户照常；六，Options.MaxBytes 小于镜像体积时返回 ErrOversizeSnapshot 且不占配额；七，清单必须保留命名空间标识、挂载源与摘要、只读标志、网络身份、打开文件摘要与密钥代际，密钥代际取 Config.EncryptionKeyGeneration；八，Restore 只接受同租户目标节点，跨租户返回 ErrCrossTenant，未注册节点返回 ErrUnknownNode；九，同一快照重复恢复到同一节点返回同一个作业号，实例数不得增加；十，Destroy 之后 Inspect 报 ErrNoSnapshot、配额归还、重复销毁保持成功，审计按 start_job、checkpoint、restore、destroy 的顺序追加、序号递增，销毁那条带快照号；十一，剥离过程中任何一步失败，整份快照必须作废，不允许留下部分脱敏的镜像，作废的快照不可再恢复；恢复目标的内核与运行时版本不兼容时要在动手之前拒绝，不能恢复到一半才失败；十二，快照里记录的打开文件跨节点无法重开，恢复时必须按路径重新绑定并核对内容摘要，摘要对不上返回 ErrMountIdentityDrift。

硬约束：导出期间作业状态要冻结或按脏页快照，不能产出撕裂镜像；恢复必须幂等，同一快照只对应一个运行实例。单租户快照数、总容量与过期销毁都要留审计，TTL 到期的快照由 Expire 收回并把容量还回去；Export 与 Import 要能跨进程搬运作业表、快照表与节点表，导入损坏载荷返回 ErrBadEnvelope 且不动现网。

验收：go test ./... 十五条全过，测试不得修改、跳过或按固定输入写死；导出与运行并发、恢复幂等与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-02`
- 实现文件：`checkpoint_restore_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`CheckpointRestoreGuard`、`Manifest`、`JobSpec`、`NodeCaps`、`Options`、`AuditEntry`
