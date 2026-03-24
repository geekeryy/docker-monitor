# docker-monitor

`docker-monitor` 是一个用 Go 编写的 Docker 日志监控程序，用来持续监听一个或多个 Docker Host 上的容器日志，筛选出告警日志，按 `log_id` 聚合后写入本地文件，并按需推送到钉钉机器人。

它适合下面这类场景：

- 你有一批容器服务，想统一抓取 `WARN` / `ERROR` 日志。
- 日志里带有 `log_id`、`trace_id`、`request_id` 之类的关联标识，希望按一次问题聚合。
- 既想保留结构化归档文件，又希望关键告警能同步通知到钉钉。
- Docker 可能跑在本机，也可能分布在多个远程主机上，用于对开发测试环境做日志监控特别方便。

## 核心能力

- 监听 Docker 容器启动、停止、重启事件，自动接入或停止日志流。
- 支持本地和多主机 Docker 监控。
- 支持 `unix`、`tcp`、`http`、`https`、`ssh` 等 Docker Host 连接方式。
- 支持文本日志和 JSON 日志。
- 可根据关键字识别告警级别，例如 `WARN`、`ERROR`。
- 可从 JSON 字段或正则中提取 `log_id` / `trace_id` / `alert_id`。
- 按 `log_id` 聚合多条相关日志，减少重复告警。
- 聚合结果默认落盘为 `jsonl` 文件。
- 可选推送钉钉机器人，且仅对指定级别执行 `@` 提醒。
- 钉钉发送失败不会影响本地文件落盘。
- 既可以部署在本地，监控多个远程主机的 Docker Host，也可以部署到远程主机，将远程主机的告警通知到钉钉。

## 工作方式

程序启动后会先列出当前 Docker Host 上已运行的容器，再订阅 Docker 事件流：

1. 只处理容器名匹配 `docker.include_patterns` 的容器。
2. 对每一行日志进行解析。
3. 如果是 JSON 日志，会优先从 JSON 字段里提取时间、消息、级别、`log_id`。
4. 如果不是 JSON，或 JSON 中没有识别出告警级别/`log_id`，会继续用纯文本关键字和正则补充识别。
5. 识别出的事件按 `log_id` 聚合。
6. 聚合结果写入本地文件，并可同步推送钉钉。

几个重要行为说明：

- 只有“命中告警”或“成功提取到 `log_id`”的日志才会进入后续处理。
- 同一个 `log_id` 的日志会累计到同一个批次中。
- 当一个批次中已经出现过告警，且累计数量达到 `aggregation.flush_size` 时，会立即落盘。
- 每隔 `aggregation.flush_interval` 会把当前有告警的批次统一刷盘。
- 如果一条告警日志没有提取到 `log_id`，会使用 `aggregation.unknown_log_id`，并单条立即输出，避免混淆。
- 监控多个 Docker Host 时，容器名会自动加上主机前缀，例如 `coach_test/api`，方便区分来源。

## 项目结构

```text
.
├── cmd/monitor            # 程序入口
├── configs/config.yaml    # 示例配置
├── internal/config        # 配置加载与校验
├── internal/docker        # Docker API 访问、事件监听、日志流处理
├── internal/parser        # 告警日志识别与 log_id 提取
├── internal/aggregator    # 日志聚合与刷盘时机控制
└── internal/store         # 文件输出、钉钉输出、多路写入
```

## 环境要求

- Go `1.25.5`
- Docker Engine 可访问
- 如果使用 `ssh://` 连接远程 Docker，需要本机存在 `ssh` 命令，并配置免密登录

## 快速体验

### 1. 运行监控程序

```bash
docker run -d --rm  -v /var/run/docker.sock:/var/run/docker.sock  -v "$(pwd)/data:/app/data" --name monitor docker.io/geekeryy/docker-monitor:latest
```

### 2. 运行测试容器

```bash
docker run -it --rm --name monitor_test alpine:latest
```

在测试容器内执行以下命令：

```bash
echo "id=1 WARN test1"
echo "id=1 WARN test2"
echo "id=2 ERROR test1"
echo "id=2 ERROR test2"

```

## 开始使用

### 1. 准备配置

默认配置文件路径是 `configs/config.yaml`。

可以先直接使用仓库里的示例配置，再按自己的容器名和日志格式调整。

### 2. 本地运行

