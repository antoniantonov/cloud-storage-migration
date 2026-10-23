#!/bin/bash
set -euo pipefail
export LC_ALL=C

usage() {
    local line
    while IFS= read -r line; do printf '%s\n' "$line"; done <<'USAGE'
Usage: ./docker-run.sh [--skip-build] --list-all[=true|false]
       ./docker-run.sh [--skip-build] --copy-item ID [--copy-item=ID ...]

Exactly one action is required. IDs are opaque OneDrive item IDs, not paths.
Action flags override LIST_ALL and COPY_ITEM_IDS together. Without action
flags, use LIST_ALL=true or COPY_ITEM_IDS='["ID", "ID2"]'.

  --skip-build    Use an existing local DOCKER_IMAGE; never build or pull.
  --help         Show this help without Docker or cloud access.

Normally rebuilds DOCKER_IMAGE (default cloud-storage-migration:latest).
Export MICROSOFT_CLIENT_ID and, for copies, GOOGLE_CLIENT_CREDENTIALS_FILE
when needed. No .env file is executed. AUTH_DIR and TEMP_COPY_DIR default to
.auth and temp-copy under this script's repository; relative host paths are
resolved there. Both directories persist privately on the host.

AUTH_INTERACTIVE defaults to true. Interactive copies publish only
127.0.0.1:GOOGLE_OAUTH_PORT:GOOGLE_OAUTH_PORT (default 8765).
The container listens internally on 0.0.0.0. Use a local Docker daemon and
a non-root host user. Google credentials are mounted read-only for copies
only; listing does not need a Google credentials file.
USAGE
}

usage_error() {
    printf 'Configuration error: %s\nUse --help for usage.\n' "$*" >&2
    exit 2
}

operation_error() {
    printf 'Error: %s\n' "$*" >&2
    exit 1
}

parse_bool() {
    case "$1" in
        1|t|T|true|TRUE|True) boolean=true ;;
        0|f|F|false|FALSE|False) boolean=false ;;
        *) usage_error "$2 must be a boolean." ;;
    esac
}

