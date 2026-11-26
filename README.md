# 超线程兄弟隔离：按物理核组分配、兄弟让出与超卖回退

## 题面

线上隔离评分掉了两档：两个安全域本来各自独占物理核，压测时却拿到了同一颗核的两条超线程，缓存互相踢，时延毛刺一路传到业务侧；另一台机器把两颗同编号的处理器当成同核做亲和，绑定完之后线程在两个域之间来回跳。现在要把按物理核分组的算力分配收进一个组件，落在 gsb/batch50-go-safety-d-02 的 smt_sibling_isolator.go，类型 SmtSiblingIsolator，包名 task。

公开接口保持：NewSmtSiblingIsolator(limits Limits) *SmtSiblingIsolator、SetTopology(cpus []CPU) error、Topology() []Core、Reserve(domain string, req Resource) (Allocation, error)、Release(allocID string) error、Rebalance() error、Allocation(allocID string) (Allocation, bool)、Allocations() []Allocation、DomainUsage(domain string) Usage、SetLoad(domain string, load int) error、Domains() []string、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的故障与边界：

一、拓扑必须至少有一个处理器，处理器编号唯一且非负，每颗核最多两条超线程，违反报 ErrBadTopology。

二、域名为空报 ErrBadDomain，线程数非正报 ErrBadRequest。

三、核的归属按核标识加兄弟列表判定，不能凭线程编号猜；一颗核只要有一条线程分给了某个域，这条核的其余线程就只对同域开放，绝不允许跨域共用，跨域共用的核一经发现就要拒绝分配。

四、独占请求按整核向上取整，取整后要标降级并给出整核取整的原因码，分配记录里写实际给出的线程数；核内空位仍归本域，释放后亲和掩码要复原。

五、分配优先取编号最小的可用核，核内优先取编号最小的线程；同域再次申请时优先复用已有核的空闲兄弟；分配记录只列本次用到的核与处理器，用量记录给出该域占用的处理器、核、线程数与分配条数。

六、容量不足报 ErrCapacity，不能把别的域的核拆一半出来；失败时把原因写进该域的用量记录，兄弟被占和彻底没容量要能区分。

七、离线的核与被预留的核不进分配池，估算容量时不能算进去。

八、相同超线程标识不代表同核，必须按核标识加兄弟列表判断归属。

九、让出兄弟导致容量下降时请求要回退并记降级原因，不能静默少给线程；域负载回落到阈值以下后，让出状态要自动结束，兄弟线程归还可用池，让出状态要能在用量记录里看出来。

十、Release 对未知编号报 ErrUnknownAlloc，释放后容量与用量都要回到释放前的口径。

十一、有分配时改拓扑报 ErrBusy，必须先释放干净；拓扑返回值是只读副本，改动不得影响内部。

十二、快照保存拓扑、分配与计数器，恢复要么全量成功要么不动现网，坏快照报 ErrBadSnapshot，恢复后分配序号继续递增。

硬约束：并发预留串行裁决，同一颗核不会落进两个域；让出与释放在同一把锁内完成，用量记录不出现半更新；拓扑与分配数设上限，超限直接拒绝而不是降级分配。

验收：go test ./... 15 条全过，测试不得修改、跳过或按固定输入写死；并发预留与重启恢复自行构造场景验证；只做本地实现，交付源码与跑测结果。

## 运行

模块路径 `gsb/batch50-go-safety-d-02`，包名 `task`，实现文件 `smt_sibling_isolator.go`，测试在 `tests/test_smt_sibling_isolator_test.go`。

```bash
go test ./...
```
