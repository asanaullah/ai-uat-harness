#!/usr/bin/env bash
# Assisted by Claude Opus
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: ./deploy.sh -c clusters/<cluster>.yml [-b tests/batches/<batch>.yml] [-t test[,test...]] [options] [-- ansible-playbook options]

Runs a batch and/or a list of tests and keeps everything about the run under
runs/<run-id>/: metadata, step records, timesheet, pod logs, failure
diagnostics, and the results copied back from the cluster.

What to run:
  -c FILE     cluster config (an inventory under clusters/), required
  -b FILE     batch playbook to run
  -t LIST     tests to run, comma-separated, e.g. fio,iperf3 (after the batch if both are given)

Options for every test in the run:
  -m MODE     node, serial, pairs or project
  -f POLICY   continue, skipTest or abort
  -n NODES    only these nodes, comma-separated
  -d          dry run: render and record every manifest, create nothing

Where to run:
  -r RUN_ID   run id (default: the UTC time, YYYYmmdd_HHMMSS)
  -p          run in a runner pod on the cluster instead of from this machine
  -g URL      with -p: the runner pod clones this git repo instead of using the local files
  -G REF      with -g: branch or tag to clone
  -k          with -p: start the runner pod and return without waiting
  -F RUN_ID   copy a finished runner-pod run back from the cluster (with -c)
  -h          show this help
EOF
}

command_line="$0 $*"
cluster="" batch="" tests="" mode="" policy="" nodes="" dry=false
run_id="" in_pod=false repo="" ref="" detach=false fetch=""
while getopts "c:b:t:m:f:n:dr:pg:G:kF:h" opt; do
  case "$opt" in
    c) cluster="$OPTARG" ;;
    b) batch="$OPTARG" ;;
    t) tests="$OPTARG" ;;
    m) mode="$OPTARG" ;;
    f) policy="$OPTARG" ;;
    n) nodes="$OPTARG" ;;
    d) dry=true ;;
    r) run_id="$OPTARG" ;;
    p) in_pod=true ;;
    g) repo="$OPTARG" ;;
    G) ref="$OPTARG" ;;
    k) detach=true ;;
    F) fetch="$OPTARG" ;;
    h) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
shift $((OPTIND - 1))

args=("$@")
[ -n "$cluster" ] || { echo "-c is required" >&2; usage >&2; exit 2; }
[ -f "$cluster" ] || { echo "cluster config not found: $cluster" >&2; exit 2; }
[ -z "$batch" ] || [ -f "$batch" ] || { echo "batch not found: $batch" >&2; exit 2; }
cluster="$(realpath "$cluster")"
[ -z "$batch" ] || batch="$(realpath "$batch")"
cd "$(dirname "$(realpath "$0")")"
[ -d collections/ansible_collections/kubernetes/core ] || { echo "collections missing; run ./build.sh first" >&2; exit 1; }

runs_dir="${UAT_RUNS_DIR:-$PWD/runs}"
if [ -n "$fetch" ]; then
  exec ansible-playbook -i "$cluster" -e "uat_run_id=$fetch" -e "uat_runs_dir=$runs_dir" plugins/playbooks/fetch.yml ${args[@]+"${args[@]}"}
fi

run_id="${run_id:-$(date -u +%Y%m%d_%H%M%S)}"
run_dir="$runs_dir/$run_id"
mkdir -p "$run_dir/meta"

vars="$run_dir/meta/deploy-vars.json"
python3 - "$vars" "$run_id" "$runs_dir" "$mode" "$policy" "$nodes" "$dry" "$command_line" <<'PY'
import json, sys
path, run_id, runs_dir, mode, policy, nodes, dry, command = sys.argv[1:9]
v = {"uat_run_id": run_id, "uat_runs_dir": runs_dir, "uat_command_line": command}
if mode: v["uat_mode"] = mode
if policy: v["uat_on_failure"] = policy
if nodes: v["uat_only_nodes"] = nodes
if dry == "true": v["uat_dry_run"] = True
json.dump(v, open(path, "w"), indent=2)
PY

playbooks=()
[ -n "$batch" ] && playbooks+=("$batch")
for t in ${tests//,/ }; do
  [ -f "tests/$t.yml" ] || { echo "no such test: tests/$t.yml" >&2; exit 2; }
  playbooks+=("tests/$t.yml")
done
[ ${#playbooks[@]} -gt 0 ] || { echo "nothing to run: give -b and/or -t" >&2; exit 2; }

# Relative to the repository root, which is the working directory here (GNU
# realpath --relative-to is not on macOS).
relpath() { python3 -c 'import os, sys; print(os.path.relpath(sys.argv[1]))' "$1"; }

if $in_pod; then
  for f in "$cluster" ${batch:+"$batch"}; do
    case "$f" in "$PWD"/*) ;; *) echo "with -p, $f must be inside this repository" >&2; exit 2 ;; esac
  done
  pod_args=(-c "$(relpath "$cluster")" -r "$run_id")
  [ -n "$batch" ] && pod_args+=(-b "$(relpath "$batch")")
  [ -n "$tests" ] && pod_args+=(-t "$tests")
  [ -n "$mode" ] && pod_args+=(-m "$mode")
  [ -n "$policy" ] && pod_args+=(-f "$policy")
  [ -n "$nodes" ] && pod_args+=(-n "$nodes")
  $dry && pod_args+=(-d)
  python3 - "$vars" "$repo" "$ref" "$detach" "${pod_args[@]}" <<'PY'
import json, shlex, sys
path, repo, ref, detach = sys.argv[1:5]
v = json.load(open(path))
v.update({"uat_runner_args": shlex.join(sys.argv[5:]), "uat_runner_repo": repo,
          "uat_runner_ref": ref, "uat_runner_detach": detach == "true"})
json.dump(v, open(path, "w"), indent=2)
PY
  export ANSIBLE_LOG_PATH="$run_dir/runner-ansible.log"
  exec ansible-playbook -i "$cluster" -e "@$vars" plugins/playbooks/runner.yml ${args[@]+"${args[@]}"}
fi

export ANSIBLE_LOG_PATH="$run_dir/ansible.log"
exec ansible-playbook -i "$cluster" -e "@$vars" \
  plugins/playbooks/start.yml "${playbooks[@]}" plugins/playbooks/finish.yml ${args[@]+"${args[@]}"}
