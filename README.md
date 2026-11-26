# 访客网络准入门户与围墙花园

## 题面

园区网络上周给访客区换了新的准入门户，运维当天就收到三类反馈：访客在授权页点完同意之后刷新浏览器，页面又被弹回门户；访客在授权前访问合作方提供的固件下载站，一直卡在门户页进不去；还有一部手机连着 Wi-Fi 反复点了三次同意，后台同时挂着三份通行凭据，第二天凌晨全部失效却没有通知。现场代码在 gsb/batch50-go-safety-b-12 的 captive_portal_gate.go，类型 CaptivePortalGate，需要把门户拦截、围墙花园放行、设备绑定授权与到期回收这条链路补全。

接口保持如下签名：NewCaptivePortalGate(limits Limits, rules []Rule) *CaptivePortalGate、Intercept(client Client, req Request) (Verdict, error)、Authorize(clientID string, grant Grant) (Lease, error)、WalledGarden() []Rule、Revoke(leaseID string) error、Expire(now time.Time) int、Validate(leaseID string, now time.Time) error、ClientOf(clientID string) (Client, bool)、LeaseOf(leaseID string) (Lease, bool)、Leases() []Lease、Snapshot() ([]byte, error)、Restore([]byte) error。

现场需要落实的行为与边界：

1. 首次出现的客户端被拦到门户页：动作 portal、原因 reason_portal_intercept，客户端进入 pending，Attempts 累加一次并记下设备指纹与地址。
2. 未授权客户端访问围墙花园命中项放行，原因 reason_walled_garden，状态仍是 pending；域名按标签边界匹配后缀，notvendor.example 不能命中 vendor.example。
3. 地址段规则只在连接建立阶段生效，203.0.113.0/24 内的地址放行，段外回门户页。
4. 已存在客户端的授权租约未过期时直接放行，原因 reason_authorized，Verdict 里带上租约 ID。
5. 授权租约已过期时，Intercept 就地把它和客户端都改成 expired，动作回 portal，原因 reason_lease_expired。
6. Authorize 对未知客户端报 ErrUnknownClient，对空指纹或与客户端记录不一致的指纹报 ErrDeviceMismatch，两种情况都不产生租约。
7. Grant 的 TTL 小于等于零报 ErrBadTTL；超过 Limits.MaxTTL 时按上限钳制后照常签发，不拒绝。
8. Revoke 对未知租约报 ErrLeaseNotFound，对已撤销或已过期的租约报 ErrLeaseNotActive；撤销成功后客户端进入 blocked，后续 Intercept 返回 deny 加 reason_blocked。
9. Expire(now) 一次性关闭所有到期的 active 租约并返回条数，同一批租约第二次扫描返回零。
10. 门户页自身的静态资源在任何状态下都必须放行，原因 reason_portal_asset，否则授权页会把自己拦掉。
11. 租约一到期，已经建立的在途连接要当场失效：Validate 对过期租约返回 ErrLeaseExpired，不能等到下一次新建连接才拒。
12. 同一设备指纹换了地址重新授权时，只能留最新一份有效租约，旧租约要作废，不能出现两份并行。

两类硬约束必须满足：Intercept、Authorize、Revoke 与 Expire 串行修改客户端与租约表，并发拦截同一客户端不得丢计数，并发授权同一设备只能留下最新一份租约；快照整体生效或整体不生效，Restore 遇到损坏载荷回 ErrBadSnapshot 且不动现网，客户端状态、租约、围墙花园规则与序号要一起恢复。

验收方式：在题目目录执行 go test ./...，十五条用例必须全部通过；不得修改测试、跳过用例或按固定输入写死结果；并发拦截与重启恢复要自己构造场景验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-12`
- 实现文件：`captive_portal_gate.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`CaptivePortalGate`
