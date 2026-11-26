# 画面黑边检测与稳定裁切

## 题面

投诉单 2026-1114-22：新一季番剧上架后，带黑边的片源裁切不干净——第 3 集右侧留了一条 6 像素黑边，第 7 集在镜头切换的瞬间裁切框跳了一下，还有一集把暗场景里站在画面边缘的人物直接裁掉了；审核同事重新扫一遍，同一集又给出另一个结果。落点在 gsb/batch50-go-game-c-12 的 letterbox_detector.go，类型名 LetterboxDetector，包名 task。

构造签名 NewLetterboxDetector(cfg Config) *LetterboxDetector；Config 的 Threshold 为零时按 40 处理，StableWindow 为零时按 3 处理，窗口上限 120。帧用 Frame 传入，Luma 按行优先存放，长度必须等于 Width*Height。

需要落地的公开接口：Feed(frame Frame) (Detect, error)、Rect() CropRect、FrameSize() (int, int)、SetThreshold(luma int) error、SetStableWindow(n int) error、Commit(rect CropRect) error、Reset() error、Stats() DetectStats、Snapshot() ([]byte, error)、Restore(data []byte) error。

要修的现象：

一、逐帧先按行扫上下边、再按列扫左右边，亮度低于阈值的连续边缘算黑边；上下与左右同时存在时必须分别判定，不能让一边把另一边吞掉。

二、候选矩形要连续 StableWindow 帧完全一致才提交；提交之前 Rect 一直保持上一张画面的整幅尺寸，不能一帧就改。Detect 每次都要给出本帧扫到的四边数值与当前连续帧数，稳定与否只体现在 Stable 字段与是否提交上。

三、Rect 报告当前生效的裁切矩形，FrameSize 报告最后一张被接受帧的宽高，Reset 之后这两项都要能继续读出来。

四、暗场景整帧偏暗、边缘很黑但中部还有内容时只能判为无黑边，不能按行均值把画面内容切掉。

五、稳定窗内候选矩形在奇偶像素之间来回跳变时，提交前要归一化到偶数对齐，不能提交奇数宽度或奇数偏移。

六、镜头切换后旧矩形继续生效，直到新候选重新稳定，不能让裁切矩形在切换的瞬间跳变。

七、阈值合法区间是 1 到 255，越界报 ErrBadThreshold；稳定窗合法区间是 1 到 120，越界报 ErrBadWindow。

八、帧宽高为零、或者 Luma 长度与 Width*Height 不符时报 ErrBadFrame；被拒绝的帧不计入扫描帧数，只累加拒绝计数。

九、手工 Commit 只接受完全落在画面内、宽高为正的矩形，越界报 ErrBadRect；还没有接受过任何帧时报 ErrNoFrame；提交成功后稳定窗从零重算。

十、Stats 要分别给出扫描帧数、稳定提交次数、拒绝次数、手工提交次数与最长连续一致帧数。

十一、快照要同时覆盖已发布矩形、候选矩形、稳定计数与各项统计；损坏快照 Restore 报 ErrBadSnapshot 且不动现网状态，正常恢复后稳定窗计数与当前矩形延续。

十二、同一次扫描里上下边与左右边的判定互不影响，任何一边没有黑边时另外三边仍要按自己的结果给出，不能因为一边为零就把整幅画面当成全黑或全亮。

硬约束：多路解码线程并发 Feed 时状态只能被串行推进，扫描帧数与提交次数必须与真实帧数一致，不能丢帧也不能重复计数；统计与矩形发布必须成对可见，不能出现统计已经加过而矩形还没更新的中间状态；重启恢复后继续喂同样的帧，稳定窗要接着上次的进度走，而不是从头再攒一遍。

验收：go test ./... 十五条必须全过，不得修改测试、跳过或用固定输入写死；并发与重启恢复自行验证；只做本地实现，只用标准库，不得引入第三方解码库。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-c-12`
- 实现文件：`letterbox_detector.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`LetterboxDetector`
