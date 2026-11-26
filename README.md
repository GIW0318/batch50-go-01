# 双层 802.1Q 标签准入与学习表治理

## 题面

某写字楼接入改造后，二层网络开始串 VLAN：三楼财务的广播跑到二楼市场部，客户侧看到的视频组播忽有忽无；另一次是楼层交换机学习表被打满，整段办公网断了三分钟，恢复后同一台机器的 MAC 在两张表里各占一条。还有运维反馈，trunk 口把 native VLAN 的帧原样发出去，对端交换机又补了一层标签，回程流量全乱。改造方案里写明要支持外层 0x88a8，现场却有人把 0x8100 的口当成同一套规则来配；配置改完以后，也有几个口仍旧按旧规则放行，像是转发面没跟上。落点在模块 gsb/batch50-go-safety-b-05 的 qinq_tag_policy.go，类型 QinQTagPolicy，签名保持：NewQinQTagPolicy(cfg PolicyConfig) *QinQTagPolicy、Parse(frame []byte) (TagStack, error)、Classify(frame []byte, port PortConfig) (Verdict, error)、Egress(frame []byte, port PortConfig) ([]byte, error)、Learn(port string, frame []byte) error、LearnAt(port string, frame []byte, at time.Time) error、Lookup(svlan uint16, mac string) (string, bool)、PortStats(port string) PortStats、SetPortConfig(port PortConfig) error、AgingSweep(now time.Time, idle time.Duration) int、EntryCount() int、Snapshot() ([]byte, error) 与 Restore(data []byte) error。

需要落地的行为：

1. 入口按端口模式判定标签层数：接入口只收未打标签帧，trunk 收单层，qinq 口必须双层，层数不符一律丢弃并计原因。
2. 外层 TPID 按端口配置识别，0x88a8 与 0x8100 混用时要认端口上配的那一种，不能只认一种。
3. 外层服务 VLAN 必须在端口允许集合内，不在集合内按策略丢弃，集合大小超过上限时 SetPortConfig 报 ErrSVLANSetTooLarge，被拒的配置不能顶掉旧配置。
4. 内层客户 VLAN 等于该口 native VLAN 时判泄漏丢弃，不能放行。
5. 未打标签帧只能落到 native VLAN，接口没配 native VLAN 时丢弃。
6. 帧长不足、TPID 不在支持范围内分别报 ErrShortFrame 与 ErrUnknownTPID。
7. 出口按端口选择保留、剥离外层或丢弃；剥离后内层标签原样保留，优先级位与丢弃资格位不得被改写。
8. native VLAN 帧从 trunk 口发出时要不要补标签由对端配置决定，不能和接入端口一样处理。
9. 学习表按外层 VLAN 与源 MAC 建键并记录端口，容量满报 ErrTableFull，不覆盖旧条目，也不悄悄淘汰正在转发的条目。
10. 同一 MAC 在同一服务 VLAN 内换了端口按移动处理，只保留一条记录，Moved 计数加一。
11. 老化按最后学习时间清理，AgingSweep 返回清理条数，清理后查询不到，且不牵连其他服务 VLAN 的条目。
12. 配置切换对转发路径原子生效，入口判定与出口改写不得看到半新半旧的端口配置。

客户侧的诉求很具体：一台新终端接进来，三十秒内要能查到它在哪个口、属于哪个服务 VLAN；查不到就是没学到，不能靠猜。

硬约束：Classify、Egress、Learn 会被多个收包协程并发调用，学习表的读写必须串行且不丢更新，同一 MAC 的移动在并发下只能落一条记录；快照要包含学习表与各端口计数，恢复后条目、归属与计数与快照一致，重启不把已老化的条目带回来。

验收：go test ./... 的 15 条用例必须全过，不得修改测试、跳过或按固定输入写死；并发学习与老化自行验证；只做本地实现，交付源码与测试记录。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-b-05`
- 实现文件：`qinq_tag_policy.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`QinQTagPolicy`
