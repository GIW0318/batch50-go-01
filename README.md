# 安全事件令牌的签发、筛选与投递台账

## 题面

账号禁用推送上线后，两家下游都投诉重复收到同一条事件，另有一家反馈会话已经恢复却还在被反复投递；运维在台账里发现某条事件的接收方进度在重启后归零，重试从第一次重新开始；还有一条事件台账写着已投递，下游拿到的却是另一份签名的副本。落点 gsb/batch50-go-safety-a-09 的 ssf_event_transmitter.go，类型 SetEventTransmitter，签名保持：NewSetEventTransmitter(Sender, Limits) *SetEventTransmitter、SetKey(tenantID string, key []byte) error、Subscribe(spec ReceiverSpec) error、Publish(evt SecurityEvent) (EventID, error)、Delivery(eventID string) ([]DeliveryState, error)、Retry(eventID, receiverID string) error、Subscribers(tenantID string) []ReceiverSpec、Pending() int、Snapshot() ([]byte, error)、Restore([]byte) error。Sender 由外部注入，Deliver(receiverID, endpoint, token string) (int, error) 返回接收方应答的状态码。令牌是三段 base64url：头固定 alg 为 HS256、typ 为 JWT；载荷字段 iss、aud、jti、iat、events；签名是对“头.载荷”用租户密钥做 HMAC-SHA256，不做填充。

要修的地方：租户为空报 ErrBlankTenant，事件类型不在三种之内报 ErrUnknownEventType，主体范围为空报 ErrEmptySubject，这三种都不许发送；订阅缺接收方 ID 或端点报 ErrBlankReceiver，一个事件类型都没订报 ErrNoEventTypes；只投给订阅了该事件类型的接收方，订阅带了主体清单的只匹配清单内的主体，清单为空表示不限；没有接收方匹配时 Publish 照样返回事件号，台账为空；每个接收方单独签令牌，aud 是接收方 ID，iss 是租户，iat 取事件签发时间，jti 互不相同；租户没有密钥时 Publish 报 ErrNoSigningKey，不发送也不落台账；投递失败（非 2xx 或 Sender 报错）记为 pending，写清尝试次数与状态码，退避秒数按 2 的尝试次数减一次方算并封顶 300；重试沿用首次签发的令牌，jti 不变；已投递的再重试报 ErrAlreadyDelivered，未知事件报 ErrUnknownEvent，接收方不属于该事件报 ErrUnknownReceiver；尝试次数达到订阅的 MaxAttempts 仍失败就标 failed，之后再重试报 ErrDeliveryExpired；待重试条目超过 MaxInFlight 时 Publish 报 ErrQueueFull，队列腾空后恢复；台账只保留最近 MaxEvents 条已结束投递的事件，更早的查询报 ErrUnknownEvent；Pending 只数还没结束的条目，Subscribers 返回该租户当前订阅的副本。

还有三条边界要点名：订阅在事件产生之后才建立时，不得补发历史事件，也不能把这些历史事件算成未送达；接收方返回 410 时要停止投递并保留台账，不能当成普通失败反复重试；主体范围在投递之前被收窄时，是否仍要投递按事件产生那一刻的订阅快照决定。

硬约束两条：同一事件对同一接收方只能成功投递一次，并发重试只有一个能推进，其余必须明确失败，接收方靠 jti 做幂等；订阅变更只影响新产生的事件，改动不能倒改已有台账。在途队列与事件保留窗口都按配置硬顶。

重启后订阅、密钥、台账、尝试次数与已签发的令牌都要回来，恢复后重试沿用原 jti；快照损坏时报 ErrBadSnapshot 并保留现网。

验收：go test ./... 15 条全过，不得改测试、跳过、按固定输入特判；并发重试与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-a-09`
- 实现文件：`ssf_event_transmitter.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SetEventTransmitter`
