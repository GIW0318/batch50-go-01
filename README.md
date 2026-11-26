# 认证态爬虫会话维护

## 题面

漏洞扫描的爬虫上线三周，认证态页面几乎没爬进多少：登录后跑十几分钟令牌就过期，爬虫把过期响应当成页面内容存了下来，报告里全是空壳；有一次会话被服务端踢掉，爬虫跟着跳转去了登录页，把登录表单当业务页面提交了三遍，账号被风控锁了半天；更麻烦的是退出登录的链接被当成普通链接跟着点，几轮下来会话自己把自己注销了。值班同学手工重登一次能撑一阵，脚本一跑起来又原样复发。请把认证态会话维护做成可复用的组件，落点 gsb/batch50-go-safety-c-40，实现文件 crawl_session_guard.go，类型 CrawlSessionGuard，签名固定：NewCrawlSessionGuard(limits Limits) *CrawlSessionGuard、SetTransport(t Transport) error、Login(target Target) (Session, error)、Fetch(ctx context.Context, req Req) (Resp, error)、Refresh(sessionID string) error、Fingerprint(resp Resp) string、Snapshot() ([]byte, error)、Restore(data []byte) error，另配 SessionState、Sessions、Close、Stats、Reset 五个辅助方法。

要修的点：一，没有挂上传输层时登录与抓取都报 ErrNoTransport；二，登录目标的名称、站点地址、登录地址、账号四项缺一不可，缺项报 ErrBadTarget 且不建会话，会话数超过 MaxSessions 报 ErrTooManySessions；三，登录响应里的会话 cookie 与隐藏域 csrf_token 都要收下，令牌同时出现在响应头与隐藏域时以隐藏域为准，会话代数从 1 开始；四，每次抓取都要带上当前 cookie 与令牌，会话编号不认识报 ErrNoSession，地址为空报 ErrBadRequest，被拒绝的请求不得发到传输层；五，上游返回 401 表示令牌过期，只刷新令牌后重放一次，重放仍失败报 ErrReplayExhausted；六，上游重定向到登录页说明会话掉了，要重新认证后重放一次；七，同一请求最多重放一次，避免重复副作用，重放次数要计数，Stats 分别记登录、轮换、重放与抓取次数；八，刷新令牌走站点地址下的 token 端点，响应体形如 csrf=新令牌，刷新后会话代数加一；九，并发抓取共享会话时令牌轮换必须串行，同一批请求只允许一次轮换，代数未变才刷新，已经换过就复用新令牌重放，其余请求等新令牌再发，不能各刷一次；十，退出登录链接不得被爬虫跟随，遇到这类地址直接拒绝；十一，表单状态按动作、字段名集合与隐藏域值算指纹，字段顺序与映射遍历顺序不影响结果，指纹为六十四位十六进制；十二，缓存键要带上会话代数与令牌，同一地址在不同会话状态下不得互相命中，关闭会话后其状态一并丢弃，再操作报 ErrNoSession。

硬约束：同一会话的刷新、重放与关闭串行推进，不同会话之间互不阻塞；快照要么完整包含会话、代数与计数，要么整体拒绝损坏载荷，恢复后会话仍能继续抓取；重放预算按请求记账，不得因为并发而多发一次。验收：go test ./... 的 15 条用例全过，测试不得修改、跳过或按固定输入写死；并发轮换与重启恢复自行验证，只做本地实现。

## 运行方式

```bash
go test ./...
```

## 模块与包

- 模块路径：`gsb/batch50-go-safety-c-40`
- 实现文件：`crawl_session_guard.go`
- 包名：`task`；测试目录 `tests/` 同样声明 `package task`，通过导入路径引用本模块
- 主要类型：`CrawlSessionGuard`
