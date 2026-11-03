<!-- Assisted by Claude Opus -->
# AI UAT Harness

Acceptance tests for AI platforms on OpenShift, written as Ansible playbooks. Each test is a playbook that starts the test's pods and other objects through Red Hat's `kubernetes.core` collection, plus a Ginkgo test that is compiled on the cluster and run inside a pod. A run records everything it did and copies the results back: run metadata, one record per step, a timesheet, pod logs, failure diagnostics, and the JUnit and benchmark output the tests write.

## Layout

| Path | Contents |
|---|---|
| `tests/` | One playbook (`<test>.yml`) and one Ginkgo test (`<test>.go`) per test, and `batches/` with playbooks that run several tests in order. [`tests/README.md`](tests/README.md) describes each test. |
| `clusters/` | One Ansible inventory per cluster: the nodes and their expected hardware, the test and peer namespaces, the PVCs, the runner settings and, for a cluster that oac-apps does not manage, the settings for `setup/namespaces.yml`. |
| `plugins/` | The shared roles every test uses (`roles/`), the filters they need (`filter/`), and the plays that start a run, plan each test, finish a run and drive the runner pod (`playbooks/`). |
| `setup/` | One-time cluster preparation: namespaces, PVCs and access (for clusters that oac-apps does not manage), model downloads, image pre-pulls. |
| `build.sh` | Prepares everything: installs the collections, checks the Python library, syntax-checks every playbook and, with `-c`, compiles the Go tests on a cluster. |
| `deploy.sh` | Runs a batch or a list of tests, from this machine or from a runner pod on the cluster. |

## Requirements

- `ansible-core` 2.16 or newer (tested with 2.18) and the Python `kubernetes` library for the Python that Ansible runs on (Fedora and RHEL: `python3-kubernetes`; pip: `kubernetes`).
- A login to the cluster (`oc login`), or a pod service account when running in the cluster.
- For a run from this machine (without `-p`), Linux: the run reads `/proc` and unpacks the results with GNU tar. From macOS, run with `-p`, which runs the harness in a pod on the cluster.
- In the test namespaces: create, get, list, watch and delete on pods, `pods/log` and services; create and get on `pods/exec`; list on events; and the APIs the tests use (KubeVirt VMs, InferenceServices, Kubeflow TrainJobs), with get on the objects those make (VMIs, JobSets, batch Jobs) so the log streamer can find their pods. The `uat-harness` chart in oac-apps grants these, and on clusters that oac-apps does not manage, `setup/namespaces.yml` does.

## Quick start

```bash
./build.sh                                     # collections, python check, syntax checks
./build.sh -c clusters/oac-dev-workload0.yml   # also compile the Go tests on the cluster

./deploy.sh -c clusters/oac-dev-workload0.yml -b tests/batches/no-gpu.yml
./deploy.sh -c clusters/oac-dev-workload0.yml -t fio,iperf3
./deploy.sh -c clusters/oac-dev-workload0.yml -t iperf3 -m node -n moc-r4pcc02u05
./deploy.sh -c clusters/oac-dev-workload0.yml -b tests/batches/all.yml -d   # render only
./deploy.sh -c clusters/oac-dev-workload0.yml -b tests/batches/no-gpu.yml -p   # from a runner pod
```

`./deploy.sh -h` lists every option. Anything after `--` is passed to `ansible-playbook`.

A batch can also be run with plain `ansible-playbook -i clusters/<cluster>.yml tests/batches/<batch>.yml`, which is what an Ansible Automation Platform job template would run. `deploy.sh` adds the run id, the log file and the options above.

## How a run works

1. **Start.** Creates `runs/<run-id>/`, records provenance (git commit and dirty state, tool versions, cluster and login), and deletes objects left in the test namespaces by earlier runs (anything labelled `app.kubernetes.io/managed-by=uat-harness`). Objects that are already being deleted are left alone. If harness pods from another run are still Pending or Running in those namespaces, the run stops instead of deleting them; `-- -e uat_preflight_force=true` deletes them anyway.
2. **Build.** Each test builds its own Go binary when it is planned, so a run builds only its own tests. It is built in each namespace where one of its steps runs the binary (a step with `labelFilter`): `uat-project` for every test today, and `uat-peer` too for ping. The first test that needs a namespace starts a builder pod there, which stays up for the rest of the run and is deleted by the finish play. The builder compiles `tests/<test>.go` onto the namespace's PVC under `<basePath>/builds/<test>/<hash>/`. The hash covers the Go source, the Ginkgo version and the builder image (plus the cluster spec for tests that embed it), so an unchanged test is not rebuilt. A test that does not compile is recorded as failed and none of its pods start; the other tests still run, unless its policy is `abort`.
3. **Tests.** For each test, a planning play numbers the test, picks its mode and nodes, and adds one in-memory Ansible host per sequence. The test's play then runs once per sequence: in parallel for `node` mode, one at a time for `serial` and `pairs`.
4. **Finish.** A collector pod in each namespace aggregates the JUnit reports into `report/summary.json` and packs the run's results, which are copied back and unpacked under `runs/<run-id>/results/`. The step records are merged into `steps.json`, `timesheet.csv` and `summary.txt`, and the run fails (non-zero exit) when any step failed, timed out or errored, or a JUnit report has failures.

