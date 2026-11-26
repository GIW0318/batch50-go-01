# 加密断言解密密钥环的代际治理

## 题面

月度结账那天，三家客户的加密断言同时被判解密失败，运维只看到一句“无法解密”：一组是 SP 侧刚做完密钥轮换的租户，一组是上游把传输算法换回旧口径后重发的通知，还有一组响应里嵌套了两层加密。翻执行记录又看到两件事：退役标记在重启后丢了，被退役的那一代又被当成当前代继续解新响应；某个租户的允许清单被改成只留旧算法，之后这家所有断言都解不开。落点 gsb/batch50-go-safety-a-07 的 saml_encrypted_assertion.go，类型 EncryptedAssertionKeyring，签名保持：NewEncryptedAssertionKeyring(Decrypter, Limits) *EncryptedAssertionKeyring、AddKey(tenantID string, k KeyEntry) (Version, error)、Retire(tenantID, version string) error、Keys(tenantID string) ([]KeyEntry, error)、Current(tenantID string) (Version, error)、DecryptResponse(tenantID string, raw []byte) (Assertion, error)、SetAlgorithmAllowlist([]string) error、Purge(tenantID string) error、Count(tenantID string) int、Snapshot() ([]byte, error)、Restore([]byte) error。私钥只经 Decrypter 注入，Unwrap(KeyEntry, []byte) ([]byte, error) 是唯一出口；响应体是 JSON 信封，字段 type、recipient、algorithm、key_version、depth、ciphertext；Unwrap 返回的明文是 Assertion 的 JSON，含 tenant_id、subject、issued_at、attributes。

要修的地方：租户号或版本号为空报 ErrBlankTenant、ErrBlankVersion；同租户版本号重复报 ErrDuplicateKey，不得覆盖已有代；单租户密钥数超过 MaxKeysPerTenant 报 ErrKeyLimit；RSA1_5 一律报 ErrAlgorithmForbidden，即使被加进允许清单也不放行；不在允许清单里的算法报 ErrAlgorithmNotAllowed；信封不是合法 JSON 或 depth 为负报 ErrBadEnvelope；type 不是 assertion 报 ErrUnsupportedType；depth 超过 MaxNestedDepth 报 ErrNestedTooDeep；key_version 指向租户没见过的代报 ErrUnknownKey，且不能拿别的代去试；退役代上新到的响应报 ErrKeyRetired，Current 要落到最新未退役的代，Keys 按年代从旧到新把退役墓碑一起列出；重复退役幂等成功，不能多出一条墓碑；Unwrap 失败一律折成 ErrDecryptFailed，不能把“填充错误”“密钥不匹配”这类原因透给调用方；解密后明文超过 MaxPayloadBytes 报 ErrPayloadTooLarge，先量长度再解析；明文里的 tenant_id 与请求租户不一致报 ErrTenantMismatch；Purge 只清指定租户的代，别的租户不受影响，Count 把退役代一起算进去。

还有三条边界要点名：信封的 recipient 与密钥登记的对端实体不一致时，即使私钥能把这段密文解开也必须拒绝，不能拿解密成功反推归属；同一次轮换里两代密钥都能解开同一段密文时，按断言的签发时间决定用哪一代，不能按尝试顺序碰运气；退役密钥只用于校验已经缓存的断言，新到达的响应一律不走退役代。

硬约束两条：新增代与退役必须原子，读侧要么看到完整旧一代要么看到完整新一代，不能看到半更新；新增代与读取清单并发时，读到的每一条记录版本号都非空，同一版本不会出现两次，计数与实际入库数一致。单条断言的解密并发与明文大小都有上限，超限按拒绝处理而不是无限堆积。重启后密钥代际、退役墓碑与允许清单要保持一致，快照损坏时拒绝恢复并保留现网。

验收：go test ./... 15 条全过，不得改测试、跳过、按固定输入特判；并发换代与重启恢复自行验证；只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-a-07`
- 实现文件：`saml_encrypted_assertion.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`EncryptedAssertionKeyring`
