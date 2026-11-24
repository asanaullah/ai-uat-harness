# Assisted by Claude Opus
from __future__ import annotations

import json
import re

IMAGE = re.compile(r"[Ii]mage: *[\"']?([^\s\"'{}]+)")


def _text(value):
    if isinstance(value, (list, dict)):
        return json.dumps(value)
    return str(value)


def uat_command(args, flags=None):
    """Return args followed by --key=value for each flag (--key when the value is null)."""
    cmd = [_text(a) for a in (args or [])]
    for key, value in (flags or {}).items():
        if value is None:
            cmd.append(f"--{key}")
        else:
            cmd.append(f"--{key}={_text(value)}")
    return cmd


def uat_env(env, extra=None):
    """Kubernetes env list with every plain value as a string, plus extra entries whose names are not already set."""
    result = []
    for entry in env or []:
        item = dict(entry)
        if "value" in item:
            item["value"] = "" if item["value"] is None else _text(item["value"])
        result.append(item)
    names = {item.get("name") for item in result}
    for item in extra or []:
        if item["name"] not in names:
            result.append(dict(item))
    return result


def uat_selector(labels):
    """Render a label dict as a comma-separated selector."""
    return ",".join(f"{k}={v}" for k, v in labels.items())


def uat_sequences(nodes, mode):
    """Sequences for a test: one per node (node, serial), one per ordered node pair (pairs), or one without a node (project)."""
    nodes = list(nodes or [])
    if mode == "project":
        sets = [[]]
    elif mode == "pairs":
        sets = [[a, b] for a in nodes for b in nodes if a != b]
    elif mode in ("node", "serial"):
        sets = [[n] for n in nodes]
    else:
        raise ValueError(f"unknown mode {mode!r}")
    return [
        {"qid": f"q{i:02d}", "nodes": s, "label": "_".join(s)}
        for i, s in enumerate(sets, 1)
    ]


PULL_FAILED = {
    "ErrImagePull",
    "ImagePullBackOff",
    "ErrImageNeverPull",
    "InvalidImageName",
    "ImageInspectError",
    "RegistryUnavailable",
}
PULLING = {"", "ContainerCreating", "PodInitializing"}


def _waiting_reason(pod):
    statuses = (pod.get("status") or {}).get("containerStatuses") or []
    if not statuses:
        return ""
    return ((statuses[0].get("state") or {}).get("waiting") or {}).get("reason", "")


def uat_pulling(pods):
    """Names of pods whose image is still being pulled."""
    return [
        p["metadata"]["name"]
        for p in pods or []
        if (p.get("status") or {}).get("phase") == "Pending"
        and _waiting_reason(p) in PULLING
    ]


def uat_pull_failed(pods):
    """Names and reasons of pods whose image could not be pulled."""
    return [
        f"{p['metadata']['name']} ({_waiting_reason(p)})"
        for p in pods or []
        if _waiting_reason(p) in PULL_FAILED
    ]


def uat_images(texts):
    """Sorted, de-duplicated container image references found in the given file contents."""
    found = set()
    for text in texts or []:
        found.update(m.group(1) for m in IMAGE.finditer(text) if "/" in m.group(1))
    return sorted(found)


def uat_enumerate(items):
    """[[0, a], [1, b], ...]"""
    return [[i, x] for i, x in enumerate(items or [])]


def _steps(node):
    if isinstance(node, dict):
        if isinstance(node.get("step"), dict):
            yield node["step"]
        for value in node.values():
            yield from _steps(value)
    elif isinstance(node, list):
        for value in node:
            yield from _steps(value)


def uat_go_places(paths, playbooks):
    """Map each test to the namespaces ('peer', 'project') whose steps run its Go binary (steps with a labelFilter), following imports of other test playbooks."""
    plays = {
        p.rsplit("/", 1)[-1].rsplit(".", 1)[0]: book
        for p, book in zip(paths or [], playbooks or [])
    }

    def places(name, seen):
        found = set()
        if name in seen or name not in plays:
            return found
        seen.add(name)
        for play in plays[name] or []:
            target = (
                play.get("ansible.builtin.import_playbook")
                or play.get("import_playbook")
                if isinstance(play, dict)
                else None
            )
            if isinstance(target, str) and "/" not in target:
                found |= places(target.rsplit(".", 1)[0], seen)
        for step in _steps(plays[name]):
            if step.get("labelFilter"):
                found.add("peer" if step.get("peer") else "project")
        return found

    return {name: sorted(places(name, set())) for name in plays}


def uat_subjects(groups, users=None):
    """RBAC subjects for the given group and user names."""
    api = "rbac.authorization.k8s.io"
    return [{"kind": "Group", "name": g, "apiGroup": api} for g in groups or []] + [
        {"kind": "User", "name": u, "apiGroup": api} for u in users or []
    ]


def uat_active_runs(pods, run_id):
    """Runs with a pod still going, excluding this run and its runner pod."""
    runs = []
    for pod in pods or []:
        metadata = pod.get("metadata") or {}
        if metadata.get("deletionTimestamp"):
            continue
        if (pod.get("status") or {}).get("phase") not in ("Pending", "Running"):
            continue
        labels = metadata.get("labels") or {}
        run = labels.get("uat/run", "")
        if not run or run == run_id:
            continue
        if run not in runs:
            runs.append(run)
    return sorted(runs)


class FilterModule:
    def filters(self):
        return {
            "uat_command": uat_command,
            "uat_env": uat_env,
            "uat_selector": uat_selector,
            "uat_sequences": uat_sequences,
            "uat_pulling": uat_pulling,
            "uat_pull_failed": uat_pull_failed,
            "uat_images": uat_images,
            "uat_enumerate": uat_enumerate,
            "uat_go_places": uat_go_places,
            "uat_subjects": uat_subjects,
            "uat_active_runs": uat_active_runs,
        }
