# 瑞士轮配对、轮空轮转与撤轮冲正

## 题面

赛事运营这周接连收到三起投诉：线上瑞士轮第二轮出现了前一晚刚打完的两名选手再次对桌，第三轮有人被同时排进两张桌子，第五轮撤轮之后积分没有回到抽签前。后台线索只有一条：配对服务重启过一次，重启前后同一名选手的轮空次数和积分都被重置。赛事还在进行，裁判长要求本周五之前收干净。落点是模块 gsb/batch50-go-game-a-01 的 swiss_pairing_director.go，类型 SwissPairingDirector，公开接口保持不变：NewSwissPairingDirector(cfg Config) *SwissPairingDirector、Register(players []Player) error、PairRound(round int) ([]Pairing, error)、ReportResult(round, table int, winnerID string, draw bool) error、Standings() []Standing、Unpair(round int) error、Round(round int) ([]Pairing, error)、ByeHistory(playerID string) []int、Floats(playerID string) int、Withdraw(playerID string) error、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的口径如下。第一，报名名单为空、选手号空白或重复，分别报 ErrNoPlayers、ErrBlankPlayer、ErrDuplicatePlayer，坏名单不能把已载入的名单顶掉。第二，首轮配对之后名单锁定，再报名报 ErrRosterLocked。第三，同一分组内按积分降序、同分按报名先后升序排好，上下半区对折，第一位对第 n/2+1 位，台号从 1 连续编，白方取排序在前的人。第四，人数为奇数时轮空给积分最低且还没轮空过的人，轮空条目排在整轮列表末尾，台号写 0、两个选手字段留空，轮空当场按 1 分入账，ByeHistory 按轮次升序列出该选手轮空过的轮次。第五，上一轮还有桌子没上报结果时不许开下一轮，报 ErrResultsPending；已经配过对的轮次再调 PairRound 报 ErrRoundPaired，轮次超出 1 到 MaxRounds 报 ErrBadRound。第六，桌面不存在报 ErrNoTable，胜者不是本桌两人之一、或和局却带了胜者，都报 ErrBadWinner；同一桌重复上报同一结果幂等成功且积分只记一次，上报不同结果报 ErrResultConflict 且原结果继续有效。第七，Standings 按积分降序、对手积分和降序、报名序号升序排出名次，名次从 1 连续编号，对手列表按相遇顺序记录。第八，退赛选手立刻从后续配对摘掉，重复退赛报 ErrWithdrawn，未报名的报 ErrNotRegistered。第九，需要的桌子数超过 Config.Boards 时整轮拒绝并报 ErrBoardLimit，不能只排出前面几张桌。

还有三处边界必须一起收掉，漏一处就会当场翻脸。奇数人数时浮动和轮空可能落到同一名选手身上，向下浮动与向上浮动互相抵消的场合不能把一个人在同一轮里移动两次，浮动计数也只能记一次。已经上报了部分桌结果的轮次被撤销时，必须把那几桌的积分、对手记录和轮空计数一并冲正回抽签前的状态，而不是把整轮结果直接清空。为了避开重赛，跨分组浮动找不到合法对手时按分差最小的一方让步，但让步的底线是不能让同一名选手在一轮里出现两次。

硬约束两条。一是并发：同一场的多路上报与配对请求必须串行结算，一轮里每名选手最多出现一次、每张桌最多一条结果，不能有两个请求都判定成功的窗口。二是重启：快照要带上名单、各轮配对、每桌结果、轮空账本与序号，恢复之后 Standings、ByeHistory 与下一轮的配对输入必须与重启前完全一致；损坏的载荷在 Restore 时报 ErrBadSnapshot，且不能动到现网数据。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-01`
- 实现文件：`swiss_pairing_director.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SwissPairingDirector`
