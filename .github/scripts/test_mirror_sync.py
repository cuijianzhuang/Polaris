import importlib.util
import contextlib
import io
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock
import urllib.request

spec=importlib.util.spec_from_file_location('mirror',str(Path(__file__).with_name('mirror-sync.py')))
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
SOURCE='cuijianzhuang/Polaris';TARGET='PolarisPanelOrg/Polaris'
def release(tag='v1',date='2020-01-01T00:00:00Z',pre=False,assets=None):
 return dict(id=10,tag_name=tag,name=None,body='Notes',draft=False,prerelease=pre,created_at=date,published_at=date,assets=assets or [])

class FakeAPI:
 def __init__(self,assets=None,bad_download=False):
  self.calls=[];self.assets=assets or [];self.bad_download=bad_download
 def request(self,path,method='GET',data=None,**kw):
  self.calls.append((path,method,data))
  if '/git/ref/' in path:return {'object':{'sha':'same'}}
  if method=='POST' and path.endswith('/releases'):return dict(id=20,upload_url='https://uploads.github.com/repos/PolarisPanelOrg/Polaris/releases/20/assets{?name,label}')
  if method=='PATCH' and '/releases/' in path:return dict(id=20,upload_url='https://uploads.github.com/repos/PolarisPanelOrg/Polaris/releases/20/assets{?name,label}')
  return None
 def pages(self,path):return self.assets
 def download(self,*args):
  Path(args[-1]).write_bytes(b'bad');return (3,'sha256:bad')

