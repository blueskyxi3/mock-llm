# Mock LLM：独立模拟模型服务

用于 AI 网关、流式连接、RAG / Agent 链路及账务测试。独立 Go module，无 MySQL、Redis 或 AI Hub 包依赖，不调用真实供应商，不执行外部工具。

它模拟接口和负载行为，**没有模型推理能力**。答案、工具参数来自可配置夹具；向量为确定性伪随机向量。模拟测试可以定位网关和账务瓶颈，不能证明供应商限流、真实生成速度或语义质量，也不能直接承诺生产最大并发。

## 启动

在仓库根目录执行（Go 1.25+）：

```sh
go run ./cmd/mock-llm --config config/default.json
```

默认监听 `:8080`，不设密钥时可本地直接访问。设置密钥后，聊天、向量和模型列表要求 `Authorization: Bearer <key>`。密钥建议通过环境变量设置，不提交到配置文件：

```sh
export MOCK_LLM_API_KEY='local-test-only'
export MOCK_LLM_LISTEN='127.0.0.1:8080'
go run ./cmd/mock-llm --config config/default.json
```

Docker（从项目根目录）：

```sh
docker build -t ai-hub/mock-llm:0.1.0 .
docker compose -f compose.yaml up -d
# host: http://127.0.0.1:18080
curl http://127.0.0.1:18080/readyz
docker compose -f compose.yaml down
```

Compose 默认 1 CPU / 256 MiB、只读文件系统、非 Root、仅绑定回环端口，关闭时等待35秒。镜像内置 `--healthcheck <URL>`；默认检查8080的 `/healthz`。修改容器监听端口时，也应调整健康检查。Compose 不设置 `MOCK_LLM_LISTEN`，因此容器内监听所有接口，映射端口仅对本机开放。

支持多架构构建，不自动推送：

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  -t ai-hub/mock-llm:0.1.0 --output type=oci,dest=/tmp/mock-llm.oci.tar .
```

## API 与兼容范围

| 路径 | 方法 | 返回 |
|---|---|---|
| `/v1/chat/completions` | POST | `chat.completion` 或 SSE `chat.completion.chunk` |
| `/v1/embeddings` | POST | 单条／批量向量，float 或 base64 |
| `/v1/models` | GET | 配置中的模型列表 |
| `/healthz` | GET | 进程存活 |
| `/readyz` | GET | 配置有效、服务未进入退出阶段 |
| `/metrics` | GET | Prometheus 文本指标 |

聊天支持文本 messages（system / developer / user / assistant / tool）、文本内容数组、`stream`、`stream_options.include_usage`、两种输出长度参数、`n=1`、function tools、`tool_choice`、`parallel_tool_calls`、`response_format=json_object`。两种输出长度参数同时提供时必须相等。

`temperature`、`top_p`、penalty、seed等常见采样参数可以携带，但不会改变确定性响应；决定结果的种子位于服务配置。图片／音频、Responses API、旧 `functions/function_call`、logprobs及任意JSON Schema生成不支持，显式返回错误。第一版不是全部 OpenAI API 的替代实现。未知字段不参与生成，README列出的兼容子集才有行为保证。

未知模型返回404，无效参数400，超大请求体413，活动请求满429，退出阶段503。错误使用 `error.message/type/param/code`。服务生成或沿用 `X-Request-ID`，响应ID由其摘要生成；指标不使用该ID作为标签。

## 预置场景

| 模型 | 场景 | 默认目标总时长 | 场景输出tokens |
|---|---|---:|---:|
| `mock-chat-short` | 短对话 | 1秒 | 128 |
| `mock-chat-long` | 长回答 | 10秒 | 2048 |
| `mock-stream-10s` | 长连接 | 10秒 | 512 |
| `mock-stream-30s` | 长连接 | 30秒 | 512 |
| `mock-stream-60s` | 长连接 | 60秒 | 512 |
| `mock-rag` | 引用两篇示例文档的回答 | 1秒 | 128 |
| `mock-agent` | 检索／计算工具调用及最终回答 | 1秒 | 128 |
| `mock-embedding` | 1536维向量 | 0秒 | 不适用 |

**全部聊天模型既支持普通返回，也支持流式返回**，由请求 `stream` 决定。默认请求输出预算256，因此长回答模型若未提供更大的 `max_completion_tokens`，会受到256的限制。

普通调用：

```sh
curl http://127.0.0.1:18080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"mock-chat-short","messages":[{"role":"user","content":"你好，请介绍 AI 网关。"}]}'
```

流式调用：

```sh
curl -N http://127.0.0.1:18080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"mock-stream-30s","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"请解释账务流程。"}],"max_completion_tokens":512}'
```

设置密钥后为上述curl增加 `-H "Authorization: Bearer $MOCK_LLM_API_KEY"`。

`fixtures/`提供中英混合短输入、多轮会话和RAG检索片段请求，可从项目根目录直接运行：

```sh
curl -N http://127.0.0.1:18080/v1/chat/completions \
  -H 'Content-Type: application/json' --data-binary @fixtures/multi-turn.json
