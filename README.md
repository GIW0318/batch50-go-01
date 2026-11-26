# gsb/batch50-go-safety-c-24

## 题面

依赖治理平台在梳理一批 Java 反序列化风险时暴露了问题：同一个通用工具类出现在三个不同坐标的制品里，扫描报告把它们拼成一条跨制品利用链，复核时发现这条链在任何一个运行时都不存在；另一条链的中间跳转靠反射完成，静态索引看不到，平台却直接标成可用，运营按它复现屡屡失败；制品升级后旧索引失效，缓存仍在引用旧链，报告只说“已失效”，没有给出依据。

需要在 gsb/batch50-go-safety-c-24 的 gadget_chain_matcher.go 里补全 GadgetChainMatcher，入口 NewGadgetChainMatcher(Options) *GadgetChainMatcher。公开接口：Index(Dependency) (ArtifactID, error)；Query(EntryPoint, Sink) ([]Chain, error)；Explain(string) (ChainTrace, error)；Validate(string, EnvSpec) (Verdict, error)；Invalidate(string) error；Artifacts() []ArtifactID；Chain(string) (Chain, bool)；NodeCount(string) int；Snapshot() ([]byte, error)；Restore([]byte) error。

索引阶段：Index 校验 group、artifact、version 均非空白，版本必须是三段数字，任一不满足分别返回 ErrBlankArtifact 与 ErrBadVersion；节点按方法计，超过 MaxNodesPerArtifact 返回 ErrNodeLimit 且不留半成品；ArtifactID 的 Key 渲染为 group:artifact:version；同一坐标重复 Index 视为整体替换，索引只保留一份，节点数按新清单重算；制品之间按坐标区分，类名相同不算同一节点。

查询阶段：Query 从入口类方法出发，沿已索引调用逐层走到汇点，Chain 带 ID、Depth、Hops、Artifacts 与 Status，Artifacts 是链上去重后的坐标集合；每条 Hop 记录声明该方法的坐标、类名与方法名。多条路径按 Depth 升序、同深度按跳转序列字典序，同一索引下重复查询必须给出相同 ID，返回条数不超过 MaxChains。没有可达路径返回 ErrNoPath；超过 MaxDepth 的路径剪掉，剪完为空也按 ErrNoPath 处理。Explain 回放一条链的跳转与状态，可用链原因 reason_usable，未知 ID 返回 ErrUnknownChain。

校验阶段：Validate 拿环境里已解析的坐标比对链上每个制品：缺少制品返回 ErrArtifactMissing；同一 group 与 artifact 的主版本不同返回 ErrVersionMismatch；环境排除的类出现在链上时返回 Usable 为假、原因 reason_class_excluded；全部满足时 Usable 为真、原因 reason_usable。

失效阶段：Invalidate 按坐标删除索引，未知坐标返回 ErrUnknownArtifact；删除后 Artifacts 与 NodeCount 都不再包含它。

现场还有三类容易漏判的情况：同一类名在不同制品里重复出现时必须按制品坐标去重，否则会拼出跨制品的假链；中间跳转依赖反射或动态代理时静态索引看不到，这类链要标注为待验证而不是直接可用；制品升级后旧索引失效，已缓存的链仍被引用时要能说明失效原因，不能静默返回。

硬约束：索引构建与查询可以并行，同一制品的重建要串行，对外只能看到旧索引或新索引，不得看到半份；节点数、查询深度与返回条数三个上限同时生效；重启后恢复索引与失效水位，未完成的校验作废。

验收口径：go test ./... 的 15 条用例必须全过；不得修改测试、跳过或按固定输入写死；并发与重启自行验证；只做本地实现。交付源码、测试输出与故障说明。

## 运行

```bash
go test ./...
```

## 模块

- 模块路径：`gsb/batch50-go-safety-c-24`
- 包名：`task`
- 实现文件：`gadget_chain_matcher.go`
- 测试文件：`tests/test_gadget_chain_matcher_test.go`
