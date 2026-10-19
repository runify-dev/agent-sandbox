# Agent Sandbox

基于 Docker 的代码执行沙箱。Go 服务通过 Docker Engine API 管理容器。

数据按两级隔离：

- `scope`：数据归属，比如用户 id。每个 scope 有一个长期空间，跨对话共享
- `session`：scope 下的一次对话，比如 chat_id，可选。每个 session 有自己私有的工作目录和容器

```
宿主机                                      容器 (scope=u1, session=chatA)
<root>/u1/workspace/          ──bind──>     /opt/user        用户长期空间
<root>/u1/sessions/chatA/     ──bind──>     /opt/workspace   当前对话目录（cwd）
```

- chatA 的容器里根本没有挂载 chatB 的目录，对话之间真正隔离；`/opt/user` 所有对话共享
- 不传 session 时，`/opt/workspace` 和 `/opt/user` 都是 scope 的长期空间
- 第一次调用时创建容器，之后的调用通过 `exec` 复用；空闲超过 `SANDBOX_IDLE_TTL`（默认 10 分钟）自动销毁容器，**数据保留**
- 运行时镜像包含 Python 3.13、Node.js 22、ffmpeg 和常用的 Python 包

建议在给模型的工具说明里写明：当前对话的工作目录是 `/opt/workspace`，生成的文件都放在这里；`/opt/user` 是用户的长期空间，只有用户明确要求时才使用。scope 和 session 由业务方根据当前会话填写，不要让模型传。

## 构建与启动

```bash
cd agent-sandbox
docker build -t agent-sandbox-runtime:latest ./runtime
export SANDBOX_API_KEY=$(openssl rand -hex 24)
docker compose up -d --build sandbox-server
```

macOS 的 Docker Desktop 默认不共享 `/opt`，可以在 `.env` 里设置 `SANDBOX_DATA_ROOT=$HOME/agent-sandbox-data`。

启动后运行冒烟测试（会读取 `.env` 里的 key）：

```bash
./smoke_test.sh .
```

不用 Docker 直接运行（需要 Go 1.26+，并有 docker.sock 访问权限）：

```bash
cd agent-sandbox
SANDBOX_API_KEY=xxxxxxxxxxxxxxxx go run .
```

## API

所有 `/v1/*` 接口都需要携带 `X-API-Key: <key>` 或 `Authorization: Bearer <key>`。

### POST /v1/exec

```json
{
  "scope": "u1",
  "session": "chatA",
  "language": "python",
  "code": "import os; print(os.listdir('.'))",
  "timeout": 30,
  "env": {"FOO": "bar"}
}
```

- `scope`：必填；`session`：可选
- `language`：`python` | `node` | `bash`（ffmpeg 通过 bash 调用）
- `timeout`：秒，不传使用默认值，最大不超过 `SANDBOX_MAX_TIMEOUT`
- 代码通过 stdin 传给解释器，工作目录是 `/opt/workspace`

返回：

```json
{"stdout": "...", "stderr": "", "exit_code": 0, "timed_out": false, "truncated": false, "duration_ms": 42}
```

| 状态码 | 含义 |
|---|---|
| 400 | 参数错误（scope / session 只允许 `[A-Za-z0-9_-]`，最长 64） |
| 401 | key 错误 |
| 429 | 活跃容器数达到 `SANDBOX_MAX_SESSIONS` |

### 清理

| 方法 | 路径 | 说明 |
|---|---|---|
| `DELETE` | `/v1/scopes/{scope}/sessions/{session}` | 销毁这个对话的容器，数据保留 |
| `DELETE` | `/v1/scopes/{scope}/sessions/{session}?purge=true` | 同时删除这个对话的目录，对话被删除时调用 |
| `DELETE` | `/v1/scopes/{scope}` | 销毁这个 scope 下所有对话的容器，数据保留 |
| `DELETE` | `/v1/scopes/{scope}?purge=true` | 同时删除 scope 的全部数据，用户被删除时调用 |

有正在执行的任务时返回 409。

### 文件接口

`{path}` 是相对路径。带 `?session=<session>` 时操作这个对话的目录（容器里的 `/opt/workspace`），不带时操作 scope 的长期空间（容器里的 `/opt/user`）。所有操作都通过 `os.Root` 限制在对应目录内，`../` 和指向目录外的符号链接都会被拒绝。

| 方法 | 路径 | 说明 |
|---|---|---|
| `PUT` | `/v1/files/{scope}/{path}` | 上传。请求体就是文件的原始字节，流式写入，自动创建上级目录，已存在则覆盖。上限 `SANDBOX_MAX_UPLOAD_MB` |
| `GET` | `/v1/files/{scope}/{path}` | 下载。文件返回内容（支持 Range，可以断点续传、拖动视频进度）；目录返回 JSON 文件列表 |
| `GET` | `/v1/files/{scope}/` | 列出根目录 |
| `DELETE` | `/v1/files/{scope}/{path}` | 删除文件或整个目录 |

