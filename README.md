# GOP 结构规划：IDR 间隔、场景切强制插入与开放 GOP 参考约束

## 题面

编码农场这周被投诉了三次：一集动画片拖进度条要卡两秒才出画面，一集访谈片的场景切点后面有半秒花屏，还有一部片子的码率在片头十分钟冲到预算的两倍。翻编码参数发现三件事都指向同一张 GOP 表：IDR 放得太稀、场景切没有强制插入、开放 GOP 的 B 帧跨过 IDR 去参考。落点是模块 gsb/batch50-go-game-c-05 的 gop_structure_planner.go，类型 GopStructurePlanner，公开接口保持不变：NewGopStructurePlanner(limits Limits) *GopStructurePlanner、SetProfile(profile GopProfile) error、Profile() GopProfile、Constraints() Limits、Plan(frames []FrameHint) ([]FramePlan, error)、InsertCut(atFrame int) error、Validate() []GopIssue、Stats() GopStats、PlanFrame(index int) (FramePlan, bool)、Reset() error、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收清的地方共 11 条：其一，新建的规划器用关闭 GOP，最大长度取编码器上限，最小长度 1，B 帧层级 0，Profile 与 Constraints 都要能回读；其二，模式只接受关闭与开放两种，其他字符串报模式错误；其三，GOP 长度必须落在 1 到编码器上限之间且不小于最小长度，B 帧层级限定 0 到 3 且不超过编码器上限，越界报错且不改动现值；其四，帧列表为空报空列表错误，长度超过 MaxFrames 报帧数超限错误；其五，首帧必须是 IDR，此后每隔最大 GOP 长度放一个 IDR，并从上一个 IDR 重新起算；其六，FrameHint 里的场景切标记或 InsertCut 请求的帧强制成为 IDR，并从该帧重新起算 GOP；其七，GOP 内的锚点从 IDR 起每隔层级加一帧放一个 P 帧，锚点之间是 B 帧，最后一个锚点之后的余帧退化为 P 帧，层级为 0 时除 IDR 外全是 P 帧；其八，解码顺序里每个锚点先出、随后出它前面的 B 帧，计数跨 GOP 连续，同一次规划的解码序号必须是 0 到帧数减一的排列；其九，Ref 在 I 帧上为 -1、锚点参考上一帧、B 帧参考其后的锚点，POC 在每个 IDR 归零并在 GOP 内按显示序递增；其十，InsertCut 负索引报越界，同一位置重复请求幂等，规划完成后再请求报已有规划，规划时切点落在片段之外报越界且不产生新规划；其十一，统计给出 GOP 数、IDR 数、B 帧数与平均 GOP 长度，Validate 在 IDR 数超过每分钟上限乘片段时长再除六十向上取整（至少为 1）的预算时给出 idr_budget_exceeded。

另有几条必须一起收掉：场景切正好落在计划 IDR 的位置时只保留一个 IDR，不能连着放两个浪费码率；开放 GOP 里场景切之后的 B 帧不得引用切点之前的帧，否则解码花屏；最大 GOP 长度与场景切冲突时场景切优先，但统计里要把被缩短的 GOP 标出来。

硬约束：Plan 与 InsertCut 跟 PlanFrame、Stats、Validate、Profile 的并发调用不得看到半更新的规划，规划、统计与切点表在同一次提交里推进，读侧只会看到上一次提交完成的整组值；快照与恢复要么全量替换要么完全不动，损坏载荷报快照无效并保留现网规划。

验收：go test ./... 15 条全过，测试不得修改、跳过或按固定输入写死；并发与重启恢复自行验证；只做本地实现，不接外部依赖。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-c-05`
- 实现文件：`gop_structure_planner.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`GopStructurePlanner`、`GopProfile`、`FrameHint`、`FramePlan`、`GopStats`、`GopIssue`
