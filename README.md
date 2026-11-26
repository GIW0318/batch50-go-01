# 单向导入链路的发送窗口与密钥代次

## 题面

单向导入链路改造后，接收侧报来的问题一条比一条严重：序号出现重复和跳号；确认通道中断期间窗口里的消息被直接丢掉；设备重启后重发用了新序号，对端整批丢弃；密钥轮换后旧消息被新密钥校验失败，链路停摆半小时。值班同事用旧版本重放才清掉积压，新版本里队列与水位的口径仍然对不上。请落在 gsb/batch50-go-safety-b-09 的 data_diode_bridge.go，类型 DiodeBridge，签名保持 NewDiodeBridge(Config) *DiodeBridge、Submit(payload []byte, meta Meta) (Seq, error)、AcceptAck(ack Ack) error、PollAck() (Ack, error)、Stats() Stats、RotateKey() error、Generation() int、Lookup(seq Seq) (Message, bool)、InFlight() []Message、Resend(seq Seq) error、Snapshot() ([]byte, error)、Restore([]byte) error。

要修的行为：
一、提交与序号：序号从 1 起严格递增，被拒的提交不得消耗序号。
二、载荷与元数据：空载荷报 ErrEmptyPayload，超过 MaxPayloadBytes 报 ErrPayloadTooLarge，Kind 或 SourceID 为空报 ErrBadMeta。
三、摘要与代次：每条消息带 SHA-256 十六进制内容摘要与密钥代次，新消息使用当前代次。
四、窗口：未确认条数达到 WindowMessages 或未确认字节超过 WindowBytes 时报 ErrWindowFull；确认后同一区间可继续提交；窗口按条数与字节双限，任一条件先到即拒绝。
五、队列容量：保留总条数超过 MaxQueueMessages 报 ErrQueueFull，被拒消息不得入队。
六、确认错误：未知或已裁剪序号报 ErrUnknownSeq；代次不符或状态不是 ok 报 ErrBadAck。
七、确认幂等与水位：重复确认幂等返回 nil 且累计确认数只加一次；水位只随连续确认前缀推进，乱序确认只记状态不推水位；跨代确认不得推进水位。
八、轮询：PollAck 按到达顺序返回确认，取空后报 ErrNoAckPending。
九、代次轮换：RotateKey 使代次加一，超过 MaxGeneration 报 ErrKeyGenExhausted，代次不得回退。
十、统计：Stats 给出保留条数、在飞条数、累计确认数、在飞字节、水位、代次与是否阻塞；Blocked 必须与窗口是否已满一致，确认释放后立即恢复为假。
十一、重启：Snapshot 覆盖队列、水位、代次与摘要，Restore 全量替换；恢复后新提交的序号必须大于此前任一序号；损坏数据报 ErrBadSnapshot 且不得动现网；恢复时校验水位、序号与代次的相互关系，不自洽的快照按损坏处理。
十二、在飞视图：InFlight 按序号升序返回未确认消息，内容与摘要须与提交时一致，调用方改写返回切片不得影响内部状态。

最容易漏的三处：确认通道长时间中断导致窗口满时必须阻塞提交而不是丢弃窗口内消息；重启后未确认消息的重发必须沿用原序号，不能重新编号；密钥轮换期间旧代消息仍按旧代校验，新消息必须使用新代且代次不得回退。

硬约束：提交与序号分配原子完成，并发提交不得重号、乱序或丢消息；确认处理幂等且只在水位推进时裁剪，裁剪与快照互斥。发送队列容量、单条载荷上限与密钥代次都受配置硬顶。

验收：go test ./... 15 条全过，不得改测试、跳过或按固定输入写死；并发提交与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-09`
- 实现文件：`data_diode_bridge.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`DiodeBridge`