### Modes

| Mode | Sequences | Runs | Node placement |
|---|---|---|---|
| `node` | one per node | in parallel | every step on that node |
| `serial` | one per node | one after another | every step on that node |
| `pairs` | one per ordered pair of nodes | one after another | a step goes on the node at its `nodeIndex` (0 or 1) |
| `project` | one | once | no node selector |

Each test lists the modes it supports in `uat_modes`; the first is the default. `-n` limits the nodes for a run.

### Failure policies

Set per test with `uat_on_failure` (or `-f` for the whole run):

- `continue`: a failed step is recorded and the remaining steps still run.
- `skipTest`: the rest of that sequence is skipped; teardown still runs; other sequences and tests carry on.
- `abort`: like `skipTest`, then every later test is skipped and recorded as skipped. Collection and the summary still run.

## What a run records

```
runs/<run-id>/
  meta/run.json          run id, cluster, command, provenance, start/finish, final status
  meta/<cluster>.yml     the cluster config used
  meta/cluster.yaml      the cluster spec compiled into tests that embed it
  meta/builds.json       the build hash of each test in the run
  meta/deploy-vars.json  the options deploy.sh passed
  ansible.log            the full Ansible output
  tests/<tid>.json       each test: mode, policy, timeout, sequences and their nodes, or why it was skipped
  steps/<step>.json      each step: Kubernetes name, namespace, nodes, status, phase, exit code, reason, times, results path
  logs/<step>/           manifest.yaml (what was applied), <container>.log, diag.yaml (pod status and events on failure),
                         and for a resource step <pod>.<container>.log for each pod it made (a VM's launcher pod, a job's pods)
  logs/streamer.log      what the log streamer followed, and when
  logs/<NNN>-<test>-build-<project|peer>.log   the compile output (with a step record of the same name)
  logs/collect-*.log, logs/preflight-*.json
  meta/streams/          the log streamer's heartbeat, a .done file per finished container log, and summary.json
  results/<project|peer>/  the results directory copied back from each PVC, with report/summary.json
  steps.json, timesheet.csv, summary.txt
```

Pod logs are streamed while the run goes, by one process per run (`plugins/roles/uat_run/files/pod_logs.py`). It follows every container of every pod the run creates, and of the pods those objects make, and writes each log byte for byte as it arrives. A dropped stream is resumed without repeating lines, and a restarted container is appended after a `--- <container> restarted ---` line. Each step starts the streamer if it is not running, and waits for a finished pod's logs to be complete before deleting it; the step record says whether they were (`logs_complete`). The finish play stops the streamer.

Kubernetes objects get short names built from IDs: `uat-<tid>-<seq>-<step>[-<sweep>]`, for example `uat-t003-q02-s01-w04` is test 3, sequence 2, step 1, sweep entry 4. The step records map each name to the readable step name used for the log and results directories: `<test id>-<test>-<node or node pair>-<step>[-<sweep entry>]`, for example `003-fio-moc-r4pcc02u05-fio-runner-seq-read-1`. On the PVC, each step writes to `<basePath>/<run-id>/<step name>/`.

## Cluster config

Each file in `clusters/` is an Ansible inventory. The nodes under `uat_nodes` are the nodes tests run on; `componentValidation` holds their expected hardware, which the component test checks and some tests read (for example the GPU count).

```yaml
all:
  vars:
    uat_cluster_name: oac-dev-workload0
    uat_compliance: {fipsEnabled: false}
    uat_namespace: uat-project
    uat_storage: {pvc: uat-project-storage, basePath: uat/results, models: {pvc: uat-models}}
    uat_peer_namespace: uat-peer
    uat_peer_storage: {pvc: uat-peer-storage, basePath: uat/results, models: {pvc: uat-models}}
    uat_runner: {namespace: uat-runner, serviceAccount: uat-runner-sa, pvc: uat-runner-storage}
  children:
    uat_nodes:
      vars: {ansible_connection: local}
      hosts:
        moc-r4pcc02u05:
          componentValidation: {sanity: {...}, ideal: {...}}
```

