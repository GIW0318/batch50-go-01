# 参数均衡器设计：频带级联、采样率切换与总增益补偿

## 题面

现场调音台升级后连着出问题：主播把 12 kHz 高架推起来时整条通路人声发毛，换到 44.1 kHz 工程后同一条曲线听着完全变了味；副路串了六段均衡，改其中一段的低频时耳机里冒出一声爆音；还有一台机器上两个频段叠在 1 kHz 附近，总增益补偿把整体电平压掉十几 dB，怎么推都回不来。这套均衡器原来由每个频段各自打补丁，现在要收口到模块 gsb/batch50-go-game-c-01 的 parametric_eq_designer.go，类型 ParametricEqDesigner，公开接口保持：NewParametricEqDesigner(limits Limits) *ParametricEqDesigner、SetSampleRate(hz int) error、AddBand(spec BandSpec) (BandID, error)、UpdateBand(id BandID, spec BandSpec) error、RemoveBand(id BandID) error、Coefficients(id BandID) (Biquad, error)、Response(freq float64) (float64, error)、Bands() []BandID、SetOutputGain(db float64) error、SetBypass(on bool) error、SetCompensation(on bool) error、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收清的地方共 11 条：其一，新建的设计器采样率固定 48000，频段表为空，修订号从 0 起算；其二，频段频率必须不小于 20 Hz 且低于当前采样率的 0.45 倍，否则报频段频率越界，采样率切换后同一条件按新采样率重新判定；其三，Q 只在峰值、低通、高通上校验并落在 Limits 给出的区间内，两种架式忽略 Q；其四，增益绝对值不得超过 Limits 的 MaxGainDB，低通与高通不参与增益；其五，频段数量到顶后再加报频段上限错误，并且不消耗频段编号；其六，编号只增不复用，删除后新增的编号必须大于此前最大值；其七，级联顺序按频率升序，频率相同按编号升序，Bands 返回的必须是处理顺序；其八，峰值段按 RBJ peaking 计算并以 a0 归一化，架式用斜率 1 的 RBJ shelf，低通高通按 Q 的 RBJ 二阶，系数取不到就报频段不存在；其九，采样率切换要重算全部系数并让修订号加一，BandState 里的修订号记录该段最后一次成功变更后的修订号；其十，探针频率必须落在 0 与当前采样率一半之间（不含端点），否则报探针频率越界；其十一，总旁路打开后响应只剩输出增益，输出增益限制在正负 24 dB，越界报错且不改动现值。

另有几条必须一起收掉：采样率降到更低值时，贴近奈奎斯特的频段要自动降级或旁路，绝不能算出一组不稳定系数还继续挂在级联里；更新某一段时，实时处理线程读到的必须是一整组新旧一致的值，不能旧系数配新系数，否则就是那声爆音；总增益补偿要把所有频段在 1 kHz 参考点的叠加响应算进去再取反，不能把各段增益直接相加。

硬约束：频段表、系数组、增益与旁路标志的修改对读侧是一组原子快照，并发读写不得出现半更新状态；快照与恢复要么全量替换要么完全不动，损坏载荷报快照无效且保留现网状态，恢复后修订号与编号水位继续往前走。

验收：go test ./... 15 条全过，测试不得修改、跳过或按固定输入写死；并发与重启恢复自行验证；只做本地实现，不接外部依赖。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-c-01`
- 实现文件：`parametric_eq_designer.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`ParametricEqDesigner`、`BandSpec`、`Biquad`、`BandState`、`Limits`
