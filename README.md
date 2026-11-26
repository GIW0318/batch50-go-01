# 日志注入识别与分级处置

## 题面

月初的审计复核发现，攻击者把换行塞进了用户名字段，落盘后凭空多出一行“登录成功”的伪记录，值班同学按这行假日志排查了两天；同一批日志里还有终端转义序列，运维在终端里 grep 时整个会话被刷屏卡死。更麻烦的是，清洗过的结构化日志偏移错位，下游按旧偏移取值，读到的全是下一条记录的片段。现场代码在 gsb/batch50-go-safety-c-34 的 log_injection_detector.go，类型 LogInjectionDetector，要在日志落盘或推送前识别注入尝试，覆盖换行伪造、终端转义、JSON 结构破坏与超长字段，并按来源和输出目标给出处置。

接口保持这些签名：NewLogInjectionDetector(limits Limits) *LogInjectionDetector、Inspect(entry Entry) (Finding, error)、Sanitize(entry Entry) (Entry, error)、Policy(origin string, p Policy) error、PolicyState(origin string) (Policy, int, bool)、Explain(entryID string) (Trace, error)、Suppress(origin string, ttl time.Duration) error、Suppression(origin string) (time.Time, bool)、Findings() []Finding、Snapshot() ([]byte, error)、Restore([]byte) error。

要修的行为与边界：

1. 条目编号或来源为空，Inspect 返回 ErrEmptyEntry，不能拿字段名顶上。
2. 单行文本或 JSON 字段里出现换行要记 newline_in_single_line，形如 \u001b 的转义文本记 ansi_escape，JSON 字段出现","这类结构破坏记 json_break，超过 MaxEntryBytes 记 oversize，其余控制字符记 control_char，信号按名字升序；一个信号算 suspicious，两个及以上算 quarantined，没有信号算 clean 并回填原始字节长度。
3. 来源策略动作为 quarantine 且命中可疑及以上时，Inspect 返回 ErrEntryQuarantined，条目进隔离而不是照常落盘。
4. Policy 的来源为空返回 ErrBlankOrigin，动作不在 allow/redact/quarantine 之内返回 ErrBadPolicy；同一来源每次设置版本加一，PolicyState 要能看到当前动作与版本。
5. Sanitize 只处理检查过的条目，未检查的返回 ErrUnknownEntry；换行、回车与转义序列要转义成文本，对同一原值重复清洗结果一致。
6. 清洗后条目要保留原始字节长度 OriginalLen 与原值摘要 Digest，长度按原值算而不是按清洗后的值算。
7. 抑制窗口内 Inspect 返回 suppressed，信号照记但不占告警额度；窗口到期后自动恢复可疑判定，不需要手动清标记；每来源每分钟告警数超过 MaxAlertsPerOriginPerMinute 返回 ErrAlertRate，干净条目不消耗告警额度。
8. 合法的多行堆栈与伪造换行在文本上完全一样，判定要结合字段类型与来源声明，不能只看有没有换行。
9. 清洗会改变字节长度，结构化日志里的偏移引用要按新长度同步修正，不能留下指向旧偏移的引用。
10. 终端转义序列写入文件时无害、写终端时有害，处置要按输出目标区分，不能一刀切删除。
11. 同一来源的策略变更串行执行，并发检查下告警计数精确等于上限；重启恢复后策略版本、抑制窗口、条目与告警计数都还在。
12. 损坏的快照 Restore 返回 ErrBadSnapshot，且不得清掉现有策略与抑制窗口。

硬约束：条目体积、每来源告警速率、跟踪条目数三项上限在并发下精确生效，不能事后裁剪；策略变更、告警记账与快照互斥，快照只能看到全量旧状态或全量新状态。

验收：go test ./... 15 条全过；不得修改测试、跳过或用固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-34`
- 实现文件：`log_injection_detector.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`LogInjectionDetector`；辅助类型 `Limits`、`Entry`、`Finding`、`Policy`、`Trace`
