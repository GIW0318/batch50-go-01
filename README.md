# batch50-go-safety-c-45

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-45`
- 根包名：`task`
- 实现文件：`json_parse_differential.go`
- 测试目录：`tests/test_json_parse_differential_test.go`
- 类型：`JsonParseDifferential`

## 故障单

同一份登录报文在网关被解析成普通用户，业务侧却按管理员处理，越权就发生在两条链路之间；复盘时各方都坚持自己的解析没问题，报文来回贴了十几遍十六进制也没吵出结论，最后只能把责任推给「框架差异」。这段时间线上已经有三家客户报同一类越权，安全组要在本迭代内给出能复现、能定位到具体层的结论。要把解析差异做成可归因的结论，模块路径 gsb/batch50-go-safety-c-45，实现文件 json_parse_differential.go，类型 JsonParseDifferential，签名固定：NewJsonParseDifferential(cfg Config, cases []Case) *JsonParseDifferential、AddTarget(t Target) error、SetChain(c Chain) error、Case(name string) (Payload, error)、Probe(target Target, c Case) (Observation, error)、Compare(obs []Observation) (Verdict, error)、Explain(verdictID string) ([]Diff, error)、Targets() []string、BudgetLeft(targetID string) int、Observations() []Observation、Verdicts() []string、Snapshot() ([]byte, error)、Restore(data []byte) error。

十二处要落地：一，用例注册表按标识索引，每条用例只改一个维度，取不到报 ErrUnknownCase；载荷摘要取报文体的 SHA-256 小写十六进制。二，目标要先登记，未登记报 ErrUnknownTarget；没有挂链报 ErrNoChain，挂空链同样报错。三，Probe 按配置里的层序逐层送同一份报文，各层的接受与否、解析结果、是否被重写都要原样记下来，一次探测只产生一条观测，层序取配置值，改配置后按新层序记录。四，每个目标的探测预算独立记账，用完报 ErrBudgetExhausted，扣减只发生在真的发出请求之后。五，同一用例重复探测结果应当稳定，前后两次不一致的按不确定处理，既不能当成差异也不能当成一致。六，比较只接受同一个用例的观测，混了用例报 ErrMixedCases，空输入报 ErrNoObservations。七，各层接受与否不一致判高危，原因是接受度分歧，这个结论与用例影响面无关。八，各层解析结果不一致时，只有影响鉴权或写入的才升级为高危，纯展示差异最多算低危，不许一律报高危。九，两层都拒绝同一份畸形报文属于一致，不能算差异。十，报文被中间层重写后各层回显一致，不能据此判定解析一致，要单独标出重写原因。十一，结论标识按 verdict-1 起递增，Explain 按结论标识返回差异列表，每条差异写清两个层名与两侧取值，未知标识报 ErrUnknownVerdict，比较过程不得改动传入的观测。十二，快照要恢复目标、观测、结论与每个目标剩余预算，损坏快照整体拒绝且不动现有状态。

硬约束：同一目标的探测串行推进、不同目标并行，预算扣减与观测写入在同一临界区内完成，读方拿到的观测不得只写了一半；探测有冷却期与请求预算两道闸，重启后按快照恢复的预算继续计算，不能因为重启把预算清零。

验收：go test ./... 的 15 条用例全过，测试不得修改、跳过或按固定输入写死；并发探测与重启恢复自行验证；只做本地实现，交付源码与跑测记录。

## 运行

```bash
go test ./...
```

测试位于 `tests` 目录，包名同样为 `task`，通过 `impl "gsb/batch50-go-safety-c-45"` 引用根包，共 15 条用例。
