# 字幕烧录渲染

## 题面

投诉单 2026-1118-45：字幕轨重新烧录后，上一句刚结束、下一句刚好开始的同一帧上两行字幕叠在一起；换了字体包之后，原本回退到黑体的日文全变成方框；一帧里几条字幕同时出现时整条被吞掉，主字幕也跟着消失。落点在 gsb/batch50-go-game-c-14 的 subtitle_burnin_renderer.go，类型名 SubtitleBurninRenderer，包名 task。

构造签名 NewSubtitleBurninRenderer(cfg Config) *SubtitleBurninRenderer；Config 的 MaxLayers 为零时按 3 处理，GlyphBudget 为零时按 40 处理、上限 4096。

需要落地的公开接口：Load(style StyleSheet) error、AddCue(cue Cue) error、Cues(from, to time.Duration) []Cue、At(frame Frame) ([]RenderItem, error)、SetSafeArea(r Rect) error、ResolveFont(name string) (FontRef, error)、RegisterFont(name string, ref FontRef) error、ReloadFonts() error、Budget() (int, int)、Stats() RenderStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的现象：

一、cue 按帧时间窗生效，起点那一帧可见、终点那一帧不可见，即左闭右开；上一条结束时刻与下一条开始时刻相同的帧只显示后一条，不能两条同时出现。

二、样式按全局、轨道、cue 三级继承，后一级只在字段非零时覆盖前一级，没覆盖的字段继续沿用上一级。

三、同层重叠的 cue 只保留优先级最高的一条，优先级相同取先加入的一条；跨层可以同时显示，输出时按层号升序排列，条数受 MaxLayers 限制，超出时保留层号最小的若干条。

四、字形数按可见 cue 的文本字符数累加，每帧有预算；超预算的次要层降级为省略号，主层字幕不能被省略，也不能被整条丢弃。

五、缺失字体按回退链逐个尝试，命中回退链的解析结果要标注来源是回退；回退链全部落空报 ErrFontMissing。

六、字体回退的解析结果要缓存，但字体重载之后同一个请求必须重新解析，不能继续返回重载前的旧字体引用。

七、cue 的编号为空、文本为空、结束不晚于开始、层号为负都报 ErrBadCue，被拒绝的 cue 不进表也不占层。

八、Cues 返回与给定区间有交集的 cue，按开始时间升序，开始时间相同的按加入顺序。

九、安全区的宽高必须为正、原点不能为负，否则报 ErrBadArea；帧宽高为零的渲染请求按无效帧处理。

十、Budget 返回最近一帧实际使用的字形数与预算上限。

十一、快照要覆盖 cue 表、样式表、字体注册表与安全区；损坏快照 Restore 报 ErrBadSnapshot 且不动现网状态，恢复后同一帧的字幕、样式与字体都能照旧渲染。

十二、Cues 的区间为空或者起点晚于终点时返回空列表，不报错也不返回全表。

硬约束：多线程同时渲染同一帧时计数与预算只能串行更新，统计帧数必须等于调用次数，任何一次预算读数都只能是完整的一帧结果；样式表与字体表在加载期间对渲染线程必须原子可见，不能读到只换了一半的样式；重启恢复后样式与字体解析缓存延续，不能因为重启把已经加载的字幕丢掉。

验收：go test ./... 十五条必须全过，不得修改测试、跳过或用固定输入写死；并发与重启恢复自行验证；只做本地实现，只用标准库。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-c-14`
- 实现文件：`subtitle_burnin_renderer.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`SubtitleBurninRenderer`
