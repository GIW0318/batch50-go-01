# batch50-go-safety-c-41

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-41`
- 根包名：`task`
- 实现文件：`rbac_escalation_planner.go`
- 测试目录：`tests/test_rbac_escalation_planner_test.go`
- 类型：`RbacEscalationPlanner`

## 故障单

攻防演练结束后，红队交上来的报告只有一句「已从流水线机器人拿到集群管理员权限」，具体走了哪条绑定、几跳、中间靠什么提权，值班同学谁也复盘不出来，只能把几百条角色与绑定人工对一遍，第二天还是同一句话。两条业务线、三个集群加起来一千多条对象，人工核对已经排到下周，审计要求每条结论都能指回具体绑定。请把这套复盘口径固化成工具，模块路径 gsb/batch50-go-safety-c-41，实现文件 rbac_escalation_planner.go，类型 RbacEscalationPlanner，签名保持：NewRbacEscalationPlanner(limits Limits) *RbacEscalationPlanner、Load(objects []Object) error、Graph() Graph、Holds(subject Subject, target Target) bool、Paths(subject Subject, target Target) ([]Path, error)、Explain(pathID string) ([]Step, error)、Diff(prev, next Graph) ([]Delta, error)、Privileges(subject Subject) []string、Aggregated(role string) []string、Snapshot() ([]byte, error)、Restore(data []byte) error、Reset()。

十二处要落地：一，Load 按类型、命名空间、名称建索引，重复对象报 ErrDuplicateObject，不认识的类型报 ErrBadObject，名称或资源为空的查询报 ErrBadSubject 与 ErrBadTarget；任何一条校验失败整批作废。二，直连路径只允许一条绑定，跳数为一，步骤里必须写清绑定名、角色名、以「动词 资源」形式给出的授予。三，通配资源与子资源分开判定：资源写 pods 不覆盖 pods/exec，写 pods/* 只覆盖子资源不覆盖集合，写 * 两者都覆盖。四，规则的 resourceNames 非空时只对同名单个资源生效，集合级请求不放行。五，聚合角色在加载时按标签选择展开，子角色去重后按名称升序；选择器成环报 ErrAggregateCycle，不许递归到底。六，提权边只在主体还缺这条权限时才成立：已经能直连拿到目标的主体，不再生成经 bind 或 escalate 的两跳链。七，绑定的 roleRef 找不到对应角色时，该绑定授予空权限，加载本身仍然成功。八，内置超级用户组 system:masters 不产生任何可见绑定，但判定权限时视为拥有全部权限，路径要给出来源标识。九，身份模拟类规则带附加条件时，条件不可满足的模拟不构成路径。十，路径按跳数升序、同跳数按路径标识字典序稳定排序；超出最大跳数的链不再搜索，目标不可达报 ErrNoPath，条数超上限报 ErrPathLimit，都不许截断。十一，Diff 比较前后两份图，给出新增、删除、变更三类差异，按类型、命名空间、名称排序，变更项写明字段级说明，例如规则条数从一变二写成 rules:1->2。十二，Explain 按路径标识返回步骤，序号从一连续；快照与恢复要让路径标识、图版本、对象计数逐项对上。

硬约束：图更新必须原子替换，Load 与 Holds、Paths 并发时读方只能看到完整的旧图或完整的新图，失败的加载不得留下半张图；对象数、路径数、跳数都是硬上限，超限报错而不是降级；还原损坏快照整体拒绝，现有图不受影响。

验收：go test ./... 的 15 条用例全过，测试不得修改、跳过或按固定输入写死；并发加载与重启恢复自行验证；只做本地实现，交付源码与跑测记录。

## 运行

```bash
go test ./...
```

测试位于 `tests` 目录，包名同样为 `task`，通过 `impl "gsb/batch50-go-safety-c-41"` 引用根包，共 15 条用例。
