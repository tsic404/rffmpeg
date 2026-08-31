# rffmpeg

**rffmpeg** 是一个分布式 FFmpeg 转码系统，允许将 FFmpeg 转码任务分发到多个 Worker 节点执行，实现负载均衡和资源利用最大化。

## 项目简介

rffmpeg 提供了一个与原生 FFmpeg 命令行兼容的客户端工具，用户可以像使用本地 FFmpeg 一样提交转码任务，任务会被自动分发到集群中的 Worker 节点执行。系统支持：

- **分布式转码**：将转码任务分发到多个 Worker 节点
- **负载均衡**：自动调度任务到空闲 Worker
- **实时日志**：通过 WebSocket 实时获取转码进度
- **断点续传**：支持大文件分块上传和断点续传
- **安全通信**：支持 TLS/HTTPS 和双向 TLS 认证 (mTLS)
- **Worker 健康监控**：自动检测和处理离线 Worker

## 系统架构

```
┌─────────────┐     ┌─────────────────────────────────────┐     ┌─────────────┐
│   CLI       │────▶│              Server                 │◀────│   Worker    │
│  (rffmpeg)  │     │  ┌─────────┐  ┌─────────┐  ┌──────┐│     │  (ffmpeg)   │
└─────────────┘     │  │  API    │  │Scheduler│  │  DB  ││     └─────────────┘
                    │  │ Handler │  │         │  │SQLite││
                    │  └─────────┘  └─────────┘  └──────┘│
                    │  ┌─────────┐  ┌─────────┐          │
                    │  │Storage  │  │WebSocket│          │
                    │  └─────────┘  └─────────┘          │
                    └─────────────────────────────────────┘
```

### 组件说明

| 组件 | 说明 |
|------|------|
| **Server** | 中央协调服务器，管理任务队列、Worker 注册、文件存储和任务调度 |
| **Worker** | 执行节点，运行 FFmpeg 转码任务，定期向 Server 发送心跳 |
| **CLI** | 命令行客户端，兼容原生 FFmpeg 命令参数，将任务提交到 Server |

## 快速开始

### 前置要求

- Go 1.23+
- FFmpeg（Worker 节点需要）

### 编译

```bash
# 克隆仓库
git clone https://github.com/tsip404/rffmpeg.git
cd rffmpeg

# 编译所有组件
go build -o bin/server ./cmd/server
go build -o bin/worker ./cmd/worker
go build -o bin/rffmpeg ./cmd/cli
```

### 启动 Server

```bash
# 使用默认配置启动
./bin/server

# 指定端口和数据目录
./bin/server --port 8080 --data-dir ./data

# 使用配置文件
./bin/server --config server.json
```

### 启动 Worker

**认证前置**：Server 采用 fail-closed 设计，未配置 `--auth-token` 时拒绝全部 API 请求。启动 Worker 前请先 `export RFFMPEG_TOKEN=<T>`（或为命令追加 `--token <T>`），令牌须与 Server 的 `--auth-token <T>` 一致。

```bash
# 连接到本地 Server
./bin/worker --server-url http://localhost:8080

# 通过 JSON 配置文件指定完整配置（名称、编码器、GPU 等）
./bin/worker --config worker.json

# 通过环境变量覆盖配置项（如名称、最大并发）
RFFMPEG_WORKER_NAME=worker-1 RFFMPEG_MAX_CONCURRENT=2 ./bin/worker
```

### 使用 CLI 提交任务

**认证前置**：CLI 提交任务前请先 `export RFFMPEG_TOKEN=<T>`（或为命令追加 `--token <T>`），令牌须与 Server 的 `--auth-token <T>` 一致；未设置凭证时会在上传阶段收到 401 并直接退出。

```bash
# 基本转码（与原生 ffmpeg 命令兼容）
./bin/rffmpeg -i input.mp4 -c:v libx264 -c:a aac output.mp4

# 指定远程 Server
./bin/rffmpeg --server http://your-server:8080 -i video.mkv output.mp4

# 静默模式
./bin/rffmpeg -q -i input.mp4 -vf scale=1280:720 output.mp4

# 指定任务超时时间
./bin/rffmpeg --timeout 30m -i input.mp4 -c:v libx264 output.mp4

# 调整连接中断后的重试次数（默认 14 次，约 5 分钟；也可用环境变量 RFFMPEG_MAX_RETRIES）
./bin/rffmpeg --max-retries 5 -i input.mp4 -c:v libx264 output.mp4
```

