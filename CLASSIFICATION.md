# Webshell 内容判定口径

## 输入与解码

1. `ID` 或 `Path` 仅含空白时立即返回 `ErrBadFile`，不产生缓存结论。原始内容按字节计长，超过 `MaxBytes` 返回 `ErrTooLarge`。
2. 只识别内容开头的 `b64:`、`hex:`、`url:` 前缀（前缀大小写不敏感）。`b64` 使用 Go 标准字母表；`hex` 使用十六进制；`url` 使用 query 反转义。载荷畸形返回 `ErrBadEncoding`。
3. 解码层数取调用方预算与 `Limits.MaxLayers` 的较小值。层数用尽后仍有编码前缀，或中间解码结果触及体积上限，均停止解码并置 `Truncated=true`。
4. 截断内容不会判为 `benign`：按已完成层保留能力证据，状态最低降为 `suspicious`。`Decoded.Bytes` 是最终保留内容的字节数。

## 能力与分值

能力只从**解码后已接受层**的真实调用点匹配，注释和字符串中的同名词不算调用；每个能力最多计一次。

| 能力项 | 调用名 | 权重 |
| --- | --- | --- |
| `exec` | `runCode`、`evalExpr`、`callNative` | 40 |
| `net` | `openSocket`、`postHttp`、`fetchRemote` | 25 |
| `decode` | `fromBase64`、`inflateBlob`、`rot13Text` | 20 |
| `file_write` | `writeFile`、`appendFile` | 15 |

总分是命中项权重之和（最高 100）：`>=70` 为 `malicious`，`40..69` 为 `suspicious`，其余为 `benign`。`Features` 按能力名升序输出；`Evidence` 逐条记录层号、行号、列号、调用名、能力项和可疑原因。

## 路径与调用上下文

- 上传、临时、缓存、附件目录或伪装图片/文本扩展名，视为不可信路径；其中出现能力调用直接计入。
- 已知框架目录（如 `/app/`、`/src/`、`/vendor/`、`/controllers/`、`/var/www/`、`/public/`）视为可信路径。可信路径中，参数不含外部请求来源时，框架自身的动态执行/解码调用不计分。
- `$_GET`、`$_POST`、`$_REQUEST`、`php://input`、`input(...)`、`request->input(...)` 等请求取值，以及 `cmd`/`shell`/`payload` 等参数，仍判为可疑上下文；裸 `$request` 依赖对象本身不直接视为外部数据。
- 函数定义同名调用不计为框架调用点；不可信路径中的危险包装函数定义仍会保留证据。

## 白名单、缓存与恢复

- 白名单键是原始内容（未解码）的 SHA-256 小写十六进制。输入去除全部空白并转小写；空值返回 `ErrBadHash`，非正 TTL 返回 `ErrBadTTL`。
- 命中有效白名单时 `Score=0`、`State=whitelisted`、能力项为空。`Hashes` 只列出仍有效的哈希并升序排列。
- 结论按 `FileID + 原始内容哈希 + Path` 命中；同路径内容变化会重新判定。重复命中标记 `Cached`，`Rescan` 用缓存的原始文件强制重算并清除标记。
- 同一文件由文件级门锁串行；不同文件可并行。`MaxFiles` 满时淘汰 `ScannedAt` 最早的结论，时间相同则淘汰最早写入者。
- 结论与白名单都保存绝对时间戳。`Prune(now)` 的 `now` 是调用方给出的统一清理基准，按 `now >= expiry` 删除两类条目并返回总数。
- 快照包含原始内容、结论、证据、时间戳和白名单，并带 SHA-256 校验。`Restore` 先完整解析、校验和字段一致性检查，全部成功后才原子替换；损坏快照不影响现有状态。