validate_id() {
    local id=$1 remaining=$1 whitespace
    local c1_pattern=$'\302[\200-\237]'
    case "$id" in
        ''|.|..|-*|*/*|*\\*|*:*|*[[:cntrl:]]*)
            usage_error 'Copy selections must be nonempty OneDrive item IDs, not paths, URLs, flags, or control characters.'
            ;;
    esac
    if [[ $id =~ $c1_pattern ]]; then
        usage_error 'OneDrive item IDs must not contain control characters.'
    fi
    # Match Go's Unicode whitespace check without imposing an ID alphabet.
    for whitespace in ' ' $'\t' $'\n' $'\v' $'\f' $'\r' \
        $'\302\205' $'\302\240' $'\341\232\200' \
        $'\342\200\200' $'\342\200\201' $'\342\200\202' \
        $'\342\200\203' $'\342\200\204' $'\342\200\205' \
        $'\342\200\206' $'\342\200\207' $'\342\200\210' \
        $'\342\200\211' $'\342\200\212' $'\342\200\250' \
        $'\342\200\251' $'\342\200\257' $'\342\201\237' $'\343\200\200'; do
        remaining=${remaining//"$whitespace"/}
    done
    [[ -n "$remaining" ]] || usage_error 'OneDrive item IDs must not be empty or whitespace.'
}

json_error() {
    usage_error 'COPY_ITEM_IDS must be a JSON array of item-ID strings.'
}

json_whitespace() {
    while (( position < ${#json} )); do
        case "${json:position:1}" in
            ' '|$'\t'|$'\n'|$'\r') position=$((position + 1)) ;;
            *) break ;;
        esac
    done
}

json_hex() {
    local hex=${json:position:4}
    [[ ${#hex} == 4 && "$hex" != *[!0-9a-fA-F]* ]] || json_error
    codepoint=$((16#$hex))
    position=$((position + 4))
}

json_unicode() {
    local high octal
    json_hex
    if (( codepoint >= 55296 && codepoint <= 56319 )); then
        high=$codepoint
        [[ "${json:position:2}" == '\u' ]] || json_error
        position=$((position + 2))
        json_hex
        (( codepoint >= 56320 && codepoint <= 57343 )) || json_error
        codepoint=$((65536 + (high - 55296) * 1024 + codepoint - 56320))
    elif (( codepoint >= 56320 && codepoint <= 57343 )); then
        json_error
    fi
    if (( codepoint < 32 || (codepoint >= 127 && codepoint <= 159) )); then
        usage_error 'OneDrive item IDs must not contain control characters.'
    fi
    if (( codepoint < 128 )); then
        printf -v octal '\\%03o' "$codepoint"
    elif (( codepoint < 2048 )); then
        printf -v octal '\\%03o\\%03o' \
            "$((192 + (codepoint >> 6)))" "$((128 + (codepoint & 63)))"
    elif (( codepoint < 65536 )); then
        printf -v octal '\\%03o\\%03o\\%03o' \
            "$((224 + (codepoint >> 12)))" "$((128 + ((codepoint >> 6) & 63)))" \
            "$((128 + (codepoint & 63)))"
    else
        printf -v octal '\\%03o\\%03o\\%03o\\%03o' \
            "$((240 + (codepoint >> 18)))" "$((128 + ((codepoint >> 12) & 63)))" \
            "$((128 + ((codepoint >> 6) & 63)))" "$((128 + (codepoint & 63)))"
    fi
    printf -v character '%b' "$octal"
}

json_string() {
    [[ "${json:position:1}" == '"' ]] || json_error
    position=$((position + 1))
    local value='' character codepoint
    while (( position < ${#json} )); do
        character=${json:position:1}
        position=$((position + 1))
        case "$character" in
            '"')
                validate_id "$value"
                ids[${#ids[@]}]=$value
                return
                ;;
            '\')
                character=${json:position:1}
                position=$((position + 1))
                case "$character" in
                    '"'|'\'|'/') ;;
                    u) json_unicode ;;
                    b|f|n|r|t) usage_error 'OneDrive item IDs must not contain control characters.' ;;
                    *) json_error ;;
                esac
                ;;
            [[:cntrl:]]) json_error ;;
        esac
        value=$value$character
    done
    json_error
}

parse_json_ids() {
    local json=$1 position=0
    json_whitespace
    [[ "${json:position:1}" == '[' ]] || json_error
    position=$((position + 1))
    json_whitespace
    if [[ "${json:position:1}" != ']' ]]; then
        while :; do
            json_string
            json_whitespace
            case "${json:position:1}" in
                ',') position=$((position + 1)); json_whitespace ;;
                ']') break ;;
                *) json_error ;;
            esac
        done
    fi
    position=$((position + 1))
    json_whitespace
    (( position == ${#json} )) || json_error
}

skip_build=false
has_action=false
list=false
ids=()
while (( $# > 0 )); do
    case "$1" in
        --help|-h) usage; exit 0 ;;
        --skip-build) skip_build=true; shift ;;
        --list-all) has_action=true; list=true; shift ;;
        --list-all=*)
            has_action=true
            parse_bool "${1#*=}" '--list-all'
            list=$boolean
            shift
            ;;
        --copy-item)
            (( $# >= 2 )) || usage_error '--copy-item requires an item ID.'
            validate_id "$2"
            ids[${#ids[@]}]=$2
            has_action=true
            shift 2
            ;;
        --copy-item=*)
            validate_id "${1#*=}"
            ids[${#ids[@]}]=${1#*=}
            has_action=true
            shift
            ;;
        *) usage_error "Unknown argument: $1" ;;
    esac
done

if [[ "$has_action" == false ]]; then
    parse_bool "${LIST_ALL:-false}" LIST_ALL
    list=$boolean
    [[ -z "${COPY_ITEM_IDS:-}" ]] || parse_json_ids "$COPY_ITEM_IDS"
fi
if [[ "$list" == true ]]; then
    (( ${#ids[@]} == 0 )) || usage_error 'Select exactly one action: list or copy.'
else
    (( ${#ids[@]} > 0 )) || usage_error 'Select exactly one action: list or copy.'
fi

[[ -n "${MICROSOFT_CLIENT_ID:-}" && "${MICROSOFT_CLIENT_ID:-}" == *[![:space:]]* ]] ||
    usage_error 'MICROSOFT_CLIENT_ID must identify your registered Microsoft public-client app.'
parse_bool "${AUTH_INTERACTIVE:-true}" AUTH_INTERACTIVE
interactive=$boolean
port=8765
if [[ "$list" == false ]]; then
    port=${GOOGLE_OAUTH_PORT:-8765}
    port=${port#+}
    [[ -n "$port" && "$port" != *[!0-9]* ]] ||
        usage_error 'GOOGLE_OAUTH_PORT must be an integer from 1 to 65535.'
    while [[ ${#port} -gt 1 && "$port" == 0* ]]; do port=${port#0}; done
    [[ ${#port} -le 5 ]] && (( port >= 1 && port <= 65535 )) ||
        usage_error 'GOOGLE_OAUTH_PORT must be an integer from 1 to 65535.'
    case "${GOOGLE_OAUTH_LISTEN_HOST:-0.0.0.0}" in
        0.0.0.0|127.0.0.1) ;;
        *) usage_error 'GOOGLE_OAUTH_LISTEN_HOST must be 127.0.0.1 or 0.0.0.0.' ;;
    esac
fi
image=${DOCKER_IMAGE:-cloud-storage-migration:latest}
case "$image" in
    -*|*[[:space:][:cntrl:]]*) usage_error 'DOCKER_IMAGE must be a Docker image tag.' ;;
esac

script=${BASH_SOURCE[0]}
while [[ -L "$script" ]]; do
    directory=$(cd -- "$(dirname -- "$script")" && pwd -P)
    script=$(readlink "$script")
    [[ "$script" == /* ]] || script="$directory/$script"
done
root=$(cd -- "$(dirname -- "$script")" && pwd -P)
cd -- "$root"

private_directory() {
    local path=$1 label=$2
    [[ "$path" == /* ]] || path="$root/$path"
    mkdir -p -- "$path" || operation_error "Cannot create $label directory: $path"
    path=$(cd -- "$path" && pwd -P) || operation_error "Cannot access $label directory: $path"
    [[ "$path" != / ]] || usage_error "$label must not be the filesystem root."
    [[ -O "$path" ]] || operation_error "$label must be owned by the current host user: $path"
    chmod 700 "$path" || operation_error "Cannot set private permissions on $label: $path"
    [[ -w "$path" && -x "$path" ]] || operation_error "$label is not writable: $path"
    resolved_directory=$path
}

uid=$(id -u)
gid=$(id -g)
[[ "$uid" != 0 ]] || operation_error 'Run docker-run.sh as a non-root host user so private state can be shared with native runs.'
command -v docker >/dev/null 2>&1 ||
    operation_error 'Docker is required. Install Docker and start its local daemon.'

credentials=''
if [[ "$list" == false && -n "${GOOGLE_CLIENT_CREDENTIALS_FILE:-}" ]]; then
    credentials=$GOOGLE_CLIENT_CREDENTIALS_FILE
    [[ "$credentials" == /* ]] || credentials="$root/$credentials"
    [[ -f "$credentials" && -r "$credentials" ]] ||
        usage_error 'GOOGLE_CLIENT_CREDENTIALS_FILE must be a readable regular file when configured for a copy.'
    credentials="$(cd -- "$(dirname -- "$credentials")" && pwd -P)/$(basename -- "$credentials")"
fi

umask 077
private_directory "${AUTH_DIR:-.auth}" AUTH_DIR
auth_directory=$resolved_directory
private_directory "${TEMP_COPY_DIR:-temp-copy}" TEMP_COPY_DIR
temp_directory=$resolved_directory
[[ "$auth_directory" != "$temp_directory" ]] ||
    usage_error 'AUTH_DIR and TEMP_COPY_DIR must be separate directories.'

copy_json='['
separator=''
# ${array[@]+...} also handles empty arrays under Bash 3.2's nounset.
for item in ${ids[@]+"${ids[@]}"}; do
    item=${item//\\/\\\\}
    item=${item//\"/\\\"}
    copy_json=$copy_json$separator\"$item\"
    separator=,
done
copy_json=$copy_json']'

run_args=(run --rm --init --pull=never --user "$uid:$gid" --workdir /work
    --env "LIST_ALL=$list"
    --env "COPY_ITEM_IDS=$copy_json"
    --env "MICROSOFT_CLIENT_ID=$MICROSOFT_CLIENT_ID"
    --env "AUTH_DIR=/state"
    --env "TEMP_COPY_DIR=/work/temp-copy"
    --env "AUTH_INTERACTIVE=$interactive"
    --env "GOOGLE_OAUTH_PORT=$port"
    --env "GOOGLE_OAUTH_LISTEN_HOST=0.0.0.0")

add_mount() {
    local source=${1//\"/\"\"}
    # Docker --mount is CSV: quote the entire source field for commas/quotes.
    run_args[${#run_args[@]}]=--mount
    run_args[${#run_args[@]}]="type=bind,\"src=$source\",dst=$2${3:-}"
}
add_mount "$auth_directory" /state
add_mount "$temp_directory" /work/temp-copy
if [[ -n "$credentials" ]]; then
    add_mount "$credentials" /run/credentials/google.json ,readonly
    run_args[${#run_args[@]}]=--env
    run_args[${#run_args[@]}]='GOOGLE_CLIENT_CREDENTIALS_FILE=/run/credentials/google.json'
fi
if [[ "$list" == false && "$interactive" == true ]]; then
    run_args[${#run_args[@]}]=--publish
    run_args[${#run_args[@]}]="127.0.0.1:$port:$port"
fi

if [[ "$skip_build" == true ]]; then
    docker image inspect -- "$image" >/dev/null 2>&1 ||
        operation_error "No usable local image '$image' (or Docker daemon unavailable). Run without --skip-build to build it."
else
    if docker build --tag "$image" "$root" >&2; then
        :
    else
        status=$?
        printf '%s\n' 'Docker build failed; no container was started.' >&2
        exit "$status"
    fi
fi
exec docker "${run_args[@]}" "$image"
