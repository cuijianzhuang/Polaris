import importlib.util, unittest
from pathlib import Path
spec=importlib.util.spec_from_file_location('ci_scope',Path(__file__).with_name('ci-scope.py'))
scope=importlib.util.module_from_spec(spec);spec.loader.exec_module(scope)

class ScopeTests(unittest.TestCase):
 def test_docs_only(self):
  result=scope.select(['docs/user-guide.md','README.md'])
  self.assertFalse(result['go']);self.assertFalse(result['browser']);self.assertFalse(result['full'])
 def test_release_is_always_full(self):
  self.assertTrue(scope.select(['README.md'],'workflow_call')['full'])
 def test_backend_and_unknown_are_full(self):
  for path in ['internal/core/mihomo/meter.go','api/proto/polaris/v1/agent.proto','.github/workflows/ci.yml','Dockerfile','web/src/new-shared.ts','web/src/pages/NewPage.tsx']:
   self.assertTrue(scope.select([path])['full'],path)
 def test_page_selects_related_suites(self):
  result=scope.select(['web/src/pages/Nodes.tsx'])
  self.assertFalse(result['go']);self.assertTrue(result['web'])
  self.assertEqual({r['suite'] for r in result['matrix']['include']},{'nodes-users','settings-state'})
 def test_script_and_settings(self):
  result=scope.select(['web/scripts/settings-workflow-smoke.py','web/src/pages/settings/Updates.tsx'])
  self.assertTrue(result['portal']);self.assertEqual(len(result['matrix']['include']),2)
 def test_unknown_mixed_with_docs_does_not_skip(self):
  self.assertTrue(scope.select(['docs/help.md','deploy/new-config'])['full'])
 def test_empty_fails_closed(self):self.assertTrue(scope.select([])['full'])
