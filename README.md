# batch50-go-safety-c-43

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-43`
- 根包名：`task`
- 实现文件：`mitigation_posture_prober.go`
- 测试目录：`tests/test_mitigation_posture_prober_test.go`
- 类型：`MitigationPostureProber`

## 故障单

一次交付评审里，安全团队按老版本检查脚本给出「全部开启地址随机化与栈保护」的结论，抽查两台机器却发现编译参数早被换成裸奔配置，脚本只看了文件名和编译日志里残留的一行标记；更糟的是有的二进制被 strip 过，脚本直接跳过当通过，评审结论当场被打回。盘点的对象是三条产品线每月交付的近百个可执行文件，还要和上季度基线逐个比对，人工已经覆盖不过来。要把编译期缓解措施的盘点做扎实，模块路径 gsb/batch50-go-safety-c-43，实现文件 mitigation_posture_prober.go，类型 MitigationPostureProber，签名固定：NewMitigationPostureProber(cfg Config) *MitigationPostureProber、Inspect(name string, data []byte) (Posture, error)、InspectAll(items []Artifact) ([]Posture, error)、Compare(base, cur Posture) ([]Delta, error)、Baseline(items []Posture) error、BaselineOf(name string) (Posture, bool)、BaselineSize() int、Explain(id string) ([]Evidence, error)、Posture(id string) (Posture, bool)、Artifacts() []string、Snapshot() ([]byte, error)、Restore(data []byte) error。

十处要落地：一，只认 ELF 魔数，别的容器报 ErrUnknownFormat，空载荷报 ErrEmptyInput，超过字节上限报 ErrFileTooLarge；类字段为二按 64 位解析，为一按 32 位，其它值报 ErrMalformed。二，地址无关按可执行文件类型判定，共享对象类型判 enabled，可执行类型判 disabled，其它类型给 unknown。三，栈保护按符号表里是否存在名字完全等于 __stack_chk_fail 的符号判定，存在判 present，符号表在但没这条判 absent，只有编译期标记字符串而没有符号的不能据此判 present。四，栈不可执行按程序头里的栈段标志位判定，带执行位判 disabled，不带判 enabled，缺这段程序头给 unknown。五，只读重定位要有重定位只读段并且动态段声明立即绑定，两者齐备判 full，只有一项判 partial，绝不能报成完全只读，两项都没有判 none。六，控制流检查看属性笔记里 x86 特性位是否置位，置位判 present，未置位判 absent。七，结论不确定时给 unknown 而不是猜；同一份输入多次检测，结构体逐字段一致，摘要取输入字节的 SHA-256 小写十六进制，同名构件重复交付只保留最后一次结论。八，Compare 按字段名升序给出差异并标注回归或改善，顺序稳定；Explain 每个字段一条证据，写清来源是文件头、程序头、动态段、符号表还是属性笔记。九，基线与检测结果都按名称索引，重复检测同名覆盖，基线只能整批替换，条目缺标识报 ErrBadBaseline，替换失败保留原基线。十，符号被剥离时改看节属性与属性笔记，再判不出来才给 unknown；带后缀的带检查函数符号（例如 __stack_chk_fail_local）也算数，不能只看编译期标记的字符串。

硬约束：批量检测的并发度受 MaxWorkers 封顶且结果顺序与输入一致；单文件字节数与构件数都是硬上限，超限报错不截断；快照恢复后基线条目、检测结论与证据查询逐项可用，损坏快照整体拒绝且不动现有状态。

验收：go test ./... 的 15 条用例全过，测试不得修改、跳过或按固定输入写死；并发批量检测与重启恢复自行验证；只做本地实现，交付源码与跑测记录。

## 运行

```bash
go test ./...
```

测试位于 `tests` 目录，包名同样为 `task`，通过 `impl "gsb/batch50-go-safety-c-43"` 引用根包，共 15 条用例。
