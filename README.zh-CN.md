# gcloud-local

[English](README.md) | 简体中文

在本机 Docker 中执行 Cloud Build YAML/JSON 配置，支持 `gcloud builds submit` 的本地构建子集，无需 Google Cloud SDK。

## 安装

需要 Linux、Docker CLI 和本地 Docker Unix socket。

从 GitHub Releases 下载 Linux amd64/arm64 二进制，或使用 Go 1.23+ 编译安装：

```bash
make build
sudo make install
```

## 使用

```bash
gcloud-local builds submit . --no-push
gcloud-local builds submit . --config cloudbuild.yaml --substitutions '_IMAGE=app:local'
gcloud-local builds submit . --dry-run
```

源码默认使用当前目录，配置默认使用 `cloudbuild.yaml`。完整参数见 `gcloud-local --help`，示例见 [examples/smoke.yaml](examples/smoke.yaml)。

推送镜像前需执行 `docker login`。不支持宿主机 credential helper；可通过 `--docker-config DIR` 指定独立的登录配置。

## 执行与限制

- 步骤串行执行，验证 `waitFor` 引用但不并行调度。不支持的配置字段在执行前报错；云端机器、磁盘和日志选项会提示忽略。
- 步骤以 root 运行，共享宿主机 Docker daemon。源码可写挂载，自动识别 Git 仓库根目录；不处理 `.gcloudignore` 或源码快照。
- `gcr.io/cloud-builders/docker` 自动替换为本地缓存的 builder，包含 Docker CLI、Buildx、bash、make 和 git。基础镜像可通过 `GCLOUD_LOCAL_DOCKER_CLI_IMAGE` 覆盖，须兼容 Alpine `apk`；浮动标签更新需手动清理缓存。
- `--no-push` 跳过最终镜像推送及步骤内常见的 Docker push、manifest 命令；拒绝 Buildx 远端输出。绝对路径调用 Docker 或直接调用 registry API 不受其控制。
- `gcloud auth configure-docker` 使用预先登录的 Docker 凭据，其他嵌套 `gcloud` 命令不受支持。
- 仅支持本地同步构建，不提供 GCP 身份、云日志、云端 secrets、artifacts 或远程 Docker context。
