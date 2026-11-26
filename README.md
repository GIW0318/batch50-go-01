# 内核模块加载签名闸门

## 题面

上一轮灰度把一台编译机的内核模块签名密钥轮换做漏了，结果带外编出来的驱动模块在强制模式的机器上照样被 insmod 进去，dmesg 里 taint 位一直是零，安全组还以为这批机器是干净的；另一台机器更别扭，签名校验失败的模块确实没装上，可内核 taint 状态被动过，重启后基线一对就露馅。这批驱动是第三方存储厂商提供的，出问题的那两台机器上跑着计费库，重启之后模块表还是空的，谁也说不清上一轮到底放行了哪些东西。现在要把加载前后的闸门收口，落点是模块 gsb/batch50-go-safety-c-08 的 module_sign_gate.go，类型 ModuleSignGate，公开接口保持不变：NewModuleSignGate(cfg GateConfig) *ModuleSignGate、TrustRoot(fingerprint string) error、AllowModule(p Policy) error、Load(req ModuleRequest) (LoadResult, error)、Unload(name string) (bool, error)、SetEnforce(on bool, at int64) error、Sweep(at int64) ([]Removal, error)、VerifyChain(req ModuleRequest) (ChainReport, error)、Modules() []LoadedModule、Taint() TaintState、AckTaint(bits uint32, at int64) error、Audit() []AuditEntry、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的行为：一、模块名、版本、哈希为空或名字里带路径分隔符、点点，报 ErrBadModule。二、同名模块已加载且哈希一致按幂等处理，AlreadyLoaded 为真且不占新容量；哈希不同报 ErrHashMismatch。三、白名单按模块名先命中、再校验哈希，哈希不符报 ErrPolicyDenied，不能只凭名字放行；同名条目重复登记以最后一次为准。四、签名链逐段校验，issuer 必须衔接下一段的 subject，空链或断链报 ErrChainBroken。五、链尾指纹必须落在信任集内，否则报 ErrUntrustedRoot；链上任一证书的加载时刻晚于 NotAfter 报 ErrCertExpired。六、未签名模块在非强制模式可以加载，但必须置 TaintUnsignedModule；树外模块另置 TaintOutOfTree。七、强制模式开启后，新的未签名加载报 ErrEnforceDenied，这次调用不得改动已加载模块。八、模块表受 MaxModules 硬顶，超出报 ErrModuleLimit；Unload 未知模块返回 false 与 nil。九、taint 只增不减，AckTaint 只清传入的位，传零或未定义位报 ErrBadTaint。十、每次加载、卸载都写一条审计，记录模块名、结果与序号，只留指纹不留签名原文，序号严格递增。十一、快照覆盖信任集、模块表、taint、强制开关与序号；数据损坏报 ErrBadSnapshot 且不动现有状态；恢复不得把 taint 降下来。

最容易漏的两处：强制模式打开时，先前已经在表里的未签名模块不能被静默放行，Sweep 必须把它们报成待移除；签名验证失败的尝试必须保留原有 taint 状态，不能先置位再回滚。

硬约束：Load、Unload、SetEnforce、Sweep 共用同一份状态，并发调用不得看到模块表与 taint 不一致的中间态，序号严格递增；模块表与白名单都受配置上限约束，加载失败必须整次不写入，不能只落半个模块行，重复加载与卸载按幂等口径记账。

验收：go test ./... 15 条全过，不得改测试、跳过或按固定输入写死；强制模式、并发与快照恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-08`
- 实现文件：`module_sign_gate.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`ModuleSignGate`
