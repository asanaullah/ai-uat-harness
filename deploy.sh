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
  -c FILE     cluster config (an inventory under clusters/); -a defaults to clusters/uat-admin.yml
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
  -g URL      with -p or -a: the runner pod clones this git repo instead of using the local files
  -G REF      with -g: branch or tag to clone
  -k          with -p or -a: start the runner pod and return without waiting
  -F RUN_ID   copy a finished runner-pod run back from the cluster (with -c)
  -a          admin run: implies -p; local copies use oc --as; runner and tests use uat-admin
  -h          show this help
EOF
}

command_line="$0 $*"
cluster="" batch="" tests="" mode="" policy="" nodes="" dry=false
run_id="" in_pod=false repo="" ref="" detach=false fetch="" admin=false
while getopts "c:b:t:m:f:n:dr:pag:G:kF:h" opt; do
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
    a) admin=true ;;
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
if $admin && [ -z "$cluster" ]; then
  cluster="$(dirname "$(realpath "$0")")/clusters/uat-admin.yml"
fi
[ -n "$cluster" ] || { echo "-c is required" >&2; usage >&2; exit 2; }
[ -f "$cluster" ] || { echo "cluster config not found: $cluster" >&2; exit 2; }
[ -z "$batch" ] || [ -f "$batch" ] || { echo "batch not found: $batch" >&2; exit 2; }
cluster="$(realpath "$cluster")"
[ -z "$batch" ] || batch="$(realpath "$batch")"
cd "$(dirname "$(realpath "$0")")"
[ -d collections/ansible_collections/kubernetes/core ] || { echo "collections missing; run ./build.sh first" >&2; exit 1; }

if $admin; then
  if ! oc --as system:admin auth can-i create pods -n uat-admin >/dev/null 2>&1; then
    echo "-a needs a kubeconfig that can act as system:admin and create pods in uat-admin" >&2
    exit 2
  fi
  # The Kubernetes Python client forwards only authorization and
  # sec-websocket-protocol on exec WebSockets, so Impersonate-User is lost
  # in k8s_cp and k8s_exec. The admin staging and fetch below use oc --as;
  # the REST calls in runner.yml stay in the collection.
  admin_vars='{"uat_namespace":"uat-admin","uat_peer_namespace":"","uat_storage":{"pvc":"uat-admin-storage","basePath":"uat/results"},"uat_runner":{"namespace":"uat-admin","serviceAccount":"uat-admin-sa","pvc":"uat-admin-storage"}}'
  export K8S_AUTH_IMPERSONATE_USER=system:admin
  args=(-e "$admin_vars" "${args[@]}")
  in_pod=true
fi

runs_dir="${UAT_RUNS_DIR:-$PWD/runs}"
admin_phase=""
admin_pod=""

# Admin staging and fetch use oc --as. The Kubernetes Python client forwards
# only authorization and sec-websocket-protocol on exec WebSockets, so
# Impersonate-User is lost in k8s_cp and k8s_exec. The REST calls in
# runner.yml stay in the collection.
admin_slug() {
  python3 -c 'import re, sys; print(re.sub(r"[^a-z0-9]+", "-", sys.argv[1].lower()))' "$1"
}

admin_stage() {
  local pod src
  if [ -n "$repo" ]; then
    return 0
  fi
  pod="uat-runner-$(admin_slug "$run_id")"
  src="$(mktemp)"
  tar czf "$src" --exclude=./.git --exclude=./collections --exclude=./runs -C "$PWD" .
  oc --as system:admin cp -n uat-admin -c runner "$src" "$pod:/src/src.tgz"
  oc --as system:admin exec -n uat-admin -c runner "$pod" -- sh -c \
    'mkdir -p /src/repo && tar xzf /src/src.tgz -C /src/repo && touch /src/.ready'
  rm -f "$src"
}

admin_wait() {
  local pod phase i
  pod="uat-runner-$(admin_slug "$run_id")"
  admin_pod="$pod"
  for i in $(seq 1 8640); do
    if ! phase="$(oc --as system:admin -n uat-admin get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)"; then
      admin_phase="Missing"
      return 0
    fi
    case "$phase" in
      Succeeded|Failed) admin_phase="$phase"; return 0 ;;
    esac
    sleep 10
  done
  admin_phase="Timeout"
}