**连接中断与重试**：任务提交成功后，若传输中 Server 或 Worker 断连，CLI 会在 WebSocket 与 HTTP 轮询两条路径上重试。重试次数达到上限（`--max-retries` / `RFFMPEG_MAX_RETRIES` / 配置文件 `"max_retries"`，默认 14 次、约 5 分钟）后 CLI 以独立退出码 `2` 结束，并在 stderr 提示作业已提交、可通过 `GET /api/v1/jobs/{id}` 查询最终状态——此时**作业仍在服务端运行**，不是永久卡死，也不同于提交阶段失败（退出码 `1`，作业未创建）。三个通道均支持 `0`：显式设为 `0` 表示**不重试、首次失败即退出**，不会被静默回落为默认值。

## 配置说明

### Server 配置

Server 支持通过配置文件、环境变量和命令行参数三种方式配置，优先级：命令行 > 环境变量 > 配置文件。

#### 配置文件 (JSON)

```json
{
  "port": "8080",
  "data_dir": "./data",
  "version": "1.0.0",
  "worker_heartbeat_timeout": "90s",
  "worker_offline_threshold": "10m",
  "worker_health_check_interval": "30s",
  "job_timeout": "30m",
  "schedule_interval": "5s",
  "timeout_check_interval": "30s",
  "max_jobs_per_worker": 1,
  "auth_token": "",
  "allowed_origins": ["http://localhost:3000"],
  "tls": {
    "enabled": false,
    "cert_file": "",
    "key_file": "",
    "client_ca_file": "",
    "mtls": false,
    "min_version": "TLS1.2",
    "expiration_warning_days": 30
  }
}
```

#### 环境变量

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `PORT` | Server 监听端口 | `8080` |
| `DATA_DIR` | 数据存储目录 | `./data` |
| `VERSION` | Server 版本标识 | `1.0.0` |
| `WORKER_HEARTBEAT_TIMEOUT` | Worker 心跳超时时间 | `90s` |
| `WORKER_OFFLINE_THRESHOLD` | Worker 离线阈值 | `10m` |
| `WORKER_HEALTH_CHECK_INTERVAL` | Worker 健康检查间隔 | `30s` |
| `JOB_TIMEOUT` | 任务执行超时时间 | `30m` |
| `SCHEDULE_INTERVAL` | 任务调度间隔 | `5s` |
| `TIMEOUT_CHECK_INTERVAL` | 超时检查间隔 | `30s` |
| `MAX_JOBS_PER_WORKER` | 每个 Worker 最大并发任务数 | `1` |
| `NO_WORKER_JOB_TIMEOUT` | 无可调度 Worker 时 pending 任务的最长等待时间，超时判失败；`0` 禁用 | `2m` |
| `ALLOWED_ORIGINS` | WebSocket 允许的源（逗号分隔） | - |
| `TLS_ENABLED` | 启用 TLS | `false` |
| `TLS_CERT_FILE` | TLS 证书文件路径 | - |
| `TLS_KEY_FILE` | TLS 私钥文件路径 | - |
| `TLS_CLIENT_CA_FILE` | 客户端 CA 证书路径 (mTLS) | - |
| `RFFMPEG_SERVER_TOKEN` | API 认证令牌 (PSK) | `""` (拒绝全部请求) |

#### 命令行参数

```bash
./bin/server --help
  --port string                        Server port (default: 8080)
  --data-dir string                    Data directory (default: ./data)
  --config string                      Path to configuration file (JSON)
  --worker-heartbeat-timeout string    Timeout before marking worker offline (default: 90s)
  --worker-offline-threshold string    Duration after which offline workers are removed (default: 10m)
  --worker-health-check-interval string Interval for checking worker health (default: 30s)
  --job-timeout string                 Timeout for running jobs (default: 30m)
  --schedule-interval string           Interval for job scheduling (default: 5s)
  --timeout-check-interval string      Interval for checking job timeouts (default: 30s)
  --no-worker-job-timeout string       Fail pending jobs waiting longer than this with no schedulable worker; 0 disables (default: 2m)
  --tls                                Enable TLS (HTTPS)
  --tls-cert string                    Path to TLS certificate file
  --tls-key string                     Path to TLS private key file
  --tls-client-ca string               Path to client CA certificate file (for mTLS)
  --auth-token string                  Authentication token (PSK) for API requests
  --mtls                               Enable mTLS (mutual TLS authentication)
```

#### 认证（重要）

认证采用 **fail closed** 策略：

