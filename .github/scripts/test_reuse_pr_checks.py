import copy
import importlib.util
import io
import os
import subprocess
import tempfile
import unittest
import zipfile
from contextlib import redirect_stdout
from pathlib import Path
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('reuse', Path(__file__).with_name('reuse-pr-checks.py'))
reuse = importlib.util.module_from_spec(spec)
spec.loader.exec_module(reuse)
REPO = 'cuijianzhuang/Polaris'
TREE = 'a' * 40
HEAD = 'b' * 40
SHA = 'c' * 40


def archive(tree=TREE, filename='validated-tree.txt', extra=False):
    out = io.BytesIO()
    with zipfile.ZipFile(out, 'w') as proof:
        proof.writestr(filename, tree + '\n')
        if extra:
            proof.writestr('extra.txt', 'unexpected')
    return out.getvalue()


class ReuseTests(unittest.TestCase):
    def setUp(self):
        self.run = dict(id=123, run_attempt=2, event='pull_request', path='.github/workflows/ci.yml',
                        head_sha=HEAD, head_repository=dict(full_name=REPO), status='completed', conclusion='success')
        self.artifact = dict(id=456, name='pr-validated-tree-2', expired=False, size_in_bytes=200)
        self.proof = archive()
        self.requests = []

    def request(self, path, binary=False):
        self.requests.append(path)
        if '/workflows/ci.yml/runs?' in path:
            return dict(workflow_runs=[self.run])
        if '/runs/123/artifacts' in path:
            return dict(artifacts=[self.artifact] if self.artifact else [])
        self.assertEqual(path, f'repos/{REPO}/actions/artifacts/456/zip')
        self.assertTrue(binary)
        return self.proof

    def check(self):
        return reuse.reusable_run(REPO, TREE, HEAD, self.request)

    def test_reuses_successful_same_repository_exact_tree_and_attempt(self):
        self.assertEqual(self.check(), 123)
        self.assertIn(f'head_sha={HEAD}', self.requests[0])

    def test_untrusted_incomplete_or_unrelated_runs_do_not_download_proofs(self):
        original = copy.deepcopy(self.run)
        for field, value in [('event', 'push'), ('path', '.github/workflows/other.yml'),
                             ('head_sha', 'd'*40), ('head_repository', dict(full_name='fork/Polaris')),
                             ('head_repository', None), ('status', 'in_progress'), ('conclusion', 'failure')]:
            with self.subTest(field=field, value=value):
                self.run = dict(original, **{field: value})
                self.requests.clear()
                self.assertIsNone(self.check())
                self.assertEqual(len(self.requests), 1)

    def test_missing_expired_old_attempt_or_oversize_proofs_run_full_suite(self):
        original = copy.deepcopy(self.artifact)
        for artifact in [None, dict(original, expired=True), dict(original, name='pr-validated-tree-1'),
                         dict(original, size_in_bytes=5000)]:
            with self.subTest(artifact=artifact):
                self.artifact = artifact
                self.assertIsNone(self.check())

    def test_changed_tree_and_unexpected_zip_entries_are_rejected(self):
        for proof in [archive('d'*40), archive('a'), archive(filename='../validated-tree.txt'), archive(extra=True)]:
            with self.subTest(proof=proof):
                self.proof = proof
                self.assertIsNone(self.check())

    def invoke(self, event='push', ref='refs/heads/main', failure=None, parents=None):
        with tempfile.TemporaryDirectory() as tmp:
            output = Path(tmp) / 'output'
            env = dict(GITHUB_EVENT_NAME=event, GITHUB_REF=ref, GITHUB_SHA=SHA,
                       GITHUB_REPOSITORY=REPO, GITHUB_OUTPUT=str(output))
            with patch.dict(os.environ, env, clear=True), redirect_stdout(io.StringIO()), \
                 patch.object(reuse, 'git', side_effect=[SHA, parents or f'{"d"*40} {HEAD}', TREE]) as git, \
                 patch.object(reuse, 'reusable_run', return_value=123, side_effect=failure) as verify:
                reuse.main()
            return output.read_text(), git.call_count, verify.call_count

    def test_manual_tags_and_direct_pushes_always_run_full_suite(self):
        for event, ref in [('workflow_dispatch', 'refs/heads/main'), ('push', 'refs/tags/v1.0.0')]:
            self.assertEqual(self.invoke(event, ref), ('reuse=false\n', 0, 0))
        self.assertEqual(self.invoke(parents=HEAD), ('reuse=false\n', 2, 0))

    def test_main_merge_uses_proof_but_api_and_archive_errors_fall_back(self):
        self.assertEqual(self.invoke(), ('reuse=true\n', 3, 1))
        for error in [subprocess.TimeoutExpired('gh', 10), subprocess.CalledProcessError(1, 'gh'),
                      zipfile.BadZipFile(), ValueError('invalid'), KeyError('missing')]:
            with self.subTest(error=error):
                self.assertEqual(self.invoke(failure=error)[0], 'reuse=false\n')


if __name__ == '__main__':
    unittest.main()
