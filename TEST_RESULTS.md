# 测试记录

日期：2026-11-27  
环境：Ubuntu / Go 1.23，工作目录 `/home/admin/feedback/uid30950`

## 验收测试

命令：

```bash
GOCACHE=/tmp/go-build-cache go test -v ./...
```

结果：`tests` 包 15/15 通过，`PASS`，耗时约 `0.003s`。覆盖空白文件引用、恶意组合、干净文件、能力去重、超体积、解码计分、层数截断、畸形 b64/hex、Explain、白名单、Rescan、Prune、快照恢复和损坏快照拒绝。

## 自检

临时自检（验证后已移除，不改动交付测试）使用：

```bash
GOCACHE=/tmp/go-build-cache go test -race ./...
```

通过项包括：

- 两层 base64 完整解码、40+25 分值及调用点 Evidence。
- 合法框架路径 + 非请求参数的误报抑制；`$_GET` 请求参数仍判可疑。
- 同路径内容变化后按新 SHA-256 重算，不复用旧结论。
- `MaxFiles` 满时淘汰最旧结论，`Forget` 释放槽位。
- 白名单到期与结论缓存过期后 `Prune` 清理并重新扫描。
- 64 次并发扫描（含同 ID 竞争）在 `-race` 下无竞态。
- `Scan`/`Rescan`/`Explain`/`VerdictFor`/`Whitelist`/`Snapshot` 混合并发在 `-race` 下无竞态。
- 含截断层、白名单、Evidence 的状态快照恢复后可继续 `Rescan`。
- 非法 `url:` 载荷返回 `ErrBadEncoding`，请求层数超过硬上限时仍受 `MaxLayers` 约束。

## 静态检查

```bash
GOCACHE=/tmp/go-build-cache go vet ./...
```

结果：无输出，退出码 0。