- **未设置 `--auth-token` / `RFFMPEG_SERVER_TOKEN` 时，Server 拒绝全部 API 请求**（返回 401），包括 worker 注册（`/api/v1/workers/register`）和心跳（`/api/v1/workers/heartbeat`），仅日志输出 `WARNING: No auth token configured. All API requests will be rejected (401). Set --auth-token to enable access.`。
- 因此部署时**必须同时配置 server 与 worker 凭证**：Server 设置 `--auth-token <token>`，Worker 通过配置文件 `"token": "<token>"`、环境变量 `RFFMPEG_TOKEN` 或命令行 `--token <token>` 提供相同令牌；CLI 同样需要设置 `RFFMPEG_TOKEN`。
- **Worker 注册与心跳必须认证**：`/api/v1/workers/register` 与 `/api/v1/workers/heartbeat` 不再豁免鉴权，未携带正确 token 的请求返回 401。这防止未认证攻击者注入"幽灵 worker"并持续心跳保活、诱导调度器把任务派给其控制的节点。
- 例外路径：仅 `/health` 与 `/api/v1/health` 健康检查无需认证（包括 tokenless 部署），便于负载均衡探活。

### Worker 配置

Worker 的配置以 **JSON 配置文件 + 环境变量为主**，命令行仅提供少数覆盖项。优先级：命令行 flag > 环境变量 > 配置文件 > 默认值。

#### 命令行参数

Worker 仅定义以下 3 个 flag（见 `cmd/worker/main.go:25`）：

```bash
./bin/worker --help
Usage of ./bin/worker:
  -config string
    Path to worker config file (JSON)
  -server-url string
    Server URL (overrides config file and RFFMPEG_SERVER_URL env)
  -token string
    Worker authentication token (overrides config file and RFFMPEG_TOKEN env)
```

#### 配置文件 (JSON)

名称、编码器、GPU 等其余配置项均通过 JSON 配置文件（或对应环境变量）设置：

```json
{
  "server_url": "http://localhost:8080",
  "name": "worker-1",
  "token": "<与 Server --auth-token 一致的令牌>",
  "temp_dir": "",
  "ffmpeg_path": "ffmpeg",
  "timeout": "2h",
  "heartbeat_interval": "30s",
  "poll_interval": "5s",
  "max_concurrent": 1,
  "auto_detect_gpu": true,
  "auto_detect_codecs": true,
  "manual_encoders": ["libx264"],
  "manual_decoders": [],
  "manual_gpu_model": "",
  "manual_ffmpeg_version": "",
  "encoder_priority": [],
  "encoder_blacklist": [],
  "cache_enabled": true,
  "cache_dir": "",
  "cache_ttl": "24h",
  "cache_max_size_mb": 10240,
  "retry_max_retries": 3,
  "retry_initial_interval": "1s",
  "retry_use_exponential_backoff": false,
  "retry_max_interval": "30s",
  "retry_enable_software_fallback": true
}
```

#### 环境变量

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `RFFMPEG_CONFIG` | JSON 配置文件路径（即 `-config`） | - |
| `RFFMPEG_SERVER_URL` | Server API URL | `http://localhost:8080` |
| `RFFMPEG_WORKER_NAME` | Worker 名称 | 自动生成 |
| `RFFMPEG_WORKER_ID` | Worker ID | 自动生成 |
| `RFFMPEG_TOKEN` | API 认证令牌（须与 Server 一致） | - |
| `RFFMPEG_TEMP_DIR` | 临时文件目录 | `~/.cache/rffmpeg-worker/<workerID>`（XDG 私有）<br>`$TMPDIR/rffmpeg-worker-<uid>/<workerID>`（root 或 XDG 不可用时的 fallback） |
| `RFFMPEG_FFMPEG_PATH` | FFmpeg 可执行文件路径 | `ffmpeg` |
| `RFFMPEG_TIMEOUT` | 任务执行超时时间 | `2h` |
| `RFFMPEG_MAX_CONCURRENT` | 最大并发任务数 | `1` |
| `RFFMPEG_HEARTBEAT_INTERVAL` | Worker 心跳上报间隔 | `30s` |
| `RFFMPEG_POLL_INTERVAL` | Worker 任务轮询间隔 | `5s` |
| `RFFMPEG_AUTO_DETECT_GPU` | 自动检测 GPU（布尔值：`1`/`true`/`yes`/`on` 启用，`0`/`false`/`no`/`off` 禁用，大小写不敏感；非法值不覆盖配置文件） | `true` |
| `RFFMPEG_AUTO_DETECT_CODECS` | 自动检测编解码器（同上布尔值语义） | `true` |
| `RFFMPEG_CACHE_ENABLED` | 启用任务文件缓存（`false`/`0` 关闭） | `true` |
| `RFFMPEG_CACHE_DIR` | 缓存目录 | 自动（`/var/cache/rffmpeg` 或 `~/.cache/rffmpeg`） |
| `RFFMPEG_CACHE_TTL` | 缓存 TTL | `24h` |
| `RFFMPEG_CACHE_MAX_SIZE_MB` | 缓存最大大小 (MiB) | `10240` |
| `RFFMPEG_RETRY_MAX_RETRIES` | 任务重试次数 | `3` |
| `RFFMPEG_RETRY_INITIAL_INTERVAL` | 重试初始间隔 | `1s` |
| `RFFMPEG_RETRY_EXPONENTIAL_BACKOFF` | 指数退避（`true`/`1` 开启） | `false` |
| `RFFMPEG_RETRY_MAX_INTERVAL` | 重试最大间隔 | `30s` |
| `RFFMPEG_RETRY_ENABLE_SOFTWARE_FALLBACK` | 软件编码回退（`false`/`0` 关闭） | `true` |

