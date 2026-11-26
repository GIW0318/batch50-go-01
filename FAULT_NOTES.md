# 故障说明

## 误判根因

- 回显判定直接在原始正文中查找 `Marker`。当连续空白被压缩或 `-`、空格等被编码成 HTML/数字实体时，肉眼能确认标记，字符串精确匹配却会漏判。
- 时延判定没有固定“先基线、后载荷”的比较顺序，也没有识别 `Response.RateLimited` 或 HTTP 429。全局限流/网络抖动会抬高观察耗时，造成假阳性。
- 基线只有一次采样时，单次高位抖动会压低 `observed-baseline` 差值，可能把真实时延注入误判为未命中。
- 用例顺序、在途尝试、已提交响应、预算扣减和判定结论只存在于调用方日志中；并发推进或进程重启后无法恢复同一状态。

## 实现要点

- 所有公开方法通过同一把 `sync.Mutex` 串行化状态推进和预算扣减，返回的 `Case`、`Attempt`、`Outcome` 均为值拷贝。
- `Plan` 先完成全部校验和对象构造，最后才写入状态，拒绝时不会留下半成品用例；尝试总数按 `len(Payloads) * 3` 校验。
- `Run` 严格按 `baseline`、`echo`、`time` 派发，`Ordinal` 从 1 开始；同一尝试未提交返回 `ErrAttemptPending`，同一点有在途尝试的其他用例返回 `ErrCasePending`。
- `Submit` 校验尝试 ID、`Response.AttemptID` 和重复提交；提交成功时原子增加已提交响应数和累计耗时，并释放点级在途槽。
- 回显判定先执行 `html.UnescapeString`（含数字实体和必要的二次解码），再用 `strings.Fields` 压缩所有连续空白。
- 时延阈值为 `max(100, DelayMillis/2)`，`delta >= threshold` 才命中；结论、原因码和置信度按题面组合并缓存。
- `Response.RateLimited`、HTTP 429 或常见限流正文会使该点的时延归因暂停：限流造成的高耗时不会判为 `time`/`both` 命中。下一条非限流基线可作为复核并恢复时延判定。
- 基线通道保持在时延通道之前。若首个基线样本抖动较大，可在同一个 baseline 响应中附带追加的 `BaselineSamples`；存在多样本时使用抗离群的中位基线复核。未提供追加样本时仍严格使用 `ElapsedMillis`，兼容原有三通道契约。
- `Budget.AttemptsLeft = MaxAttemptsPerPoint - Submitted`，`Budget.MillisLeft = TotalDelayBudgetMillis - SpentMillis`；预算耗尽后下一次 `Run` 返回 `ErrDelayBudget`。
- `Snapshot` 使用带版本号的 JSON；`Restore` 先反序列化和引用完整性校验，再整体替换状态，损坏快照返回包装后的 `ErrBadSnapshot` 且不影响在线状态。
- `Judge` 成功后缓存 `Outcome`、把用例置为 `judged`，并丢弃原始 `Response`；结论和累计预算保留。未判定用例的响应和在途尝试继续保留，重启后可继续提交或派发下一通道。
- `MaxConcurrentPoints` 限制仍有 `planned`/`running` 用例的注入点；点下全部用例进入 `judged`/`aborted` 后释放并发槽，但同 Key 仍因 `ErrPointExists` 不能重复计划。

## 验证结果

- `go test -v ./...`：`tests` 包内 15 条用例全部 PASS，输出见 `test-output.txt`。
- `go test -race ./...`：通过，输出见 `race-output.txt`。
- 另用临时测试验证过 40 条用例、8 个并发 worker 的同点串行提交，以及在途尝试快照恢复；临时测试文件已删除，未修改验收测试。

当前沙箱的默认 Go 缓存目录不可写时，可用：

```bash
GOCACHE=/tmp/gsb-c22-gocache GOPATH=/tmp/gsb-c22-gopath go test ./...
```
