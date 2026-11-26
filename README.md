# 上传网关的 multipart 准入与配额治理

## 题面

上传网关升级后，运维报来一串故障：4GB 视频传到 99% 才被拒，临时盘已经被打满；同名表单字段后一个把前一个覆盖，业务侧拿到的值不对；带 Content-Transfer-Encoding: base64 的 part 被当成原始字节直接落盘；正文里恰好出现 boundary 字符串时，解析器把文件切成两半。走查还发现超限后整段临时文件仍留在盘上，清理任务只认已完成回执。请落在 gsb/batch50-go-safety-b-07 的 multipart_intake_guard.go，类型 MultipartIntakeGuard，签名保持 NewMultipartIntakeGuard(Limits) *MultipartIntakeGuard、Begin(header http.Header) (Upload, error)、Write(uploadID string, p []byte) (int, error)、NextPart(uploadID string) (PartMeta, error)、NextNested(uploadID string) (PartMeta, error)、ReadPart(uploadID string, p []byte) (int, error)、CommitPart(uploadID string, index int, payload []byte) error、Finish(uploadID string) (Receipt, error)、Abort(uploadID string) error、Expire(now time.Time) int、Snapshot() ([]byte, error)、Restore([]byte) (int, error)。

要修的行为：
一、boundary 解析：Content-Type 不是 multipart/form-data 或缺少 boundary 报 ErrMissingBoundary；boundary 带空格、为空或超过 70 字节报 ErrBadBoundary；带引号时必须正确去引号。
二、流式配额：累计字节超过总配额时当次 Write 整块拒收并报 ErrQuotaExceeded，上传立即作废并从活跃表移除，不允许先收满再判断。
三、part 数量与头大小：超过 MaxParts 报 ErrPartLimit，单个 part 头块超过 MaxHeaderBytes 报 ErrHeaderLimit；part 头必须以空行结束，头部不完整时按解析失败处理。
四、文件名：先按 / 与 \ 取基名；空名、点、点点以及 CON、PRN、AUX、NUL 等保留名报 ErrBadFilename；超过 MaxNameLen 报 ErrNameTooLong。
五、提交：index 必须在已解析范围内，否则报 ErrPartIndex；payload 长度与该 part 声明大小不一致报 ErrPartSize；同一 index 重复提交报 ErrPartConflict，并发提交同一 index 只能有一个成功；提交按序号顺序落账，未提交的 part 不得出现在回执里。
六、收尾：必须读到结束分隔符且每个 part 都已提交，否则 Finish 报 ErrBadState；成功回执给出 part 数、总字节与按序拼接载荷的 SHA-256 十六进制摘要。
七、生命周期：Abort 立即回收并从活跃表移除，重复 Abort 同一 id 返回 nil，之后 Write 报 ErrUnknownUpload；Expire 只清理超过 IdleTimeout 未再写入的上传，并返回被回收的上传数，已完成回执不受影响。
八、重启：Snapshot 保存已完成回执与计数，Restore 返回被丢弃的在途上传数，损坏数据报 ErrBadSnapshot。
九、嵌套：part 的 Content-Type 为 multipart/* 时只允许白名单类型，否则报 ErrNestedType；NextNested 逐层下降，超过 MaxNestedDepth 报 ErrNestedDepth；嵌套 part 自身的头块同样受 MaxHeaderBytes 约束。
十、统计：Accepted、Rejected、Bytes 与 Active 四个计数必须随完成、作废与过期同步变化。

最容易漏的三处：同名字段重复出现时按 DuplicatePolicy 保留首个或整单拒绝，不能静默让后者覆盖前者；part 的 Content-Transfer-Encoding 不是 7bit 或 8bit 时不能按原始字节直传；boundary 字符串出现在正文里而前一行不是分隔符时要按协议语义识别，不能简单按字符串切分。

硬约束：配额在流式接收中逐块预扣，任何一块被拒都不得落盘；同一上传的 part 提交按序号原子生效，任一部分失败整单不可 Finish；Abort 与超时后缓冲必定回收，重启后只保留已完成回执；同一上传的缓冲与临时文件在同一把锁下变更，快照只能看到完整状态。

验收：go test ./... 15 条全过，不得改测试、跳过或按固定输入写死；并发提交与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-07`
- 实现文件：`multipart_intake_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`MultipartIntakeGuard`
