#!/bin/bash
# Prepare/publish named stopped bases and suspended slots. No Pulse/Orchard RPC.
set -eo pipefail # Bash 3.2 treats empty arrays as unset under nounset.
set +x
umask 077
root=$(cd "$(dirname "$0")/.." && pwd)
usage() {
  cat <<'HELP'
Usage: update-macos-images.sh --owner NAME [options]
  --base NAME         OS catalog entry (default: macos-26-arm64)
  --group NAME        Config.SlotGroup.Name (default: standard)
  --slots COUNT       Number of suspended slots (default: 1)
  --cpu COUNT         CPU per slot (default: 4)
  --memory MIB        Memory per slot (default: 8192)
  --c2j VERSION       latest (default), release tag, branch, or commit
  --image REFERENCE --os-version VERSION  Custom digest-pinned macOS image
  --ssh-user USER     Guest SSH login (default: admin)
  --ensure            Prepare only missing sources; do not update existing ones
  --timeout SECONDS   Per-VM preparation limit (default: 2700)

Run on the Apple Silicon worker as the Tart/Orchard worker user, using its
TART_HOME. Set PULSE_IMAGE_SSH_PASSWORD for a custom guest password (default:
admin). Requires Tart with clone --overwrite, Python 3, curl, and OpenSSH.
Existing job clones are never stopped, deleted, or modified.
HELP
}
owner= base=macos-26-arm64 group=standard count=1 cpu=4 memory=8192 version=latest
image= os_version= ssh_user=admin ensure=0 seconds=2700
while [[ $# -gt 0 ]]; do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --ensure) ensure=1; shift; continue ;;
    --owner|--base|--group|--slots|--cpu|--memory|--c2j|--image|--os-version|--ssh-user|--timeout)
      [[ $# -ge 2 ]] || { usage >&2; exit 2; }
      case "$1" in
        --owner) owner=$2 ;; --base) base=$2 ;; --group) group=$2 ;;
        --slots) count=$2 ;; --cpu) cpu=$2 ;; --memory) memory=$2 ;;
        --c2j) version=$2 ;; --image) image=$2 ;; --os-version) os_version=$2 ;;
        --ssh-user) ssh_user=$2 ;; --timeout) seconds=$2 ;;
      esac; shift 2 ;;
    *) usage >&2; exit 2 ;;
  esac
