# 句柄式打开的准入校验与授权台账

## 题面

沙箱逃逸面的巡检发现了三类绕过：作业先拿到一个文件句柄，之后拿着同一个句柄打开了早被改名换掉的宿主目录，路径上的目录权限完全没起作用；另一次是挂载点被卸载又重新挂上，旧句柄仍指向新挂载上的同号 inode，校验按旧记录放了过去；还有一起是同一 inode 有三条硬链接，其中两条落在只读目录下，调用方挑最宽松的那条路径去比对就通过了。请在 gsb/batch50-go-safety-c-05 的 file_handle_guard.go 里补完 FileHandleGuard，管住解析、登记、打开与撤销这条链路，并留下可复核的授权台账。

对外接口保持：NewFileHandleGuard(limits Limits) *FileHandleGuard；AddMount(id uint64, prefix, namespace string, generationSupported bool) error；Lookup(path string) (Handle, error)；Register(h Handle, owner Owner) error；Open(h Handle, flags int, caller Caller) (File, error)；Revoke(h Handle) error；Verify(h Handle) error；Grant(h Handle, tenant string, expires time.Time) error；Grants(tenant string) []Grant；OpenedFiles(tenant string) []File；OpenCount(tenant string) int；Export() ([]byte, error) 与 Import([]byte) error。

要修的地方：一，Lookup 的路径为空、是相对路径或含点点段时返回 ErrBadPath；二，路径按最长前缀匹配挂载，挂载不在 Limits.AllowedMounts 里或路径落在任何挂载之外返回 ErrMountDenied；三，同一路径反复 Lookup 必须得到完全相同的句柄，不同路径不得得到相同 inode，支持生成号的挂载要记下非零生成号；四，AddMount 的编号为零或前缀不是绝对路径返回 ErrBadMount；五，Register 的属主租户为空或模式位为零返回 ErrBadOwner，句柄不是 Lookup 产出的返回 ErrNoHandle，同一属主重复注册保持成功，换属主返回 ErrAlreadyOwned；六，Open 之前必须有属主记录，否则返回 ErrNotRegistered；七，调用方租户与属主不一致返回 ErrOwnerMismatch 且不计入打开数；八，属主模式位没有读权限而调用方要读时返回 ErrPermissionDenied，只写可以打开，打开记录里要带标志与时间；九，单租户打开数超过 MaxOpensPerTenant 返回 ErrOpenLimit，被拒的不计数；十，Revoke 之后新打开返回 ErrRevoked，已经打开的句柄不受影响、仍能在 OpenedFiles 里看到，Verify 返回 ErrRevoked，撤销未知句柄返回 ErrNoHandle；十一，句柄里的生成号与登记时不一致返回 ErrStaleHandle，Verify 重新解析路径发现 inode 不一致同样返回 ErrStaleHandle；十二，跨命名空间打开必须显式授权，没有授权返回 ErrNamespaceDenied，授权过期返回 ErrGrantExpired，挂载传播之后句柄可能指向另一命名空间里的同号 inode，归属要按目标命名空间重新判定而不是沿用创建时的那一个。

另外两处边界：同一 inode 存在多条硬链接时，权限判定要取最严的那条路径，不能挑最宽松的用；文件系统不提供生成号时不能依赖复用检测，要用内容摘要兜底，摘要对不上按句柄失效处理。

硬约束：句柄校验与打开之间不能被改名或卸载的竞态绕过，打开落定后要再复核一次挂载与 inode，复核不过就回滚这次打开；授权台账与打开计数在并发下必须一致，撤销一旦生效，之后不再有任何新打开成功。句柄缓存条目与每租户打开数受上限约束，超限按最久未用淘汰，不能悄悄放大；Export 与 Import 要搬运属主、撤销记录与授权台账，导入之后已撤销的句柄仍然拒绝打开。

验收：go test ./... 十五条全过，测试不得修改、跳过或按固定输入写死；并发打开与撤销、改名与卸载竞态自行造场景验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-05`
- 实现文件：`file_handle_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`FileHandleGuard`、`Handle`、`Caller`、`Owner`、`File`、`Grant`、`Mount`