```bash
go run ./cmd/monitor -f configs/config.yaml
```

### 3. 编译运行

```bash
go build -o bin/monitor ./cmd/monitor
./bin/monitor -f configs/config.yaml
```

### 4. 运行测试

```bash
go test ./...
```

## Docker 运行

项目内已经提供 `Dockerfile`，默认会把二进制放到镜像里的 `/usr/local/bin/monitor`，并使用 `/app/configs/config.yaml` 作为配置文件。

示例：

```bash
docker build -t docker-monitor:local .

docker run --rm \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$(pwd)/configs:/app/configs" \
  -v "$(pwd)/data:/app/data" \
  docker-monitor:local
```

如果你需要推送钉钉，确保容器所在环境可以访问公网。

## 配置说明

完整示例：

```yaml
docker:
  hosts:
    - "unix:///var/run/docker.sock"
    - "ssh://user@prod-a"
  include_patterns:
    - "api-*"
    - "worker-*"
  since: "30s"

filters:
  warn_match:
    keywords:
      - "WARN"
      - "ERROR"
    json_fields:
      - "level"
      - "severity"
    message_fields:
      - "message"
      - "msg"
      - "log"
    time_fields:
      - "time"
      - "timestamp"
      - "@timestamp"
      - "ts"
  log_id_extract:
    json_keys:
      - "log_id"
      - "trace_id"
      - "request_id"
    regexps:
      - '(?i)\blog[_-]?id["''=:\s]+([A-Za-z0-9._:-]+)'
      - '(?i)\btrace[_-]?id["''=:\s]+([A-Za-z0-9._:-]+)'
      - '(?i)\brequest[_-]?id["''=:\s]+([A-Za-z0-9._:-]+)'

aggregation:
  flush_size: 20
  flush_interval: "10s"
  unknown_log_id: "unknown"

dingtalk:
  webhook_url: ""
  secret: ""
  mention_levels:
    - "ERROR"
  at_mobiles:
    - "13800000000"
  at_all: false

storage:
  output_dir: "data"
```

### `docker`

- `hosts`: 要监听的 Docker Host 列表。
- `host`: 单主机配置，兼容旧写法；会与 `hosts` 合并去重。
- `include_patterns`: 容器名匹配规则，使用 Go `filepath.Match` 风格通配符。
- `since`: 启动时回看历史日志时长，例如 `30s`、`5m`、`1h`。

支持的 Docker Host 示例：

- 本地默认 Docker：留空，或使用 `unix:///var/run/docker.sock`
- 本地显式指定：`unix:///var/run/docker.sock`
- 远程 TCP：`tcp://127.0.0.1:2375`
- HTTP API：`http://docker-host:2375`
- HTTPS API：`https://docker-host:2376`
- SSH：`ssh://user@prod-a`
- SSH 指定远程 Docker Socket：`ssh://user@prod-a/var/run/docker.sock`

### `filters.warn_match`

用于识别“这条日志是不是告警”。

- `keywords`: 文本或级别关键字，默认包含 `WARN`、`ERROR`
- `json_fields`: JSON 中用于表示级别的字段名
- `message_fields`: JSON 中优先作为消息体展示的字段名
- `time_fields`: JSON 中优先作为时间戳读取的字段名

如果日志是这样的：

```json
{
  "ts": "2026-03-24T10:00:00Z",
  "level": "ERROR",
  "message": "create order failed",
  "trace_id": "abc-123"
}
```

程序会识别出：

- 时间：`ts`
- 级别：`ERROR`
- 消息：`message`
- 关联标识：`trace_id`

### `filters.log_id_extract`

用于提取聚合键。

- `json_keys`: JSON 中优先读取的字段名
- `regexps`: 纯文本或兜底场景下使用的正则规则

默认配置已经覆盖常见字段：

- `log_id`
- `logId`
- `alert_id`
- `alertId`
- `trace_id`
- `traceId`

### `aggregation`

- `flush_size`: 单个 `log_id` 聚合到多少条后立即输出
- `flush_interval`: 定时刷盘间隔
- `unknown_log_id`: 告警日志未提取到标识时使用的占位值

建议：

- 日志量大、重复告警多时，增大 `flush_size`
- 希望更快看到聚合结果时，减小 `flush_interval`

### `dingtalk`

