# 保护键域分配与线程权限切换治理

## 题面

值班同事转来一张工单：某图像处理服务接入第三方插件后，画布渲染进程偶发"踩到别人的内存"——一个域的线程能读到另一个域刚释放的缓冲区；释放域之后重建同名域，还能拿到上一个域残留的键权限；服务重启后键位占用表整个丢失，几个隔离域抢到同一批键号，日志里成片出现越域访问告警。运维把线程池从固定大小调大以后问题更频繁，怀疑是协程切换时把某个线程上的键权限带到了新协程，插件侧又拿不到任何可解释的拒绝原因。

落点 gsb/batch50-go-safety-c-16 的 pkey_domain_allocator.go，类型 PkeyDomainAllocator。签名保持：NewPkeyDomainAllocator(cfg Config) *PkeyDomainAllocator、NewDomain(name string, mode PkeyMode) (Domain, error)、AllocKey(domainID string) (Pkey, error)、SetRights(pkey Pkey, rights Rights) error、Bind(domainID string, r Region) error、Check(domainID string, r Region) error、PinThread(domainID string) (Pin, error)、UnpinThread(pin Pin) error、ReleaseDomain(domainID string) error、Domain(domainID string) (Domain, bool)、PkeyState(pkey Pkey) (PkeyState, bool)、Domains() []Domain、Snapshot() ([]byte, error)、Restore([]byte) error。Config 里的 MaxKeys、MaxRegionsPerDomain、MaxPinsPerDomain 分别约束键位总量、单域区域数与单域线程钉数量，非正值按 15、4、8 兜底。

要修的点：

1. 域名空白或模式不是 exclusive/shared 报 ErrInvalidDomain，被拒请求不得入表。
2. 重名报 ErrDomainExists；域号按 dom-1、dom-2 递增，域记录带单调递增序号，Domains() 按创建顺序返回，Domain 查不到时 ok 为 false。
3. 键位从 1 开始取最小空闲号，初始权限只读、版本号 1，键状态里要能查到归属域；超出 MaxKeys 报 ErrPkeyExhausted，被拒的请求不得占用任何槽位。
4. 权限掩码为零、或写权限不带读权限，报 ErrBadRights；每次成功改权限版本号加一，非法请求不推进版本。
5. 区域长度为零报 ErrBadRegion；同域重叠报 ErrRegionOverlap，相邻区域不算重叠。
6. 与其他域已占区域重叠报 ErrDomainViolation；Check 只放行完整落在本域区域内的区间，跨域或越界同样报 ErrDomainViolation。
7. exclusive 域只能绑一个区域，shared 域按 MaxRegionsPerDomain 限流，超限报 ErrRegionLimit。
8. 未知域、未分配键、未知线程钉分别报 ErrUnknownDomain、ErrPkeyUnknown、ErrUnknownPin，重复解钉也算未知；钉数量超 MaxPinsPerDomain 报 ErrPinLimit，解钉后钉号继续递增不回收。
9. 释放域要连带清掉键位、区域与钉表记录，之后重建域能拿到被释放的最小键号；重复释放报 ErrUnknownDomain。
10. 快照与现网状态互斥：Snapshot 只能看到全量旧状态或全量新状态；Restore 全成或全不成，损坏载荷报 ErrBadSnapshot 且不动现网。
11. 恢复后域号与键号水位只增不减，已释放的键位可以被重新分配，但编号不得回退。

硬约束：

- 并发取键必须串行裁决，24 个协程抢 4 个键位只能成功 4 个，键号不得重复，失败方一律拿到 ErrPkeyExhausted。
- 改权限的协程要先经 runtime.LockOSThread 固定线程再取钉，钉未归位的线程不能带着旧键权限跑到别的域；域释放时若仍有钉挂着，要报 ErrPinsActive 而不是直接回收。释放后键位清零并回收，但还有线程持有该键的线程局部状态时不能立即复用该键号。
- Restore 全成或全不成，损坏载荷报 ErrBadSnapshot 且不动现网，恢复过程中任何一步失败都要回到调用前状态。由同一快照拉起的子实例与父实例各自持有权限副本，父子任一方调整权限都不应影响对方。

验收：go test ./... 15 条全过；不得改测试、跳过、按固定输入写死；并发与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-16`
- 实现文件：`pkey_domain_allocator.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`PkeyDomainAllocator`