```

长文档由`examples/client.py long`生成，中英混合文档重复1000次；长输出、JSON回答与工具参数夹具保存在场景配置中。

## 配置与计量

完整默认配置见 `config/default.json`，也可生成：

```sh
go run ./cmd/mock-llm --dump-default-config
```

配置文件覆盖默认配置中的相应字段；`models`数组若提供则替换整个模型列表，每个模型应给全字段。模型名只能包含字母、数字、点、下划线、冒号、横线，长度不超过128。配置在启动时验证，修改后重启；不支持在线热更新。

| 服务字段 | 默认 | 作用 |
|---|---:|---|
| `max_body_bytes` | 16777216 | 请求体最大16 MiB |
| `max_context_tokens` | 131072 | 模拟输入上限，含消息和工具定义 |
| `default_output_tokens` | 256 | 未提供长度限制时的预算 |
| `max_output_tokens` | 8192 | 请求输出预算上限 |
| `max_active` | 4096 | 正在读入／生成请求数量，满后直接拒绝 |
| `max_batch` | 128 | embedding输入批量上限 |
| `request_timeout_seconds` | 120 | 一次请求的执行期限 |
| `shutdown_seconds` | 30 | 停止接收生成请求后，等待已有请求的时间 |

单次请求包括读取输入、生成和输出；HTTP服务同时设置读写期限。活动请求名额不建立等待队列；并发满不改变ready状态。收到退出信号后ready变false，新请求拒绝，已有请求继续；退出期限到达后取消剩余生成并关闭连接。

模型字段：

- `kind`: `chat` / `embedding`；聊天 `scenario`: `chat` / `rag` / `agent`。
- `first_token_ms`: 首段生成等待；`input_ms_per_token`: 输入量附加等待系数。
- 首段等待 = `first_token_ms + 实际估算输入tokens × input_ms_per_token`。`total_ms`表示不小于首段等待的目标总时长；若长输入使首段超过它，则延长总时长。配置和输入组合超过请求期限时明确报错。
- `output_tokens`: 场景自然输出规模；实际普通文本规模为它与请求预算的较小值。预算不足时`finish_reason=length`，自然结束为`stop`。
- `chunk_tokens`: 每次生成／发送的模拟文本token单位数量；工具参数使用该数量的Unicode字符分片。输出按目标时间逐块生成，响应夹具循环延展；不会先构造整个长回答再假装流式。
- `response`: 普通文本夹具；`json_response`: JSON对象夹具；`tool_fixtures`: 函数名到合法参数对象的映射。
- JSON与工具响应须能在预算内保持完整，否则400；不会伪造合法JSON或静默忽略预算。
- `dimensions`: embedding默认维度，允许1～3072；`seed`: 向量和故障选择的固定种子。

### usage

估算规则不是任何供应商tokenizer：中文及其他非空非ASCII字符每字符1，连续ASCII字母数字每4字符向上取整，标点每字符1，空白不计。输入统计包括文本、历史工具参数和当前工具定义；文本内容数组按顺序拼接。嵌入token ID输入按ID个数计量，不解码真实token词表。

标准usage按该估算和实际生成规模计算；固定模式见 `config/fixed-usage.json` 的 `mock-fixed-usage`，返回输入10、输出5、合计15。**固定usage只用于账务确定金额验收，实际输入仍用于上下文限制、延迟和内部指标**。

流式依次返回角色、文本或工具参数增量、结束原因，`include_usage=true`时追加空choices的usage块，再发送`[DONE]`。同一响应的ID、模型及created不变。普通响应的`X-Mock-Duration-Ms`是实际服务耗时；流式完整耗时见结束日志，收到首字不代表响应完成。

### Agent 与 RAG 示例

```sh
MOCK_BASE_URL=http://127.0.0.1:18080/v1 python3 examples/client.py agent
MOCK_BASE_URL=http://127.0.0.1:18080/v1 python3 examples/client.py rag
MOCK_BASE_URL=http://127.0.0.1:18080/v1 python3 examples/client.py long
```

开启密钥时同时设置 `MOCK_API_KEY`。示例使用Python标准库，没有额外依赖。

Agent预置 `search_docs(query)` 和 `calculate(expression)` 参数模板，示例客户端执行本地夹具，服务不执行工具。可以在 `tool_fixtures` 定义其他工具参数，必须与调用方schema一致；服务不实现任意JSON Schema参数生成。`auto`根据最后一条消息是否为tool决定调用工具还是结束；`required`始终要求调用工具，强制函数只调用指定函数，`none`返回文本。默认可以并行返回多个工具，`parallel_tool_calls=false`只返回一个。所有工具结果都必须关联本轮assistant中未回答的tool_call_id，并在下一条非tool消息前提交完毕。

多轮会话来自每次传入的完整messages，服务不保存会话。最终Agent答案仍是夹具，不会推理工具结果。RAG示例执行向量生成、余弦排序、拼接上下文及带引用回答；保留两篇示例文档，明确不评估检索准确率或答案质量。

向量用输入、模型、维度、种子生成并归一化；同一输入在单条／批量及相同配置下结果一致。输入支持字符串、字符串数组、整数token ID数组及二维ID数组，不能混合类型。`base64`为float32小端字节的base64，和float模式等价。

## 故障与指标

`config/faults.json`提供独立`mock-fault-*`模型，正常模型保持不注入故障。选择该配置启动：

```sh
go run ./cmd/mock-llm --config config/faults.json
```

`fault.kind`: `429`、`503`、`timeout`、`disconnect`、`stream_error`、`invalid_usage`；`percent`为1～100，`after_chunks`指定流式中断点。流中断和流错误要求`stream=true`，异常usage故意返回负数。故障选择由请求ID、模型和种子摘要决定；固定`X-Request-ID`时可复现，与并发调用顺序无关。不提供请求ID时自动随机生成。

首字前timeout等待请求期限；客户端可以配置更短的超时来验证取消。正常请求不会自动重试或调用真实供应商。

指标包括活动生成请求、按模型／场景／结果分类的请求总数、输入输出计量、首段和总耗时。累计时间和计数可用于均值；逐请求结构化结束日志可用于延迟分位数。`_unknown`代表未解析模型、鉴权失败或并发拒绝，用户任意模型名／请求ID不会进入指标标签。

## 直连基线与 AI Hub 渠道

健康冒烟脚本使用Python标准库，检查就绪、模型列表、普通响应、SSE结束原因／usage／`[DONE]`及64维批量向量；任一检查失败退出码为1，全部通过为0。密钥仅通过环境变量读取，不写入报告：

```sh
# 直连本地模拟服务，约2秒完成
python3 examples/smoke.py --output /tmp/mock-direct-smoke.json

