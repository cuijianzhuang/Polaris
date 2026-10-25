# Readiness, explicit install, operation recovery and actionable trace failures.
# Real Agent commands and package-manager boundaries are covered by Go tests.
import ast
from pathlib import Path
source=Path(__file__).with_name('operations-ui-smoke.py').read_text();tree=ast.parse(source)
exec(compile(ast.Module(body=[n for n in tree.body if not isinstance(n,ast.With)],type_ignores=[]),str(Path(__file__).with_name('operations-ui-smoke.py')),'exec'))

class RouteToolFixture(Fixture):
 def __init__(self):
  super().__init__();self.tool=dict(state='missing',issue='tool_missing',canInstall=True,manager='/usr/bin/apt-get',manualCommand='apt-get update && apt-get install -y traceroute',checkedAt=now)
  self.install_count=0;self.sudo=True;self.fail_check=False;self.fail_trace=False;self.issue='permission_denied'
 def route(self,route):
  req=route.request;path=urlparse(req.url).path.removeprefix('/api/v1')
  if path in ['/nodes/n1/route-tool','/nodes/n1/route-tool/install']:
   self.requests.append((req.method,path))
   if req.method=='POST':
    self.writes.append((req.method,path,req.post_data_json,req.headers))
    if self.sudo:
     self.sudo=False;route.fulfill(status=403,content_type='application/json',body=json.dumps(dict(error='敏感操作需要重新验证身份',sudoRequired=True)));return
    self.install_count+=1;self.tool=dict(state='checking',canInstall=False,checkedAt=now,installState='installing')
   if self.fail_check:route.fulfill(status=409,content_type='application/json',body=json.dumps(dict(error='节点离线或已撤销，无法检查回程环境')));return
   route.fulfill(status=200,content_type='application/json',body=json.dumps(self.tool));return
  if path=='/nodes/n1/route-trace':
   self.writes.append((req.method,path,req.post_data_json,req.headers))
   if self.fail_trace:route.fulfill(status=502,content_type='application/json',body=json.dumps(dict(error='回程检测未完成，请检查节点状态或稍后重试')));return
   result=dict(family='ipv4',target='1.1.1.1',state='failed',issue=self.issue,raw='socket: Operation not permitted',hops=[])
   route.fulfill(status=200,content_type='application/json',body=json.dumps(dict(results=[result])));return
  if path=='/nodes/n1/route-traces/20':
   row=copy.deepcopy(trace);row['result'].update(state='failed',issue=self.issue,raw='socket: Operation not permitted')
   route.fulfill(status=200,content_type='application/json',body=json.dumps(row));return
  return super().route(route)

def txt(lang,zh,en):return zh if lang=='zh' else en

