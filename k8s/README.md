# Kubernetes 部署与网关压测接入

从仓库根目录执行。`mock-llm.yaml` 包含 ConfigMap、Deployment 和 ClusterIP Service；保留服务内置的八个模型，不依赖数据库或 Redis。命名空间由 `kubectl -n` 指定，以下使用 `ai-hub-perf`。

## 镜像和密钥

现有 `ai-hub/mock-llm:0.1.0` 是本地镜像标签，不代表远程仓库已有镜像。先构建并推送到集群可访问的仓库，再将 YAML 的 `image` 改为同一地址。以下地址是示例，请替换：

```sh
export MOCK_IMAGE='your-registry.example.com/ai-hub/mock-llm:0.1.0'
docker buildx build --platform linux/amd64,linux/arm64 \
  -f Dockerfile -t "$MOCK_IMAGE" --push .
```

私有仓库如需认证，在 Deployment 的 `spec.template.spec` 中配置该命名空间已有的 `imagePullSecrets`。

创建命名空间及密钥；Deployment 要求存在 `mock-llm-auth` Secret 的 `api-key` 字段。密钥与 AI Hub 测试用户令牌是两种不同凭据：

```sh
kubectl create namespace ai-hub-perf --dry-run=client -o yaml | kubectl apply -f -
export MOCK_LLM_API_KEY="$(openssl rand -hex 32)"
kubectl -n ai-hub-perf create secret generic mock-llm-auth \
  --from-literal=api-key="$MOCK_LLM_API_KEY" \
  --dry-run=client -o yaml | kubectl apply -f -
```

## 部署和冒烟

修改 YAML 中的镜像地址后：

```sh
kubectl apply --dry-run=server -n ai-hub-perf -f k8s/mock-llm.yaml
kubectl apply -n ai-hub-perf -f k8s/mock-llm.yaml
kubectl -n ai-hub-perf rollout status deployment/mock-llm --timeout=180s
kubectl -n ai-hub-perf get pods -l app.kubernetes.io/name=mock-llm
```

本机协议冒烟可以通过端口转发完成。在一个终端保持运行：

```sh
kubectl -n ai-hub-perf port-forward service/mock-llm 18080:8080
```

另一个终端设置创建 Secret 时的密钥，再执行：

```sh
export MOCK_API_KEY='<mock-llm-auth 中的 api-key>'
python3 examples/smoke.py \
  --base-url http://127.0.0.1:18080/v1 --output /tmp/mock-k8s-smoke.json
curl http://127.0.0.1:18080/metrics
```

端口转发仅用于冒烟；正式压测从集群内发压器访问 Service，以免端口转发成为瓶颈。

## AI Hub 渠道

为 OpenAI 兼容协议测试渠道设置：

- Base URL：`http://mock-llm.ai-hub-perf.svc.cluster.local:8080`，不附加 `/v1`。如果集群使用自定义 DNS 域名，请替换 `cluster.local`。
- 渠道密钥：`mock-llm-auth` 中的 `api-key`。
- 模型：`mock-chat-short`、`mock-chat-long`、`mock-stream-10s`、`mock-stream-30s`、`mock-stream-60s`、`mock-rag`、`mock-agent`、`mock-embedding`。
- 独立测试用户组和令牌，并配置模型价格、额度及 embeddings 接口能力，步骤见上一级 [README](../README.md#直连基线与-ai-hub-渠道)。

这个地址用于集群内的网关；集群外网关需要另外配置可达的服务入口。

## 压测参数

默认每个 Pod 请求 1 CPU / 512 MiB，内存上限 1 GiB，不设 CPU limit；实际资源应以直连基线结果调整。若命名空间有 LimitRange，检查是否自动注入了 CPU limit。每副本 `max_active=4096`，超出返回 429；3 副本不代表一定能承载 12288 并发，HTTP 长连接复用可能导致请求分布不均。

先使用集群内发压器直连上述 Service，确认模拟服务吞吐高于目标网关吞吐，再以相同模型、输入、输出预算和并发测试网关。直连使用模拟服务密钥，经网关使用 AI Hub 测试令牌。观察每个 Pod 的 CPU、内存、重启次数、`/metrics` 活动请求和拒绝计数；指标需按 Pod 采集，Service 单次抓取只会命中一个副本。

预置短对话目标时长 1 秒，长回答 10 秒，长连接 30 秒；全部聊天模型支持 `stream=true/false`。长回答应显式设置 `max_completion_tokens=2048`，30 秒流式模型应设置 512，避免默认预算 256 截短输出。吞吐比较应同时记录响应耗时与并发，模拟延迟会占用连接。

若要测试低延迟网关转发开销，可把 `config/default.json` 的完整模型列表复制进 ConfigMap 的 `config.json`，再调整目标模型的 `first_token_ms` / `total_ms`；提供 `models` 数组会替换全部模型，不是追加。压测期间保持副本数和配置固定，不启用自动扩缩容。

调整副本数：

```sh
kubectl -n ai-hub-perf scale deployment/mock-llm --replicas=6
```

下次 apply YAML 会恢复文件中的 `replicas`，要长期保留请同步修改 YAML。服务在启动时加载配置；修改 ConfigMap 或 Secret 后需要重启，并等待全部副本完成更新再压测：

```sh
kubectl apply -n ai-hub-perf -f k8s/mock-llm.yaml
kubectl -n ai-hub-perf rollout restart deployment/mock-llm
kubectl -n ai-hub-perf rollout status deployment/mock-llm --timeout=300s
```

退出等待配置为 120 秒，Pod 退出宽限为 135 秒，用于等待已接收请求完成；压测期间避免滚动更新。ConfigMap 启动加载与探针配置参考 Kubernetes 官方 [ConfigMap](https://kubernetes.io/docs/concepts/configuration/configmap/) 和 [健康检查](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/) 文档。
