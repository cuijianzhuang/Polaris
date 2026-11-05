"""Reuse PR browser/PostgreSQL checks only with a successful run's exact-tree proof.

Missing, expired or unverifiable evidence falls back to the full browser/PostgreSQL suites.
Go, private pro sources and real-core tests are always revalidated at release.
"""

import io
import json
import os
import re
import subprocess
import zipfile


def git(*args):
    return subprocess.check_output(["git", *args], text=True, timeout=10).strip()


def api(path, *, binary=False):
    result = subprocess.run(
        ["gh", "api", path], capture_output=True, check=True, timeout=10
    ).stdout
    return result if binary else json.loads(result)


def proof_matches(archive, tree):
    with zipfile.ZipFile(io.BytesIO(archive)) as proof:
        if proof.namelist() != ["validated-tree.txt"]:
            return False
        entry = proof.getinfo("validated-tree.txt")
        if entry.file_size != 41:
            return False
        return proof.read(entry) == (tree + "\n").encode("ascii")


def reusable_run(repo, tree, head, request=api):
    runs = request(
        f"repos/{repo}/actions/workflows/ci.yml/runs"
        f"?event=pull_request&status=success&head_sha={head}&per_page=3"
    )["workflow_runs"]
    for run in runs[:3]:
        if not (
            run.get("event") == "pull_request"
            and run.get("path") == ".github/workflows/ci.yml"
            and run.get("head_sha") == head
            and (run.get("head_repository") or {}).get("full_name") == repo
            and run.get("status") == "completed"
            and run.get("conclusion") == "success"
        ):
            continue
        run_id = int(run["id"])
        name = f"pr-validated-tree-{int(run['run_attempt'])}"
        artifacts = request(f"repos/{repo}/actions/runs/{run_id}/artifacts")["artifacts"]
        matches = [a for a in artifacts if a["name"] == name and not a["expired"]]
        if len(matches) != 1 or matches[0]["size_in_bytes"] > 4096:
            continue
        artifact_id = int(matches[0]["id"])
        archive = request(f"repos/{repo}/actions/artifacts/{artifact_id}/zip", binary=True)
        if proof_matches(archive, tree):
            return run_id
    return None


def main():
    run_id = None
    reason = "Full browser/PostgreSQL suites: no verified PR run for this release tree."
    try:
        # Manual releases and tags may rebuild changed external dependencies.
        if os.environ.get("GITHUB_EVENT_NAME") == "push" and os.environ.get("GITHUB_REF") == "refs/heads/main":
            sha = os.environ["GITHUB_SHA"]
            if not re.fullmatch(r"[0-9a-f]{40}", sha) or git("rev-parse", "HEAD") != sha:
                raise ValueError("checkout mismatch")
            parents = git("show", "-s", "--format=%P", sha).split()
            # Regular two-parent GitHub PR merges only; direct/squash pushes run everything.
            if len(parents) == 2:
                run_id = reusable_run(
                    os.environ["GITHUB_REPOSITORY"],
                    git("rev-parse", "HEAD^{tree}"), parents[1],
                )
    except (OSError, ValueError, KeyError, TypeError, AttributeError, RuntimeError, subprocess.SubprocessError, zipfile.BadZipFile):
        reason = "Full browser/PostgreSQL suites: PR verification unavailable or invalid."
    if run_id:
        reason = f"Browser/PostgreSQL checks already passed for the exact release tree in PR CI run {run_id}; Go/pro/core checks still run."
    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
        output.write(f"reuse={'true' if run_id else 'false'}\n")
    if os.environ.get("GITHUB_STEP_SUMMARY"):
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as summary:
            summary.write(reason + "\n")
    print(reason)


if __name__ == "__main__":
    main()
