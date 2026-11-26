# 智能卡会话与 PIN 计数治理

## 题面

网点柜台报障：客户在交易中途把卡拔出，系统里那条会话句柄仍然可用，后续指令照发照回，账务侧看到一条"成功"的凭证；客户把卡重新插回同一台读卡器，PIN 错误次数又回到满格，等于插拔一次就重置了锁定。另一台读卡器上插同一张卡时，两边各自算次数，一边锁了另一边还能继续试。读卡器掉电重启后，锁定状态和通道占用全丢，之前占住的通道再也放不出来。

落点 gsb/batch50-go-safety-c-19 的 smartcard_session_broker.go，类型 SmartcardSessionBroker。签名保持：NewSmartcardSessionBroker(cfg Config) *SmartcardSessionBroker、Provision(cardID string, pin []byte) error、Plug(reader string, cardID string) error、Unplug(reader string) error、Open(reader string, app AID) (Session, error)、Transmit(sessionID string, apdu []byte) ([]byte, error)、VerifyPIN(sessionID string, pin []byte) (PINState, error)、Close(sessionID string) error、ReaderState(reader string) (ReaderView, error)、PINState(cardID string) (PINState, error)、Snapshot() ([]byte, error)、Restore([]byte) error。Config 的 MaxSessionsPerReader、MaxChannelsPerCard、PINRetries、MaxReaders 分别约束单读卡器会话、单卡通道、重试次数与读卡器数量。

要修的点：

1. Provision 校验卡号非空、PIN 长度在 4 到 12 之间，否则报 ErrBadCard、ErrBadPIN；重复登记同一张卡要重置剩余次数与锁定状态。
2. Plug 未登记卡报 ErrUnknownCard，同一读卡器已有卡报 ErrReaderExists，超过 MaxReaders 报 ErrReaderLimit；Unplug 未知读卡器报 ErrUnknownReader。
3. Open 依次校验读卡器存在、卡在位、应用标识非空，分别报 ErrUnknownReader、ErrNoCard、ErrBadApp；单读卡器会话超限报 ErrSessionLimit，单卡通道超限报 ErrChannelLimit；通道号取最小空闲值。
4. Transmit 未知会话报 ErrUnknownSession，卡已拔出或已换卡报 ErrSessionGone，APDU 短于 4 字节报 ErrBadAPDU；成功时回显原 APDU 并追加 9000 状态字。
5. VerifyPIN 长度非法报 ErrBadPIN 且不扣次数；错误 PIN 扣一次并返回剩余次数，扣到 0 置锁；已锁再验报 ErrPINLocked；正确 PIN 把剩余次数复位。
6. Close 未知会话报 ErrUnknownSession，关闭后通道号可被下一次 Open 复用。
7. ReaderState、PINState 查不到对象时分别报 ErrUnknownReader、ErrUnknownCard，视图里的会话数与通道数要与实际一致。
8. 卡拔出后该读卡器上的会话全部作废，读卡器变回空槽；重新插入后旧句柄仍然失效，新会话编号继续递增。
9. 同一张卡插到另一台读卡器时，锁定状态与剩余次数按卡标识共享，不能各算一份。
10. 拔出恰好发生在 APDU 传输途中时结果未知，必须报 ErrIndeterminate 并标记不确定，不能当成成功或失败。
11. PIN 剩余次数按卡标识记账，拔出重插后接着累计，不得因为会话重建而归零。
12. 快照要带卡片 PIN 状态与锁定、读卡器占用、通道与序号；Restore 全成或全不成，损坏载荷报 ErrBadSnapshot 且不动现网；重启后会话一律作废不恢复，编号只增不减。

硬约束：

- 同一张卡的通道分配与 PIN 扣减串行，次数扣减与锁定提交原子；16 个协程同时开通道只能成功配置条数，通道号不得重复。
- 单读卡器会话数与单卡通道数都是硬上限，超限报错而不是挤掉已有会话。
- PIN 状态与锁定跨重启保持，恢复过程任何一步失败都不得留下半套状态。

验收：go test ./... 15 条全过；不得改测试、跳过、按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-19`
- 实现文件：`smartcard_session_broker.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SmartcardSessionBroker`
