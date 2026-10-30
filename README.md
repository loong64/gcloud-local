# gcloud-local

English | [简体中文](README.zh-CN.md)

Run Cloud Build YAML/JSON configurations with local Docker. Supports a subset of `gcloud builds submit` without the Google Cloud SDK.

## Installation

Requires Linux, the Docker CLI, and a local Docker Unix socket.

Download a Linux amd64/arm64 binary from GitHub Releases, or build and install with Go 1.23+:

```bash
make build
sudo make install
```

## Usage

```bash
gcloud-local builds submit . --no-push
gcloud-local builds submit . --config cloudbuild.yaml --substitutions '_IMAGE=app:local'
gcloud-local builds submit . --dry-run
```

The source defaults to the current directory and the config to `cloudbuild.yaml`. See `gcloud-local --help` for all options and [examples/smoke.yaml](examples/smoke.yaml) for a sample config.

Run `docker login` before pushing images. Host credential helpers are unsupported; use `--docker-config DIR` to supply a separate login configuration.

## Behavior and limitations

- Steps run sequentially. `waitFor` references are validated but do not enable parallel execution. Unsupported config fields fail before execution; cloud machine, disk, and logging options are ignored with a notice.
- Steps run as root and share the host Docker daemon. Source files are mounted writable, with automatic Git repository root detection. `.gcloudignore` and source snapshots are not supported.
- Set `steps[].name` to `ghcr.io/loong64/cloud-builders/docker` or a custom image.
- `GCLOUD_LOCAL_BEFOCE_SCRIPT_LINUX` runs commands inside each step container before `args` or `script`; dry runs skip it.
- `--no-push` skips final image pushes and common Docker push and manifest commands within steps; Buildx remote outputs are rejected. Absolute-path Docker calls and direct registry API calls bypass this option.
- `gcloud auth configure-docker` uses existing Docker credentials. Other nested `gcloud` commands are unsupported.
- Only local synchronous builds are supported. GCP authentication, cloud logging, cloud secrets, artifacts, and remote Docker contexts are unavailable.

Attribution and licenses are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
