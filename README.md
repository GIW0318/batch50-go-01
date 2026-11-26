# 邮件 DATA 分帧规范化与大小对账

## 题面

邮件网关上周升级后，外发的营销邮件被下游批量退信，理由都是正文被截断或者凭空多出几行；运维复现时发现，客户端用纯换行分隔的行被网关当成了行尾，本该逐字保留的正文被改写；还有客户的批量投递走分块传输，最后一块少带了两个字节，网关照样收下，下游解析时整封信错位；最危险的一次是终止符之后还挂着一串没被消费的字节，下一封信直接把这串内容当成了自己的开头。现场代码在 gsb/batch50-go-safety-b-14 的 smtp_data_framing_guard.go，类型 SmtpDataFramingGuard，需要把 DATA 分帧、点填充还原、大小对账与连接复位这条链路补全。

接口保持如下签名：NewSmtpDataFramingGuard(limits Limits) *SmtpDataFramingGuard、Begin(sessionID string, declared int64) (Transaction, error)、Feed(chunk []byte) (Progress, error)、Finish(sessionID string) error、Abort(sessionID string) error、Body() []byte、BodyLines() []string、State(sessionID string) (SessionState, bool)、Sessions() []SessionState、Snapshot() ([]byte, error)、Restore([]byte) error。

现场需要落实的行为与边界：

1. 空白会话号报 ErrBadSession，声明长度非正报 ErrBadDeclared，两者都不留下会话记录。
2. 一条连接同时只能有一封在途：已有会话时再次 Begin 报 ErrSessionBusy，原会话必须继续可用。
3. 没有在途事务时 Feed 报 ErrNoSession；空 chunk 不改变任何计数。
4. 行尾只认回车换行：裸换行报 ErrBareLF，回车后面跟的不是换行报 ErrBareCR，两种都要立刻丢弃当前事务并把连接复位到命令态。
5. 每一行的长度按原始字节计算，不含行尾两个字节，也必须在还原点填充之前算；超过 MaxLineLength 报 ErrLineTooLong。
6. 累计收到的字节超过声明长度立即报 ErrSizeOverflow 并作废事务；Progress 里的 Remaining 是声明长度减去已收字节，Lines 只数已经收满的正文行，Chunks 是 Feed 的调用次数，InData 表示事务还在收数据。
7. 行首连续两点还原成一个点，非行首的点原样保留；还原次数记在 Progress.DotStuffed。
8. 单独一行的点就是终止符，此后不再并入正文。
9. Finish 之前必须对账：最后一块补齐后，已收字节要与声明总长完全一致，少一个字节也算失败，报 ErrSizeMismatch 并关闭会话；一致才返回成功。
10. Finish 成功后会话变为 closed，正文保留供投递；Abort 成功后正文清空、会话变 aborted，重复 Abort 报 ErrSessionNotFound。
11. 未知会话调用 Finish 或 Abort 一律报 ErrSessionNotFound，不能返回成功。
12. 终止符之后同一连接上的残留字节不得并入下一封邮件，必须当成新的命令态输入处理。

两类硬约束必须满足：同一连接上的 Begin、Feed、Abort 串行执行，并发开单只能有一个成功，chunk 到达的顺序与计数不能被并发打乱；快照要包含未完成的行缓冲、已收字节、分块数与已还原正文，Restore 整体生效或整体不生效，损坏载荷报 ErrBadSnapshot 且不动现网。

验收方式：在题目目录执行 go test ./...，十五条用例必须全部通过；不得修改测试、跳过用例或按固定输入写死结果；并发开单与中途重启要自己构造场景验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-14`
- 实现文件：`smtp_data_framing_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SmtpDataFramingGuard`
