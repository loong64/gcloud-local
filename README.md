# gcloud-local

gcloud 的本地实现，目前支持 `gcloud builds submit` 的本地构建子集。读取 Cloud Build YAML/JSON 配置，在本机 Docker 中执行构建步骤，无需安装 Google Cloud SDK 或调用 Cloud Build API。

本项目使用 Go 编写，不是完整的 Google Cloud SDK，也不提供 GCP 云服务。

## 安装

需要 Linux、Docker CLI 和可访问的本地 Docker Unix socket。从源码构建还需要 Go 1.23+。

可从仓库的 GitHub Releases 下载对应架构的 `gcloud-local_linux_amd64.tar.gz` 或 `gcloud-local_linux_arm64.tar.gz`，使用同一 Release 的 `checksums.txt` 校验后解压安装。压缩包内含二进制文件、README 和许可证。

在本项目目录执行：

```bash
go build -o gcloud-local ./cmd/gcloud-local
sudo install -m 0755 gcloud-local /usr/local/bin/gcloud-local
gcloud-local --help
```

若现有脚本使用 `gcloud` 命令，可在独立目录中创建指向 `gcloud-local` 的 `gcloud` 软链接，并将该目录加入 `PATH`。仅支持下文列出的命令。

## 使用

在待构建项目目录执行：

```bash
# 使用当前目录的 cloudbuild.yaml
gcloud-local builds submit . --no-push

# 指定配置和变量
gcloud-local builds submit . \
  --config cloudbuild.yaml \
  --substitutions '_IMAGE=my-image:local' \
  --no-push

# 仅输出构建计划，不执行 Docker
gcloud-local builds submit . --config cloudbuild.yaml --dry-run
```

`SOURCE` 默认为当前目录，`--config` 默认为 `cloudbuild.yaml`。构建步骤共享本机 Docker daemon，源码以可写方式挂载，生成的文件会保留在源码目录中。

需要推送镜像时，先使用 `docker login` 登录目标 registry，再去掉 `--no-push`。工具会将 Docker 登录配置复制到临时目录供步骤使用，结束后清理。不支持依赖宿主机 credential helper 的配置，可在独立的 `DOCKER_CONFIG` 目录登录后，通过 `--docker-config DIR` 指定。

### 常用参数

| 参数 | 说明 |
| --- | --- |
| `--config FILE` | Cloud Build YAML/JSON 配置文件 |
| `--substitutions K=V,...` | 覆盖替换变量，可重复传入 |
| `--project ID` | 设置 `PROJECT_ID`，默认 `local` |
| `--region REGION` | 设置 `LOCATION`，默认 `local` |
| `--verbosity LEVEL` | 兼容 gcloud 参数，接受但不改变本地日志输出 |
| `--workspace-root DIR` | 为没有 `.git` 的源码指定工作区根目录 |
| `--no-push` | 跳过最终镜像推送，以及步骤内常见的 Docker push 和 manifest 命令 |
| `--dry-run` | 输出 JSON 构建计划，不执行 Docker |
| `--timeout DURATION` | 构建总超时，例如 `3600s` 或 `2h` |
| `--builder-image OLD=NEW` | 替换步骤镜像，可重复传入 |
| `--docker-config DIR` | Docker 登录配置目录 |
| `--docker-socket PATH` | 本地 Docker socket，默认 `/var/run/docker.sock` |

完整参数见 `gcloud-local --help`。

本地 builder 默认基于 `docker:cli`。可通过环境变量覆盖基础镜像：

```bash
GCLOUD_LOCAL_DOCKER_CLI_IMAGE=docker:28-cli gcloud-local builds submit . --no-push
```

自定义基础镜像需提供 Docker CLI，并兼容 Alpine 的 `apk` 包管理器。不同基础镜像使用独立的 builder 缓存；相同配置会复用缓存，浮动标签更新不会自动触发重建。需要刷新时，可删除对应的 `gcloud-local/builder` 缓存镜像后重试。

## 支持范围

- 支持 `builds submit`，构建在本地同步执行。步骤按声明顺序串行运行；验证 `waitFor` 引用，但不并行调度。
- 支持配置中的 `steps`、`images`、`substitutions`、`timeout` 和 `tags`，以及步骤的入口、参数、目录、环境变量、脚本、超时、共享卷和失败处理等。
- 自动将 `gcr.io/cloud-builders/docker` 替换为本地 builder，首次运行自动构建并缓存。该镜像包含 Docker CLI、Buildx、bash、make 和 git；其他步骤使用配置指定的镜像。
- 失败容忍仅适用于步骤命令退出；镜像准备失败及 Docker 保留的启动错误（退出码 125–127）始终使构建失败。
- 步骤以 root 运行，共享 `/workspace`、`/builder/home`、声明的 named volumes 和宿主机 Docker socket。自动识别 Git 仓库根目录作为工作区，相对步骤目录按 `SOURCE` 解析；不模拟源码上传、`.gcloudignore` 或源码快照。
- 步骤中的 `gcloud auth configure-docker` 使用预先登录的 Docker 凭据，其他嵌套 `gcloud` 命令不支持。
- `--no-push` 不生成远端多架构 manifest；遇到 Buildx 的 `--push`、registry output 或 `imagetools` 会报错，需要调整为本地构建方式。启用该参数时，无法识别的 Docker 全局选项会报错。通过绝对路径调用 Docker 或直接调用 registry API 不受该参数控制。
- 云端机器、磁盘和日志选项会提示忽略。不支持的配置字段会在执行前报错，包括 Secret Manager、KMS secrets、云 artifacts、`dynamicSubstitutions` 和 `serviceAccount`。
- 仅支持本地 Unix Docker socket，不支持远程 Docker context、GCP 身份、云日志或异步任务。
