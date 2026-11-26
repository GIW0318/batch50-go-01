# 跑测记录

环境：`go1.23.4 linux/amd64`；构建缓存置于可写目录 `/tmp/go-build-uid30949`。

## 验收用例

```bash
GOCACHE=/tmp/go-build-uid30949 go test -count=1 ./...
```

结果：`ok gsb/batch50-go-safety-c-29`、`ok gsb/batch50-go-safety-c-29/tests`；
验收文件中的 15 条用例全部通过，未修改、跳过或写死断言。

## 自验用例（临时文件，验证后已删除）

覆盖内容：

- 异簇回填返回 `ErrDifferentCrash`、最好输入不前进并推进到另一半；
- 同一在途候选重复获取字节相同且不消耗预算，重复回填只成功一次；
- 二进制长度 + CRC32、大端总长度 + CRC、纯长度前缀、头+负载 CRC、SHA-256
  校验尾、十进制定宽/分隔符长度帧的连续重建；
- 长度字段错配按 `ErrBadSample` 拒绝；
- 快照发生在候选回填前：恢复后不把候选写入最好输入，继续候选序列与中断前一致；
- 已收敛作业快照可恢复并继续返回 `ErrConverged`；
- 64 个并发 `Candidate` 获得同一输入；32 个并发同候选回填仅 1 个成功，其余为
  `ErrNoCandidate`。

```bash
GOCACHE=/tmp/go-build-uid30949 go test -race -count=1 -v ./...
```

结果：公开 15 条与上述自验全部 PASS，race detector 无报告
（`ok ... 1.015s`）。

```bash
GOCACHE=/tmp/go-build-uid30949 go vet ./...
```

结果：无输出，退出码 0。

## 最终复测

临时自验文件删除后再次执行公开验收：

```bash
GOCACHE=/tmp/go-build-uid30949 go test -count=1 ./...
GOCACHE=/tmp/go-build-uid30949 go test -race -count=1 ./...
```

两次结果均为 `ok`，后者无数据竞争报告。
