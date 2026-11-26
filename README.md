# 脚本内容判定与解码

## 题面

上传目录里一批伪装成图片说明的脚本被静态扫描放过了，上线后其中两个开始接收外部指令，事后追查发现它们各自套了两层 base64，而检测只解了一层就按干净放行。安全组要求把内容判定做成可解释的流程，落点 gsb/batch50-go-safety-c-30，实现文件 webshell_content_classifier.go，类型 WebshellClassifier，签名固定：NewWebshellClassifier(limits Limits) *WebshellClassifier、Scan(file FileRef) (Verdict, error)、Decode(file FileRef, maxLayers int) (Decoded, error)、Explain(fileID string) (Features, error)、Whitelist(hash string, ttl time.Duration) error、Rescan(fileID string) error、Prune(now time.Time) (int, error)、Snapshot() ([]byte, error)、Restore(data []byte) error，另配 Hashes、VerdictFor、Forget 三个辅助方法。

要修的点：一，文件编号或路径为空的引用直接拒绝并报 ErrBadFile，不能产生结论；二，单文件体积与解码层数都有上限，超体积报 ErrTooLarge，解码只认 b64、hex、url 三种前缀，b64 按标准字母表解，畸形载荷报 ErrBadEncoding；三，层数用尽后内容仍带编码前缀要标记 Truncated 并按未完全解码降级，不能当成干净；四，能力按解码后的内容匹配，动态执行 runCode、evalExpr、callNative，网络 openSocket、postHttp、fetchRemote，解码 fromBase64、inflateBlob、rot13Text，写盘 writeFile、appendFile；五，四种能力各计一次，权重为动态执行四十、网络二十五、解码二十、写盘十五，总分封顶一百，七十分及以上判 malicious，四十分到六十九分判 suspicious，其余判 benign；六，结论要给出命中的能力项并按名字升序，Explain 返回分值、状态与能力项，文件没有结论时报 ErrNoVerdict；七，白名单按内容哈希压制，哈希取原始内容的 SHA-256 小写十六进制，输入要去空格转小写，空哈希报 ErrBadHash，非正有效期报 ErrBadTTL，命中白名单的文件分值为零且状态为 whitelisted；八，结论缓存必须按内容哈希失效，路径相同而内容变了要重新判定，不能沿用旧结论；九，同一内容重复扫描要标记 Cached，Rescan 强制重算并清掉该标记，没有结论的文件重算报 ErrNoVerdict；十，Prune 清掉超过缓存有效期的结论与到期白名单，返回清理条数，清理后再扫要重新计算；十一，合法框架的入口文件也常含动态执行与解码调用，判定要结合路径与调用上下文，只匹配关键字会把正常业务误判，结论必须能说清是哪几处调用落在可疑上下文里；十二，多层编码可能在中间层就撞上体积上限，此时按未完全解码降级并保留已有能力证据，不得判成干净。

硬约束：同一文件的扫描、重算与白名单变更串行执行，不同文件之间不互相阻塞；单文件体积、解码层数与结论缓存条数都是硬上限，缓存满了按最旧淘汰而不是无限增长；结论缓存与白名单条目都带时间戳，时间基准统一由调用方传入，避免各机器时钟漂移；进程重启后白名单与结论缓存都要恢复，损坏的快照整体拒绝且不动现有结论。

验收：go test ./... 的 15 条用例全过，测试不得修改、跳过或按固定输入写死；并发扫描与重启恢复自行验证；只做本地实现，交付源码、跑测记录与一页判定口径说明。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-30`
- 实现文件：`webshell_content_classifier.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`WebshellClassifier`
