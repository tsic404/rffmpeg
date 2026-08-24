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

```bash
# 连接到本地 Server
./bin/worker --server http://localhost:8080/api/v1

# 指定 Worker 名称和支持的编码器
./bin/worker --name worker-1 --encoders libx264,h264_nvenc

# 使用 GPU 加速
./bin/worker --gpu "NVIDIA RTX 3080" --encoders h264_nvenc,hevc_nvenc
```

### 使用 CLI 提交任务

```bash
# 基本转码（与原生 ffmpeg 命令兼容）
./bin/rffmpeg -i input.mp4 -c:v libx264 -c:a aac output.mp4

# 指定远程 Server
./bin/rffmpeg --server http://your-server:8080/api/v1 -i video.mkv output.mp4

# 静默模式
./bin/rffmpeg -q -i input.mp4 -vf scale=1280:720 output.mp4

# 指定任务超时时间
./bin/rffmpeg --timeout 30m -i input.mp4 -c:v libx264 output.mp4
```

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
  --max-jobs-per-worker int            Maximum concurrent jobs per worker (default: 1)
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

#### 环境变量

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `RFFMPEG_SERVER_URL` | Server API URL | `http://localhost:8080/api/v1` |
| `RFFMPEG_WORKER_NAME` | Worker 名称 | 自动生成 |
| `RFFMPEG_WORKER_ID` | Worker ID | 自动生成 |
| `RFFMPEG_TEMP_DIR` | 临时文件目录 | 系统临时目录 |
| `RFFMPEG_FFMPEG_PATH` | FFmpeg 可执行文件路径 | `ffmpeg` |
| `RFFMPEG_ENCODERS` | 支持的编码器（逗号分隔） | `libx264` |
| `RFFMPEG_DECODERS` | 支持的解码器（逗号分隔） | - |
| `RFFMPEG_GPU_MODEL` | GPU 型号名称 | - |
| `RFFMPEG_FFMPEG_VERSION` | FFmpeg 版本 | 自动检测 |

#### 命令行参数

```bash
./bin/worker --help
  --server string           Server URL (/api/v1 suffix optional)
  --name string             Worker name (auto-generated if empty)
  --id string               Worker ID (auto-generated if empty)
  --temp-dir string         Temporary directory for files
  --ffmpeg string           Path to ffmpeg binary
  --timeout duration        Job execution timeout (default: 2h)
  --heartbeat-interval duration Heartbeat interval (default: 30s)
  --poll-interval duration  Job polling interval (default: 5s)
  --encoders string         Comma-separated list of supported encoders
  --decoders string         Comma-separated list of supported decoders
  --gpu string              GPU model name
  --max-concurrent int      Maximum concurrent jobs (default: 1)
```

### CLI 配置

CLI 配置文件搜索顺序（优先级从高到低）：
1. `./rffmpeg.json`
2. `~/.rffmpeg.json`
3. `/etc/rffmpeg.json`

#### 配置文件示例

```json
{
  "server_url": "http://localhost:8080", // /api/v1 suffix is optional
  "token": "your-auth-token"
}
```

#### 环境变量

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `RFFMPEG_SERVER_URL` | Server URL | `http://localhost:8080` |
| `RFFMPEG_TOKEN` | 认证令牌（server 启用认证时必填） | - |

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
./bin/worker --server http://localhost:8080/api/v1 --name local-worker

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
./bin/rffmpeg --server http://worker-host:8080/api/v1 \
  -i /mnt/media/videos/input.mp4 \
  -c:v libx264 \
  /mnt/media/output.mp4

# 机器 B (Worker 端)
export RFFMPEG_SHARED_FS=1
export RFFMPEG_SHARED_FS_ALLOWED_PREFIX="/mnt/media"
./bin/worker --server http://localhost:8080/api/v1 --name nfs-worker
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

##### 路径遍历防护

Worker 在直通模式下会对传入路径进行验证：
- 拒绝包含 `..` 路径遍历的请求
- 如果配置了 `RFFMPEG_SHARED_FS_ALLOWED_PREFIX`，仅允许匹配前缀的路径
- Worker 执行前通过 `stat` 检查路径是否存在，不存在则返回明确错误

##### 运行时验证

Worker 在启动 ffmpeg 前会执行以下检查：
1. 解析路径为绝对路径
2. 验证路径没有 `..` 遍历
3. 如果设置了白名单前缀，验证路径是否匹配
4. `stat` 检查输入文件是否可读
5. 验证输出目录是否可写

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
  "file_id": "uuid",
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
    "status": "running",
    "input_files": ["file_id"],
    "args": [...],
    "output_files": ["output_file_id"],
    "worker_id": "worker_uuid",
    "exit_code": 0,
    "created_at": "2024-01-01T00:00:00Z",
    "started_at": "2024-01-01T00:00:05Z",
    "finished_at": null
  }
}

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
    "max_concurrent": 2
  }
}

# 心跳（含 GPU 利用率与显存指标，由 nvidia-smi 采样）
POST /api/v1/workers/heartbeat
{
  "worker_id": "uuid",
  "status": "busy",
  "active_jobs": ["job_id_1"],
  "throughput_fps": 120.5,
  "gpu_util_percent": 87,
  "gpu_mem_used_mb": 4096
}

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
        "throughput_fps": 120.5,
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
./bin/worker --server http://localhost:8080/api/v1 \
  --name gpu-worker \
  --encoders libx264,h264_nvenc,hevc_nvenc \
  --gpu "NVIDIA RTX 3080" \
  --max-concurrent 2

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
./bin/worker --server https://localhost:8080/api/v1
```

## 许可证

MIT License