# 经AI Hub检查，令牌通过环境变量设置
export MOCK_API_KEY='<AI Hub测试令牌>'
python3 examples/smoke.py --mode gateway \
  --base-url http://127.0.0.1:3000/v1 --output /tmp/mock-channel-smoke.json
```

管理员的Relay测试令牌可增加`--channel-id <ID>`，利用现有渠道定向机制保证请求经过指定渠道；普通用户令牌不支持定向，使用专属模拟模型验证路由即可。此脚本只验证HTTP与协议健康，不等同于账务排空或最大容量验收。

先直连，避免把模拟服务或发压器饱和误认为AI Hub瓶颈。在同级 AI Hub 仓库根目录执行：

```sh
export MOCK_API_KEY='local-test'
go run ./cmd/llm-perf -config ../mock-llm/examples/perf.local.yaml \
  -mode concurrency -target gateway -model mock-chat-short \
  -concurrency 8 -duration 5s -output /tmp/mock-llm-direct-baseline
```

该配置两类target均指向本地模拟服务；不是调用火山真实接口。输出CSV／JSON和成功率、并发及延迟。这是冒烟基线，**不是最大容量验收**。

AI Hub测试渠道：

1. 在隔离实例中建立启用的测试用户组，并加入测试渠道。
2. 在供应商模型目录登记上述模型，配置非零测试价格（Token单位M，即每百万tokens）；embedding模型的API支持设为embeddings。
3. 建立OpenAI协议渠道，Base URL使用模拟服务地址，例如同Docker网络下的`http://mock-llm:8080`，不要重复添加`/v1`；配置对应模型及测试组。
4. 配置独立测试令牌和足够额度，保持正常Billing Worker与准入检查。
5. 将`MOCK_BASE_URL`改为隔离AI Hub的`http://127.0.0.1:<port>/v1`、`MOCK_API_KEY`改为测试令牌，运行上述示例。
6. 请求结束后检查订单终态、消费日志、Pending与补偿及钱包一致性。HTTP成功不等于账务已完成。

如在既有环境配置，应使用独立测试供应商／渠道，不覆盖原模型目录。不要把容器内`127.0.0.1`当成其他容器的地址。

可选OpenAI SDK验证：

```sh
python3 -m venv /tmp/mock-llm-sdk
/tmp/mock-llm-sdk/bin/pip install openai
MOCK_BASE_URL=http://127.0.0.1:18080/v1 \
  /tmp/mock-llm-sdk/bin/python examples/sdk.py
```

## 开发验证

在本仓库根目录：

```sh
go test -short -race ./...    # 快速协议、故障、取消及并发测试
go test -race ./...           # 另包含真实1/10/30秒时序，总计约41秒
go vet ./...
```

Go测试用可替换等待器加速协议验证，同时保留真实时间测试。AI Hub 仓库的 `go test ./...` 不会自动执行本仓库测试，请分别执行。本阶段不修改连接池和账务逻辑；实际验证记录见 AI Hub 仓库 `.req/billing/` 中的模拟服务交付报告。

接口形状参考官方[Chat Completions](https://developers.openai.com/api/reference/resources/chat)及[Embeddings](https://developers.openai.com/api/reference/resources/embeddings/methods/create)文档；本项目仅实现上文列出的兼容子集。
