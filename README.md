# 本地化文本包热切换：回退链与整包原子激活

## 题面

多语言版本上线后，客服收到的截图越来越离谱。切到繁体中文时界面上一半是中文、一半还是英文，有的按钮干脆显示成 `battle.start` 这样的键名；带玩家名的欢迎语在英文下正常，切到繁体就报格式化错误，或者显示空串；最要命的是热更之后同一屏里上半部分还是旧文案、下半部分已经是新文案，截图流到论坛，运营只能回滚整个包。问题落在模块 gsb/batch50-go-game-a-14 的 locale_bundle_switcher.go，类型 LocaleBundleSwitcher，公开接口按现状保留：NewLocaleBundleSwitcher(limits Limits) *LocaleBundleSwitcher、Register(locale string, bundle Bundle) error、Activate(locale string) (Activation, error)、Resolve(key string, args map[string]string) (string, error)、ResolvePlural(key string, count int, args map[string]string) (string, error)、Missing(locale string) []string、Rollback(version int) error、Active() string、Version() int、Keys(locale string) []string、Bundle(locale string) (Bundle, bool)、Snapshot() ([]byte, error)、Restore(data []byte) error。

要收的口径如下。第一，登记时语言标签先做规范化，去首尾空白、转小写、下划线换成连字符；标签只剩空白报 ErrBadLocale，一条文本都没有报 ErrEmptyBundle，被拒的包不入表。第二，单包键数超过 MaxKeys 报 ErrKeyLimit，键与文本的字节总量超过 MaxBytes 报 ErrBytesLimit，超限整包拒收，不做截断。第三，文本里的占位符必须成对出现，只有左花括号或多了右花括号报 ErrBadPlaceholder，整包拒收；解析时缺参数报 ErrMissingArg，不能输出空串，也不能把花括号原样吐给玩家。第四，每个包登记时分配一个从 1 开始递增的版本号，激活、查询与回滚都要能对得上这个号。第五，激活未知语言报 ErrUnknownLocale，且不得把当前生效的语言换掉；激活成功要返回语言、版本、键数与缺失键清单。第六，解析链是当前语言、语言主体（zh-tw 退到 zh）、默认语言 en，逐级找第一个带该键的包；整条链都没有该键报 ErrUnknownKey，一个包都没激活时报 ErrNoActiveBundle。第七，回退到默认语言时占位符集合必须与目标语言一致，不能因为多一个参数就格式化失败。第八，复数按数量取变体，数量为 1 取 one，其余取 other，变体里的占位符由数量自动填入；缺少对应变体时按语言规则取最接近的变体，不能直接把键名返回给玩家。第九，缺失清单以默认语言为基准，列出该语言没有的键并按字典序返回，键清单同样按字典序返回。第十，激活是整包原子切换，同一帧内的读取必须落在同一版本上，激活过程中并发读取不得出现一半旧文本一半新文本。第十一，回滚按版本号重新激活历史包，未知版本报 ErrUnknownVersion 且不改动当前生效版本。第十二，快照要能描述全部已登记版本与当前生效指针，恢复后解析结果与回滚目标都不变；损坏的快照报 ErrBadSnapshot 且不动现网。

硬约束有三条。激活与解析会并发发生，读方只能看到完整的旧包或完整的新包，同一帧内的多次读取必须固定在同一版本上，不得半新半旧。进程重启后版本表与生效指针都要从快照恢复，Active、Version 与解析结果跟重启前一致，回滚仍能命中历史版本。单包键数、单包字节数与保留版本数都是硬顶，超限报错而不是截断、丢键或静默淘汰旧版本。

验收口径：go test ./... 的 15 条用例必须全过，测试不得修改、跳过、按固定输入写死；并发激活与重启恢复自行验证；只做本地实现，交付改动后的源码与跑测记录。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-game-a-14`
- 实现文件：`locale_bundle_switcher.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`LocaleBundleSwitcher`
