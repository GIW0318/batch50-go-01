# HTTP 响应头注入探测编排

## 题面

渗透报告被客户退回：测试同学在搜索参数里塞了编码过的回车换行，网关把非法响应头丢掉了，可下游仍按原始字节拼接，浏览器实际收到了一条注入头；探测平台却因为只看了归一后的头集合，判定未命中，整条漏洞被漏报。还有一次，探测响应被前面的缓存层收下并复用，普通用户随后也拿到了带探测头的结果。现场代码在 gsb/batch50-go-safety-c-33 的 header_injection_prober.go，类型 HeaderInjectionProber，要对候选参数与请求头做响应头注入探测，覆盖 CRLF 变体、多重编码与网关归一差异，并定位注入结果落在响应的哪一段。

接口保持这些签名：NewHeaderInjectionProber(limits Limits) *HeaderInjectionProber、Plan(point HeaderPoint) (CaseSet, error)、Craft(caseID string) (Request, error)、Submit(caseID string, resp Response) error、Judge(caseID string) (Outcome, error)、Cleanup(caseID string) error、Cases(pointID string) []Case、Progress(pointID string) (int, int)、Snapshot() ([]byte, error)、Restore([]byte) error。

要修的行为与边界：

1. 注入点的编号、路径、参数名、请求头名任一为空，Plan 返回 ErrBlankPoint，不能默默补默认值。
2. 每个注入点固定生成 8 条用例：CRLF、裸 LF、二次百分号编码、UTF-16 编码四种载荷，各配查询参数与请求头值两个落点；用例编号唯一，载荷不能为空。
3. 注入点数超过 MaxPoints 报 ErrPointLimit；单点用例预算小于 8 时 Plan 直接报 ErrCaseLimit，不许静默少建几条。
4. Craft 未知用例返回 ErrUnknownCase；生成的请求沿用注入点的方法与路径，查询落点把载荷编进参数，请求头落点写进指定头，两者都带 X-Probe-Case 标注用例编号。
5. Craft 受滚动窗口速率限制，超过 MaxRequestsPerWindow 报 ErrRateLimit，且不产生半成品请求。
6. Submit 前必须先 Craft，否则 ErrCaseNotCrafted；同一用例重复提交返回 ErrCaseAlreadySubmitted，后到的响应不得覆盖前一份。
7. Judge 前必须 Submit，否则 ErrNotSubmitted；已清理的用例再判返回 ErrCaseCancelled，清理本身可重复调用。
8. 命中判定认响应头集合里出现的 x-gsb-probe 头，不区分大小写，Outcome 要回填落点 header、头名与实际值；未命中也必须给出原因码，不能只回一个 false。
9. 判定同时保留原始响应字节与解析后的头集合，网关丢掉非法头而下游仍按原始字节解析时不能漏判。
10. 多重编码的载荷可能在某一层被解码后进入头值，要逐层判定，不能只试最终形态一次。
11. 清理要覆盖探测期间产生的缓存条目，避免缓存层把注入响应复用给其他会话；缓存键与用例编号的对应关系要能从状态里查到。
12. 同一注入点的用例串行处理，速率窗口与进度在并发下精确生效；快照恢复后用例、响应、进度与原速率窗口都还在，未提交的请求作废，损坏的快照 Restore 返回 ErrBadSnapshot 且不动现有用例与进度。

硬约束：单点用例数、注入点数、每分钟请求数三项上限在并发下精确生效，不能事后裁剪；用例状态迁移、判定与快照互斥，快照只能看到全量旧状态或全量新状态。

验收：go test ./... 15 条全过；不得修改测试、跳过或用固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-33`
- 实现文件：`header_injection_prober.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`HeaderInjectionProber`；辅助类型 `Limits`、`HeaderPoint`、`Case`、`CaseSet`、`Request`、`Response`、`Header`、`Outcome`
