import importlib.util, unittest, tempfile, json, zipfile
from pathlib import Path
spec=importlib.util.spec_from_file_location('diagnostics',Path(__file__).parent/'browser_support'/'diagnostics.py')
module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
class DiagnosticsTests(unittest.TestCase):
 def test_network_log_redacts_credentials(self):
  result=module.safe_url('https://user:password@example.test/sub/private-token?token=secret&email=person%40example.test#private')
  self.assertEqual(result,dict(origin='https://example.test',path='/sub/<redacted>',queryKeys=['email','token']))
  self.assertNotIn('password',str(result));self.assertNotIn('secret',str(result))
 def test_invalid_url_is_safe(self):self.assertEqual(module.safe_url('http://[invalid')['path'],'<invalid>')

 def test_trace_retains_actions_without_secrets(self):
  with tempfile.TemporaryDirectory() as directory:
   path=Path(directory)/'trace.zip'
   events=[dict(type='context-options',options={'viewport':{'width':390},'httpCredentials':{'password':'SENSITIVE'}}),dict(type='before',apiName='locator.fill',callId='call@1',params={'value':'SENSITIVE'}),dict(type='after',callId='call@1',result={'token':'SENSITIVE'})]
   with zipfile.ZipFile(path,'w') as target:
    target.writestr('0.trace','\n'.join(json.dumps(e) for e in events))
    target.writestr('0.network','SENSITIVE');target.writestr('resources/response','SENSITIVE')
   module.sanitize_trace(path)
   with zipfile.ZipFile(path) as source:
    content=b'\n'.join(source.read(name) for name in source.namelist())
    self.assertNotIn(b'SENSITIVE',content);self.assertIn(b'locator.fill',content)
    self.assertNotIn('resources/response',source.namelist())
