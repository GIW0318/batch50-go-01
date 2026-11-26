# 设备证书登记的引导凭据、CSR 策略与重试预算

## 题面

设备远程登记上线两周后，运维发现三类怪事：同一台设备连点两次申请，后台排出两条队列记录，最终签出两张证书；已经用完的引导码还能被别的设备再用一次；还有一批申请把通配 SAN 写成 *.example.com，直接盖住了整个域名后缀，签发流程照样收下。登记日志里还能看到同一台设备的两条记录时间相差不到一秒，签发结果却各自占了一个序列号。落点是模块 gsb/batch50-go-safety-a-05 的 certificate_enrollment_gate.go，类型 EnrollmentGate，公开接口保持：NewEnrollmentGate(cfg Config) *EnrollmentGate、RegisterBootstrapToken(token, tenantID string) error、SetProfile(p Profile) error、Enroll(ctx context.Context, req EnrollRequest) (Enrollment, error)、Poll(enrollmentID string) (State, error)、Revoke(enrollmentID string) error、Budget(deviceID string) (Remaining, error)、Issue(enrollmentID string) error、RotateCA() error、CAEpoch() int、Pending(tenantID string) int、Sweep(now time.Time) int、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的行为如下。设备号、租户号、引导凭据任一为空报 ErrBadRequest；凭据未登记或已被消费报 ErrBootstrapRejected；CSR 解析失败报 ErrBadCSR。公钥强度上，RSA 不低于 MinRSABits，ECDSA 只认 P-256 与 P-384，弱密钥报 ErrKeyTooWeak。SAN 一个都没有报 ErrSANMissing；SAN 必须落在租户域名后缀之下，否则报 ErrSANOutsideTenant；通配只允许最左一段且必须整体落在后缀内，*.corp.example.com 可以，*.example.com 报 ErrWildcardTooBroad，这一判断要在这个分支先给出，不能先报成 SAN 越界。CSR 里带 CA 基本约束报 ErrCAFlagForbidden。用途档案不存在报 ErrUnknownProfile，keyUsage 与 EKU 对不上档案报 ErrKeyUsageMismatch。受理成功后返回 queued 状态、RetryAfter 与当前 CA 代际，客户端按 retry-after 轮询。

排队与预算：同一设备同一时刻只能有一张在途 CSR，无论重复提交的是同一份 CSR 还是另一份，都返回那一条在途登记，不新增队列条目。租户队列长度超过 MaxQueue 报 ErrQueueFull，不能静默淘汰旧申请。每设备失败次数达到 MaxAttemptsPerDevice 后，后续提交报 ErrAttemptsExhausted；Budget 里的 Attempts 只统计校验失败次数，成功受理不占失败配额。Revoke 对不存在的登记报 ErrUnknownEnrollment，重复撤销幂等成功。Poll 读到的 State 要包含状态、已用失败次数与序列号，未知登记同样报 ErrUnknownEnrollment；Sweep 回收排队超时的申请并返回条数。

还有三条边界。引导 token 已经消费但 CSR 校验没过时，配额是否归还要按一次性凭据的语义处理，不能自动补发一个新的。同一把公钥换一个 Subject 再来申请，要按重复登记拒绝，不能覆盖已有的证书。CA 轮换之后，在途申请的签发与归属仍按提交时的那一代 CA 完成。

硬约束：同一设备并发提交靠原子占位串行化，只能留下一条在途记录；同 CSR 指纹的签发结果幂等。每设备与每租户的尝试配额、队列长度与在途时长都有上限。重启后排队状态、已消费凭据与重试计划要延续，损坏载荷在 Restore 时报 ErrBadSnapshot，恢复后已撤销的登记不能被重新激活。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；并发占位与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-a-05`
- 实现文件：`certificate_enrollment_gate.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`EnrollmentGate`
