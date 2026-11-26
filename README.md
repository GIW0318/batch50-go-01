# 日志中继的 RFC 5424 分帧、对端校验与磁盘背压

## 题面

机房节点上线集中日志之后，运维发现三件事：中心偶尔收不到某个来源的日志，重启中继后同一批日志又被重发一遍，值班群里刷了屏；某台节点根分区被中继自己写满，机器直接进入只读；证书轮换当天还有一批节点一直连不上中心，抓包只看到握手中断。现场还有个细节：中心按 msgid 去重，重复投递会被丢弃，可中继在重连时若把在途批次又发一遍，中心看到的顺序就会乱，事后完全对不上。落点在模块 gsb/batch50-go-safety-b-04 的 syslog_relay_guard.go，类型 SyslogRelayGuard，签名保持：NewSyslogRelayGuard(p RelayPolicy) (*SyslogRelayGuard, error)、Write(msg Message) error、Flush(ctx context.Context) error、Peer() PeerState、SetPolicy(p RelayPolicy) error、Normalize(msg Message) (Message, error)、Frame(msg Message) ([]byte, error)、FrameNonTransparent(msg Message) ([]byte, error)、VerifyPeer(chain []CertInfo, serverName string) (PeerState, error)、QueueDepth() (int, int)、Pending() []Message、Inflight() []Message、Ack(msgID string) error、Snapshot() ([]byte, error) 与 Restore(data []byte) error。

需要落地的行为：

1. 八位组计数分帧：消息前加十进制字节数与一个空格，字节数只算后面那条 RFC 5424 报文的长度。
2. 非透明分帧以换行分隔消息，正文里的换行按转义规则保留，不能截断。
3. 报文头按 PRI、版本 1、UTC 时间戳、来源主机、应用名、占位进程号、消息 ID、占位结构化数据、正文的顺序组织，占位字段用一个短横线。
4. 正文超过单条上限时截断，同时记录原始长度，下游据此发现被裁剪的记录。
5. 来源为空或正文为空报 ErrBadMessage；未指定消息 ID 时按来源与单调序号生成，重启后序号不回退。
6. TLS 对端按允许的 CA 与 SNI 校验，CA 不在清单报 ErrPeerUntrusted，名字对不上报 ErrSNIMismatch。
7. 证书轮换期允许两个 CA 并存，旧 CA 在新 CA 生效窗口内仍可通过。
8. 中心不可达时消息落磁盘队列，条数与字节数双上限，超限报 ErrQueueFull 且不覆盖旧记录，QueueDepth 报待发队列的条数与正文字节数。
9. 队列满时按来源优先级取舍，高优先级来源的保留额度不能被低优先级来源占满。
10. 至少一次投递：Ack 之后才删除记录，重复 Ack 幂等成功；TLS 重连后不得重复投递已在途但未确认的批次，否则中心的 msgid 去重窗口会被打穿。
11. 同一来源严格先进先出，并发写入与批量发送都要保持接收顺序。
12. 重启后从持久化状态续传，队列内容与消息 ID 水位都要还原，不得把已确认的记录再发一次。

值班口径是：宁可多等几秒也不能丢一条，但积压到上限时当场拒收，让上游业务自己降级，而不是把磁盘写炸。队列里的记录还要能按来源追溯，哪条来源积压多少、丢了哪些，值守时一眼看得出来。

硬约束：Flush 与 Write 可能并发，入队、挪到在途与确认删除三步对同一来源必须保持顺序且不丢记录；磁盘队列的文件大小与条目数都有上限，进程重启后从续传位置继续，不得整体重扫。

验收：go test ./... 的 15 条用例必须全过，不得修改测试、跳过或按固定输入写死；TLS 重连与并发写入自行验证；只做本地实现，交付源码与测试记录。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-04`
- 实现文件：`syslog_relay_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SyslogRelayGuard`