with sync_playwright() as p:
 browser=p.chromium.launch(executable_path=CHROMIUM,headless=True,args=['--no-sandbox','--disable-dev-shm-usage'])
 for width in [390,1440]:
  for lang in ['zh','en']:
   f=RouteToolFixture();page=browser.new_page(viewport=dict(width=width,height=1100),locale='zh-CN' if lang=='zh' else 'en-US');page.set_default_timeout(10000)
   page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.mode','dark')"%json.dumps(lang))
   errors=[];page.on('pageerror',lambda e:errors.append(str(e)));page.route('**/api/v1/**',f.route)
   page.goto(BASE_URL+'/nodes?network=route')
   install_name=txt(lang,'一键安装 traceroute','Install traceroute');check_name=txt(lang,'重新检查环境','Check environment again');trace_name=txt(lang,'立即检测回程','Trace route now')
   install=page.get_by_role('button',name=install_name,exact=True);expect(install).to_be_enabled()
   assert f.install_count==0,'opening the page installed a package'
   page.get_by_label(txt(lang,'IPv4 检测目标','IPv4 trace target'),exact=True).fill('1.1.1.1');expect(page.get_by_role('button',name=trace_name,exact=True)).to_be_disabled()
   install.click();dialog=page.get_by_role('dialog');expect(dialog).to_be_visible();dialog.locator('input[type=password]').fill('fixture-password');dialog.get_by_role('button',name=txt(lang,'确认','Confirm'),exact=True).click()
   expect(page.get_by_text(txt(lang,'正在节点安装 traceroute…','Installing traceroute on the node…'),exact=True)).to_be_visible();assert f.install_count==1
   assert f.last('/nodes/n1/route-tool/install')[3]['authorization']=='Bearer fixture'
   expect(page.get_by_role('button',name=install_name,exact=True)).to_have_count(0)
   # Closing/reopening retains the Agent's install state, even with dashboard refresh disabled.
   page.reload();expect(page.get_by_text(txt(lang,'正在节点安装 traceroute…','Installing traceroute on the node…'),exact=True)).to_be_visible();assert f.install_count==1
   f.tool=dict(state='missing',issue='tool_missing',canInstall=True,checkedAt=now,installState='failed',installIssue='install_failed',installOutput='repository fixture failed')
   expect(page.get_by_role('button',name=install_name,exact=True)).to_be_visible(timeout=7000)
   expect(page.get_by_role('alert').get_by_text(txt(lang,'安装失败，请检查软件源、网络或包管理器锁，修复后重试。','Installation failed. Check repositories, connectivity and package manager locks before retrying.'),exact=True)).to_be_visible()
   page.get_by_text(txt(lang,'安装日志','Installation log'),exact=True).click();expect(page.get_by_text('repository fixture failed',exact=True)).to_be_visible()
   page.get_by_role('button',name=install_name,exact=True).click();expect(page.get_by_text(txt(lang,'正在节点安装 traceroute…','Installing traceroute on the node…'),exact=True)).to_be_visible();assert f.install_count==2
   f.tool=dict(state='ready',canInstall=False,path='/usr/bin/traceroute',checkedAt=now,installState='succeeded',installOutput='installed fixture')
   expect(page.get_by_text(txt(lang,'traceroute 安装并验证成功','traceroute was installed and verified'),exact=True)).to_be_visible(timeout=7000)
   page.get_by_label(txt(lang,'IPv4 检测目标','IPv4 trace target'),exact=True).fill('1.1.1.1')
   trace_button=page.get_by_role('button',name=trace_name,exact=True);expect(trace_button).to_be_enabled();trace_button.click()
   permission=txt(lang,'工具执行权限或网络探测权限不足；请检查 Agent 运行用户和容器网络权限。','The executable or network probe lacks permission. Check the Agent user and container network permissions.')
   expect(page.get_by_text(permission,exact=True)).to_be_visible()
   page.get_by_text(txt(lang,'原始检测输出','Raw trace output'),exact=True).click();expect(page.get_by_text('socket: Operation not permitted',exact=True)).to_be_visible()
   # Saved history retains the same reason and raw evidence.
   page.get_by_role('button',name=txt(lang,'详情','Details'),exact=True).first.click();dialog=page.get_by_role('dialog');expect(dialog.get_by_text(permission,exact=True)).to_be_visible();dialog.get_by_role('button',name=txt(lang,'关闭','Close'),exact=True).first.click()
   f.fail_trace=True;trace_button.click();expect(page.get_by_role('alert').first).to_be_visible();f.fail_trace=False
   # An installed but unusable tool has self-test evidence and no pointless install action.
   f.tool=dict(state='unusable',issue='permission_denied',path='/usr/bin/traceroute',checkOutput='Operation not permitted',canInstall=False,checkedAt=now)
   page.get_by_role('button',name=check_name,exact=True).click();expect(page.get_by_text(txt(lang,'traceroute 已安装但不可用','traceroute is installed but unusable'),exact=True)).to_be_visible();expect(trace_button).to_be_disabled();expect(page.get_by_role('button',name=install_name,exact=True)).to_have_count(0)
   page.get_by_text(txt(lang,'环境自检输出','Environment self-test output'),exact=True).click();expect(page.get_by_text('Operation not permitted',exact=True)).to_be_visible()
   no_overflow(page);page.screenshot(path=str(OUT/f'route-tool-{lang}-{width}.png'),full_page=True,animations='disabled',style='.toasts { visibility: hidden; }')
   # The new command is optional for older Agents: existing traces still work.
   f.tool=dict(state='unsupported',issue='agent_upgrade_required',agentVersion='1.2.3',panelVersion='2.0.0',canInstall=False,checkedAt=now)
   page.get_by_role('button',name=check_name,exact=True).click();expect(trace_button).to_be_enabled()
   expect(page.get_by_text(txt(lang,'尚未检查 traceroute 安装状态','traceroute installation has not been checked'),exact=True)).to_be_visible()
   expect(page.get_by_text('1.2.3',exact=True)).to_be_visible();expect(page.get_by_text('2.0.0',exact=True)).to_be_visible()
   expect(page.get_by_role('button',name=install_name,exact=True)).to_have_count(0)
   f.fail_check=True;page.get_by_role('button',name=check_name,exact=True).click();expect(page.get_by_text(txt(lang,'节点离线或已撤销，无法检查回程环境','The node is offline or revoked; the route environment cannot be checked'),exact=True)).to_be_visible()
   assert not errors,errors;page.close();print(width,lang,'automatic check, explicit sudo install, recovery polling, retry, native failure evidence and legacy compatibility passed',flush=True)
 browser.close()
