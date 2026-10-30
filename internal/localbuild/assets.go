package localbuild

// These wrappers are mounted ahead of the image's own binaries, never installed
// into or substituted inside the repository being built.
const gcloudShim = `#!/bin/sh
while [ "${1-}" = "--quiet" ] || [ "${1-}" = "-q" ]; do shift; done
if [ "${1-}" = auth ] && [ "${2-}" = configure-docker ]; then
  echo 'gcloud-local: auth configure-docker uses preconfigured Docker credentials (run docker login on the host)' >&2
  exit 0
fi
echo "gcloud-local: unsupported nested gcloud command: $*" >&2
exit 2
`
const dockerShim = `#!/bin/sh
# Entrypoint calls retain their original executable, including absolute paths.
docker_executable=docker
if [ "${1-}" = --gcloud-local-executable ]; then
  [ "$#" -ge 2 ] || { echo 'gcloud-local: missing Docker executable' >&2; exit 2; }
  docker_executable=$2
  shift 2
fi
# Preserve the image PATH, removing this wrapper's directory before delegation.
real_path=
old_ifs=$IFS
IFS=:
for part in $PATH; do
  [ "$part" = /gcloud-local/bin ] && continue
  if [ -z "$real_path" ]; then real_path=$part; else real_path=$real_path:$part; fi
done
IFS=$old_ifs
export PATH=$real_path
if [ "${GCLOUD_LOCAL_NO_PUSH:-0}" = 1 ]; then
  check_no_push() {
    # Parse a copy so delegation retains the original global options.
    group=
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --config|--context|-c|--host|-H|--log-level|-l|--tlscacert|--tlscert|--tlskey)
          [ "$#" -ge 2 ] || { echo 'gcloud-local: missing Docker option value' >&2; exit 2; }
          shift 2 ;;
        --config=*|--context=*|-c?*|--host=*|-H?*|--log-level=*|-l?*|--tlscacert=*|--tlscert=*|--tlskey=*|--debug|--debug=*|-D|--tls|--tls=*|--tlsverify|--tlsverify=*|--help|-h|--version|-v)
          shift ;;
        image|manifest|buildx)
          [ -z "$group" ] || break
          group=$1
          shift ;;
        --) shift ;;
        -*) echo "gcloud-local: --no-push refuses unknown Docker option $1" >&2; exit 2 ;;
        *) break ;;
      esac
    done
    case "$group ${1-}" in
      ' push'|'image push'|'manifest push'|'manifest create'|'manifest annotate')
        echo "gcloud-local: --no-push skipped docker ${group:+$group }$*" >&2
        exit 0 ;;
      'buildx imagetools')
        echo 'gcloud-local: --no-push refuses buildx imagetools (registry write)' >&2
        exit 2 ;;
    esac
    for arg in "$@"; do
      case "$arg" in
        --push|--push=true|*type=registry*|*push=true*)
          echo 'gcloud-local: --no-push refuses a build with registry output; use --load in the config' >&2
          exit 2 ;;
      esac
    done
  }
  check_no_push "$@"
fi
exec "$docker_executable" "$@"
`
