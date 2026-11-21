# llmchat

<!-- CI 徽章指向实际仓库。workflow 在 .github/workflows/ci.yml，
     触发分支同时接受 main 和 master。 -->
[![CI](https://github.com/dylinpym/llmchat/actions/workflows/ci.yml/badge.svg)](https://github.com/dylinpym/llmchat/actions/workflows/ci.yml)

一个聊天后端，围绕 LLM 应用真正的难点构建：**对话历史是无限的，上下文窗口不是。**

大多数聊天 demo 把消息存成一个列表、每次发最近 N 条。这个做法在三个地方会崩，本项目就是为了把这三件事都处理对：

1. **编辑某条消息**会使它之后的所有内容失效。列表表达不了这种结构，所以历史存成**树**，可见对话是其中一条从根到叶的路径。
2. **按 token 数截断可能正好切在 `assistant` 的 tool-call 和它的 `tool` 结果之间。** 服务商会直接以 400 拒绝整个请求。
3. **客户端关掉标签页必须停止生成。** 否则你在为没人会读的 token 持续付费。

## 技术栈

PostgreSQL（递归 CTE） · pgx · chi · OpenAI 兼容的服务商抽象层 · Server-Sent Events · Prometheus + Grafana · goose 迁移 · Kubernetes

刻意保持轻框架：`internal/` 的领域层不 import 任何 HTTP 或存储包，所以分支与打包逻辑无需数据库、无需 API key 就能单测。

## 快速开始

不需要 API key —— mock 服务商流式输出一段固定回复，整条链路可离线跑通。

```bash
cp .env.example .env
docker compose up -d postgres redis
go run ./cmd/server
```

迁移用 `make migrate-up` 执行（goose，版本记录在 `goose_db_version` 表里），用 `make status` 查看。**不要**把迁移文件直接管进 `psql`：`-- +goose Down` 段对 psql 来说只是普通 SQL 注释，所以那些 drop 语句也会被执行，最后你会得到一个空库。如果一定要手动执行，只把 Up 段喂给 psql：
`awk '/\+goose Up/{f=1;next} /\+goose Down/{f=0} f' migrations/001_init.sql | psql "$DATABASE_URL"`。
（这个坑是在 CI workflow 上线前发现的。）

然后：

```bash
# 创建一个会话
curl -sX POST localhost:8080/api/v1/conversations \
  -H 'X-User-Id: demo' -H 'Content-Type: application/json' \
  -d '{"title":"Demo","system_prompt":"Be concise."}'

# 流式获取回复（SSE）
curl -N -X POST localhost:8080/api/v1/conversations/<id>/messages \
  -H 'X-User-Id: demo' -H 'Content-Type: application/json' \
  -d '{"content":"Hello"}'
```

接真实模型时，设置 `LLM_PROVIDER=openai` 以及 `OPENAI_BASE_URL`、`OPENAI_API_KEY`。任何 OpenAI 兼容网关都能用 —— 只需改 base URL 和 key。

## 目录结构

```
cmd/server/            装配、优雅关闭
internal/domain/       消息树、分支视图 —— 无外部依赖
internal/contextpack/  历史 -> 在 token 预算内构造请求  ← 核心
internal/provider/     服务商接口 + OpenAI 兼容实现 + mock
internal/service/      编排、流式回复的持久化
internal/store/        仓储接口 + Postgres 实现
internal/httpin/       chi 路由、SSE、中间件
internal/observability/ Prometheus 指标
migrations/            goose SQL
db/query/              sqlc 源查询（递归 CTE 在这里）
```

## 设计决策

### 历史是树，不是列表

`messages.parent_id` 加上 `conversations.current_leaf`。编辑中间某条消息并重新生成，会挂上一个**兄弟**子树；原来的回答依然可达。切换分支只是重指一个字段，因此是一次带索引的 `UPDATE`，而不是重写。

取出可见对话是从叶到根的递归 CTE（[`db/query/queries.sql`](db/query/queries.sql)），一次往返完成。另一种做法——把全部消息读出来在内存里重建树——会随着用户每一次编辑变得越来越慢；而"逐个取父节点"的版本则是 N 次串行查询。

`is_active_leaf` 做了反范式化，并配一个**部分唯一索引**，这样两个标签页同时发消息时不可能同时成为活跃叶、悄悄把可见历史分叉。

### 三种压缩策略，一条不变式

[`internal/contextpack`](internal/contextpack/packer.go) 保留最近能放下的若干轮（截断）、把中间部分折叠成 LLM 写的摘要（摘要）、或两者都做（混合）。三者都满足同一条规则：

> 保留下来的对话永远是一段**连续的后缀**，且 exchange 对齐只施加一次。

早期版本把一段"钉住"的开场对话拼接在一个独立挑选的尾部窗口前面。这产生了一个中间有洞的窗口，而正是这个洞让 tool 结果变成孤儿、从而触发 400。现在"钉住"改为把开场对话引用进摘要器的 prompt —— 召回效果相同，结构上不违规。

system prompt 与历史争用同一个窗口的预算；如果它自身就超了，打包会返回错误而不是悄悄截断它：截断 system prompt 会改变助手的身份。

### 取消是传播的，不是轮询的

客户端一断开，`net/http` 就会取消 `r.Context()`。这个 context 传到服务商层，关闭响应体、停止读取 —— 于是生成停止，计费也停止。

转发循环在**接收侧**就对 `<-ctx.Done()` 做 select，而不只是发送侧。`for c := range in { select { case out <- c: ... } }` 看起来等价，但是错的：`range` 会无视 context 继续从 channel 取数，而带缓冲的发送也总能成功，所以那个取消分支永远不会被选中。它能通过一个缓冲区溢出型的测试，在生产里却什么都不做。

断开之前已经生成的内容仍会被持久化，用的是 `context.WithoutCancel` —— 读者已经走了，但之后再来访问的人应该看到那半截回复，而不是一个空洞。

### 存活探针不能查数据库

`/healthz` 只报告进程自身健康；`/readyz` 才检查 Postgres。如果存活探针依赖数据库，一次 30 秒的主备切换会让所有 pod 同时判为不健康，kubelet 把它们全部杀掉，而这场重启风暴又会砸向一个本已吃力的数据库。就绪探针只是把 pod 从 Service 端点里摘掉，因此能自行恢复。

优雅关闭会等待在途的流，且 `terminationGracePeriodSeconds`（180s）长于 `SHUTDOWN_GRACE`（15s）；否则 kubelet 会在流中途 SIGKILL，于是每次发布用户都会看到被截断的回复。

### 刻意不做的事

- **不用 `middleware.Timeout`。** 它会包装 `ResponseWriter` 从而破坏 SSE，而且硬性上限会杀掉合法的长生成。
- **流式请求不设 `http.Client.Timeout`。** 那会切断一条健康的多分钟长流；只设了 `ResponseHeaderTimeout`。
- **不用 `viper`。** 十来个配置项全部来自环境变量；结构体 tag 可审计，`kubectl set env` 直接生效。
- **`requireUser` 是占位实现。** 认证是刻意不实现，而不是做一半，免得有人把它当成已完成的安全工作。
- **清单里没有 CPU limit**；CPU 限流会给单 token 延迟带来抖动。内存仍然限制，因为它不可压缩。

## 测试

```bash
go test -race ./...                                          # 自包含；DB 测试自动跳过
DATABASE_URL=postgres://… go test -race ./internal/store/    # 加上 Postgres 集成测试
go test -cover ./internal/contextpack/
```

`internal/contextpack` 语句覆盖率 **94.7%**，race 干净。核心用例构造一段以 `assistant(tool_calls) → tool → tool` 结尾的对话，在三种预算、三种策略下断言：任何 tool 结果都不会脱离它的 assistant 头被保留，且窗口永远不会**以** tool 消息**开头**。

有两个测试套件专门守着本项目**真实踩过**的 bug，而且两者都验证过**对着坏代码会失败**，而不只是对着修好的代码通过：

- `internal/service/stream_cancel_test.go` —— 用一个「断开时不关闭 channel、只是变哑」的服务商驱动 `persistStream`。这是唯一能区分修好的接收侧 select 和错误的 `for c := range in` 的条件，因为一个「一取消就关 channel」的 fake 会让坏循环也正确退出。
- `internal/store/race_integration_test.go` —— 对着真实 Postgres 并发执行 append 和分支切换，断言最终恰好剩一个活跃叶。通过 `DATABASE_URL` 开关控制，所以默认跑测试仍然自包含。

两者共同体现的教训：**先确认测试对着 bug 会失败，再相信它能守住这个 bug。**

CI（`.github/workflows/ci.yml`）跑三个 job：lint（gofmt、vet、build、golangci-lint）、一个**不带任何数据库**的自包含 `go test -race ./...`（以此证明单测不需要外部状态），以及一个集成 job —— 起 `postgres:17` service、应用 schema、带 `-race` 跑 store 测试。建表这步用 `awk` 只把 goose 的 **Up** 段喂给 psql —— 第一版直接管整个文件，结果 `+goose Down` 里的 DROP 把它刚建好的表全删了；这个是在提交前本地跑一遍 CI 步骤时发现的。

对着真实 PostgreSQL 17、一个可注入延迟的慢速 SSE 上游、以及一个真实 OpenAI 兼容网关（`chat.ecnu.edu.cn`，`ecnu-plus`/`ecnu-max`）验证过：

| 行为 | 结果 |
|---|---|
| 多轮对话记录 | 7/7 条消息串在同一条分支上 |
| 分叉 + 切换分支 | 两个兄弟叶，各自给出自己的路径 |
| 跨会话切换叶 | 被拒绝，404 |
| 流中途客户端中断 | 上游记录到在第 18 个 chunk 处取消；半截回复已持久化 |
| 中断生成指标 | 中断时递增 |
| 120 token 窗口下的压缩 | 每次请求约 4.5 条消息被摘要，prompt 保持在预算内 |
| 压缩后的存储历史 | 未改变（17 行） |
| token 预算配置错误 | 拒绝启动 |
| 真实流式 + usage 记账 | `prompt=33 completion=23 total=56`；存储的 `TokenCount` 与网关完全一致 |
| 把 tool-call 邻接关系回放给真实模型 | 模型答出 `24×7=168`，证明它收到的是 tool 结果而非一对孤儿 |

### 值得留档的兼容性发现

去探测真实网关、而不是相信文档，得出三个改变了代码的结论：

- **tool call 是嵌套结构**，形如 `tool_calls[].function.{name,arguments}`。领域类型保持扁平，所以转换发生在服务商边界。把嵌套结构解码进扁平 struct 不会报错 —— 它会悄悄得到空 name，于是回放的 `assistant.tool_calls` 是非法的，正好产生打包器本来要避免的那个 400。
- **usage 帧的 `choices` 是空数组。** `stream_options.include_usage` 会把 usage 放在单独的最后一块、`choices: []`，所以取内容的代码必须先判长度，否则索引越界 panic。
- **文档里写的模型未必可调用。** `ecnu-decision` 在文档里列着，但返回 `401 获取第三方元数据失败`；旧别名也只有一部分能解析（`ecnu-turbo` 可以，`ecnu-chat`、`gpt-4o-mini` 不行）。另外 `response.model` 报的是后端名（`qwen3.8-flash`）而不是你请求的别名 —— 所以固定你被授权使用的别名，绝不要固定后端名。

## 性能

本地单节点（Go 1.27，Docker 里的 Postgres 17，连接池上限 `MaxConns=10`，mock 服务商，因此测出的是后端自身的开销而非模型的开销）。压测工具：[`hey`](https://github.com/rakyll/hey) 用于单 URL 负载，`cmd/loadtest`（本仓库）用于跨多会话分散负载 —— 因为 hey 只能打一个固定 URL。

**读路径** —— `GET /conversations/{id}/messages`，一条 41 条消息的分支，由递归 CTE 提供。并发扫描，每组 1000 请求：

| 并发 | RPS | P50 | P99 | 错误 |
|---|---|---|---|---|
| 1 | 63 | 14 ms | 54 ms | 0 |
| 5 | 330 | 11 ms | 96 ms | 0 |
| 10 | 455 | 17 ms | 110 ms | 0 |
| **20** | **581** | 27 ms | 176 ms | 0 |
| 50 | 453 | 76 ms | 632 ms | 0 |

吞吐在并发 20 处见顶，之后回落、延迟陡增。原因不是查询，而是连接池：负载中途采样，`llmchat_db_connections{in_use}` 顶在 `max_open=10` 的上限上，而 50 个 handler 在抢，于是请求在排队等连接。单个请求是 14 ms，说明 CTE 本身很快。解法是加容量（`MaxConns` 或 PgBouncer），不是优化查询 —— 这正是饱和指标的价值。

**写路径** —— `POST` 一条消息：两个事务（写入用户消息，再写入助手回复）+ mock 流。真实负载下 —— 50 个 worker 分散在 200 个不同会话 —— 稳定在 **136 msg/s，P50 154 ms，P99 258 ms，0 错误**。

### 压测挖出的一个竞态

单会话写压测暴露了一个真实的正确性 bug：在「先清后插」的写入路径下，对同一会话的并发 append 会与部分唯一索引发生竞争。在 `READ COMMITTED` 下，两个写者都在对方提交前清掉了活跃叶、又都插入了一条新的活跃行，于是其中一个撞上 `23505` 唯一性冲突，返回 **HTTP 500**。在单会话 50 并发下，**74.5% 的请求失败**（298/400）。

索引是在正确地**检测**违规；只是写入路径没有**串行化**，所以它把冲突暴露出来、而不是避免它。修复方式是在 append 和分支切换事务的开头，对父会话行加 `SELECT … FOR UPDATE`。这让同一会话的写者排队，而不同会话之间完全不竞争，同时保留唯一索引作为防御性的最后一道线 —— 现在它应该永远不会触发。

| 场景 | 修复前 | 修复后 |
|---|---|---|
| 单会话，50 并发 | 82 req/s 但 **74.5% 是 500** | 36–106 req/s，**0 错误** |
| 分散在 200 个会话，50 并发 | 203 req/s，0 错误 | 136 req/s，**0 错误** |

这个取舍如实说明：锁让分散负载的吞吐下降（约 203→136），因为每次 append 现在都要拿一次行锁，换来的是把 74.5% 的失败率变成零。对聊天后端来说正确性优先；如果吞吐更重要，下一步应该是「每会话一条队列」或乐观重试。`TestConcurrentAppendKeepsOneActiveLeaf`（在 `internal/store`，由 `DATABASE_URL` 开关控制）会并发发 25 次 append，一旦竞态回归就失败；已验证它对着未串行化的代码会失败。

## 后续计划

- 用 sqlc 生成的代码替换手写仓储（`sqlc generate`；`db/query` 里的查询已经为此写好了）
- 接上 Redis 的请求/token 配额限流 —— 成本随 token 数而非请求数增长，所以按请求限流是错的轴
- 用真实认证替换 `requireUser`
- 摘要的召回评测（replay 式）：在长对话里埋入事实，衡量每种策略之后还剩下什么
- 流式 tool-call 参数需要在 SSE chunk 之间做增量解析
- 如果写吞吐比行锁约 30% 的代价更重要，用每会话队列或乐观重试替换 `FOR UPDATE` 串行化
- 针对同一套 API 的 `bubbletea` TUI 客户端

## 许可证

[MIT](LICENSE) © Dylin

## 附：push 前的密钥检查

```powershell
pwsh -File scripts/pre-push-check.ps1
```

在 `git push` 之前跑一次。它检查的是 **git 暂存区里真正会提交的内容**（不是工作区），做四件事：

1. 检出禁止提交的文件（`.env`、`*.pem`、`*.key`、`id_rsa`、`credentials*` 等），`git add -f` 强制绕过 `.gitignore` 也能抓到；
2. 在待提交的文本里扫密钥形态 —— `sk-` 开头的 key、`AKIA`、`ghp_`、`xox[baprs]-`、私钥块、以及 `KEY|SECRET|TOKEN|PASSWORD = <长串>` 的赋值形式；
3. 标记超过 2 MB 的文件和构建产物；
4. 如果当前目录其实嵌套在别的仓库里，直接报错拒绝 —— 否则脚本会去检查那个上级仓库，给出对你毫无意义的「通过」。

退出码 0 表示可以 push，1 表示别 push。匹配到的密钥只回显前 8 位并标注「已遮蔽」，不会把完整密钥打进终端历史。

写这个脚本时测过一个关键用例：用项目真实的 `.env` 模拟泄漏，确认它能报出来，**同时确认完整 key 没有出现在输出里**。一个从不报警的检查脚本比没有脚本更危险 —— 它给你的是假的安全感。