编码器/GPU 等能力默认由 Worker 自动探测；如需手动指定，通过配置文件的 `manual_encoders`/`manual_decoders`/`manual_gpu_model`/`manual_ffmpeg_version` 字段（配合 `auto_detect_gpu: false`/`auto_detect_codecs: false`）设置，或使用 `encoder_priority`/`encoder_blacklist` 调整优先级与黑名单。

### CLI 配置

CLI 配置文件搜索顺序（优先级从高到低）：
1. `./rffmpeg.json`
2. `~/.rffmpeg.json`
3. `/etc/rffmpeg.json`

#### 配置文件示例

```json
{
  "server_url": "http://localhost:8080", // /api/v1 suffix is optional
  "token": "your-auth-token",
  "max_retries": 14
}
```

#### 环境变量

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `RFFMPEG_SERVER_URL` | Server URL | `http://localhost:8080` |
| `RFFMPEG_TOKEN` | 认证令牌（server 启用认证时必填） | - |
| `RFFMPEG_MAX_RETRIES` | WS/HTTP 重试次数上限（连接中断后） | `14`（约 5 分钟） |

#### 认证

- Server 未配置 token 时**拒绝一切请求**（401），并在启动日志打印 WARNING；因此 CLI 必须设置与 Server `--auth-token` 一致的令牌：环境变量 `RFFMPEG_TOKEN`、命令行 `-token <token>` 或配置文件 `"token": "<token>"`。
- 未设置凭证时，CLI 会在提交/上传阶段收到 401 错误并直接退出，**不会回退到本地 ffmpeg**。

### 共享文件系统直通模式

当 CLI 和 Worker 位于同一台机器或共享文件系统（NFS、NAS、Kubernetes 共享 PV）上时，可以通过 `RFFMPEG_SHARED_FS` 环境变量启用「共享文件系统直通」模式，跳过不必要的文件上传/下载步骤，Worker 直接使用本地路径读写文件。

#### 环境变量

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `RFFMPEG_SHARED_FS` | 启用共享文件系统直通模式 | 未设置（禁用） |
| `RFFMPEG_SHARED_FS_ALLOWED_PREFIX` | Worker 端允许访问的路径前缀白名单（逗号分隔） | 无限制 |

##### `RFFMPEG_SHARED_FS`

- **取值**：`1`、`true`、`yes`（不区分大小写）视为启用；`0`、`false`、`no` 或未设置视为禁用。
- **作用范围**：需同时在 CLI 端和 Worker 端设置。CLI 端控制是否跳过上传和结果下载，Worker 端控制是否跳过输入下载和输出上传。
- **优先级**：环境变量 > 默认行为。如果环境变量已设置，配置文件中的相关字段会被忽略。

##### `RFFMPEG_SHARED_FS_ALLOWED_PREFIX`（Worker 端）

限制 Worker 在直通模式下可访问的路径前缀。多个前缀用逗号分隔。如果设置，Worker 会验证传入的路径是否以任一允许的前缀开头，不匹配的请求将被拒绝。

示例：
```bash
# 允许访问 /data/media 和 /mnt/nfs 下的所有文件
export RFFMPEG_SHARED_FS_ALLOWED_PREFIX="/data/media,/mnt/nfs"
```

#### 行为变化对比

| 环节 | 标准模式 | 直通模式 (`RFFMPEG_SHARED_FS=1`) |
|------|---------|----------------------------------|
| CLI 输入处理 | 上传 `-i /path/to/input.mp4` 到 Server | 跳过上传，在 job 请求中传递原始路径 (`direct_path`) |
| Server 调度 | 存储文件、分发下载链接给 Worker | 透传原始路径给 Worker |
| Worker 输入 | 从 Server 下载到临时目录 | 直接使用路径调用 `ffmpeg -i /path/to/input.mp4` |
| Worker 输出 | 上传输出文件到 Server | 直接将输出写入指定路径 |
| CLI 结果获取 | 从 Server 下载输出文件 | 直接读取本地输出路径（文件已在本地） |

