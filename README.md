# 随机源健康治理

## 题面

一批新机器上线后，安全基线检查连着报了三条：有的机器在启动早期把全零字节当成随机数交给了 TLS 握手，会话密钥因此可预测；有的虚拟机从同一份快照批量克隆，克隆机的随机源信用刷得比物理机还高，实际熵一点没多；还有一台机器重启后拿恢复出来的旧种子继续出数，直到有人手工重新播种才停。值班想按来源看信用曲线、想看哪几次取用被拒，系统只能给出一个总字节数，其余全查不到。

要把这块能力补到 gsb/batch50-go-safety-d-08 的 entropy_health_guard.go，核心类型 EntropyHealthGuard，入口 NewEntropyHealthGuard(Options) *EntropyHealthGuard。公开接口：Feed(src string, sample []byte) (Credit, error)、Draw(n int, blocking bool) ([]byte, error)、Health() Health、Reseed(seed []byte) error、CreditOf(src string) (Credit, bool)、Weight(src string) int、PoolBytes() int、Sources() []string、Records() []DrawRecord、Snapshot() ([]byte, error)、Restore([]byte) error。

1. 来源名为空白返回 ErrBadSource，样本为空返回 ErrBadSample，两种拒绝都不入账、不涨池。
2. 首次被接受的样本给来源记一次基础信用，之后每个新样本继续加分，上到 CreditMax 封顶；Credit 里带来源、分数、是否接受与原因，接受记 reason_accepted。
3. 同一来源重复提交与上一份完全相同的样本不加分，记 reason_duplicate 并按 CreditPenalty 扣分；扣分有下限，不能把来源扣成永久禁用。
4. 同一来源并发喂入的先后不影响它的信用上限，最终分数只取决于有多少个不同样本被接受。
5. 只有被接受的样本才进池，池按字节先进先出；Draw 取最旧的 n 个字节，成功一次记一条 DrawRecord。
6. Draw 的 n 小于等于 0 返回 ErrBadDraw，不消耗池；取用量按传入值严格判断，不做截断或补齐。
7. 池里不够 n 时，非阻塞取用必须返回明确错误，不能回零字节、短读或降级成弱随机，早期启动最容易踩的就是这一格。
8. 阻塞取用要等到 BlockTimeout 再返回 ErrPoolEmpty，不允许无限等待，等待期间也不许偷偷降级。
9. 池见底时 Health 状态为 starved；有水位但没有一个来源权重达到 4 时是 degraded；否则 ok。权重按分数分段：90 以上 8，70 以上 6，50 以上 4，20 以上 2，其余 1，未知来源 0。
10. Reseed 的种子短于 MinSeedBytes 返回 ErrShortSeed 且不动现有池；通过后池被整份替换成种子内容，状态回到 degraded。
11. 恢复快照后旧种子作废，在重新播种前只能阻塞取用；快照要含池、信用账、每个来源的最后样本与取用记录。
12. 损坏载荷返回 ErrBadSnapshot 且不动现网；正常恢复后信用账、来源清单与取用记录都要在，同一来源的重复判定也接着有效。

硬约束两条：一是并发与原子性，池水位与信用账必须作为一份原子快照读写，并发取用不得超发，一个字节只能被取走一次；二是恢复与上限，快照自包含、可重复恢复，恢复后旧种子不得继续出数，种子长度与取用量都按传入值严格校验，触限返回错误而不是静默截断。

验收口径：go test ./... 的 15 条用例全过，不得修改测试、跳过用例或按固定输入写死结果；并发与重启行为自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-d-08`
- 实现文件：`entropy_health_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`EntropyHealthGuard`
