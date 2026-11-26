# EDNS 客户端子网隐私截断与前缀隔离缓存

## 题面

递归解析上线子网裁剪之后，两个不同办公网拿到同一份 CDN 调度结果，视频一直回源；另一个分部的解析结果又互相污染，A 楼改完记录 B 楼还在用旧地址。合规审计还发现，出问题时导出的日志里能还原出完整客户端 IP，这与上线时的承诺不符。值班同学起初怀疑上游调度，换了出口地址后现象跟着变，才把方向转到缓存归属上。落点在模块 gsb/batch50-go-safety-b-03 的 ecs_privacy_guard.go，类型 EcsPrivacyGuard，签名保持：NewEcsPrivacyGuard(cfg CacheConfig) *EcsPrivacyGuard、Decide(clientAddr netip.Addr, q Question, policy TenantPolicy) (Scope, error)、Rewrite(q Question, scope Scope) ([]byte, error)、CacheKey(q Question, scope Scope) (string, error)、ObserveAnswer(key string, ttl uint32, prefixLen int) error、TTLFor(key string) (uint32, bool)、BeginLookup(key string) (LookupToken, bool)、EndLookup(token LookupToken, ttl uint32) error、UpstreamRejected(key string) (Scope, error)、Stats() CacheStats、SetCapacity(n int) error 与 Reset()。

需要落地的判定：

1. IPv4 与 IPv6 分别按租户配置的前缀截断，截断后的地址低位清零，回填到 Scope 里的是截断后的地址；原始地址只参与判定，不进入缓存键。
2. 私网、回环、链路本地与未指定地址不生成选项，Family 记为 none，前缀长度为 0。
3. 租户关闭选项时不生成子网，照常按全局解析。
4. 生效前缀不超过租户允许的最大前缀，配置比上限更细时按上限截断，比上限更粗时保留原配置。
5. 地址无效报 ErrInvalidAddr，带 zone 的地址报 ErrZoneAddr，不能静默降级成全局解析。
6. Rewrite 产出 ECS 选项字节：类型码 8、长度、地址族、源前缀、作用域前缀 0，后面跟覆盖前缀所需的地址字节，IPv4 与 IPv6 分别计算；长度字段按地址字节数加 4，写错一个字节上游就整条丢弃。
7. 全局范围（前缀 0）的选项不带地址字节，长度固定为 4。
8. 缓存键包含生效前缀，同一前缀的客户端共键，前缀 0 时跨子网共享，不同租户不共键。
9. 缓存键要区分记录类型与 class，域名大小写归一到小写，同一问题只有一把键。
10. 上游拒绝 EDNS 时回退到不带选项的查询，缓存键随之变成全局键，不能把截断答案继续挂在子网键上。
11. 同一个答案被不同前缀命中时 TTL 取各来源的最小值，先写入的较宽值不能覆盖后来的较短值。
12. 客户端地址是运营商级 NAT 出口时不额外放宽前缀，仍按租户策略截断。

审计侧另有要求：任意一条缓存记录都要能说清它服务哪个前缀、属于哪个租户，导出时不能出现完整客户端地址。

硬约束：同一前缀的并发回源必须合并成一次真实查询，后到的请求等待首个查询的结果，不能各自打上游；缓存容量满时按最旧淘汰，条数与每租户上限都是硬边界，淘汰不能跨前缀误伤其他租户的条目。

验收：go test ./... 的 15 条用例必须全过，不得修改测试、跳过或按固定输入写死；并发回源与容量淘汰自行验证；只做本地实现，交付源码与测试记录。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-03`
- 实现文件：`ecs_privacy_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`EcsPrivacyGuard`
