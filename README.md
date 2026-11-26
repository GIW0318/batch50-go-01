# 证书透明度 SCT 核验：签名、日志名单与合并延迟上限

## 题面

支付网关按合规要求开启了证书透明度核验。上线第一周运维同时收到两类工单：一批刚续签的边缘节点证书在握手阶段被判成无有效 SCT 整体拒绝，值班只能先把核验关掉止血；另一批来自合作方 CA 的证书，签发的 SCT 时间戳明显超出日志承诺的合并上限，却照样被放进生产。合规审计还指出，放行与拒绝都要能按日志 ID 逐条复盘，现在只能看到一个总数。问题集中在 gsb/batch50-go-safety-b-15 的 sct_log_verifier.go，类型 SctLogVerifier，需要把这套核验链路补齐。

公开签名保持不变：NewSctLogVerifier(VerifierPolicy) *SctLogVerifier；ApproveLog(LogPolicy) error；RevokeLog(logID string, at int64) error；Log(logID string) (LogPolicy, bool)；Verify(cert Certificate, at int64) (Decision, error)；VerifySct(cert Certificate, sct SctEntry, at int64) (SctResult, error)；MergeDelay(cert Certificate, sct SctEntry, log LogPolicy) (int64, error)；ApprovedLogs() []string；Stats() VerifierStats；Snapshot() ([]byte, error)；Restore([]byte) error。

调度口径：Verify 对一张证书只调用一次，结果里的 UsedLog、Valid、Checked 和按日志 ID 索引的拒绝原因表就是审计取数的地方，不能留空；ApprovedLogs 必须按日志 ID 升序，方便与配置中心的名单逐行比对；Verified、Allowed、Denied 三个计数只增不减。

要处理的故障与边界：一、证书序列号为空时直接报 ErrBlankSerial，不能拿它去查日志；二、NotAfter 不晚于 NotBefore 报 ErrInvalidValidity；三、随证书带来的 SCT 条数超过 MaxSctsPerCert 报 ErrTooManyScts，不做静默截断；四、日志 ID 为空报 ErrBlankLogID，状态不在 active、retired、revoked 之内报 ErrBadLogState；五、同一日志重复登记且状态与合并上限完全一致算幂等成功，改过任一字段报 ErrLogConflict，原登记不得被覆盖；六、撤销不存在的日志报 ErrUnknownLog，重复撤销保留第一次的撤销时刻并返回成功；七、签名必须以 sct-sig: 开头且不短于 20 个字符，否则该条记 bad_signature；八、摘要算法只认 sha256 与 sha384，其余记 unsupported_hash，不参与放行；九、日志不在名单记 unknown_log，处于 retired 记 log_retired，撤销之后的 SCT 记 log_revoked；十、SCT 时间戳早于证书生效时间超出偏差容忍、晚于证书失效时间、或晚于核验时刻超出容忍，分别记 outside_validity 与 timestamp_skew，时间窗判定先于合并延迟；十一、合并延迟按 SCT 时间戳减证书生效时间算，超过该日志上限（未设则取默认值）记 merge_delay_exceeded，负得超过容忍报 ErrTimestampSkew；十二、多份 SCT 里只要有一份核验通过即可放行，不能要求携带的日志全部在名单内；日志被撤销后，撤销之前已经签发的旧证书在宽限期内仍应放行。

后两条是本次对接明确的口径，别按全部合格或撤销即全拒来实现。

硬约束：Verify 与 ApproveLog、RevokeLog 会被多个握手协程并发调用，任何一次判定只能看到全量的旧名单或全量的新名单，计数不得丢失；同一日志被并发撤销时只有一个撤销时刻生效；同一张证书在名单未变时重复核验要给出同样的 Verdict 与 Reason。快照必须带出日志名单、撤销水位与核验计数，Restore 之后的状态与进程重启前一致，损坏载荷报 ErrBadSnapshot 且现网名单不动。拒绝原因表按 MaxRejectReasons 封顶，不随证书携带的 SCT 条数增长。

验收：go test ./... 的 15 条用例必须全过，不许改测试、不许跳过、不许按固定输入写死；并发与重启自行写程序验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-15`
- 实现文件：`sct_log_verifier.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SctLogVerifier`
