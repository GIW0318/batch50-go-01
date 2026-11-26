# SCIM 2.0 用户与组列表的过滤求值与稳定游标分页

## 题面

身份目录的同步作业从上周开始大面积失败：上游把 SCIM 2.0 的 filter 原样透传过来，我们的用户与组列表接口要么直接忽略 filter 返回全量，要么同一个表达式两次给出不同结果，按游标翻页时还会把已经翻过的账号重复吐出来。值班同学拿两条同样的请求比对，返回的账号顺序都不一样，前端列表跟着乱跳，客服已经收到十几张工单。落点是模块 gsb/batch50-go-safety-a-01 的 scim_filter_engine.go，类型 ScimFilterEngine，公开接口保持不变：NewScimFilterEngine(limits Limits) *ScimFilterEngine、RegisterSchema(s AttrSchema) error、Parse(filter string) (Expr, error)、Match(expr Expr, res Resource) (bool, error)、Query(kind string, req QueryRequest) (QueryPage, error)、Explain(expr Expr) []AttrRef、Fingerprint(expr Expr) string、Put(res Resource) error、Get(kind, id string) (Resource, bool)、Delete(kind, id string) error、Snapshot() ([]byte, error)、Restore(data []byte) error、CacheStats() CacheStats。

要修的地方如下。第一，空表达式、只有空白、括号不配对、比较运算符缺右值一律报 ErrBadFilter，不能退化成全量查询。第二，表达式里出现未通过 RegisterSchema 登记的属性路径时报 ErrUnknownAttr，在解析阶段就拦下。第三，字符串比较是否区分大小写由该属性的 CaseExact 决定，eq、ne、co、sw、ew 都走同一口径。第四，数值属性上的 gt、ge、lt、le 按数值比大小，字符串属性按字典序比，布尔属性只接受 eq 与 ne。第五，not 只作用于紧跟其后的一个条件，and 的结合比 or 紧，括号可以改变结合顺序。第六，Explain 返回表达式引用到的属性，按路径与子属性升序排列并去重。第七，Query 的排序键为空时按 ID 升序，排序键相同时用 ID 升序兜底，降序时排序键与兜底键一起反转。第八，请求页大小超过 MaxPageSize 时按上限截断，Total 始终等于命中过滤条件的总条数。第九，命中总条数超过 MaxResults 时直接报 ErrResultLimit，不返回半页。

还有三条边界必须一起收掉。属性在资源上根本不存在时，pr 判定为假、ne 判定为真，不能把缺失属性当成空字符串拉去比较。多值属性只要任意一个值满足条件就算命中，大小写不敏感属性上的 co 与 sw 要和 eq 保持同样的大小写处理。游标必须基于上一页最后一条的排序键与 ID 生成，翻页期间有新增或删除时不得把同一条记录返回两次。这三处只要漏一处，翻页与判等就会在生产上给出自相矛盾的结果。

硬约束有三类。编译结果按表达式指纹缓存并只读共享，同一个 filter 文本被多路并发查询时只解析一次，CacheStats().Entries 只增加一条，缓存条目数受 MaxCacheEntries 硬顶；Fingerprint 对同一个表达式必须稳定，缓存与游标都以它为准。游标被改动过要报 ErrCursorInvalid，拿另一个表达式的游标来查要报 ErrCursorMismatch，两种情况都必须拒绝而不是悄悄回到第一页。快照要带上 schema 登记表、资源、删除标记与序号，损坏的载荷在 Restore 时报 ErrBadSnapshot 且不能动到现网数据。

验收：go test ./... 的 15 条用例必须全过，测试不得修改、跳过或按固定输入写死；并发共享编译与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-a-01`
- 实现文件：`scim_filter_engine.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`ScimFilterEngine`
