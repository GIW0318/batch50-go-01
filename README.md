# 色调映射曲线烘焙：单调性校验、白点渐近与半浮点 LUT

## 题面

调色组这周退回来三张图：夜景里的路灯一圈发绿，明明是高光却带上了青边；同一套参数在编辑器里预览正常，出到实机整体发灰；还有一次改完曝光重烘一遍表，画面直接变成负片，回滚参数才恢复。查下来是烘焙流程把参数校验放在了出表之后，肩部压缩在白点附近硬截断，半浮点输出也没钳范围，坏表还会直接覆盖掉上一份能用的。落点是模块 gsb/batch50-go-game-b-03 的 tonemap_curve_baker.go，类型 TonemapCurveBaker，公开接口保持：NewTonemapCurveBaker(limits Limits) *TonemapCurveBaker、SetCurve(params CurveParams) error、Current() (CurveParams, bool)、SetWhitePoint(nits float64) error、WhitePoint() float64、SetLUTSize(n int) error、LUTSize() int、Bake(format string) (LUT, error)、Sample(x float64) (float64, error)、Validate() []CurveIssue、LUT() (LUT, bool)、Stats() BakerStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的地方如下。第一，曲线定义域是 0 到 1，Sample 的返回值也必须落在 0 到 1，0 端给 0，1 端给 1。第二，输入是 NaN 或正负无穷报 ErrBadInput，有限但越界的输入钳到端点而不是报错。第三，响应先把输入按对比度绕 0.5 做枢轴旋转并钳回 0 到 1，再乘 2 的曝光次幂得到线性量 v，肩部按 A 等于 Shoulder 的指数饱和 1 减 exp(-v/A)，趾部按 1 加 Toe 乘 ln(1+v) 抬影调，两者相乘得到响应，最后按 1 端的响应归一，保证两端各就各位。第四，SetCurve 只做结构校验：任一参数是 NaN 或无穷、Shoulder 不在 0 到 8 之间、Toe 不在 0 到 8 之间、Exposure 不在 -8 到 8 之间、Contrast 不在 -4 到 4 之间，一律报 ErrBadCurve 并沿用上一套参数。第五，Contrast 取负在结构上是合法的，但 Validate 要报出非单调，Bake 遇到非单调报 ErrNonMonotonic，不允许产出坏表。第六，Validate 对干净参数返回空列表。第七，白点范围是大于 0 不超过 10000，越界或 NaN 报 ErrBadWhitePoint，WhitePoint 读回当前值。第八，SetLUTSize 只接受 1 到 MaxLUTSize，越界报 ErrBadLUTSize，MaxLUTSize 留成 0 或负数按 256 处理。第九，Bake 支持一维、三维与半浮点三种格式：一维按 SetLUTSize 的条数，三维固定 17 的立方，半浮点按一维条数另存一份编码，解码后必须钳在 0 到 1，1.0 对应 0x3C00。第十，未知格式报 ErrBadFormat，失败时不得发布表。第十一，成功后 LUT 能读回最后一张表，Stats 里的烘焙次数与最近尺寸跟着更新。第十二，快照要带曲线、白点、表尺寸与最后一张表，损坏载荷在 Restore 时报 ErrBadSnapshot 且不动现网。

还有三处必须一起收掉：白点提高以后曲线尾部要渐近收敛，不能硬截断，硬截断会在高光里带出色相偏移；参数组合导致非单调时 Validate 要报出具体输入区间，并且保留上一张可用表，不能让坏表覆盖线上；LUT 在 0 与 1 两个端点的采样值必须和解析曲线完全一致，否则边界一圈会持续偏色。

硬约束两类。一是并发：多路同时 Bake、Sample 与 SetCurve 必须串行裁决，参数与表不能出现半更新的中间态，计数累加不能丢。二是幂等与快照：同一套参数重复 Bake 出来的表必须逐元素一致，白点、表尺寸与最后一张表要能跨进程恢复，恢复后紧接着采样与恢复前一致。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、不得跳过、不得按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-03`
- 实现文件：`tonemap_curve_baker.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`TonemapCurveBaker`
