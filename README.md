# 执行计划的安全策略评审

## 题面

月度资源评审连续两次把同一批高危变更判成通过，回看执行计划才发现问题出在评审脚本自己身上：带变量的属性在计划里是未知值，脚本把未知当空字符串，新开的存储桶既没被判成公开也没被判成不合规；子模块复制出的三套实例只报了第一套，另外两套的地址压根没出现在报告里；还有一条加密开关写成非布尔值，脚本抛出错误后整批评审中断，后面四十多条资源一条结论都没有。请把评审流程重做成可复用的组件，落点 gsb/batch50-go-safety-c-36，实现文件 iac_plan_evaluator.go，类型 IacPlanEvaluator，签名固定：NewIacPlanEvaluator(limits Limits) *IacPlanEvaluator、Load(plan []byte) (Snapshot, error)、Evaluate(snap Snapshot) ([]Finding, error)、Explain(findingID string) (Trace, error)、Diff(prev, next Snapshot) ([]Delta, error)、Snapshot() ([]byte, error)、Restore(data []byte) error，另配 Rules、SetRules、Findings、Snapshots、Reset 五个辅助方法。

要修的点：一，计划按 JSON 读入，resources 每项带 address、type、action、before、after、unknown、sensitive，modules 每项带 name、count、resources；解析失败、地址或类型为空、动作不在 create/update/delete/replace/read 之内、同一地址出现两次，一律报 ErrBadPlan 且不登记；二，模块资源展开成带计数字段的完整地址，形如 module.app[0].aws_ebs_volume.data，根模块保持原地址，展开后按地址升序存放，带键的集合地址要把键按字典序归一；三，被删除的资源只对变更前属性求值，变更后的属性集不参与判断，展开后的资源数超过 MaxResources 报 ErrTooManyResources；四，规则固定六条，R01 存储桶 acl 非 private 判违规且严重度 critical，R02 策略 principal 含星号判违规，R03 卷加密开关为 false 判违规，R04 安全组 ingress 出现 0.0.0.0/0 或 ::/0 判违规，R05 数据库 publicly_accessible 为 true 判违规，R06 密钥轮换为 false 判违规；五，带未知值的属性不能当空值，该属性的规则结论必须是 uncertain 而不是 violation 或通过，按属性路径精确匹配；六，布尔属性写成非法字面量时只把这条规则记成 error 结论，其余规则与资源照常出结论，不得中断整批；七，结论只列 violation、uncertain、error 三类，按规则号、地址、属性路径升序稳定排序，重复求值结论与编号一致；八，结论条数超过 MaxFindings 报 ErrTooManyFindings；九，Explain 按编号返回规则号、地址、属性路径与至少两步过程，标成 sensitive 的属性在过程里打码但仍参与求值，编号不存在报 ErrNoFinding；十，Diff 按地址比较两份快照，给出 added、removed、changed 三类，changed 要升序列出变化的属性路径；十一，求值只认已登记的 Snapshot，编号不认识报 ErrNoSnapshot；十二，SetRules 只能收窄到内置规则，未知规则号报 ErrUnknownRule 且不改动现有规则集，Rules 按规则号升序返回。

硬约束：同一实例的登记、求值、解释与快照操作串行推进，并发求值不得出现结论条数或编号漂移；快照要么完整包含全部已登记计划、结论与解释，要么整体拒绝损坏载荷，恢复失败不得破坏现有状态。验收：go test ./... 的 15 条用例全过，测试不得修改、跳过或按固定输入写死；并发求值与重启恢复自行验证，只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-36`
- 实现文件：`iac_plan_evaluator.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`IacPlanEvaluator`