admin_fetch() {
  local id slug pod fetchpod run_dir manifest phase
  id="$1"
  slug="$(admin_slug "$id")"
  pod="uat-runner-$slug"
  fetchpod="uat-fetch-$slug"
  run_dir="$runs_dir/$id"
  if phase="$(oc --as system:admin -n uat-admin get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)"; then
    case "$phase" in
      Pending|Running)
        echo "Run $id is still running; follow it with: oc --as system:admin logs -f $pod -n uat-admin" >&2
        return 2
        ;;
    esac
  fi
  mkdir -p "$run_dir"
  manifest="$(mktemp)"
  cat > "$manifest" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $fetchpod
  namespace: uat-admin
  labels:
    app.kubernetes.io/managed-by: uat-harness
    uat/run: $id
spec:
  serviceAccountName: uat-admin-sa
  restartPolicy: Never
  containers:
    - name: fetch
      image: python:3-slim
      command: [sleep, infinity]
      volumeMounts:
        - name: workspace
          mountPath: /uat_workspace
  volumes:
    - name: workspace
      persistentVolumeClaim:
        claimName: uat-admin-storage
EOF
  oc --as system:admin apply -f "$manifest"
  rm -f "$manifest"
  oc --as system:admin -n uat-admin wait --for=condition=Ready "pod/$fetchpod" --timeout=600s
  oc --as system:admin exec -n uat-admin -c fetch "$fetchpod" -- sh -c \
    "tar czf /tmp/run.tgz -C /uat_workspace/runs/$id ."
  oc --as system:admin cp -n uat-admin -c fetch "$fetchpod:/tmp/run.tgz" "$run_dir/run.tgz"
  tar xzf "$run_dir/run.tgz" -C "$run_dir"
  rm -f "$run_dir/run.tgz"
  if oc --as system:admin -n uat-admin get pod "$pod" >/dev/null 2>&1; then
    oc --as system:admin -n uat-admin logs "$pod" > "$run_dir/runner.log" 2>&1 || true
  fi
  oc --as system:admin -n uat-admin delete pod "$fetchpod" "$pod" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  if [ -f "$run_dir/summary.txt" ]; then
    echo
    sed -n '1,120p' "$run_dir/summary.txt"
  fi
  echo
  echo "Run directory: $run_dir"
}

if [ -n "$fetch" ]; then
  if $admin; then
    admin_fetch "$fetch"
    exit $?
  fi
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
  $admin && pod_args+=(-- -e "$admin_vars")
  python3 - "$vars" "$repo" "$ref" "$detach" "${pod_args[@]}" <<'PY'
import json, shlex, sys
path, repo, ref, detach = sys.argv[1:5]
v = json.load(open(path))
v.update({"uat_runner_args": shlex.join(sys.argv[5:]), "uat_runner_repo": repo,
          "uat_runner_ref": ref, "uat_runner_detach": detach == "true"})
json.dump(v, open(path, "w"), indent=2)
PY
  export ANSIBLE_LOG_PATH="$run_dir/runner-ansible.log"
  if $admin; then
    ansible-playbook -i "$cluster" -e "@$vars" \
      -e uat_runner_skip_copy=true -e uat_runner_detach=true -e uat_admin_run=true \
      plugins/playbooks/runner.yml ${args[@]+"${args[@]}"}
    admin_stage
    if $detach; then
      pod="uat-runner-$(admin_slug "$run_id")"
      echo "Runner pod uat-admin/$pod started for run $run_id."
      echo "Follow it with: oc --as system:admin logs -f $pod -n uat-admin"
      echo "Copy it back later with: ./deploy.sh -a -F $run_id"
      exit 0
    fi
    admin_wait
    admin_fetch "$run_id"
    [ "$admin_phase" = "Succeeded" ] || exit 1
    exit 0
  fi
  exec ansible-playbook -i "$cluster" -e "@$vars" plugins/playbooks/runner.yml ${args[@]+"${args[@]}"}
fi

export ANSIBLE_LOG_PATH="$run_dir/ansible.log"
exec ansible-playbook -i "$cluster" -e "@$vars" \
  plugins/playbooks/start.yml "${playbooks[@]}" plugins/playbooks/finish.yml ${args[@]+"${args[@]}"}
