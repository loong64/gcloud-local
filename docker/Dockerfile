ARG DOCKER_CLI_IMAGE=docker:cli
FROM ${DOCKER_CLI_IMAGE}
RUN apk add --no-cache bash make git coreutils findutils curl ca-certificates tar gzip sed grep rsync jq python3 openssl
RUN git config --system --add safe.directory /workspace
ENTRYPOINT ["docker"]
