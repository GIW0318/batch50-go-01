# 未成年人时长配额与宵禁准入

## 题面

防沉迷那边的复核报告下来了：上周有一批未成年账号在凌晨一点还在开新局，值班同学查到这些会话是晚上九点多开的，跨过零点之后时长既没有算进前一天也没有算进新的一天，整段用量像是凭空消失；另一个账号家长在晚上十点点了豁免，第二天白天额度明明已经用完却还能继续开局，家长反过来投诉我们豁免等于加时长；还有玩家把手机时间往回调了两个小时，客户端报上来的心跳让当天已用时长变成了负数。落点是模块 gsb/batch50-go-game-a-04 的 playtime_quota_guard.go，类型 PlaytimeQuotaGuard，公开接口保持不变：NewPlaytimeQuotaGuard(policy Policy) *PlaytimeQuotaGuard、BeginSession(accountID string, at time.Time) (Admission, error)、Heartbeat(accountID string, at time.Time) (SessionState, error)、EndSession(accountID, reason string) error、GrantExemption(accountID string, until time.Time, by string) error、RevokeExemption(accountID string) error、ExemptionOf(accountID string) (Exemption, bool)、Usage(accountID string, day time.Time) (UsageState, error)、Segments(accountID string, day time.Time) []Segment、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的口径如下。第一，账号号为空时，开局、心跳、结算、查用量、发豁免一律报 ErrBadAccount。第二，同一账号已经有开放会话时再开局报 ErrAlreadyActive，不同账号互不影响；被拒绝的开局不能消耗额度，Usage 的已用仍为 0。第三，当天已用时长达到 DailyMinutes 时，BeginSession 不报错但返回 Allowed 为假、Reason 为 quota_exhausted、RemainingMinutes 为 0，且不产生开放会话。第四，宵禁窗口按 UTCOffsetMinutes 折算成当地小时后判断，跨零点的窗口要能正确回绕；窗口内开局返回 Allowed 为假、Reason 为 curfew，CurfewUntil 给出当地下一次开禁时刻。第五，心跳返回的 UsedMinutes 是当天累计（含当前会话），Seq 严格递增，超过 DailyMinutes 的部分按上限截断，不结转到第二天。第六，没有开放会话时心跳与结算都报 ErrNoSession。第七，结算按最后一次心跳截断记账，不按调用时刻补记；结算之后同一账号可以立刻开新会话。第八，两次心跳之间超过 HeartbeatGapMinutes，会话按最后心跳截断关闭并返回 ErrHeartbeatGap；宵禁开始后仍有 GraceMinutes 宽限，宽限内心跳正常续期，超过宽限的心跳按最后心跳截断关闭并报 ErrCurfewGraceExpired，两种关闭之后心跳都报 ErrNoSession。第九，豁免必须带操作人和到期时刻，缺一报 ErrBadExemption，撤销不存在的豁免报 ErrNoExemption，ExemptionOf 要能读回到期时刻与操作人；Usage 的 Day 用当地日期 YYYY-MM-DD，LimitMinutes 取 DailyMinutes，RemainingMinutes 不足时归零，没记过账的账号返回全量额度。

三处边界必须一起处理。跨零点仍在线的会话必须按当地日界切成两段，零点前记前一天、零点后记当天，不能整段算到开始日。家长豁免只放开宵禁准入，不增加每日时长额度，豁免到期之后仍然按当天已用时长判定。客户端时钟回拨或切换时区时，时长的增减一律以服务端时钟为准，回拨不得产生负时长，也不得把已经消耗的额度退回去。

硬约束两条。一是并发与顺序：同一账号的开局、心跳、结算必须串行结算，额度扣减不能出现两个请求同时判定成功的窗口，Seq 在任一时刻只能单调递增。二是重启：快照要带上开放会话、每日分桶、豁免记录与 Seq，恢复之后 Usage、ExemptionOf 与下一次心跳的结果必须与重启前一致；损坏载荷在 Restore 时报 ErrBadSnapshot，且不能动到现网会话。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-04`
- 实现文件：`playtime_quota_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`PlaytimeQuotaGuard`