- `webhook_url`: 钉钉机器人 Webhook，留空表示禁用
- `secret`: 钉钉加签密钥，可选
- `mention_levels`: 哪些级别触发 `@` 提醒
- `at_mobiles`: 要提醒的手机号列表
- `at_all`: 是否 `@所有人`

行为说明：

- 每个批次都会先发送一条 Markdown 摘要。
- 只有批次内存在 `mention_levels` 指定的级别时，才会发送第二条文本消息用于 `@`。
- 钉钉失败只会记日志警告，不会中断主流程，也不会影响文件落盘。

### `storage`

- `output_dir`: 本地输出目录

输出按月份分目录、按天写入 `jsonl` 文件，例如：

```text
data/
└── 2026-03/
    └── 2026-03-24.jsonl
```

## 输出格式

每一行是一个完整的 JSON 对象，对应一个聚合批次。

示例：

```json
{
  "log_id": "abc-123",
  "first_seen": "2026-03-24T10:00:00Z",
  "last_seen": "2026-03-24T10:00:05Z",
  "count": 3,
  "containers": ["local/api"],
  "events": [
    {
      "timestamp": "2026-03-24T10:00:00Z",
      "container": {
        "id": "container-id",
        "name": "local/api"
      },
      "stream": "stderr",
      "level": "ERROR",
      "log_id": "abc-123",
      "alert_matched": true,
      "message": "create order failed",
      "raw": "{\"level\":\"ERROR\",\"message\":\"create order failed\",\"trace_id\":\"abc-123\"}",
      "parsed_from_json": true
    }
  ],
  "flushed_at": "2026-03-24T10:00:10Z"
}
```

字段说明：

- `log_id`: 聚合键
- `first_seen`: 该批次第一条日志时间
- `last_seen`: 该批次最后一条日志时间
- `count`: 本批次日志总数
- `containers`: 涉及到的容器列表
- `events`: 具体日志事件
- `flushed_at`: 该批次写出时间

## 钉钉消息内容

钉钉 Markdown 消息会包含：

- 标题：`Docker日志告警 <log_id>`
- 多主机场景下的主机标签
- 日志条数
- 关联容器列表
- 首次/最后时间
- 最近最多 5 条日志详情

如果开启 `@`，第二条文本消息会只负责提醒相关人员，不会把手机号直接混进 Markdown 正文。

## 常见配置示例

### 只监听本机 `api-*` 容器

```yaml
docker:
  host: "unix:///var/run/docker.sock"
  include_patterns:
    - "api-*"
```

### 同时监听多台机器

```yaml
docker:
  hosts:
    - "unix:///var/run/docker.sock"
    - "ssh://user@prod-a"
    - "ssh://user@prod-b"
  include_patterns:
    - "api-*"
    - "worker-*"
```

### 只把 `ERROR` 级别告警 `@` 到钉钉

```yaml
dingtalk:
  webhook_url: "https://oapi.dingtalk.com/robot/send?access_token=xxx"
  secret: "SECxxx"
  mention_levels:
    - "ERROR"
  at_mobiles:
    - "13800000000"
  at_all: false
```

## 故障排查

### 没有采集到日志

优先检查：

- Docker Host 是否可访问
- 容器名是否匹配 `docker.include_patterns`
- 日志是否确实输出到容器的 `stdout` / `stderr`
- `docker.since` 是否过小，导致启动时回看窗口不足

### 识别不到 `log_id`

优先检查：

- 日志是否是 JSON，且字段名是否已包含在 `filters.log_id_extract.json_keys`
- 纯文本日志是否能被 `filters.log_id_extract.regexps` 命中
- 你的实际字段可能叫 `request_id`、`traceId`、`biz_no`，需要补到配置里

### 钉钉没有收到消息

优先检查：

- `dingtalk.webhook_url` 是否填写正确
- 如果配置了 `secret`，机器人是否启用了加签
- 运行环境是否可以访问钉钉接口
- 该批次是否真的包含 `mention_levels` 指定的级别

## 镜像发布

`Makefile` 中提供了多架构构建示例：

```bash
make build-image
```

它会使用 `docker buildx` 构建并推送 `linux/amd64` 和 `linux/arm64` 镜像。

## 待办事项

- [ ] 支持更多 Docker Host 连接方式
- [ ] 支持灵活的日志格式和过滤规则
- [ ] 支持自定义输出格式和存储位置