The peer and runner settings are optional. A cluster that oac-apps does not manage also needs `uat_setup` for `setup/namespaces.yml`: the storage class, and the groups and users who get access (see `clusters/ocp-test.yml`). On an oac-apps cluster the `uat-harness` chart sets these, so its file has no `uat_setup`. The tool settings in `plugins/roles/uat_config/defaults/main.yml` (images, timeouts, labels) can be overridden in a cluster file, for example `uat_builder_image` for a mirrored registry.

## Writing a test

A test is `tests/<name>.yml` and, when it has Ginkgo steps, `tests/<name>.go`. The playbook has two plays: the planning import, and the test play that runs the steps in order through the shared roles.

```yaml
- name: Plan mytest
  ansible.builtin.import_playbook: ../plugins/playbooks/sequences.yml
  vars:
    uat_test: mytest          # must match tests/mytest.go
    uat_modes: [node, serial]
    uat_on_failure: continue  # optional
    uat_timeout: 600          # optional, per pod that runs to completion

- name: mytest
  hosts: "{{ hostvars.localhost.uat_hosts | default('uat_none') }}"
  serial: "{{ hostvars.localhost.uat_serial | default(0) }}"
  gather_facts: false
  vars:
    serverConfig:
      model: /models/ibm-granite/granite-3.3-8b-instruct
  tasks:
    - name: Start the sequence
      ansible.builtin.import_role:
        name: uat_sequence

    - name: Steps
      block:
        - name: server
          ansible.builtin.include_role:
            name: uat_pod
          vars:
            step:
              name: server
              image: nvcr.io/nvidia/vllm:26.03-py3
              command:
                args: [python, -m, vllm.entrypoints.openai.api_server]
                flags: {model: "{{ serverConfig.model }}", port: 8000}
              service: {enabled: true, port: 8000, name: server, headless: true}
              readinessProbe: {httpGet: {path: /health, port: 8000}}
              resources: {requests: {nvidia.com/gpu: "1"}, limits: {nvidia.com/gpu: "1"}}
              persistsThroughSweep: true

        - name: client
          ansible.builtin.include_role:
            name: uat_pod
          vars:
            step:
              name: client
              image: registry.redhat.io/ubi9/ubi:9.8
              labelFilter: pass-fail
              env:
                - {name: SERVER_URL, value: '{{ services["server"].url }}'}
      rescue:
        - name: Handle the failure
          ansible.builtin.import_role:
            name: uat_sequence
            tasks_from: failed
      always:
        - name: Tear down
          ansible.builtin.import_role:
            name: uat_sequence
            tasks_from: teardown
```

Any other Ansible task can go between the steps.

### Adding a test

1. Write `tests/<name>.yml` from the template above, or copy a test with similar steps. `uat_test` must be `<name>`, and `uat_modes` lists the modes the test supports, default first.
2. If a step runs the Ginkgo binary (`labelFilter`), write `tests/<name>.go`: a Ginkgo v2 suite in `package test` whose specs carry the labels the steps filter on (most tests use `pass-fail` and `quantitative`). The builder compiles it as `test_test.go` in a module of its own with `go mod tidy`, so it can import only the standard library and public modules. The step runs it as `test.bin --ginkgo.label-filter=<labelFilter>` with the step's `env` and `RESULTS_DIR=/uat_workspace`, and its JUnit report goes to `/uat_workspace/junit.xml`. A test that needs the cluster spec embeds it with `//go:embed cluster.yaml`, as `tests/component.go` does.
3. If a resource step creates a kind that no other test uses, grant it in `uat-user-role` in the test namespace (the `uat-harness` chart in oac-apps, and `setup/templates/namespaces.yaml.j2`), and add it to `uat_cleanup_kinds` in `plugins/roles/uat_config/defaults/main.yml` so preflight deletes what an earlier run left.
4. Add the test to the batches in `tests/batches/` that should run it, and describe it in `tests/README.md`.
5. Check it with `./build.sh`, then a dry run (`./deploy.sh -c clusters/<cluster>.yml -t <name> -d`, which records every manifest under `runs/<run-id>/logs/` and creates nothing), then a real run (`./deploy.sh -c clusters/<cluster>.yml -t <name>`). A run compiles each test when it plans it; `./build.sh -c clusters/<cluster>.yml` compiles every test ahead of time.

### Pod steps (`uat_pod`)