#### 使用场景

##### 场景 1：同一台机器

CLI 和 Worker 在同一台机器上运行，文件存储在本地磁盘。

```bash
# 在 CLI 端和 Worker 端均设置环境变量
export RFFMPEG_SHARED_FS=1

# 启动 Worker（同一台机器）
export RFFMPEG_WORKER_NAME=local-worker
./bin/worker --server-url http://localhost:8080

# CLI 提交任务，输入/输出均为本地路径
./bin/rffmpeg -i /data/videos/input.mp4 -c:v libx264 /data/videos/output.mp4
```

此模式下，CLI 不进行文件上传，Worker 直接读取 `/data/videos/input.mp4` 进行处理，输出直接写入 `/data/videos/output.mp4`，CLI 完成后直接读取本地输出文件。

##### 场景 2：NFS 共享挂载

CLI 在机器 A，Worker 在机器 B，两者通过 NFS 共享 `/mnt/media` 目录。

```bash
# 两台机器均挂载 NFS：
#   mount -t nfs nfs-server:/exports/media /mnt/media

# 机器 A (CLI 端)
export RFFMPEG_SHARED_FS=1
./bin/rffmpeg --server http://worker-host:8080 \
  -i /mnt/media/videos/input.mp4 \
  -c:v libx264 \
  /mnt/media/output.mp4

# 机器 B (Worker 端)
export RFFMPEG_SHARED_FS=1
export RFFMPEG_SHARED_FS_ALLOWED_PREFIX="/mnt/media"
export RFFMPEG_WORKER_NAME=nfs-worker
./bin/worker --server-url http://localhost:8080
```

##### 场景 3：Kubernetes 共享 PV

多个 Pod 共享同一个 PersistentVolume（例如 ReadWriteMany PV）。

```yaml
# Worker Deployment
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
      - name: worker
        image: rffmpeg-worker:latest
        env:
        - name: RFFMPEG_SHARED_FS
          value: "1"
        - name: RFFMPEG_SHARED_FS_ALLOWED_PREFIX
          value: "/shared/media"
        volumeMounts:
        - name: media
          mountPath: /shared/media
      volumes:
      - name: media
        persistentVolumeClaim:
          claimName: media-pvc
```

#### 安全性

Worker 在直通模式下会对传入路径进行验证：
- 要求 `direct_path` 与输出路径为绝对路径
- 拒绝包含 `..` 路径遍历的请求
- 如果配置了 `RFFMPEG_SHARED_FS_ALLOWED_PREFIX`，解析符号链接后仅允许真实路径匹配前缀的路径
- Worker 执行前通过 `stat` 检查输入文件是否存在，不存在则返回明确错误

##### 运行时验证

Worker 在启动 ffmpeg 前会执行以下检查：
1. 要求 `direct_path` 与输出路径为绝对路径
2. 验证路径没有 `..` 遍历
3. 如果设置了白名单前缀，解析符号链接后验证真实路径是否匹配
4. `stat` 检查输入文件是否可读
5. 输出路径解析父目录符号链接后校验仍位于白名单内

任一检查失败，Worker 返回明确错误信息（包含失败原因），不会静默失败。

##### 信任模式说明

`RFFMPEG_SHARED_FS=1` 本质上是信任模式——用户需确保 CLI 和 Worker 之间的路径一致且可达。建议：
- 在生产环境中始终配置 `RFFMPEG_SHARED_FS_ALLOWED_PREFIX` 限制可访问范围
- 仅对受信任的网络环境启用（如内网、Kubernetes 集群内部）

#### 错误处理

当 Worker 无法访问指定路径时，会返回包含以下字段的错误信息：

```json
{
  "error": "PATH_INACCESSIBLE",
  "detail": "Input file not accessible: /data/input.mp4 (stat: no such file or directory)",
  "retryable": false
}
```

此时任务标记为失败，CLI 端会收到错误提示。如需回退到上传模式，请取消设置 `RFFMPEG_SHARED_FS` 环境变量后重新提交任务。

#### 限制说明

- **路径一致性要求**：CLI 端传入的路径必须是 Worker 端可以访问的绝对路径。不支持相对路径自动转换。
- **跨平台兼容性**：路径分隔符使用操作系统原生格式（Linux/macOS 使用 `/`）。Windows 路径支持取决于 Worker 运行环境。
- **不支持混合模式**：同一任务中不能部分文件使用直通路径、部分使用上传。设置 `RFFMPEG_SHARED_FS=1` 后，所有 `-i` 输入和输出文件均走直通路径。
- **安全性**：不设置 `RFFMPEG_SHARED_FS_ALLOWED_PREFIX` 时，Worker 可以访问宿主文件系统上任意路径（取决于 Worker 进程的权限）。

