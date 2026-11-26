# 用户态文件系统挂载代理：跨用户访问开关、身份映射与请求背压

## 题面

沙箱里挂载的文件系统出了几起事故：一个租户的目录挂到了另一个租户的挂载点下面，上层目录一盖，原挂载直接看不见；共享挂载打开跨用户访问之后，没做身份映射的调用者也能读写，容器里看到的是宿主机的真实身份；后端一断，内核请求堆在队列里把挂载进程拖死。现在要把挂载建立、跨用户访问与请求转发收进一个组件，落在 gsb/batch50-go-safety-d-04 的 fuse_mount_broker.go，类型 FuseMountBroker，包名 task。

公开接口保持：NewFuseMountBroker(limits Limits) *FuseMountBroker、Mount(spec MountSpec) (Mount, error)、Unmount(id string) error、Forward(id string, req Request) (Reply, error)、SetMapping(id string, mapping []IDMap) error、SetBackend(id string, online bool) error、Flush(id string) (int, error)、Depth(id string) (int, error)、Stats(id string) (Stats, error)、Mounts() []Mount、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的故障与边界：

一、挂载点必须是绝对路径、本身已是规范形式且不能是根目录，空、相对、含双斜杠或 .. 的路径报 ErrBadMountpoint。

二、默认权限只认只读与读写两种，其他取值报 ErrBadPerm；属主与降权身份为负报 ErrBadOwner。

三、同一挂载点重复挂载报 ErrMountExists；挂载点落在另一个挂载点之下属于遮蔽，报 ErrMountOverlap，不能让它盖住已有挂载。

四、挂载数达到 MaxMounts 报 ErrMountLimit；Mounts 按挂载点排序返回。

五、属主请求直接放行并映射到容器内的零号身份，服务成功回执码为 200；未开跨用户访问时非属主一律 ErrAccessDenied，计入拒绝数但不计服务数。

六、开了跨用户访问后，非属主必须命中身份映射才放行，映射到哪个容器身份要写进回执；未命中报 ErrAccessDenied。

七、身份映射要求宿主编号与容器编号各自唯一、非负，超过 MaxMapping 报 ErrBadMapping；换表后旧条目立即失效，在途请求按派发时的表计算。

八、后端离线时请求进队列，回执标记为已排队与可重试，接收入队的请求计入服务数；队列达到 MaxQueueDepth 后新请求报 ErrQueueFull 并标记可重试，不能阻塞内核，丢弃要计数。

九、后端离线时 Flush 报 ErrBackendDown，请求留在队列里不丢；后端恢复后 Flush 返回排出的条数。

十、还有排队请求时 Unmount 报 ErrQueueBusy，排空后才能卸载；卸载后对编号的查询一律 ErrUnknownMount。

十一、请求编号或路径为空报 ErrBadRequest；未知挂载报 ErrUnknownMount；并发转发同一挂载的计数不丢不重。

十二、快照保存挂载、身份映射、队列与计数，恢复要么全量成功要么不动现网，坏快照报 ErrBadSnapshot。

另外两条现场口径：特权身份访问要按降权配置落到容器内身份，不能把宿主特权直接透传；后端重连后不得重放非幂等请求，未确认的非幂等请求要归还失败结果。

硬约束：转发准入、映射与入队是一次原子提交，不会出现已计数未入队的请求；队列深度与挂载数设上限，超限直接拒绝而不是降级；卸载必须排空。

验收：go test ./... 15 条全过，测试不得修改、跳过或按固定输入写死；并发转发与重启恢复自行构造场景验证；只做本地实现，交付源码与跑测结果。

## 运行

模块路径 `gsb/batch50-go-safety-d-04`，包名 `task`，实现文件 `fuse_mount_broker.go`，测试在 `tests/test_fuse_mount_broker_test.go`。

```bash
go test ./...
```