done
[[ $(uname -s) == Darwin && $(uname -m) == arm64 ]] || { echo 'Run on an Apple Silicon Mac.' >&2; exit 2; }
[[ $owner =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ && ${#owner} -le 64 && $group =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ && ${#group} -le 32 ]]
[[ $ssh_user =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]
for value in "$count" "$cpu" "$memory" "$seconds"; do [[ $value =~ ^[1-9][0-9]*$ && ${#value} -le 7 ]] || { echo 'Sizes/timeouts must be positive integers.' >&2; exit 2; }; done
[[ $count -le 1000 && $cpu -le 256 && $memory -ge 1024 && $memory -le 1048576 && $seconds -ge 60 && $seconds -le 7200 ]]
for tool in tart python3 curl ssh scp shasum; do command -v "$tool" >/dev/null; done
tart clone --help | grep -q -- --overwrite || { echo 'Tart must support clone --overwrite.' >&2; exit 2; }
export TART_NO_AUTO_PRUNE=true
if [[ -z $image && -z $os_version ]]; then
  IFS=$'\t' read -r image os_version < <(python3 - "$root/bases.json" "$base" <<'PY'
import json,sys
for base in json.load(open(sys.argv[1])):
    if base['Name']==sys.argv[2]:
        print(base['Image']+'\t'+base['OSVersion']);break
else: sys.exit('Unknown base; supply --image and --os-version.')
PY
)
fi
[[ $image =~ ^[A-Za-z0-9][A-Za-z0-9.:/_-]*@sha256:[a-f0-9]{64}$ && $os_version =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || { echo 'Supply a valid image digest and macOS version.' >&2; exit 2; }
names=()
while IFS= read -r name; do names+=("$name"); done < <(python3 "$root/scripts/image-names.py" "$owner" "$base" "$group" "$cpu" "$memory" "$count")
[[ ${#names[@]} -eq $((count+1)) ]]
work=$(mktemp -d /tmp/pulse-image-update.XXXXXX)
echo "Preparation logs: $work"
# Serialize independent updater invocations for this pool. Tart's own global
# lock serializes publication with clone commands from either backend.
lock="${TART_HOME:-$HOME/.tart}/.${names[0]}.update-lock"
mkdir -p "$(dirname "$lock")"
mkdir "$lock" || { echo "An updater is already running (lock: $lock)." >&2; exit 1; }
staged=() pids=() published=0
cleanup() {
  code=$?
  trap - EXIT INT TERM
  for name in "${staged[@]}"; do
    state=$(tart get "$name" --format json 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["State"])' 2>/dev/null) || continue
    if [[ $state == running ]]; then tart stop "$name" >/dev/null 2>&1 || continue; fi
    tart delete "$name" >/dev/null 2>&1 || true
  done
  for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
  rmdir "$lock" 2>/dev/null || true
  if [[ -n ${PULSE_IMAGE_LOG_DIR:-} ]]; then
    mkdir -p "$PULSE_IMAGE_LOG_DIR"
    for log in "$work/"*.log; do [[ ! -f $log ]] || cp "$log" "$PULSE_IMAGE_LOG_DIR/"; done
  fi
  if [[ $code == 0 ]]; then rm -rf "$work"; else echo "Preparation failed; logs retained in $work (published sources: $published)." >&2; fi
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
state() { tart get "$1" --format json | python3 -c 'import json,sys;print(json.load(sys.stdin)["State"])'; }
valid_source() {
  tart get "$1" --format json 2>/dev/null | python3 -c 'import json,sys;d=json.load(sys.stdin);sys.exit(not (d["State"]==sys.argv[1] and d["CPU"]==int(sys.argv[2]) and d["Memory"]==int(sys.argv[3])))' "$2" "$3" "$4" 2>/dev/null
}
export PULSE_IMAGE_SSH_PASSWORD=${PULSE_IMAGE_SSH_PASSWORD:-admin}
cat > "$work/askpass" <<'ASKPASS'
#!/bin/bash
printf '%s\n' "$PULSE_IMAGE_SSH_PASSWORD"
ASKPASS
chmod 700 "$work/askpass"
export SSH_ASKPASS="$work/askpass" SSH_ASKPASS_REQUIRE=force DISPLAY=pulse-image-setup
ssh_options=(-o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$work/known-hosts" -o ConnectTimeout=5 -o LogLevel=ERROR)
ssh_run() { ssh "${ssh_options[@]}" "$ssh_user@$guest_ip" "$@"; }
start_guest() {
  local name=$1
  tart run --no-graphics --suspendable "$name" > "$work/$name.run.log" 2>&1 &
  pids+=("$!")
  local start=$SECONDS
  while (( SECONDS-start < seconds )); do
    guest_ip=$(tart ip "$name" --wait 5 2>/dev/null) || { sleep 1; continue; }
    if ssh_run true </dev/null 2>/dev/null; then return; fi
    sleep 1
  done
  echo "SSH did not become ready for $name" >&2; return 1
}
wait_state() {
  local name=$1 desired=$2 start=$SECONDS
  until [[ $(state "$name") == "$desired" ]]; do
    (( SECONDS-start < seconds )) || { echo "Timed out waiting for $name to become $desired" >&2; return 1; }
    sleep 1
  done
}
base_source=${names[0]}
build_base=1
if [[ $ensure == 1 ]] && valid_source "$base_source" stopped 4 8192; then build_base=0; fi
if [[ $build_base == 1 ]]; then
  echo "Preparing base ${names[0]} with c2j $version"
  "$root/scripts/build-bundle.sh" "$work/setup.tar.gz" "${PULSE_IMAGE_PROFILE:-default}" --c2j "$version"
  base_source="pulse-build-$(date -u +%Y%m%dT%H%M%SZ)-$$-base"
  staged+=("$base_source")
  tart clone "$image" "$base_source"
  tart set "$base_source" --cpu 4 --memory 8192 --random-mac
  start_guest "$base_source"
  remote_dir=$(ssh_run 'mktemp -d /tmp/pulse-image-setup.XXXXXX' </dev/null)
  [[ $remote_dir =~ ^/tmp/pulse-image-setup\.[A-Za-z0-9]+$ ]]
  scp "${ssh_options[@]}" "$work/setup.tar.gz" "$ssh_user@$guest_ip:$remote_dir/setup.tar.gz" </dev/null
  # The preparer shuts the guest down. A closed SSH channel alone is not proof
  # of success; require its readiness marker and the stopped native state.
  ssh_run "tar -xzf '$remote_dir/setup.tar.gz' -C '$remote_dir'; sudo /bin/bash '$remote_dir/prepare-base.sh' '$remote_dir' '$seconds' '$os_version'" </dev/null > "$work/base.log" 2>&1 || true
  grep -qx PULSE_BASE_READY "$work/base.log"
  wait_state "$base_source" stopped
fi
ready=() targets=()
if [[ $build_base == 1 ]]; then ready+=("$base_source"); targets+=("${names[0]}"); fi
for ((i=1;i<=count;i++)); do
  target=${names[$i]}
  if [[ $ensure == 1 ]] && valid_source "$target" suspended "$cpu" "$memory"; then continue; fi
  echo "Preparing suspended slot $i/$count: $target"
  name="pulse-build-$(date -u +%Y%m%dT%H%M%SZ)-$$-slot-$i"
  staged+=("$name")
  tart clone "$base_source" "$name"
  tart set "$name" --cpu "$cpu" --memory "$memory" --random-mac
  start_guest "$name"
  ssh_run "sudo /bin/bash /opt/pulse-tart-image/prepare-template.sh '$seconds' '$os_version' 'nix git node pnpm uv python'" </dev/null > "$work/slot-$i.log" 2>&1
  grep -q '^PULSE_TEMPLATE_READY ' "$work/slot-$i.log"
  tart suspend "$name"
  wait_state "$name" suspended
  ready+=("$name");targets+=("$target")
done
# Nothing is published until every requested replacement is prepared. Each
# overwrite takes Tart's global lock, also used by concurrent clone operations.
for ((i=0;i<${#ready[@]};i++)); do
  aliases=()
  while IFS= read -r alias; do aliases+=("$alias"); done < <(tart list --source local --format json | python3 "$root/scripts/image-names.py" aliases "${targets[$i]}")
  for alias in "${aliases[@]}"; do
    if [[ $ensure == 1 ]] && tart get "$alias" --format json >/dev/null 2>&1; then continue; fi
    echo "Publishing: $alias"
    tart clone --overwrite "${ready[$i]}" "$alias"
    published=$((published+1))
    echo "Ready: $alias"
  done
done
echo "Prepared sources ready. Existing job clones were left running."