## API 文档

Server 提供 RESTful API，基础路径为 `/api/v1`。

### 文件上传

#### 简单上传
```
POST /api/v1/upload
Content-Type: multipart/form-data

Response:
{
  "file_id": "<sha256>",
  "message": "File uploaded successfully"
}
```

#### 分块上传

```
# 初始化分块上传
POST /api/v1/upload/init
{
  "filename": "video.mp4",
  "file_size": 1073741824,
  "chunk_size": 10485760
}

Response:
{
  "upload_id": "uuid",
  "chunk_size": 10485760,
  "total_chunks": 103
}

# 上传分块
POST /api/v1/upload/chunk/{uploadId}/{chunkIndex}
Content-Type: application/octet-stream
[chunk data]

# 获取上传进度
GET /api/v1/upload/progress/{uploadId}

# 完成上传
POST /api/v1/upload/complete
{
  "upload_id": "uuid"
}

# 取消上传
POST /api/v1/upload/cancel/{uploadId}

# 恢复上传
GET /api/v1/upload/resume/{uploadId}
```

### 任务管理

```
# 提交任务
POST /api/v1/jobs
{
  "input_files": ["file_id_1", "file_id_2"],
  "args": ["-i", "input.mp4", "-c:v", "libx264", "output.mp4"],
  "output_filename": "output.mp4",
  "priority": 0
}

Response:
{
  "job_id": "uuid",
  "message": "Job submitted"
}

# 查询任务状态
GET /api/v1/jobs/{jobId}

Response:
{
  "job": {
    "id": "uuid",
    "status": "completed",
    "input_files": ["file_id"],
    "args": [...],
    "output_files": ["output_file_id"],
    "worker_id": "worker_uuid",
    "exit_code": 0,
    "cached": true,
    "created_at": "2024-01-01T00:00:00Z",
    "started_at": "2024-01-01T00:00:05Z",
    "finished_at": "2024-01-01T00:00:12Z"
}
```

### 缓存命中可观察性

当任务结果命中 Worker 本地缓存时（TSI-2519）：

- CLI 会在 stderr 收到 `[rffmpeg] Cache hit: <key>`（`<key>` 为完整的 64 位十六进制缓存键）。
- 任务的 `GET /api/v1/jobs/{jobId}` 响应中 `cached` 字段为 `true`，底层 SQLite `jobs` 表新增 `cached` 列（`INTEGER DEFAULT 0`，`1` 表示命中缓存），可直接查询：`SELECT id FROM jobs WHERE cached = 1;`

### 缓存按内容寻址

`POST /api/v1/upload` 返回的 `file_id` 是上传文件内容的 SHA-256（`pkg/server/storage/storage.go` 的 `SaveFileByContent`；分块上传在 `complete` 时同样按最终内容哈希重命名）。缓存键 `GenerateCacheKey` 以 `inputSources`（即 `file_id` 列表）参与哈希。因此缓存是**按内容寻址**的：只要输入内容字节一致，无论它来自哪个本地路径、上传过多少次，都会得到相同的 `file_id`。就缓存键维度而言，只有内容变化（或参数/`auto_hw`/输出扩展名/编码器变化）才会改变缓存键并导致键维度的未命中；在缓存条目未过期、未被逐出、元数据完整的前提下，键相同的请求才会命中缓存——TTL 过期（默认 24h，`pkg/worker/cache.go:373`）、LRU 逐出、缓存禁用、元数据损坏或大小不符（`cache.go:386-390`）都会在键不变的情况下返回未命中。

最小可复现示例：

以下命令假定已配置认证令牌：先 `export RFFMPEG_TOKEN=<T>`（或为每条命令追加 `--token <T>`），令牌须与 Server 的 `--auth-token <T>` 一致。

```bash
# 同一内容的两个 byte-identical 副本
cp input.mp4 copy.mp4

# 两次提交：不同的本地路径，相同的内容
./bin/rffmpeg --server http://localhost:8080 -i input.mp4 -c:v libx264 out1.mp4
./bin/rffmpeg --server http://localhost:8080 -i copy.mp4  -c:v libx264 out2.mp4
```

第二次提交时 CLI 的 stderr 会出现 `[rffmpeg] Cache hit: <key>`，且该任务 `GET /api/v1/jobs/{jobId}` 的 `cached` 字段为 `true`——两次上传得到相同 `file_id`，转码参数相同，缓存键相同，输出复用第一次的结果。