class MirrorTests(unittest.TestCase):
 def test_git_temporary_transport_failure_is_retried_safely(self):
  failed=subprocess.CompletedProcess([],128,b'',b'RPC failed; HTTP 502 fixture-secret')
  ok=subprocess.CompletedProcess([],0,b'',b'')
  log=io.StringIO()
  with mock.patch.object(m.subprocess,'run',side_effect=[failed,ok]) as run, mock.patch.object(m.time,'sleep') as sleep, contextlib.redirect_stdout(log):
   m.run_git(['git','fetch'],'fetch',{})
  self.assertEqual(run.call_count,2);sleep.assert_called_once_with(1)
  self.assertIn('retry 2/3',log.getvalue());self.assertNotIn('fixture-secret',log.getvalue())
 def test_git_retry_limit_and_permanent_refusals(self):
  samples=[(b'RPC failed; curl 92 HTTP/2 stream reset',3,'transport'),
   (b'Authentication failed for https://user:fixture-secret@github.com/repo',1,'access denied'),
   (b'refusing to allow a Personal Access Token to update workflow without workflow scope',1,'Workflows'),
   (b'GH013: Repository rule violations found',1,'repository rules'),
   (b'[rejected] main -> main (non-fast-forward)',1,'history differs'),
   (b'[rejected] v1 -> v1 (already exists)',1,'history differs'),
   (b'SSL certificate problem: certificate verification failed',1,'TLS'),
   (b'Unclassified fixture-secret error',1,'unrecognized')]
  for output,attempts,hint in samples:
   with self.subTest(output=output),mock.patch.object(m.subprocess,'run',return_value=subprocess.CompletedProcess([],1,b'',output)) as run, mock.patch.object(m.time,'sleep') as sleep, contextlib.redirect_stdout(io.StringIO()) as log:
    with self.assertRaises(m.SyncError) as caught:m.run_git(['git','push'],'push',{})
    self.assertEqual(run.call_count,attempts);self.assertEqual(sleep.call_count,attempts-1)
    self.assertIn(hint,str(caught.exception));self.assertNotIn('fixture-secret',str(caught.exception)+log.getvalue())
 def test_git_timeout_is_not_retried_or_echoed(self):
  with mock.patch.object(m.subprocess,'run',side_effect=subprocess.TimeoutExpired(['fixture-secret'],900)) as run, mock.patch.object(m.time,'sleep') as sleep, contextlib.redirect_stdout(io.StringIO()):
   with self.assertRaisesRegex(m.SyncError,'fetch timed out') as caught:m.run_git(['git','fetch'],'fetch',{})
  run.assert_called_once();sleep.assert_not_called();self.assertNotIn('fixture-secret',str(caught.exception))
 def test_git_push_remains_atomic_without_force_or_prune(self):
  with mock.patch.object(m,'run_git') as run:m.git_sync(SOURCE,TARGET,'fixture-source','fixture-target')
  self.assertEqual([call.args[1] for call in run.call_args_list],['init','fetch','push'])
  command=run.call_args_list[-1].args[0]
  self.assertIn('--atomic',command)
  self.assertFalse(any(arg.startswith(('--force','--prune','+')) for arg in command))
  self.assertNotIn('fixture-source',' '.join(command));self.assertNotIn('fixture-target',' '.join(command))
 @unittest.skipUnless(shutil.which('git'),'Git required')
 def test_local_tag_conflict_does_not_partially_update_destination(self):
  env=dict(os.environ,GIT_CONFIG_GLOBAL=os.devnull,GIT_CONFIG_NOSYSTEM='1')
  def git(*args):
   return subprocess.run(['git','-c','user.name=Fixture','-c','user.email=fixture@example.invalid',*map(str,args)],env=env,check=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE).stdout.decode().strip()
  with tempfile.TemporaryDirectory() as tmp:
   work=Path(tmp,'source');dest=Path(tmp,'mirror.git')
   git('init','-b','main',work);git('init','--bare',dest)
   Path(work,'file').write_text('first');git('-C',work,'add','file');git('-C',work,'commit','-m','first');git('-C',work,'tag','v1')
   first=git('-C',work,'rev-parse','HEAD')
   command=['git','-C',str(work),'push','--atomic',str(dest),'refs/heads/*:refs/heads/*','refs/tags/*:refs/tags/*']
   with contextlib.redirect_stdout(io.StringIO()):m.run_git(command,'push',env)
   Path(work,'file').write_text('second');git('-C',work,'commit','-am','second');git('-C',work,'tag','-f','v1')
   with contextlib.redirect_stdout(io.StringIO()),self.assertRaisesRegex(m.SyncError,'history differs'):m.run_git(command,'push',env)
   self.assertEqual(git('--git-dir',dest,'rev-parse','refs/heads/main'),first)
   self.assertEqual(git('--git-dir',dest,'rev-parse','refs/tags/v1'),first)
 def test_pair_allowlist(self):
  for repo in m.NAMES:m.validate_pair('cuijianzhuang/'+repo,'PolarisPanelOrg/'+repo)
  for a,b in [(SOURCE,'attacker/Polaris'),('PolarisPanelOrg/Polaris',SOURCE),('cuijianzhuang/other','PolarisPanelOrg/other')]:
   with self.assertRaises(m.SyncError):m.validate_pair(a,b)
 def test_latest_initial_future_versions_and_existing_history(self):
  old=release('old');stable=release('stable','2022-01-01T00:00:00Z');pre=release('pre','2023-01-01T00:00:00Z',True)
  future=release('future-old-version','2025-01-01T00:00:00Z');old_edit=release('old-edit',assets=[{'updated_at':'2025-01-01T00:00:00Z'}])
  items=[old,stable,pre,future,old_edit]
  selected=m.select_releases(items,[],SOURCE,'2024-01-01T00:00:00Z','stable')
  self.assertEqual({r['tag_name'] for r in selected},{'stable','pre','future-old-version','old-edit'})
  target=dict(old,body=m.mirror_body(old,SOURCE))
  selected=m.select_releases(items,[target],SOURCE,'2024-01-01T00:00:00Z','stable')
  self.assertIn('old',{r['tag_name'] for r in selected})
 def test_manual_event_can_import_older_release(self):
  items=[release('old'),release('stable')]
  self.assertEqual(len(m.select_releases(items,[],SOURCE,'2024-01-01T00:00:00Z','stable','old')),2)
 def test_marker_preserves_text_and_nested_previous_comment(self):
  r=release();r['body']='Title\n<!-- polaris-mirror:{"source":"earlier"} -->'
  body=m.mirror_body(r,SOURCE)
  self.assertTrue(body.startswith(r['body']));self.assertEqual(m.marker(body)['source'],SOURCE)
 def test_same_size_changed_source_is_not_treated_as_synced(self):
  asset={'id':1,'size':5,'name':'a','updated_at':'new','digest':None}
  old={'id':1,'size':5,'updated_at':'old','digest':None}
  self.assertFalse(m.same_asset(asset,{'size':5,'digest':None},old))
 def test_checksum_verification_prevents_publication_and_false_provenance(self):
  asset={'id':1,'size':6,'name':'a','updated_at':'new','digest':'sha256:good'}
  source=FakeAPI();target=FakeAPI();r=release(assets=[asset])
  with tempfile.TemporaryDirectory() as tmp:
   with self.assertRaises(m.SyncError):m.sync_release(r,None,SOURCE,TARGET,source,target,'v1',tmp)
  staged=next(data for path,method,data in target.calls if method=='POST')
  self.assertTrue(staged['draft']);self.assertEqual(m.marker(staged['body'])['assets'],{})
  self.assertFalse(any(method=='PATCH' and not data.get('draft',True) for _,method,data in target.calls))
 def test_idempotent_complete_release_does_not_write(self):
  r=release();existing=dict(r,id=20,name='v1',body=m.mirror_body(r,SOURCE))
  source,target=FakeAPI(),FakeAPI()
  with tempfile.TemporaryDirectory() as tmp:self.assertFalse(m.sync_release(r,existing,SOURCE,TARGET,source,target,'v1',tmp))
  self.assertFalse(any(method!='GET' for _,method,_ in target.calls))
 def test_unmanaged_release_is_never_overwritten(self):
  with tempfile.TemporaryDirectory() as tmp:
   with self.assertRaises(m.SyncError):m.sync_release(release(),{'body':'manual'},SOURCE,TARGET,FakeAPI(),FakeAPI(),'v1',tmp)
 def test_visibility_mismatch_blocks_copy_if_update_not_confirmed(self):
  api=FakeAPI()
  with self.assertRaises(m.SyncError):
   m.sync_visibility({'private':True},{'private':False,'full_name':TARGET},api)
  self.assertEqual(api.calls[0][2],{'private':True})
 def test_cross_host_redirect_strips_auth(self):
  req=urllib.request.Request('https://api.github.com/repos/x/releases/assets/1',headers={'Authorization':'Bearer fixture-secret'})
  moved=m.SafeRedirect().redirect_request(req,None,302,'',{},'https://release-assets.githubusercontent.com/path')
  self.assertIsNone(moved.get_header('Authorization'))
  with self.assertRaises(m.SyncError):m.SafeRedirect().redirect_request(req,None,302,'',{},'https://attacker.example/path')

if __name__=='__main__':unittest.main()
