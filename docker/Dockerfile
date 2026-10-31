ARG DOCKER_CLI_IMAGE=docker:cli
FROM ${DOCKER_CLI_IMAGE}
COPY --from=ghcr.io/loong64/dockereng/cli-bin /docker /usr/local/bin/docker
RUN apk add --no-cache bash make git coreutils findutils curl ca-certificates tar gzip sed grep rsync jq python3 openssl
RUN git config --system --add safe.directory /workspace
ENTRYPOINT []
