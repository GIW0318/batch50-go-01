# 2D 界面遮罩栈与嵌套裁剪

## 题面

设置界面改版后，弹窗里的列表滚动时总有一截内容压到弹窗圆角外面，嵌套三层的面板干脆整块不显示；还有几个页面在关掉弹窗后，剩下的按钮被裁掉一半，像是裁剪区没退回去。这层遮罩栈是界面树递归绘制时唯一的状态来源，父面板压的矩形裁剪要先跟子面板的圆角遮罩求交，滚动容器再把裁剪区收窄到可视窗口内，所以栈上每一条都要记住自己入栈时的裁剪矩形和羽化扣费，出栈时逐层还原。落点在 gsb/batch50-go-game-b-17 的 ui_mask_stack.go，类型 MaskStack，要把界面元素绘制期间的裁剪区与遮罩状态维护补齐。

公开接口保持：NewMaskStack(bounds Rect, maxDepth, featherBudget int) *MaskStack、PushRect(r Rect, softness float64) (Token, error)、PushMask(m MaskRef) (Token, error)、Pop(t Token) error、Clip() Rect、MaskAt(depth int) (MaskRef, bool)、Depth() int、Reset() error、FeatherUsed() int、Stats() MaskStats、Snapshot() ([]byte, error)、Restore([]byte) error。

要修的点：

1. 空矩形、负宽高报 ErrBadRect，负柔化半径报 ErrBadSoftness，压栈被拒时深度与裁剪区都不能变；
2. 矩形裁剪按与当前裁剪区求交累积，取左上的较大值与右下的较小值，算出的宽高非正即视为空，逐层收窄，出栈后恢复上一层的裁剪矩形；
3. 遮罩条目必须带非空标识和可用矩形，羽化不得为负，否则报 ErrBadMask，遮罩自身的矩形同样参与裁剪求交；
4. 层级超过上限的压栈报 ErrDepthLimit，层级上限是硬边界，超限直接拒绝而不是截断栈，当前裁剪区、深度与羽化用量都不变；
5. 出栈必须后进先出，令牌与栈顶不匹配报 ErrTokenMismatch，空栈出栈报 ErrStackEmpty；
6. MaskAt 按栈深度从外到内取遮罩，越界或负深度返回 false；
7. 软边羽化按柔化值与遮罩羽化向上取整累加进每帧预算，零柔化不记费，预算跨帧不自动清零时第二帧起所有软边遮罩都会压不进去；
8. 羽化超出预算的压栈报 ErrFeatherBudget 且不改变任何现有状态；
9. Reset 清空整栈并把裁剪区恢复成初始 bounds，羽化计数归零；
10. Depth 与 Stats 的深度、压栈数、出栈数、遮罩数、羽化用量要随操作如实变化，被拒绝的操作不计入；
11. 栈内容、裁剪区、各层令牌与羽化用量经 Snapshot/Restore 原样延续，恢复后仍能用原来的令牌出栈，损坏载荷报 ErrBadSnapshot 且不动现有栈。

两类硬约束：整棵子树因求交为空而不绘制之后，后续压栈只记录不生效，出栈回到非空区间时必须能恢复出正确的裁剪矩形，不能一路空到底；同一个遮罩被外层与内层同时引用时只建立一次遮罩通道，不能按引用次数消耗层级。另外整体重置后旧令牌全部失效，用旧令牌出栈要返回错误而不是弹出别人的遮罩。

验收：go test ./... 的 15 条必须全过，不得改测试、跳过或按固定输入写死；并发与重启场景自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-b-17`
- 实现文件：`ui_mask_stack.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`MaskStack`

## 关键类型

- `Rect`、`MaskRef`、`Token`、`MaskStats`
- 错误集：`ErrBadRect`、`ErrBadSoftness`、`ErrBadMask`、`ErrStackEmpty`、`ErrTokenMismatch`、`ErrDepthLimit`、`ErrFeatherBudget`、`ErrBadSnapshot`