| Key | Meaning |
|---|---|
| `name`, `image` | Step name (also the container name) and image. Required. |
| `labelFilter` | Run the test's Ginkgo binary with this label filter; JUnit goes to `/uat_workspace/junit.xml` and `RESULTS_DIR=/uat_workspace` is set. |
| `command` | `args` plus `flags`, rendered as `--key=value` (`--key` for a null value). Used when there is no `labelFilter`. |
| `parameterSweep` | `baseCommand` (`args`, `flags`) and `entries` (`id`, `description`, `flags` merged over the base flags). One pod per entry, run one after another; `paramSweep.command` holds the entry's command. |
| `persistsThroughSweep` | `true` for a long-running pod: the step waits until it is Ready, and it stays up until the sequence's teardown. Otherwise the step waits for the pod to finish and then deletes it. |
| `service` | `enabled`, `port`, `name`, `headless`. Registers `services[name]` with `name`, `url` and `port` for later steps. |
| `peer` | Run in the peer namespace, with the peer PVC. |
| `nodeIndex` | Which node of a `pairs` sequence the step runs on (default 0). |
| `env`, `ports`, `readinessProbe`, `resources`, `volumes`, `volumeMounts`, `sidecars`, `labels`, `privileged`, `serviceAccountName`, `timeout` | Passed to the pod; `sidecars` become native sidecar init containers; `timeout` overrides the test's timeout for this step. |

Every pod mounts its own results directory at `/uat_workspace`, the test binaries at `/binaries`, the sequence's shared scratch directory at `/uat_scratchpad`, and the models PVC read-only at `/models`.

### Resource steps (`uat_resource`)

`resourceConfig` with `apiVersion`, `kind`, `spec` and optional `annotations` creates any other object (a VirtualMachine, a Service, an InferenceService, a training job). It gets the harness labels, `resource_name` is set to its name, and the sequence teardown deletes it. The logs of the pods it makes are streamed too (see "What a run records"): pods it owns directly or through up to two other objects (a VM's launcher pod through its VMI, a TrainJob's pods through their Job and its JobSet), and pods that carry its labels (an InferenceService's predictor, if KServe copies its labels).

### Variables in step definitions

Step values are Jinja templates, filled in when the step runs:

| Variable | Value |
|---|---|
| `serverConfig` | The test play's own settings. |
| `nodeSpec` | The step's node from the cluster config: `name` and `componentValidation`. |
| `node`, `namespace` | The step's node name ('' in project mode) and namespace. |
| `services` | Services registered by earlier steps of the sequence. |
| `resource_name`, `resources` | The latest resource step's name, and every resource step by step name. |
| `paramSweep` | The current sweep entry: `id` and `command`. |
| `scratchpad` | `pvc`, `subPath` and `mountPath` of the sequence's scratch directory, for resource steps to mount. |
| `uat_*` | Run settings, for example `uat_namespace`, `uat_peer_namespace`, `uat_managed_by`. |

## Batches

A batch is a playbook of `import_playbook` lines in run order, ending with the finish play. Values set on an import override the test's defaults for that entry, and the same test can appear more than once.

```yaml
- name: fio
  ansible.builtin.import_playbook: ../fio.yml
  vars:
    uat_mode: serial

- name: iperf3
  ansible.builtin.import_playbook: ../iperf3.yml

- name: Finish
  ansible.builtin.import_playbook: ../../plugins/playbooks/finish.yml
```

`deploy.sh -t a,b` runs tests without a batch, each with its defaults.

## Cluster setup

```bash
ansible-playbook -i clusters/<cluster>.yml setup/namespaces.yml --check --diff   # compare
ansible-playbook -i clusters/<cluster>.yml setup/namespaces.yml                  # needs cluster-admin
ansible-playbook -i clusters/<cluster>.yml setup/models.yml                      # downloads the models
ansible-playbook -i clusters/<cluster>.yml setup/prewarm-images.yml              # optional
```

- `namespaces.yml` is for clusters that oac-apps does not manage. It creates the test, peer and runner namespaces, their NetworkPolicies, the PVCs, the Roles and RoleBindings for the groups and users in `uat_setup`, and the runner service account. On an oac-apps cluster such as oac-dev-workload0 the `uat-harness` chart creates these objects, and the cluster file has no `uat_setup`, so the playbook stops at its first check.
- `models.yml` downloads `uat_models` onto the models PVC in each namespace with a pod, and onto the VM disk PVC (`uat-models-virt`) with a VM that powers off when done.
- `prewarm-images.yml` starts a pod per node and image so each node pulls the images the tests use, reports any that did not pull, and deletes the pods. `-e '{"uat_prewarm_images": [...]}'` limits the images.

## Running in the cluster

`deploy.sh -p` starts a runner pod in `uat_runner.namespace` as `uat_runner.serviceAccount` and copies this checkout into it (or, with `-g URL [-G REF]`, the pod clones that repository). The pod installs ansible-core, the Python `kubernetes` library and the collections, then runs the same `deploy.sh` with the run directory on `uat_runner.pvc`. By default `deploy.sh` waits, saves the runner log, copies the run directory back to `runs/<run-id>/` and deletes the pod. With `-k` it returns right away; `deploy.sh -c <cluster> -F <run-id>` copies the run back later.
