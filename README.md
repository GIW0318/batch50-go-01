# 云 IAM 策略模拟与裁决

## 题面

权限治理平台上线后第一次全量复核就翻车：同一条只读请求，模拟器在测试账号上判放行，生产账号上判拒绝，运维照结论收窄了一条策略，结果把线上日志采集打挂；另一批批量复核里，一条策略的条件写成了不认识的运算符，整批直接抛错，几千条请求一条结论都没有；跨账号的资源策略更离谱，模拟器只看身份策略就判了拒绝，和线上实际结果对不上。请把最终裁决做成可复用的组件，落点 gsb/batch50-go-safety-c-37，实现文件 iam_policy_simulator.go，类型 IamPolicySimulator，签名固定：NewIamPolicySimulator(limits Limits) *IamPolicySimulator、Grant(policy Policy) error、Evaluate(req Request) (Decision, error)、Explain(decisionID string) (Trace, error)、Simulate(batch []Request) ([]Decision, error)、Snapshot() ([]byte, error)、Restore(data []byte) error，另配 Revoke、Policies、DecisionFor、Stats、Reset 五个辅助方法。

要修的点：一，策略带 id、layer、effect、actions、notActions、resources、principal、conditions，layer 只认 identity、resource、boundary、scp，effect 只认 Allow 与 Deny，编号为空、层不认识、效果不认识、动作与否定动作都没写一律报 ErrBadPolicy，编号重复报 ErrDuplicatePolicy；二，动作只认 s3、iam、ec2、kms、sts、logs 六个服务前缀，策略里写了别的服务在登记时报 ErrBadPolicy，请求里出现别的服务按不支持的动作失败关闭，结论 implicit-deny 且原因码 unsupported-action；三，动作通配符按整串匹配，星号与问号可以出现在中间；资源通配符只在分隔符段内匹配，星号不能跨过冒号与斜杠，单独一个星号表示任意资源；四，显式拒绝压过一切允许，结论 deny、原因码 explicit-deny 并写明策略编号，只有身份策略允许且四层里都没有显式拒绝时才是 allow；五，权限边界只封顶允许，不产生显式拒绝记录；六，否定动作与拒绝组合时匹配的是动作补集，不能按普通动作匹配；七，资源策略对同账户主体可以直接生效，跨账户主体必须同时拿到身份策略允许；八，条件运算符覆盖 StringEquals、StringLike、ArnLike、Bool、DateLessThan、Null 与 ForAllValues:StringEquals，登记时运算符不认识、日期值不是时间、布尔值不是 true 或 false 报 ErrBadCondition；九，条件键只认 aws:PrincipalAccount、aws:RequestedRegion、aws:CurrentTime 与 aws:PrincipalTag/ 前缀，键不存在或不认识时该条不匹配，按失败关闭处理；十，全值量词把请求值按逗号拆开，每个值都要等于期望值，求值按策略编号升序推进；十一，批量求值逐条独立，结果顺序与入参一致，规模超过 MaxBatch 报 ErrTooManyRequests，空动作或空资源报 ErrBadRequest；十二，Explain 按编号返回结论、原因、匹配到的策略编号与至少两步过程，编号不存在报 ErrNoDecision，Stats 给出已求值、已允许、已拒绝与隐式拒绝四个计数，快照要么完整包含策略表与结论日志，要么整体拒绝损坏载荷，恢复后编号接着往下发不重复。

硬约束：同一实例的登记、吊销、求值与快照串行推进，并发求值的编号不得重复也不得跳号；快照恢复失败不得破坏现有策略与结论日志。验收：go test ./... 的 15 条用例全过，测试不得修改、跳过或按固定输入写死；并发求值与重启恢复自行验证，只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-37`
- 实现文件：`iam_policy_simulator.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`IamPolicySimulator`
