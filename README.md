# batch50-go-safety-c-12

- 模块路径：`gsb/batch50-go-safety-c-12`
- 包名：`task`
- 根文件：`scm_rights_broker.go`
- 类型：`ScmRightsBroker`

## 题面

边缘网关的沙箱代理接入一批第三方插件后，文件描述符数量在几小时内涨到上限，宿主机上几个目录句柄长期占着 inode，运维只能重启进程清场；审计里还查不到这些句柄到底属于哪个租户。现场代码在 gsb/batch50-go-safety-c-12 的 scm_rights_broker.go，类型 ScmRightsBroker，需要把对端凭据归属、类型准入、数量记账与孤儿回收补全。

接口保持如下签名：NewScmRightsBroker(limits Limits) *ScmRightsBroker、AcceptConn(conn *net.UnixConn) (Peer, error)、RegisterPeer(peerID, tenant string, uid uint32) (Peer, error)、SetQuota(tenant string, q Quota) error、Stage(peer Peer, in []Descriptor) error、Receive(peer Peer, maxFDs int) ([]FDGrant, error)、Release(grantID string) error、ReleasePeer(peerID string) (int, error)、Sweep() (int, error)、Audit() []GrantRecord、Stats(tenant string) BrokerStats、Snapshot() ([]byte, error)、Restore([]byte) error。

现场需要落实的行为与边界：

1. 每条消息附带的描述符数量超过 maxFDs 时整条消息拒收，返回 ErrQuotaExceeded，队列里的描述符保持原样，客户端缩小批量后仍可重发。
2. 类型不在租户白名单里返回 ErrFDTypeDenied；目录 fd、memfd、pidfd 与普通文件要分开识别，不能只看 stat 的 mode 位就当成普通文件。
3. 每租户在途描述符总量与单类型数量各有上限，超限整条消息拒收，已入账的不受影响；未配置配额的租户一律拒收。
4. 归属登记与关闭必须原子：Release 未知 grant 返回 ErrUnknownGrant，同一个 grant 重复释放是幂等成功，计数不重复增加。
5. 审计按到达顺序记录，序号从一开始严格递增，每条含租户、类型与底层对象键；日志条数受 MaxRecords 硬顶，超出淘汰最旧。
6. 同一底层文件的多个描述符要按底层对象键合并看待，关闭其中一个别名不能把仍在使用的别名标成泄漏。
7. 对端进程在接收过程中退出时，已接收但未归还的描述符由下一次 Sweep 识别并释放，ReleasePeer 负责把它名下的在途记录一次结清。
8. 重启后从快照恢复租户配额与审计，恢复出来的在途描述符一律判为孤儿并回收，不能继续算在租户头上。
9. AcceptConn 只接受有效的 unix 连接，空连接或已关闭的连接返回 ErrBadConn；对端身份取自连接凭据，不从消息内容猜。
10. Stage 只对已登记对端生效，未知对端返回 ErrUnknownPeer，空批次返回 ErrNoDescriptors。
11. 同一连接上的接收按序处理，先到先得，不能因为某个租户超限就把这条连接的后续消息乱序放行。
12. 句柄与统计按租户隔离，一个租户打满配额不能影响邻居的准入。

两类硬约束必须满足：同一租户的在途计数与审计追加要么一起生效要么都不生效，并发接收时不得出现计数已加而审计缺失；快照整体生效或整体不生效，恢复失败不得留下部分租户配额。时钟通过 Limits.Now 注入，为空时取系统时间。

验收方式：在题目目录执行 go test ./...，十五条用例必须全部通过；不得修改测试、跳过用例或按固定输入写死结果；并发接收与重启回收要自己构造场景验证；只做本地实现。

## 运行

```bash
go test ./...
```

测试位于 `tests/test_scm_rights_broker_test.go`，以 `impl` 别名导入本模块。