**为什么不把 canonicalized 本地路径放进缓存键。** 服务器拿不到可靠的原始路径：上传模式下 `file_id` 是内容哈希，本地路径只存在于 CLI 一侧，不随任务提交；唯一携带路径的是 `RFFMPEG_SHARED_FS=1` 直通模式，而 Worker 在该模式下显式跳过缓存读写（`pkg/worker/worker.go` 的 `directMode` 门控）——路径缓存既不生效也无法区分场景。若强行把路径纳入缓存键，还会把缓存从「同一内容只转码一次」退化为「每条路径转码一次」，放大存储与 CPU 成本。缓存以内容为边界是正确且可解释的：内容相同的输入本就应产生相同输出，路径只是读取位置，不应影响转码结果。

```
# 取消任务
DELETE /api/v1/jobs/{jobId}

# 更新任务（Worker 使用）
PATCH /api/v1/jobs/{jobId}
{
  "status": "completed",
  "exit_code": 0,
  "progress": 100
}

# 上传任务输出文件（Worker 使用）
POST /api/v1/jobs/{jobId}/output

# 实时日志（WebSocket）
GET /api/v1/jobs/{jobId}/log
Upgrade: websocket
```

### 任务状态

| 状态 | 说明 |
|------|------|
| `pending` | 等待调度 |
| `queued` | 已分配给 Worker，等待执行 |
| `running` | 正在执行 |
| `completed` | 执行成功 |
| `failed` | 执行失败 |
| `cancelled` | 已取消 |

### 错误响应

当提交任务时，如果服务端检测到没有可用的 Worker，会立即返回 503 Service Unavailable 错误，而不是让任务进入等待队列：

```
HTTP/1.1 503 Service Unavailable
Content-Type: application/json

{
  "code": "worker_unavailable",
  "message": "No worker available. Please ensure at least one worker is registered and idle."
}
```

如果请求的编码器没有对应的 Worker 支持，会返回：

```
HTTP/1.1 503 Service Unavailable
Content-Type: application/json

{
  "code": "worker_unavailable",
  "message": "No worker available with encoder: h264_nvenc"
}
```

这允许 CLI 快速失败并提示用户，而不是等待 30 秒超时。

### 输出下载

```
GET /api/v1/output/{fileId}
```

### Worker 接口

```
# 注册
POST /api/v1/workers/register
{
  "worker_id": "uuid",
  "name": "worker-1",
  "capabilities": {
    "gpu_model": "NVIDIA RTX 3080",
    "encoders": ["libx264", "h264_nvenc"],
    "decoders": ["h264"],
    "ffmpeg_version": "ffmpeg version 5.1",
    "max_concurrent": 2,
    "video_encoders": [
      {"name": "libx264", "type": "video", "is_hw": false},
      {"name": "h264_nvenc", "type": "video", "is_hw": true}
    ]
  }
}
```

`encoders` 是规范字段（必填）：注册校验、调度匹配（`json_each(w.encoders)`）与
worker 列表响应都只使用它。`video_encoders` 是**可选的请求侧增强元数据**，
只在 `GET /api/v1/encoders` 聚合端点为每个编码器提供 `description`/`is_hw`；
客户端只发 `encoders` 时，服务端会按名字自动派生 `video_encoders`。上例中的
`video_encoders` 为可选字段（仅展示富元数据形态），可整体省略。

#### 多 Worker 同名部署（清理路径与新鲜窗口语义）

多个 Worker 进程可以共享同一个 `name`（例如 Deployment 的多个副本、或同一逻辑节点的重启），它们以各自独立的 `worker_id` 并存。同名行通过两条路径回收，二者互补：

1. **注册时清理（register-time DELETE）**：`POST /api/v1/workers/register` 写入新 `worker_id` 前，会删除同名且 `id` 不同的 stale 行——`status = 'offline'`，或 `last_heartbeat` 早于 `--worker-heartbeat-timeout`（新鲜窗口）的"活但已死"行（crash 后未等巡检就重启的场景）。逻辑见 `pkg/server/db/db.go` 的 `CreateOrUpdateWorker`。
2. **巡检清扫（monitor sweep）**：健康监控每 `--worker-health-check-interval`（默认 30s）运行一次：先把心跳超过 `--worker-heartbeat-timeout` 的 worker 标记为 `offline` 并迁移其任务，再把 `status = 'offline'` 且 `last_heartbeat` 早于 `now - --worker-offline-threshold`（默认 10m）的行从 `workers` 表删除并移出内存状态表。逻辑见 `pkg/server/workerhealth/monitor.go` 的 `checkWorkers`。

