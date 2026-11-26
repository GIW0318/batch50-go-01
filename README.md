# NTS 安全关联的密钥交换与 cookie 池治理

## 题面

时间同步监控接上 NTS 之后，某机房的对时开始大面积失败。日志里三条线索：一是密钥交换回来的八个 cookie 只用掉两个，之后客户端不停重发旧的那个，服务端一律按伪造拒绝；二是某台时间服务器从主用地址切到备用之后，客户端还在用旧关联的 cookie 与旧唯一标识，认证全不过却继续发请求；三是密钥交换应答里的认证加密算法与请求里列的不一样，客户端没有拒绝，而是按原算法继续算，最后整批包被对端丢弃。落点在模块 gsb/batch50-go-safety-b-20 的 nts_key_exchange_guard.go，类型 NtsKeyExchangeGuard，签名保持：NewNtsKeyExchangeGuard(cfg NtsConfig) *NtsKeyExchangeGuard、Begin(server string, nonce []byte) (KeRequest, error)、Establish(res KeResponse) (Association, error)、TakeCookie() (Cookie, error)、Authenticate(pkt Packet) (Verdict, error)、Rotate(server string) error、PoolSize() int、Status() (Association, error)、Stats() NtsStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

需要落地的判定：

1. Begin 服务器为空报 ErrBlankServer，随机数不是 16 字节报 ErrBadNonce；已有未完成请求时再 Begin 报 ErrPendingRequest。
2. Establish 没有待处理请求报 ErrNoPendingRequest；应答回显的随机数与请求不一致报 ErrNonceMismatch，且待处理请求保留；已用过的随机数再次 Begin 报 ErrReplayNonce。
3. 算法不在协商列表内时整条关联作废，报 ErrAlgorithmNotNegotiated，不能降级到别的算法。
4. 双向密钥必须各 32 字节，否则报 ErrBadKeyMaterial；cookie 数必须等于约定值（默认八个），否则报 ErrCookieCount；两者失败都不建立关联。
5. 应答时间戳与当前时钟相差超过 SkewWindow 报 ErrTimestampWindow。
6. 建立成功后关联记录服务器、算法、唯一标识、代次与建立时刻，池里正好有八个可用 cookie。
7. TakeCookie 按先进先出取用一个 cookie 并标记已消费，池剩两个及以下时关联要置 NeedsRekey，供上层提前重新交换密钥。
8. 同一 cookie 被并发取用时只能有一个成功；池耗尽时不得复用旧 cookie，也不得退回未认证请求，必须返回明确错误。
9. Authenticate 依次核对服务器地址、唯一标识、算法与时间戳，任何一项不符都拒绝并计数；cookie 必须落在已取用集合里，仍留在池里的与从未发出过的都按未知 cookie 拒绝。
10. 服务器地址变化要作废整条关联：Rotate 传入不同地址报 ErrReestablishRequired，之后取 cookie、认证、读关联都只能得到未建立关联的错误。
11. 快照覆盖关联、池与已消费 cookie 的标识，恢复后已消费的 cookie 不能复活，池的顺序与剩余数量要和快照一致。
12. 损坏载荷报 ErrBadSnapshot 且不动现网关联；恢复之后重新建立关联，代次要继续往上走。

硬约束两条：同一 cookie 被并发取用时只能有一个成功，池的推进与关联作废必须原子，认证路径不允许读到半新半旧的关联；快照恢复要么全量生效要么不动，恢复后已消费标识、池顺序与代次必须和快照逐项一致。

验收：go test ./... 的 15 条用例必须全过，不得修改测试、跳过或把期望值按固定输入写死；并发取用与重启恢复自行构造验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-20`
- 实现文件：`nts_key_exchange_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`NtsKeyExchangeGuard`
- 只依赖标准库
