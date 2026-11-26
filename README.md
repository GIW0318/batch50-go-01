# mDNS 服务名冲突裁决与记录缓存

## 题面

研发楼的打印服务和投影控制服务都跑在几个固定的实验盒上，上周开始出现怪事：两台盒子的服务都用 printer 这个名字对外广播，访客手机上的服务列表隔一会儿就多出一个 printer-2，再过一会儿又变成 printer-3，序号一路往上爬；有人手动改了实例名之后，服务在两台盒子上同时出现在线，点名的时候一半请求打到旧地址；网络组抓包还发现，个别盒子的应答带着 cache-flush 标记，整张邻居表被清空，其它接口上的记录也一起没了。现场代码在 gsb/batch50-go-safety-b-13 的 mdns_service_conflict_resolver.go，类型 MdnsServiceConflictResolver，需要把注册探测、冲突裁决、改名与记录缓存这条链路补全。

接口保持如下签名：NewMdnsServiceConflictResolver(cfg Config) *MdnsServiceConflictResolver、Register(svc Service) (State, error)、Observe(msg Message) (Event, error)、Commit(instance string) error、Withdraw(instance string) error、StateOf(instance string) (State, bool)、States() []State、Events(instance string) []Event、Cache() map[string][]Record、Snapshot() ([]byte, error)、Restore([]byte) error。

现场需要落实的行为与边界：

1. 实例名与服务类型去掉空白后为空报 ErrBadService；同一实例名重复注册报 ErrDuplicateInstance，不能悄悄覆盖原记录。
2. 注册成功进入 probing，Probes 从零开始，Seq 为正；注册身份始终用最初的实例名作键，改名只更新 Current。
3. Commit 每次只推进一格：probing 到 announcing、announcing 到 established、renamed 到 announcing；其它阶段报 ErrBadPhase 并保持原状态。
4. 收到同名且同类型的应答时按字节序裁决：对端名字更小就让步改名，事件类型 renamed，候选名是原名加序号，第一次改为 printer-2；对端名字更大则保留原名，事件类型 kept。
5. 已经让步过的对端名字记入名单，同一对端再来应答不再改名，事件记 ignored，序号不能被反复推高。
6. 对不属于自己的实例名或不同类型的应答只写缓存，事件类型 cached，不得影响已有注册的相位。
7. cache-flush 置位的应答按接口分桶整体覆盖，只换命中的那个接口，其它接口的记录必须留着；未置位时同接口记录追加。
8. 每次 Observe 都往对应注册的事件表追加一条，Seq 全局递增。
9. Withdraw 成功后注册与事件一起消失，同名再次 Withdraw 报 ErrUnknownInstance。
10. probing 期间收到自己发出的多播直接忽略，不得计入冲突，也不消耗改名额度。
11. 实例名与服务类型都相同才算冲突；同名但类型不同的应答不构成冲突，不能触发改名。
12. 改名次数达到 MaxRenames 后停在 conflict，再收到冲突应答报 ErrRenameExhausted，不能继续追加序号。

两类硬约束必须满足：Observe 与 Register、Commit、Withdraw 串行修改注册表与缓存，同一对端的重复应答并发到达时只能产生一条 renamed 事件；快照整体生效或整体不生效，Restore 遇到损坏载荷回 ErrBadSnapshot 且不动现网，注册相位、事件表、缓存分桶与 Seq 要一起恢复。

验收方式：在题目目录执行 go test ./...，十五条用例必须全部通过；不得修改测试、跳过用例或按固定输入写死结果；并发应答与重启恢复要自己构造场景验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-13`
- 实现文件：`mdns_service_conflict_resolver.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`MdnsServiceConflictResolver`
