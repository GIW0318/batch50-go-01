# 剧情对话图运行：选项条件求值、环内一次性节点与图版本钉扎

## 题面

主线剧情更新后玩家卡在同一个对话里出不来：选项条件明明满足却显示灰色，全部选项都不可选时界面直接黑屏；一段可以反复进入的支线每绕一圈就重复发一次奖励，背包里堆了十几把同一件任务武器；更麻烦的是热更之后，进行中的会话在旧节点和新节点之间错位，选完一句话直接被踢出剧情。运维把当晚的会话记录拉出来，发现节点、条件、一次性标记和图版本四个东西根本没有被同一个会话持有。

落点是模块 gsb/batch50-go-game-a-10 的 dialogue_graph_runner.go，类型 DialogueGraphRunner，公开接口保持不变：NewDialogueGraphRunner(limits Limits) *DialogueGraphRunner、LoadGraph(graphID string, nodes []Node) (int, error)、GraphVersion(graphID string) (int, error)、Start(graphID, characterID string) (DialogueState, error)、Choose(sessionID, optionID string) (DialogueState, error)、Advance(sessionID string) (DialogueState, error)、VisibleOptions(sessionID string) ([]Option, error)、State(sessionID string) (DialogueState, error)、End(sessionID, reason string) error、SetVariable(sessionID, key string, value int) error、MarkQuest(sessionID, quest string) error、SessionCount() int、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收掉的地方如下。第一，图编号或节点列表为空报 ErrBadGraph，节点编号重复报 ErrDuplicateNode，Next、Fallback、选项 Next 指向不存在的节点报 ErrMissingNode，节点数超过 MaxNodesPerGraph 报 ErrGraphTooLarge。第二，每次成功加载同一张图版本号加一，GraphVersion 读当前版本，未加载的图报 ErrGraphNotFound。第三，Start 把会话钉在当前版本上，会话从节点列表第一个节点开始并带上角色编号。第四，选项条件支持变量 ge、eq、lt 与任务标记，缺变量按零值比较，全部满足才可选；VisibleOptions 按声明顺序返回可选选项。第五，Choose 只接受当前节点里可选的选项，条件不满足报 ErrOptionLocked，选中后先结算效果再进入目标节点。第六，节点没有可用选项时 Advance 走 Fallback，Fallback 也没配就报 ErrNoOption。第七，进入 end 节点后 Finished 置真，再 Advance 报 ErrSessionClosed。第八，End 关闭会话，重复调用幂等，关闭后 Choose 与 Advance 都报 ErrSessionClosed。第九，活跃会话数受 MaxSessions 硬顶，超出报 ErrSessionLimit，SessionCount 只算没结束的会话。第十，SetVariable 的变量表受 MaxVariables 硬顶，超出报 ErrVariableLimit。第十一，快照要带上图、版本与全部会话，恢复后 State、VisibleOptions、SessionCount 与关服前一致；损坏载荷报 ErrBadSnapshot 且不动现有数据。第十二，边的通过次数受 MaxEdgeVisits 限制，同一会话绕环超过上限报 ErrCycleLimit。

还有三处必须一起收掉。循环再次进入同一节点时，一次性选项与一次性节点的奖励标记必须已经用过，不能重复发放。从快照恢复的会话变量表要回到快照那一刻、也就是那次选择之前的状态，不能带回半应用的选项效果。图版本热更新时，进行中的会话继续用旧图跑完，只有新会话使用新图，节点编号不得错配。

硬约束有两类。一是原子性与一致性，Choose、Advance、End、SetVariable 会被同一会话的多路请求并发调用，一次 Choose 的变量变更与节点推进必须整体生效或整体不生效，-race 下不得有数据竞争。二是重启与上限，快照必须原子替换全部状态，恢复失败不得半套生效；MaxNodesPerGraph、MaxSessions、MaxEdgeVisits、MaxVariables 都是硬顶，超限报错而不是截断。

验收：`go test ./...` 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；环内一次性结算、快照回滚与热更新自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-10`
- 实现文件：`dialogue_graph_runner.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`DialogueGraphRunner`