目录列表返回：

```json
{"path": "videos", "entries": [{"name": "a.mp4", "is_dir": false, "size": 1048576, "mod_time": "2026-10-16T10:00:00Z"}]}
```

### GET /healthz

健康检查，不需要 key。

## 调用示例

上传视频 → 用 ffmpeg 处理 → 下载结果：

```bash
curl -s -X PUT "localhost:8090/v1/files/u1/input.mp4?session=chatA" -H "X-API-Key: $SANDBOX_API_KEY" --data-binary @input.mp4
curl -s localhost:8090/v1/exec -H "X-API-Key: $SANDBOX_API_KEY" -d '{
  "scope": "u1",
  "session": "chatA",
  "language": "bash",
  "code": "ffmpeg -y -i input.mp4 -vf scale=640:-2 output.mp4 2>&1 | tail -n 5"
}'
curl -s "localhost:8090/v1/files/u1/output.mp4?session=chatA" -H "X-API-Key: $SANDBOX_API_KEY" -o output.mp4
```

在 MaxKB 后端（Python）中调用：

```python
from urllib.parse import quote

import requests

def run_in_sandbox(scope: str, session: str | None, language: str, code: str, timeout: int = 60) -> dict:
    resp = requests.post(
        "http://127.0.0.1:8090/v1/exec",
        headers={"X-API-Key": SANDBOX_API_KEY},
        json={"scope": scope, "session": session or "", "language": language, "code": code, "timeout": timeout},
        timeout=timeout + 30,
    )
    resp.raise_for_status()
    return resp.json()


def upload_to_sandbox(scope: str, session: str | None, path: str, fileobj) -> dict:
    resp = requests.put(
        f"http://127.0.0.1:8090/v1/files/{scope}/{quote(path)}",
        params={"session": session} if session else None,
        headers={"X-API-Key": SANDBOX_API_KEY},
        data=fileobj,  # 文件对象会被流式发送，不会整个读进内存
    )
    resp.raise_for_status()
    return resp.json()


def download_from_sandbox(scope: str, session: str | None, path: str, dest: str) -> None:
    with requests.get(
        f"http://127.0.0.1:8090/v1/files/{scope}/{quote(path)}",
        params={"session": session} if session else None,
        headers={"X-API-Key": SANDBOX_API_KEY},
        stream=True,
    ) as resp:
        resp.raise_for_status()
        with open(dest, "wb") as f:
            for chunk in resp.iter_content(chunk_size=1 << 20):
                f.write(chunk)
```

## 配置

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `SANDBOX_API_KEY` | 必填 | 至少 16 位 |
| `SANDBOX_LISTEN` | `:8090` | 监听地址 |
| `SANDBOX_IMAGE` | `agent-sandbox-runtime:latest` | 运行时镜像 |
| `SANDBOX_DATA_ROOT` | `/opt` | 数据根目录：`<root>/<scope>/workspace` 和 `<root>/<scope>/sessions/<session>` |
| `SANDBOX_MOUNT_TARGET` | `/opt/workspace` | 当前对话目录在容器内的挂载点 |
| `SANDBOX_USER_MOUNT_TARGET` | `/opt/user` | scope 长期空间在容器内的挂载点 |
| `SANDBOX_RUNTIME` | 空(runc) | 设为 `runsc` 使用 gVisor |
| `SANDBOX_NETWORK` | `none`（compose 中为 `proxy`） | 见下方「网络与白名单」 |
| `SANDBOX_MEMORY_MB` | `1024` | 每个容器的内存上限 |
| `SANDBOX_CPUS` | `1.0` | 每个容器的 CPU 上限 |
| `SANDBOX_PIDS_LIMIT` | `256` | 每个容器的进程数上限 |
| `SANDBOX_TMP_MB` | `512` | `/tmp` 的 tmpfs 大小（计入内存） |
| `SANDBOX_UID` / `SANDBOX_GID` | `1000` | 容器内运行用户，工作区会被 chown 给它 |
| `SANDBOX_IDLE_TTL` | `10m` | 空闲回收时间 |
| `SANDBOX_MAX_SESSIONS` | `100` | 同时存在的容器上限（每个 scope + session 一个） |
| `SANDBOX_DEFAULT_TIMEOUT` | `60` | 默认执行超时（秒） |
| `SANDBOX_MAX_TIMEOUT` | `600` | 最大执行超时（秒），视频处理可以适当调大 |
| `SANDBOX_MAX_OUTPUT_KB` | `1024` | stdout / stderr 各自最多返回多少 KB |
| `SANDBOX_MAX_UPLOAD_MB` | `2048` | 单个上传文件的大小上限 |
| `SANDBOX_EGRESS_ALLOWLIST` | `pypi.org,files.pythonhosted.org,registry.npmjs.org` | proxy 模式下允许访问的域名，逗号分隔 |
| `SANDBOX_EGRESS_PORTS` | `80,443` | proxy 模式下允许访问的端口 |
| `SANDBOX_EGRESS_PRIVATE_CIDRS` | 空 | 额外允许访问的内网地址段，如内部镜像源 `10.1.2.3/32` |
| `SANDBOX_PIP_INDEX_URL` | 空 | 注入容器的 `PIP_INDEX_URL` |
| `SANDBOX_NPM_REGISTRY` | 空 | 注入容器的 npm registry |
| `SANDBOX_PROXY_NETWORK` | `agent-sandbox-egress` | 沙箱接入的 internal 网络（compose 自动创建） |

