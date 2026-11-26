# 粒子碰撞解算与每帧查询预算

## 题面

粒子特效换到新场景后，雨滴落到地面会先穿进去再弹出来，碎石卡在两块墙板夹角里来回抖，密集粒子一多整帧就掉到三十帧以下；运维把查询预算调大以后帧率更糟，调小又有一半粒子原地不动，第二天再看还是那些粒子没动，日志里同一批粒子两次运行的顺序也对不上。落点在 gsb/batch50-go-game-b-20 的 particle_collision_resolver.go，类型 ParticleCollisionResolver，要把粒子与场景之间的碰撞响应、穿透修正与每帧查询预算补齐。

公开接口保持：NewParticleCollisionResolver(queryBudget int) (*ParticleCollisionResolver, error)、SetProxy(proxy CollisionProxy) error、Proxies() int、Resolve(batch []Particle, dt time.Duration) ([]Particle, error)、SetRestitution(v float64) error、Restitution() float64、SetFriction(v float64) error、Friction() float64、Budget() (queries int, limit int)、SetQueryBudget(n int) error、Stats() CollisionStats、Snapshot() ([]byte, error)、Restore([]byte) error。

要修的点：

1. 查询预算非正时，构造与 SetQueryBudget 都报 ErrBadBudget，预算按帧重置，被拒的值不改原上限；
2. 代理标识为空、类型未知、平面法线为零向量、球半径非正报 ErrBadProxy，被拒的代理不进表；同一标识重复设置按替换处理，不新增条目；
3. 恢复系数与摩擦系数都必须落在零到一之间，越界分别报 ErrBadRestitution、ErrBadFriction，默认值分别为零点五与零点二五；
4. Resolve 负时长报 ErrBadDelta 且不计帧，空批次返回空结果并正常计帧；
5. 自由粒子按速度乘时长积分，时长折算成秒，不受帧率影响；
6. 平面代理以位置与法线的点积同偏移比较判定内外，内侧粒子沿法线分离到表面，穿透深度超过阈值的直接回到上一位置；
7. 命中后法线速度按恢复系数反弹，切向速度按一减摩擦衰减，单个粒子一帧最多反弹一次，二次命中只贴面滑动；
8. 球代理按到球心的距离判定，内侧粒子沿径向分离到半径处，径向速度按恢复系数反弹；
9. 每个粒子对每个代理计一次查询，预算按批次顺序逐粒子扣减，超预算的粒子保持原位并顺延；
10. Budget 与 Stats 里的已用查询、上限、命中、顺延、帧计数要互相对得上，被拒绝的调用不计入；
11. 代理表、两个系数与预算计数经 Snapshot/Restore 原样延续，恢复后新一帧从零开始扣预算，损坏载荷报 ErrBadSnapshot。

三类硬约束：预算耗尽被顺延的粒子要按粒子 ID 稳定排序，不能按到达顺序被其它粒子挤占下一帧的查询机会；反弹后落入两个代理面夹角里的粒子要以更近的面为准分离，不能两面交替推导致抖动；速度为零的粒子命中代理不应获得反弹能量，摩擦也不能把静止粒子推离表面。

验收：go test ./... 的 15 条必须全过，不得改测试、跳过或按固定输入写死；并发与重启场景自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-20`
- 实现文件：`particle_collision_resolver.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`ParticleCollisionResolver`

## 关键类型

- `Vec3`、`ProxyKind`、`CollisionProxy`、`Particle`、`CollisionStats`
- 代理类型：`ProxyPlane`、`ProxySphere`
- 错误集：`ErrBadProxy`、`ErrBadRestitution`、`ErrBadFriction`、`ErrBadBudget`、`ErrBadDelta`、`ErrBadSnapshot`