**新鲜窗口内的重启不会立即回收旧行**：worker crash 后若在心跳新鲜窗口内（如 <20s，默认窗口 90s）以新 `worker_id` 立即重启，旧行 `status` 仍是非 `offline` 且心跳尚未过期，注册时清理不会动它；该行会保留到巡检标记 `offline`，并在此后其 `last_heartbeat` 早于 `now - --worker-offline-threshold` 时由巡检删除。这是设计使然，不是泄漏的重复行——在此期间新行已正常注册并接管调度，旧行只是等待常规回收。

**并发同名 worker 不受影响**：心跳新鲜的多个同名 worker（各自持有不同 `worker_id`）在注册时互相保留，不会被清理——只有 offline 或心跳过期的同名行才会被删除。

# 心跳（含 GPU 利用率与显存指标，由 nvidia-smi 采样）
POST /api/v1/workers/heartbeat
{
  "worker_id": "uuid",
  "status": "busy",
  "active_jobs": ["job_id_1"],
  "throughput_fps": 1.2,
  "gpu_metrics_valid": true,
  "gpu_util_percent": 87,
  "gpu_mem_used_mb": 4096
}
```

字段说明：

- `throughput_fps`：历史命名，实际单位是**每秒完成的作业数**（jobs/sec），不是帧率。
- `gpu_util_percent`：多卡机器上是所有 GPU 利用率的**求和**（两块卡各 50% 上报 100），
  因此多卡主机上超过 100 属于正常。
- `gpu_metrics_valid=false` 时 `gpu_util_percent` / `gpu_mem_used_mb` 为 0，
  含义是"本次无采样"（nvidia-smi / intel_gpu_top / amdgpu sysfs 均不可用或失败），
  而不是"利用率 0%"；服务端会保留上一次有效采样。合法的 0% 读数始终会上报。

```
# Worker 列表（含实时健康指标）
GET /api/v1/workers
{
  "workers": [
    {
      "id": "uuid",
      "status": "busy",
      "gpu_model": "NVIDIA RTX 3080",
      "health": {
        "status": "busy",
        "gpu_util_percent": 87,
        "gpu_mem_used_mb": 4096,
        "active_jobs": ["job_id_1"],
        "throughput_fps": 0.4,
        "last_seen": "2024-01-01T00:00:00Z"
      }
    }
  ]
}

# 拉取任务
GET /api/v1/workers/{workerId}/jobs
```

### 健康检查

```
GET /api/v1/health

Response:
{
  "status": "ok",
  "timestamp": "2024-01-01T00:00:00Z",
  "version": "1.0.0"
}
```

## 示例

### 完整转码工作流

```bash
# 1. 启动 Server
./bin/server --port 8080 --data-dir ./data

# 2. 启动 Worker（另一个终端）
cat > worker.json << EOF
{
  "server_url": "http://localhost:8080",
  "name": "gpu-worker",
  "auto_detect_codecs": false,
  "manual_encoders": ["libx264", "h264_nvenc", "hevc_nvenc"],
  "auto_detect_gpu": false,
  "manual_gpu_model": "NVIDIA RTX 3080",
  "max_concurrent": 2
}
EOF
./bin/worker --config worker.json

# 3. 提交转码任务（客户端）
./bin/rffmpeg -i my_video.mp4 \
  -c:v h264_nvenc -preset fast -cq 20 \
  -c:a aac -b:a 128k \
  output.mp4
```

### 多输出转码

```bash
# 生成多个分辨率版本
./bin/rffmpeg -i source.mp4 \
  -filter_complex "[0:v]split=3[v1][v2][v3]; \
   [v1]scale=1920:1080[v1out]; \
   [v2]scale=1280:720[v2out]; \
   [v3]scale=640:360[v3out]" \
  -map "[v1out]" -c:v:0 libx264 output_1080p.mp4 \
  -map "[v2out]" -c:v:1 libx264 output_720p.mp4 \
  -map "[v3out]" -c:v:2 libx264 output_360p.mp4
```

### 使用配置文件

```bash
# 创建 CLI 配置
cat > rffmpeg.json << EOF
{
  "server_url": "https://rffmpeg.example.com", // /api/v1 suffix is optional
  "token": "your-api-token"
}
EOF

# 直接使用（自动读取配置）
./bin/rffmpeg -i video.mp4 -c:v libx264 output.mp4
```

### 启用 TLS

```bash
# Server 端启用 HTTPS
./bin/server \
  --tls \
  --tls-cert /path/to/cert.pem \
  --tls-key /path/to/key.pem

# 启用 mTLS（双向认证）
./bin/server \
  --tls \
  --tls-cert /path/to/server-cert.pem \
  --tls-key /path/to/server-key.pem \
  --tls-client-ca /path/to/ca.pem \
  --mtls

# Worker 连接 HTTPS Server
./bin/worker --server-url https://localhost:8080
```

## 许可证

MIT License