## 网络与白名单

| `SANDBOX_NETWORK` | 效果 |
|---|---|
| `none` | 完全断网 |
| `proxy` | 沙箱只接入一个 internal 网络（没有出网路由），唯一出口是服务内置的白名单代理 |
| 其他值 | 作为 Docker 网络名直接使用，比如 `bridge` 不做任何限制，**不建议** |

`proxy` 模式下：

- 容器里已经设置好 `HTTP_PROXY` / `HTTPS_PROXY`，`pip`、`npm`、`curl`、`requests` 等会自动走代理
- 只放行 `SANDBOX_EGRESS_ALLOWLIST` 里的域名和 `SANDBOX_EGRESS_PORTS` 里的端口，其他请求返回 403：`sandbox egress denied: host xxx is not in allowlist`
- 白名单写法：`pypi.org` 精确匹配；`*.example.com` 匹配所有子域名（不含 `example.com` 本身）；单独一个 `*` 放行所有域名
- 即使配置了 `*`，内网地址、云厂商元数据地址和 `SANDBOX_EGRESS_PORTS` 以外的端口依然被拒绝，`*` 的含义是「整个公网」
- 即使白名单域名解析到内网地址，也会被拒绝（防 DNS rebinding），云厂商元数据地址 `169.254.169.254` 同样被拒绝。确实要访问内部镜像源时，把它的地址段加到 `SANDBOX_EGRESS_PRIVATE_CIDRS`
- 沙箱不能调用沙箱服务自己的管理接口
- 需要 `docker compose` 启动服务（服务要同时接入 internal 网络和外网）

修改白名单后重启服务生效：`docker compose up -d`。

使用国内镜像源的例子（`.env`）：

```bash
SANDBOX_EGRESS_ALLOWLIST=mirrors.aliyun.com,registry.npmmirror.com
SANDBOX_PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple/
SANDBOX_NPM_REGISTRY=https://registry.npmmirror.com
```

白名单里的域名仍然 403 时，看服务日志里的 `egress denied`：

```bash
docker compose logs sandbox-server | grep "egress denied"
```

如果原因是 `destination 198.18.x.x is a private or reserved address`，说明宿主机开着 Clash / Surge 等 fake-ip 模式的代理，域名被解析到了 `198.18.0.0/15`。本地开发时可以在 `.env` 里加 `SANDBOX_EGRESS_PRIVATE_CIDRS=198.18.0.0/15` 放行，服务器上不要这样配置。

`pip install` 会自动装到用户长期空间的 `/opt/user/.local`（根文件系统只读），这个 scope 下所有对话都能用。镜像里已预装常用的包，见 [runtime/requirements.txt](runtime/requirements.txt)。

## 安全说明

- 容器配置：只读根文件系统、`cap-drop ALL`、`no-new-privileges`、非 root 运行、断网或只能通过白名单代理出网、限制内存 / CPU / 进程数
- 执行命令外层包了 `timeout -s KILL`，超时会杀掉整个进程组；如果仍然卡住，服务会直接销毁并重建容器
- 服务本身挂载了 docker.sock，相当于拥有宿主机 root 权限：只监听内网，key 不要泄露
- proxy 模式下所有沙箱容器在同一个 internal 网络里，彼此之间网络可达（但都只是普通用户进程，没有对外服务）
- 执行不可信代码时强烈建议使用 gVisor（`SANDBOX_RUNTIME=runsc`）
- **磁盘配额没有限制**：bind mount 不受 Docker 存储限制，需要的话在宿主机上用 XFS project quota 或者给 `/opt` 单独分区
- 建议把 `SANDBOX_DATA_ROOT` 设为专用目录（比如 `/opt/sandbox`），不要和 `/opt` 下其他软件混在一起
- macOS 上的 Docker Desktop 需要在 File Sharing 里共享 `SANDBOX_DATA_ROOT`
