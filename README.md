# 外部内容注入筛查与工具调用授权

## 题面

值班客服复盘时发现，模型把工单里粘贴的一段网页内容当成了系统指令，擅自调用删除接口清掉了一个客户的配置；另一起事故里，前一位会话隔离掉的可疑片段，被用户在提问中贴了一半原文引用，结果整段又回到了上下文，隔离形同虚设。现场代码在 gsb/batch50-go-safety-c-32 的 prompt_injection_gate.go，类型 PromptInjectionGate，要在外部内容拼入模型上下文之前做来源分段标注与注入筛查，并对模型发起的工具调用做白名单与参数校验。

接口保持这些签名：NewPromptInjectionGate(limits Limits, tools []string) *PromptInjectionGate、Inspect(chunk Chunk, origin Origin) (Finding, error)、BuildContext(chunks []Chunk) (Context, error)、AuthorizeCall(ctx Context, call ToolCall) (Decision, error)、Quarantine(chunkID string, reason string) error、Release(chunkID string) error、Explain(decisionID string) (Trace, error)、Quarantined() []string、Snapshot() ([]byte, error)、Restore([]byte) error。

要修的行为与边界：

1. 片段号为空、或来源为空，Inspect 与 BuildContext 都按 ErrEmptyChunk 拒收，不能靠内容非空顶上。
2. 外部内容里出现覆盖指令、角色标记、工具指令、分隔符这类信号时，一个信号算 suspicious，含角色标记或两个以上信号直接 blocked，信号名按升序回填；普通业务文本必须判成 clean 且信号为空，不能因为含有关键词碎片就误报。
3. Quarantine 必须带非空原因，否则 ErrBlankReason；未知片段 ErrUnknownChunk；同一片段重复隔离幂等成功；Release 未隔离片段返回 ErrNotQuarantined。
4. BuildContext 跳过隔离片段并在 Skipped 里计数，其余片段按传入顺序保留编号，返回的上下文编号不能为空。
5. 单会话片段数超过 MaxChunksPerContext 报 ErrContextLimit；登记片段总数超过 MaxTrackedChunks 报 ErrChunkLimit，超限拒新不动旧。
6. AuthorizeCall 只放行白名单里的工具，越权一律 ErrToolDenied，并在决定上留下工具名、原因码和上下文编号，允许与被拒都能用 Explain 查到。
7. 参数键值总字节超过 MaxArgsBytes 按 ErrToolDenied 拒绝，原因码 reason_args_too_large，不能截断参数后硬跑。
8. 每分钟调用次数超过 MaxCallsPerMinute 按 ErrToolDenied 拒绝，原因码 reason_rate_limited；被拒的调用留编号，但不占下一分钟的放行额度。
9. 外部内容里出现与系统提示同形的指令时，判定不能只靠字符串匹配，要按来源与位置标注可信级别，系统与用户片段的可信度不同。
10. 工具返回的内容同样不可信，回灌上下文之前要再筛查一次，不能因为来自自家工具就直接标注为可信。
11. 隔离片段如果被用户显式引用，要保留引用关系，但只允许摘要形式进入上下文，原文不得回流；快照里要能看出这段引用。
12. 同一会话的上下文构建与工具授权串行执行，并发调用下放行条数精确等于上限；重启恢复后隔离清单、上下文登记、调用计数窗口与未完成决定都还在，损坏的快照 Restore 返回 ErrBadSnapshot 且不得清掉现有隔离清单与计数。

硬约束：单会话片段数、登记片段数、参数字节、每分钟调用数四项上限在并发下精确生效，不能事后裁剪；隔离、授权与快照互斥，快照只能看到全量旧状态或全量新状态。

验收：go test ./... 15 条全过；不得修改测试、跳过或用固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-32`
- 实现文件：`prompt_injection_gate.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`PromptInjectionGate`；辅助类型 `Limits`、`Chunk`、`Origin`、`Finding`、`Context`、`ToolCall`、`Decision`、`Trace`
