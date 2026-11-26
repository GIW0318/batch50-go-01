# 内存页合并的跨租户阻断与去重核算

## 题面

节点上的内存合并上线之后，安全组抓到一次跨租户读到了同一份页：两个租户的匿名页内容一致，被合并成了一页，其中一个租户的进程改了内容，另一个租户在写时复制之前读到了别人的数据。同一次巡检还发现，持有密钥的进程明明配了禁用合并，统计里却仍在增长；另一台节点关闭合并之后，去重率只降不降，运营看板上全是噪声。请在 gsb/batch50-go-safety-c-04 的 ksm_cross_tenant_guard.go 里补完 KsmTenantGuard，把合并范围收到租户与安全上下文之内，并按租户记账。

对外接口保持：NewKsmTenantGuard(limits Limits) *KsmTenantGuard；Enable(tenantID string, p KsmPolicy) error；Disable(tenantID string) error；Track(pages PageSet) error；Scan(now time.Time) (Report, error)；Isolate(pages PageSet) error；Release(pages PageSet) error；Stats(tenantID string) KsmStats；StatsAll() []KsmStats；MergedPages(tenantID string) []string；Alerts() []Alert；Export() ([]byte, error) 与 Import([]byte) error。

要修的地方：一，Enable 的租户为空或全是空格返回 ErrBadTenant，策略里的扫描周期低于节点下限返回 ErrScanInterval，页数上限超过节点上限返回 ErrPageLimit，告警比例超出零到一百返回 ErrBadPolicy，被拒的租户不进入 StatsAll，扫描周期正好等于下限要放行；二，Track 的页缺少编号、租户或内容摘要返回 ErrBadPage，节点在册页数超过 MaxPagesPerNode 时整批拒绝、一条都不能进去；三，只有同租户、同安全上下文、内容摘要一致、且是匿名页的页才可以合并，跨租户即使摘要一致也不合并；四，持有密钥或正在解密的页整页禁用合并，计入 Skipped 而不计入 Merged；五，Scan 的间隔小于节点下限返回 ErrScanTooSoon，正好等于下限要放行；六，报告给出扫描页数、合并页数、隔离页数、跳过页数与去重比例，去重比例按合并页数除以扫描页数再乘一百；七，报告按租户给出合并、隔离与跳过三段计数，整体比例与逐租户比例必须自洽，Stats 给出受控页数、合并页数、去重比例、写时复制延迟与最近扫描时刻，StatsAll 按租户号升序；八，写时复制延迟按合并页的写入速率估算，每千次每秒写入计一毫秒并向上取整，超过阈值要出 wal_latency 告警，去重比例超过阈值要出 dedupe_ratio 告警，告警里带租户、实测值、阈值与时刻；九，Isolate 把页钉住、立即退出合并组且后续扫描不再参与合并，隔离未在册的页返回 ErrBadPage；十，Disable 之后该租户的合并页全部拆开、统计归零，邻居租户不受影响，未注册的租户返回 ErrNoTenant；十一，共享内存在不同租户挂载时归属按挂载方而不是创建方判定，MountTenant 与 Tenant 不一致的页不得进入任何合并组；十二，合并页被释放后 Release 要把统计回退，不能只增不减；关闭合并时已合并页的解合并顺序要按写入活跃度排序，避免同一页反复被合并又拆开。

硬约束：租户归属变化时已合并页立即失效并原子解合并，不许留下过渡态，扫描与隔离并发执行时不能重复计数，也不许一页同时出现在两个租户的合并组里；单节点受控页数、扫描周期下限与统计保留窗口都受上限约束，统计窗口之外的数据不参与比例计算。

验收：go test ./... 十五条全过，测试不得修改、跳过或按固定输入写死；扫描与隔离并发、释放回退与关闭合并自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-04`
- 实现文件：`ksm_cross_tenant_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`KsmTenantGuard`、`Page`、`PageSet`、`KsmPolicy`、`Report`、`KsmStats`、`Alert`
