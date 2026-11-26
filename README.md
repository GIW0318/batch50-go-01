# 固件镜像分区解包与清单

## 题面

车载网关的固件审核积压了两周，解包脚本在三块板子的镜像上给出了互相矛盾的结论：同一片固件里有三个分区共用同一段偏移，脚本按顺序取了最后一个，另外两个被静默丢掉；某个分区的数据被裁短了两百多字节，脚本照样把里面的文件全导出来，装到样机上才发现配置是残的；还有一次解压炸弹，压缩头里声明的体积很小，实际写入把磁盘占满，审核机的磁盘清理了两次。请把解包流程做成可复用的组件，落点 gsb/batch50-go-safety-c-39，实现文件 firmware_image_unpacker.go，类型 FirmwareImageUnpacker，签名固定：NewFirmwareImageUnpacker(limits Limits) *FirmwareImageUnpacker、Open(img []byte) (Image, error)、Partitions(img Image) ([]Partition, error)、Extract(img Image, sel Selector) ([]Entry, error)、Manifest(img Image) ([]byte, error)、Close(img Image) error、Snapshot() ([]byte, error)、Restore(data []byte) error，另配 Remaining、Images、Stats、Reset 四个辅助方法。

分区格式口径：镜像头部三十二字节，前四字节是魔数 FWIM，随后是版本、分区表偏移、分区条目数与分区表字节累加和，各占四字节小端；每个条目三十二字节，前十六字节是分区名，随后是偏移、长度、类型与标志各四字节小端，类型一表示原文、三表示嵌套文件系统；嵌套文件系统以 FWFS 开头，四字节小端计数后跟若干条，每条是四字节名字长度、名字、四字节长度与内容。

要修的点：一，魔数不对、版本不是一、头部不足三十二字节、条目数为零或超过三十二、分区表落在镜像外、累加和对不上，一律报 ErrBadImage 且不登记；二，Open 只校验头部与分区表本身，重叠与越界留给 Partitions 判定，重叠报 ErrOverlap，越界报 ErrOutOfBounds；三，原文分区整段作为一个条目，路径就是分区名；嵌套分区的每个文件路径是分区名加斜杠加文件名，结果按路径升序；四，选择器带分区名与路径前缀，分区名为空表示全镜像，分区名不认识报 ErrBadSelector；五，嵌套深度、单文件体积与总解压字节三重预算，单文件超限报 ErrBudget 且该条目不进结果，总量超限要中止并已收集的条目标成不完整；六，压缩流头部声明的解压后大小不可信，要按实际写入计量；七，只提取某分区时清单仍要覆盖全镜像并标明未提取项；八，分区表校验和正确但分区超出镜像长度时整片拒绝；九，清单按摘要去重，同一内容在多处出现只留一条并列出全部路径，路径升序，镜像编号照写；十，同一镜像并发解包只允许一个实例在跑，抢不到的报 ErrBusy，结束后锁要释放；十一，关闭镜像后其解包状态一并丢弃，再操作报 ErrNotOpen；十二，快照要带上原始镜像字节与已解包结果，恢复后仍能继续解包，损坏载荷整体拒绝且不动现有镜像，打开镜像数量超过上限报 ErrTooManyImages。

硬约束：同一镜像的解包、清单与关闭串行推进，不同镜像之间互不阻塞；总字节预算按镜像记账，超限中止后不得把半成品当成完整结果。验收：go test ./... 的 15 条用例全过，测试不得修改、跳过或按固定输入写死；并发解包与重启恢复自行验证，只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-39`
- 实现文件：`firmware_image_unpacker.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`FirmwareImageUnpacker`
