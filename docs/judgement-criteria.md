# JWT 混淆探测判定口径

## 矩阵与编号

- 样本必须同时包含非空 `Target` 与 `Alg`，否则返回 `ErrBadSample`，且不写入目标表。
- 固定顺序：`alg/none`、`alg/hs256`、`alg/rs256`，随后按 `Sample.Claims` 原顺序生成 `claim/<name>`，最后 `sig/empty`。
- 编号为 `<target>/<group>/<name>`，例如 `t1/alg/none`、`t1/claim/exp`、`t1/sig/empty`；同一样本重建得到完全相同的矩阵。
- 用例数为 `4 + 去重后的声明数`，超过 `Limits.MaxCasesPerTarget` 时整批返回 `ErrCaseLimit`。

## 令牌口径

- `alg/none`：头部算法改为 `none`，`Token.Signature` 必须为空。
- `alg/hs256`、`alg/rs256`：头部算法替换为对应值，并写入确定性的非空伪造签名。
- `sig/empty`：保留样本算法，仅将签名段置空。
- `claim/exp` 置为过去时间 `1`；`nbf`、`iat` 置为远期值；`iss`、`aud`、`sub`、`jti` 与其他声明写入明显非法值。
- 同一用例重复 `Craft` 返回完全一致的令牌；新批次使用新的 `batchID`，批次间不共享令牌状态。

## 命中判定

命中必须证明请求真正进入受保护业务逻辑，状态码本身不是充分证据。

1. `401`、`403`、`407` 或响应体包含 `unauthorized`、`forbidden`、`invalid token`、`token expired`、`invalid signature` 等拒绝证据时，判定未命中。
2. 非 `4xx/5xx` 响应中，`Response.Protected=true` 表示观测确认进入受保护逻辑，判定命中，证据类型为 `protected_logic_confirmed`。
3. `Protected=false` 时，只有响应体出现 `account-list`、`protected-data`、`business-data`、`order-list`、`user-profile`、`admin-panel` 等明确业务数据标记，才以 `protected_business_payload` 命中。
4. 仅有 `200` 或普通成功正文、但没有上述业务证据时，判定未命中，原因为 `status alone does not prove protected business logic ran`。

## 两类结论

- 算法替换与空签名/伪造签名用例使用 `signature_replacement:*` 证据前缀。
- 声明缺陷用例使用 `claim_defect:*` 证据前缀。
- 两类结论分别由对应用例的响应产生，不能用“过期声明被拒绝”替代“`alg=none` 是否进入业务”的结论，反之亦然。
- 命中时 `Hit=true` 且 `Evidence` 非空；未命中时 `Hit=false`、`Evidence` 给出未命中证据类型，`Reason` 给出类别化原因。重复 `Judge` 返回同一结果。

## 状态、清理与重启

- 状态流为 `open -> submitted -> judged -> cleaned`；批量清理会把未判定用例置为 `void`。
- 单例 `Cleanup` 要求已有判定结论，否则 `ErrNotJudged`；重复清理已关闭用例仍成功。
- `CleanupAll(target)` 关闭该目标整批用例：已判定用例转 `cleaned`，未判定用例转 `void`，返回该批次关闭条数；清理同时删除 token、response、evidence 缓存键并覆盖/脱敏审计派生字段。
- 清理完成后该目标批次不再在途，可用同目标重新 `Enumerate`；`Close(target)` 删除目标表并释放跟踪名额。
- 未知用例 ID 在 `Craft`、`Submit`、`Judge`、`Cleanup` 中均返回 `ErrUnknownCase`；未知目标的 `Close`、`CleanupAll` 返回 `ErrUnknownTarget`。

## 快照与并发

- 快照为带格式标识和 SHA-256 校验的 JSON；恢复前完整解析、校验矩阵和状态，任何损坏或不一致均返回 `ErrBadSnapshot`，不触碰当前活动状态。
- 恢复保留已回填响应与已判定结论；没有响应的用例一律置为 `void` 并丢弃已签发令牌，之后 `Craft` 返回 `ErrTokenVoid`，不会出现“既作废又可取令牌”的中间态。
- `JwtConfusionProber` 用全局读写锁保护目标表和用例索引，每个目标另有互斥锁；同目标操作串行，不同目标可并行。目标数、单用例数和每目标在途批次均受硬上限约束。

## 验证

- `GOCACHE=/tmp/uid30947-go-cache GOPATH=/tmp/uid30947-gopath go test -count=1 -v ./...`：题目 15 条用例全部通过，记录见 `artifacts/go-test-run.log`。
- 临时并发/重启/清理自验曾通过 `go test -race -count=1`，记录见 `artifacts/self-check-concurrency-restart.log`；临时测试文件已删除，未修改或跳过给定测试。
