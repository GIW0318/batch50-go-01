# HTTPS/SVCB 记录解析与别名链展开

## 题面

上线 HTTPS 记录之后，一部分用户访问公司站点握手直接失败，另一部分连上了但落在错误的端口。抓包拆开看：别名记录指向的服务名解析不到，客户端却把别名当成了最终地址；有一条记录里 mandatory 列了客户端不认识的键，客户端只把这个键忽略掉继续连接，结果协议参数全错；还有的应答里 ipv4hint 与 A 记录给的地址对不上，客户端各取一半，连到了不存在的端口。落点在模块 gsb/batch50-go-safety-b-19 的 svcb_alias_resolver.go，类型 SvcbAliasResolver，签名保持：NewSvcbAliasResolver(cfg ResolverConfig) *SvcbAliasResolver、Resolve(name string) (Chain, error)、Params(chain Chain) (Params, error)、Validate(rr Record) error、Cached(name string) (Chain, bool)、Refresh(name string) (Chain, error)、Flush(name string) bool、Stats() ResolverStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

需要落地的判定：

1. 名字为空报 ErrBlankName；查询失败报 ErrLookup；应答为空报 ErrNoRecord；记录数超过 MaxRecords 报 ErrRecordLimit，且不落缓存。
2. 同一应答里优先级 0 与大于 0 混排时报 ErrMixedMode；优先级为 0 的记录多于一条也按同一个错误处理。
3. 优先级 0 按别名模式处理：目标必须存在且不是根名，按目标继续查询；别名跳数超过 MaxAliasDepth 报 ErrAliasDepth，链上出现重复名字报 ErrAliasLoop。
4. 服务模式的目标名为根名表示只用当前名字的地址提示，不再解析新名字。
5. 别名模式记录携带服务参数属于格式错误，必须整条拒绝，报 ErrAliasParams。
6. 单条记录校验：优先级越界报 ErrBadPriority，目标为空报 ErrBadTarget，同一记录里参数键重复报 ErrDuplicateParam，端口不是数字或超出 1 到 65535 报 ErrBadPort，mandatory 列了记录里没有的键报 ErrMandatoryMissing，no-default-alpn 没有配 alpn 报 ErrParamConflict，提示地址解析失败报 ErrBadHint。
7. mandatory 出现不认识的键时该记录整条不可用，报 ErrUnknownMandatory，不能把未知键当作不存在继续合并。
8. 服务模式按优先级升序、同优先级按目标名升序排；合并只取优先级最小的一组，组内同一键不同值报 ErrParamConflict，相同值可以并存。
9. alpn 按逗号拆分并保持原始顺序去重；端口缺省 443，记录里的 port 参数优先。
10. ipv4hint 与 ipv6hint 按逗号拆分、逐项校验地址族、按出现顺序去重，超过 MaxHints 报 ErrHintLimit；提示与独立 A 记录同时存在时以提示为准，端口参数仍然生效。
11. 缓存按 ChainTTL 过期，Cached 过期后返回 false；同一名字并发 Resolve 只查一次；Refresh 强制重查并替换，Flush 返回是否命中。
12. 快照覆盖链缓存与计数，恢复后 Cached 与 Params 的结果与快照一致，损坏载荷报 ErrBadSnapshot 且不动缓存。

硬约束两条：同一名字的并发解析只允许一次查询，所有调用方拿到同一条链，缓存写入与过期判断必须原子；别名链展开过程里的跳数、环检测与缓存写入要么全成功，要么不留下半条链。

验收：go test ./... 的 15 条用例必须全过，不得修改测试、跳过或把期望值按固定输入写死；并发解析与重启恢复自行构造验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-19`
- 实现文件：`svcb_alias_resolver.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SvcbAliasResolver`
- 只依赖标准库
