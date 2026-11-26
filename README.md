# 选人锁定后的英雄交换：双向合并与封盘

## 题面

选人阶段最后半分钟，英雄交换连续出事。一名玩家同时向两个人发起换人，接受其中一个之后队伍里出现两个同款英雄，进游戏直接被判阵容非法；另一局里 A 找 B 换、B 又找 A 换，两边都点了接受，绕一圈谁也没换成，可请求记录全变成已完成，赛后复盘看不出谁拿走了谁的角色；还有玩家卡在开局倒计时结束那一瞬间点接受，封盘之后的阵容又被改了一次，比赛服与观战端阵容对不上。问题落在模块 gsb/batch50-go-game-a-15 的 hero_swap_coordinator.go，类型 HeroSwapCoordinator，公开接口按现状保留：NewHeroSwapCoordinator(limits Limits) *HeroSwapCoordinator、LockRoster(matchID string, picks map[string]string) error、Request(matchID, fromPlayer, toPlayer string) (SwapRequest, error)、Accept(matchID, requestID string) (SwapResult, error)、Decline(matchID, requestID string) error、Cancel(matchID, requestID string) error、Expire(now time.Time) (int, error)、CloseRoster(matchID string) error、Roster(matchID string) (map[string]string, error)、Pending(matchID string) []SwapRequest、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的口径如下。第一，上锁时对局号只有空白报 ErrBlankMatch，阵容为空、玩家名为空白或英雄为空白报 ErrEmptyRoster，两名玩家拿到同一个英雄报 ErrDuplicateHero，被拒的阵容一律不入表。第二，每名玩家同一时刻只能有一个待处理请求，自己发起过或者正被别人请求，都不能再发起新请求，统一报 ErrPendingExists。第三，不能和自己换，报 ErrSelfSwap；参与者不在阵容里报 ErrUnknownPlayer；对局没上过锁报 ErrUnknownMatch。第四，单局待处理请求数超过 MaxPendingPerMatch、或本局累计发起的请求数超过 MaxRequestsPerMatch，报 ErrRequestLimit。第五，请求带创建时刻与到期时刻，时长取 RequestTTL；Expire 把到期且仍待处理的请求置为过期并返回本次处理条数，同一批只计一次，过期后名额立即释放。第六，接受请求时双向原子换位，两名玩家的英雄互换且全队不得出现重复持有，结果里要带双方交换前各持的英雄与本局提交序号，序号从 1 开始递增。第七，A 向 B 与 B 向 A 同时发起请求时合并成一次交换，不能互相覆盖，更不能换两次。第八，交换完成后原请求方再次接受同一请求返回已完成状态，不得再次互换。第九，撤销与拒绝只对待处理请求生效，未知请求报 ErrUnknownRequest，已结束的请求报 ErrRequestNotPending，两者都不动阵容并立刻释放双方名额。第十，开局倒计时最后阶段封盘，封盘后新请求一律报 ErrRosterClosed；封盘瞬间已接受未提交的交换以封盘锁内状态为准，未提交的回滚且不消耗次数。第十一，阵容查询返回当前每名玩家的英雄，未上锁或已解散的对局报 ErrUnknownMatch，待处理列表按发起先后返回。第十二，快照要能描述阵容、请求与提交序号，恢复后待处理请求还能被接受且序号继续递增；损坏的快照报 ErrBadSnapshot 且不动现网。

硬约束有三条。双方同时接受同一请求只能提交一次，阵容替换与序号推进要么全见旧值要么全见新值，不得出现只改了一半阵容的中间态。待处理请求数与本局请求配额是硬顶，超限报错而不是悄悄丢弃最早的那条。进程重启后阵容、请求与序号都要从快照恢复，已提交的交换不得重放，未提交的请求要么仍可接受要么明确作废。

验收口径：go test ./... 的 15 条用例必须全过，测试不得修改、跳过、按固定输入写死；并发接受与重启恢复自行验证；只做本地实现，交付改动后的源码与跑测记录。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-15`
- 实现文件：`hero_swap_coordinator.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`HeroSwapCoordinator`
