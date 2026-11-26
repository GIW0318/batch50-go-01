# 场景射线拾取查询：实例加速结构、层掩码与每帧查询预算

## 题面

点选最近连着出单：关掉的那盏灯在墙后面，鼠标扫过去还是能把墙后的箱子选中；框选同一片区域，两次返回的实例顺序不一样；怪多时一帧点十几下，输入线程卡住、画面跟着掉帧。现在点到谁近就算谁，实例表遍历时现查，层过滤和越界靠调用方兜，被丢的查询没人记账。落点 gsb/batch50-go-game-b-11 的 scene_pick_query_service.go，类型 PickQueryService，公开接口保持：NewPickQueryService(cfg PickConfig) *PickQueryService、AddInstance(inst Instance) error、UpdateInstance(instanceID string, box AABB) error、RemoveInstance(instanceID string) error、Instance(instanceID string) (Instance, bool)、Instances() []string、SetLayerMask(layer string, mask uint32) error、LayerMask(layer string) (uint32, bool)、Submit(q PickQuery) (QueryID, error)、RunFrame() int、Resolve(id QueryID) (Hit, bool, error)、ResolveBatch(qs []PickQuery) ([]Hit, error)、Stats() PickStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的地方如下。一、实例号或层名为空、只有空白也算空，包围盒下界大于上界或含 NaN，AddInstance 报 ErrBadInstance 且不落表；重复实例号报 ErrDuplicateInstance；实例数到 MaxInstances 报 ErrInstanceLimit，上限留 0 或负按 1024 处理。二、成功的 UpdateInstance 把实例推进到新的全局代际，登记时代际从 1 起，命中结果带出实例当下代际；未登记报 ErrUnknownInstance，坏盒报 ErrBadInstance 且代际不动；RemoveInstance 后不再参与命中，重复移除报 ErrUnknownInstance。三、层须经 SetLayerMask 绑定到单一位，层名为空、掩码为 0 或含多位报 ErrBadLayer；一个位只属一个层，同一层不能改绑，冲突报 ErrLayerConflict；无绑定位的层不参与命中。四、射线方向须为单位长度（误差 1e-6 内），MaxDistance 须为正，LayerMask 不能为 0，否则报 ErrBadQuery 且不进队列。五、每条查询只出一个命中：距离最近优先，同距按实例号升序取小者，超过 MaxDistance 不算命中。六、每帧有查询条数与遍历节点数两重硬预算，节点数按本次查询检视过的实例个数计；装不下的查询整条按提交顺序顺延到下一帧，不许只算一半；单条查询自身节点数超过整帧预算仍要放行，否则队列推进不了。七、被顺延的查询不能带着提交时刻的旧包围盒去算，要在真正执行的那一帧按实例当前代际重算。八、Resolve 对没发过的查询号报 ErrUnknownQuery；已入队未执行的返回 ok 为 false；执行完没命中返回 ok 为 true 且实例号为空。九、ResolveBatch 按入参顺序逐条给结果，未命中给空实例号与 0 距离；任一查询非法整批报 ErrBadQuery 且不给部分结果，这一批不占帧预算、不动帧号与计数。十、被层掩码过滤掉的实例即使更近也不参与命中，过滤只作用于这一条查询，不能改变同批其它查询的命中与先后。十一、同一帧里查询键完全相同的重复提交（射线原点、方向、掩码、最大距离都相同）合并成一条并返回同一查询号，合并出的结果不能被先完成那条提前取走。

硬约束：Submit、RunFrame、Resolve 会被输入线程与渲染线程同时调用，同一帧的入队、执行与出队必须串行裁决，一条查询不能被执行两次；快照要么是执行前全量要么是执行后全量。快照带实例表及代际、层绑定、排队查询与帧号、累计计数，重启后排队查询按原顺序继续执行，代际与计数接着涨；快照损坏时 Restore 报 ErrBadSnapshot 且不动现网。Stats 给出帧号与累计的提交、执行、顺延、合并、排队条数和访问节点数，顺延按查询去重计数，每调一次 RunFrame 帧号加一。验收：go test ./... 15 条全过，测试不许改、不许跳过、不许按固定输入写死；并发提交与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-11`
- 实现文件：`scene_pick_query_service.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`PickQueryService`
