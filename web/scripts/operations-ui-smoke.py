# Browser regression for operational controls. Uses mocked API responses;
# this does not replace real proxy or public-network acceptance tests.
# Prerequisites: Python playwright and Chromium. Start Vite separately, then:
#   POLARIS_WEB_TEST_URL=http://127.0.0.1:5187 python3 web/scripts/operations-ui-smoke.py
import json, copy, os, shutil
from pathlib import Path
from datetime import datetime, timedelta, timezone
from urllib.parse import urlparse, parse_qs
from playwright.sync_api import sync_playwright, expect

BASE_URL=os.environ.get('POLARIS_WEB_TEST_URL', 'http://127.0.0.1:5187').rstrip('/')
CHROMIUM=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium')
OUT=Path(os.environ.get('POLARIS_UI_ARTIFACTS', '/tmp/polaris-operations-ui')); OUT.mkdir(exist_ok=True)
now=datetime.now(timezone.utc).isoformat()
node=dict(id='n1',name='Hong Kong',region='HK',labels=[],cores=[dict(id='c1',nodeId='n1',type='xray',version='26.5.9',status='running',installedVersions=[])],inboundCount=1,status='online',desiredRev=1,appliedRev=1,agentVersion='1.0',address='203.0.113.1',publicIpv4='203.0.113.1',publicIpv6='',domain='',lastSeen=now,connected=True,netHealth={},system=dict(cpu=1,mem=2,uptime=900,rxBps=0,txBps=0),defaultCore='xray',trafficRate=1)
ib=dict(id='i1',nodeId='n1',nodeName='Hong Kong',tag='reality',core='xray',port=443,listen='::',protocol=dict(type='vless',flow='xtls-rprx-vision'),transport=dict(type='tcp'),security=dict(type='reality',realityDest='www.example.com:443'),sniffing=dict(enabled=True),enabled=True,userCount=1,endpoints=1,network='tcp',groupIds=[],outboundTag='',speedLimitMbps=0,remark='',outboundIPStrategy='prefer_ipv6',relayOnly=True,relayAccess=dict(enabled=False,proxyProtocol=0))
caps=[dict(core='xray',version='26.5.9',combos=[dict(protocol='vless',transport='tcp',security='reality')],features=[],hotUsers=True)]
trace=dict(id=20,family='ipv4',target='1.1.1.1',source='manual',state='completed',startedAt=now,result=dict(state='reached',raw='RAW fixture traceroute\n1 1.1.1.1',hops=[dict(ttl=1,addresses=['1.1.1.1'],asn=[dict(ip='1.1.1.1',state='matched',asn=13335,organization='Cloudflare')])],asn=dict(state='ready',databaseSha256='a'*64,databaseType='GeoLite2-ASN',buildEpoch=1)),comparison=dict(state='baseline',changedTtls=[]))
relay=dict(businessDiagnostic=dict(supported=True,requiresRestart=False),id='r1',name='HK relay',remark='',disabled=False,batchId='b1',mode='l4',hops=[dict(nodeId='n2',port=20443),dict(nodeId='n1',inboundId='i1')],address='203.0.113.2',port=20443,shareName='',shareAddress='',sharePort=0,state='applied',nodes=[dict(nodeId='n2',name='Tokyo',state='applied',desiredRev=1,appliedRev=1)])
port=dict(nodeId='n2',port=20443,network='tcp',public=True,engine='realm')
impact=dict(action='disable',etag='"'+'d'*64+'"',blocked=False,chainIds=['r1'],changedPaths=1,remainingRelayPaths=1,remainingEnabledRelayPaths=0,nodes=[dict(id='n2',name='Tokyo',status='online',otherPaths=0)],ports=[port],references=[],warnings=[],endpointIds=['e1'],outboundIds=[],routingRuleIds=[],serviceUserIds=[])
operational=dict(available=True,checkedAt=now,issueCount=1,truncated=False,relayBusiness=dict(total=1,passed=0,failed=0,untested=1,stale=0,running=0,interrupted=0,unavailable=0,inactive=0),sampling=dict(healthy=1,stale=0,never=1,inactive=0,pending=0,legacy=0),delivery=dict(paymentPending=0,paymentReview=1,refundPending=0,clusterPending=0),issues=[dict(kind='payment',resourceId='payment-1',label='Review fixture',state='review',code='payment_review')])

class Fixture:
 def __init__(self):
  self.writes=[]; self.requests=[]; self.scoped=False; self.schedule_conflict=False; self.impact_conflict=False; self.asn_sudo=False; self.asn_unauthorized=False; self.blocked=False
  self.policy=dict(mode='custom',targets=[dict(id='target-id',name='Shanghai',carrier='ct',addr='test.example.com:80',enabled=True)],effectiveTargets=[dict(name='Shanghai',carrier='ct',addr='test.example.com:80')])
  self.schedule=dict(policy=dict(enabled=False,targets={},intervalMinutes=60,revision=4),runtime=None,agentSupported=True,online=True,pausedByGlobalProbeSwitch=False)
  self.asn=dict(policy=dict(enabled=False,sha256='',revision=0),databaseState='not_configured',database=None,effectiveEnabled=False)
  self.notification=dict(policy=dict(repeatWindowSeconds=300,channels={},maintenance=[]),channels=['telegram','email','webhook'],events=dict(node_offline=dict(paired=True,recovery=False),route_changed=dict(paired=False,recovery=False,defaultOff=True)))
 def route(self,route):
  req=route.request; u=urlparse(req.url); path=u.path.removeprefix('/api/v1'); self.requests.append((req.method,path))
  body=None
  if req.method in ['POST','PUT','DELETE']:
   try: body=req.post_data_json
   except Exception: body=req.post_data_buffer
   self.writes.append((req.method,path,body,req.headers))
  result=[]; status=200
  nodes=[copy.deepcopy(node),dict(copy.deepcopy(node),id='n2',name='Tokyo',address='203.0.113.2',inboundCount=0)]
  if path=='/auth/me': result=dict(username='admin',**(dict(nodeIds=['n1']) if self.scoped else {}))
  elif path=='/auth/sudo': result={'ok':True}
  elif path=='/info': result=dict(title='Polaris',version='1.0',edition='pro')
  elif path=='/nodes': result=nodes
  elif path=='/license': result=dict(edition='pro',state=dict(plan='pro',features=[],warnings=[],limits={}),usage={})
  elif path=='/inbounds': result=[ib]
  elif path=='/inbounds/i1' and req.method=='PUT': result=body
  elif path=='/inbounds/i1/endpoints': result=[]
  elif path=='/capabilities': result=caps
  elif path=='/auth/github': result=dict(enabled=False)
  elif path in ['/settings','/core-latest','/stats/nodes']: result={}
  elif path=='/nodes/n1/metrics': result={'points':[]}
  elif path=='/nodes/n1/probe-policy':
   if req.method=='PUT': self.policy=dict(body,effectiveTargets=body.get('targets',[]))
   result=self.policy
  elif path=='/nodes/n1/route-schedule':
   if req.method=='PUT':
    if self.schedule_conflict: status=409; result={'error':'回程定时配置已变化，请刷新后重试'}; self.schedule_conflict=False
    else: self.schedule['policy']=dict(body,revision=5)
   if status==200: result=self.schedule
  elif path=='/nodes/n1/route-tool': result=dict(state='ready',canInstall=False,path='/usr/bin/traceroute',checkedAt=now)
  elif path=='/nodes/n1/route-traces': result=dict(items=[trace],nextBefore=20 if not parse_qs(u.query).get('before') else 0)
  elif path=='/nodes/n1/route-traces/20': result=trace
  elif path=='/nodes/n1/route-traces/19':
   result=copy.deepcopy(trace); result['id']=19; result['result']['hops'][0]['addresses']=['8.8.8.8']
  elif path=='/nodes/n1/route-trace': result={'history':[trace]}
  elif path=='/notifications/policy':
   if req.method=='PUT': self.notification['policy']=body
   result=self.notification
  elif path=='/tools/route-asn/database':
   assert req.headers['content-type']=='application/octet-stream'
   assert req.post_data_buffer==b'MMDB fixture bytes'
   self.asn['policy']=dict(enabled=False,sha256='a'*64,revision=1); self.asn['databaseState']='ready'; self.asn['database']=dict(databaseType='GeoLite2-ASN',buildEpoch=1,ipVersion=6); result=self.asn
  elif path=='/tools/route-asn':
   if req.method=='PUT': self.asn['policy']['enabled']=body['enabled']; self.asn['effectiveEnabled']=body['enabled']
   result=self.asn
  elif path=='/inbounds/i1/relays': result=[relay]
  elif path=='/inbounds/i1/relays/batch/preview': result=dict(planToken='SEALED_PLAN',batchId='new-batch',expiresAt=(datetime.now(timezone.utc)+timedelta(minutes=5)).isoformat(),warnings=['Fixture warning'],entries=[dict(chain=dict(id='new-relay',name='Preview path',hops=[]),address='203.0.113.2',port=20443,ports=[port])])
  elif path=='/inbounds/i1/relays/batch': result={'batchId':'new-batch','chains':[relay]}
  elif path.endswith('/change-preview'): result=dict(impact,action=body['action'],blocked=self.blocked,references=[dict(kind='outbound',resourceId='referenced-resource')] if self.blocked else [])
  elif path.endswith('/state') or req.method=='DELETE':
   if self.impact_conflict: status=412; result={'error':'预览后配置已变化，请重新预览'}; self.impact_conflict=False
   else: result={}
  elif path.endswith('/metadata'): result=dict(relay,**body)
  elif path=='/inbounds/i1/relay-batches/b1': result=dict(batchId='b1',state='applied',chains=[relay])
  elif path.endswith('/diagnose/business'): result=dict(state='passed',businessVerified=True,cleanupVerified=True,checks=[dict(kind='credential_cleanup',state='passed')])
  elif path.endswith('/diagnose'): result=dict(state='partial',businessVerified=False,checks=[dict(kind='deployment',state='passed'),dict(kind='proxy_business',state='not_tested')])
  elif path=='/stats/overview': result=dict(operational=operational,nodes=dict(online=2,total=2,timeout=0),users=dict(online=0,active=0,total=0),today={},month={},rate={},trend=[],topUsers=[],attention=dict(warnDays=7,expiring=0,limited=0,expired=0,near=0),cores={})
  route.fulfill(status=status,content_type='application/json',body=json.dumps(result))
 def last(self,path): return next(w for w in reversed(self.writes) if w[1]==path)

def open_page(browser,path,width=1440):
 f=Fixture(); page=browser.new_page(viewport=dict(width=width,height=1000),locale='zh-CN',timezone_id='Asia/Shanghai')
 page.set_default_timeout(10000)
 page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang','zh');localStorage.setItem('polaris.refresh','0')")
 page.route('**/api/v1/**',f.route)
 errors=[]; page.on('pageerror',lambda e:errors.append(str(e)))
 page.goto(BASE_URL+path)
 return page,f,errors

def no_overflow(page): assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth+1')

def route_changed_control(page):
 page.get_by_text('按事件自定义',exact=True).click()
 group=page.locator('.notification-details > .ops-summary').filter(has=page.get_by_role('combobox',name='回程路径变化',exact=True))
 group.get_by_text('细分渠道',exact=True).click()
 return page.get_by_role('combobox',name='回程路径变化 · Telegram',exact=True)


with sync_playwright() as p:
 browser=p.chromium.launch(executable_path=CHROMIUM,headless=True,args=['--no-sandbox','--disable-dev-shm-usage'])
 for width in [390,1440]:
  page,f,errors=open_page(browser,'/nodes',width)
  page.get_by_role('button',name='测试点策略',exact=True).click()
  expect(page.get_by_label('节点测试点策略',exact=True)).to_have_value('custom')
  page.get_by_role('button',name='保存测试点策略',exact=True).click()
  expect(page.get_by_role('button',name='保存测试点策略',exact=True)).to_be_enabled()
  saved=f.last('/nodes/n1/probe-policy')[2]; assert saved==dict(mode='custom',targets=[dict(name='Shanghai',carrier='ct',addr='test.example.com:80')]),saved
  page.get_by_label('节点测试点策略',exact=True).select_option('off'); page.get_by_role('button',name='保存测试点策略',exact=True).click(); expect(page.get_by_role('button',name='保存测试点策略',exact=True)).to_be_enabled(); assert f.last('/nodes/n1/probe-policy')[2]==dict(mode='off')
  page.get_by_role('button',name='回程与定时检测',exact=True).click()
  page.get_by_label('IPv4 检测目标',exact=True).fill('1.1.1.1'); page.get_by_role('switch',name='定时回程检测',exact=True).click()
  page.get_by_role('button',name='保存定时策略',exact=True).click(); expect(page.get_by_role('button',name='保存定时策略',exact=True)).to_be_enabled(); assert f.last('/nodes/n1/route-schedule')[2]['expectedRevision']==4
  page.get_by_role('button',name='立即检测回程',exact=True).click(); expect(page.get_by_role('button',name='立即检测回程',exact=True)).to_be_enabled(); assert f.last('/nodes/n1/route-trace')[2]['ipv4']=='1.1.1.1'
  page.get_by_role('button',name='详情',exact=True).click(); expect(page.get_by_text('AS13335 · Cloudflare',exact=True)).to_be_visible(); page.get_by_text('原始检测输出',exact=True).click(); expect(page.locator('pre.ops-raw')).to_contain_text('RAW fixture')
  page.screenshot(path=str(OUT/f'route-detail-{width}.png')); page.keyboard.press('Escape'); expect(page.get_by_role('dialog')).to_have_count(0)
  page.get_by_role('button',name='更早记录',exact=True).click(); expect(page.get_by_role('button',name='更早记录',exact=True)).to_be_disabled()
  no_overflow(page); assert not errors,errors; page.close()
  print(width,'probe policy, schedule, manual trace, ASN history and pagination OK',flush=True)

  page,f,errors=open_page(browser,'/settings#notifications',width)
  control=route_changed_control(page); expect(control).to_have_value('off'); control.select_option('all')
  page.get_by_text('告警合并与维护静默',exact=True).click()
  page.get_by_role('button',name='添加维护窗口',exact=True).click()
  page.get_by_role('button',name='保存通知策略',exact=True).first.click(); expect(page.get_by_text('设置已保存',exact=True)).to_be_visible()
  saved=f.last('/notifications/policy')[2]; assert saved['channels']['telegram']['route_changed'] is True
  assert saved['channels'].get('email',{}).get('route_changed') is None
  assert saved['maintenance'][0]['startsAt'].endswith('Z'); assert saved['maintenance'][0]['nodeIds']==[]
  no_overflow(page); page.screenshot(path=str(OUT/f'notifications-{width}.png')); assert not errors,errors; page.close()
  print(width,'notification defaults, channel isolation and maintenance timestamps OK',flush=True)

  page,f,errors=open_page(browser,'/settings#network',width)
  expect(page.get_by_role('switch',name='启用 ASN 注释',exact=True)).to_be_disabled()
  page.get_by_label('上传 ASN MMDB',exact=True).set_input_files(dict(name='asn.mmdb',mimeType='application/octet-stream',buffer=b'MMDB fixture bytes'))
  expect(page.get_by_role('button',name='上传数据库',exact=True)).to_be_enabled(); page.get_by_role('button',name='上传数据库',exact=True).click()
  expect(page.get_by_role('switch',name='启用 ASN 注释',exact=True)).to_be_enabled()
  assert f.last('/tools/route-asn/database')[3]['authorization']=='Bearer fixture'
  expect(page.get_by_role('switch',name='启用 ASN 注释',exact=True)).to_have_attribute('aria-checked','false')
  page.get_by_role('switch',name='启用 ASN 注释',exact=True).click(); expect(page.get_by_role('switch',name='启用 ASN 注释',exact=True)).to_have_attribute('aria-checked','true'); assert f.last('/tools/route-asn')[2]['expectedRevision']==1
  no_overflow(page); page.screenshot(path=str(OUT/f'asn-{width}.png')); assert not errors,errors; page.close()
  print(width,'binary ASN upload, auth and explicit activation OK',flush=True)

  page,f,errors=open_page(browser,'/inbounds',width)
  page.get_by_role('button',name='中转',exact=True).click(); expect(page.get_by_role('dialog')).to_be_visible()
  page.get_by_role('button',name='整组停用',exact=True).click(); expect(page.get_by_role('button',name='确认执行',exact=True)).to_be_enabled(); page.get_by_role('button',name='确认执行',exact=True).click()
  expect(page.get_by_role('button',name='确认执行',exact=True)).to_have_count(0); assert f.last('/inbounds/i1/relay-batches/b1/state')[3]['if-match']==impact['etag']
  f.blocked=True; page.get_by_role('button',name='整组删除',exact=True).click(); expect(page.get_by_role('button',name='确认执行',exact=True)).to_be_disabled(); expect(page.get_by_text('outbound · referenced-resource',exact=False)).to_be_visible(); page.keyboard.press('Escape'); f.blocked=False
  page.get_by_role('button',name='网络诊断',exact=True).click(); expect(page.get_by_text('已执行的网络检查通过',exact=True)).to_be_visible(); page.keyboard.press('Escape')
  page.get_by_role('button',name='真实业务诊断',exact=True).click(); page.get_by_role('button',name='确定',exact=True).click(); expect(page.get_by_text('真实业务已验证',exact=False)).to_be_visible(); page.keyboard.press('Escape')
  page.get_by_role('button',name='一键配置中转',exact=False).click(); page.get_by_text('Tokyo · 在线',exact=False).click()
  page.get_by_role('button',name='预览配置',exact=True).click(); expect(page.get_by_role('button',name='确认创建并下发',exact=True)).to_be_enabled(); assert f.last('/inbounds/i1/relays/batch/preview')[2]['entries']==[dict(nodeId='n2')]
  page.screenshot(path=str(OUT/f'relay-preview-{width}.png')); page.get_by_role('button',name='确认创建并下发',exact=True).click(); expect(page.get_by_role('button',name='确认创建并下发',exact=True)).to_have_count(0); assert f.last('/inbounds/i1/relays/batch')[2]==dict(planToken='SEALED_PLAN')
  no_overflow(page); assert not errors,errors; page.close(); print(width,'relay preview, group ETag, reference protection and business diagnostics OK',flush=True)

 page,f,errors=open_page(browser,'/',390); expect(page.get_by_role('heading',name='运维汇总',exact=True)).to_be_visible(); page.get_by_text('异常明细 · 1',exact=False).click(); expect(page.get_by_text('Review fixture',exact=True)).to_be_visible(); no_overflow(page); assert not errors,errors; page.close()
 print('operational overview OK',flush=True)
 browser.close()


with sync_playwright() as p:
 browser=p.chromium.launch(executable_path=CHROMIUM,headless=True,args=['--no-sandbox','--disable-dev-shm-usage'])
 for width in [320,1440]:
  page,f,errors=open_page(browser,'/inbounds',width)
  page.locator('td.rt-head').click(); expect(page.get_by_role('dialog')).to_be_visible()
  expect(page.get_by_label('出站 IP 策略',exact=True)).to_have_value('prefer_ipv6')
  page.get_by_label('出站 IP 策略',exact=True).select_option('ipv4_only')
  page.get_by_role('switch',name='只允许中转机访问',exact=True).click(); page.get_by_label('真实客户端 IP（PROXY 协议）',exact=True).select_option('2')
  page.get_by_text('自定义中转来源地址',exact=True).click(); page.get_by_label('Tokyo CIDR',exact=True).fill('203.0.113.9/32\n')
  page.get_by_role('button',name='保存并下发',exact=True).click(); expect(page.get_by_role('button',name='保存并下发',exact=True)).to_be_enabled()
  saved=f.last('/inbounds/i1')[2]; assert saved['outboundIPStrategy']=='ipv4_only' and saved['relayOnly'] is True
  assert saved['relayAccess']==dict(enabled=True,proxyProtocol=2,sourceOverrides={'n2':['203.0.113.9/32']}),saved
  page.get_by_role('button',name='中转管理',exact=True).click()
  dialog=page.get_by_role('dialog',name='中转管理 · reality',exact=True); expect(dialog).to_be_visible()
  page.wait_for_function('()=>{const drawers=document.querySelectorAll(".drawer"); return drawers.length===2 && Math.abs(drawers[1].getBoundingClientRect().right-window.innerWidth)<1}')
  box=dialog.bounding_box(); assert abs(box['x']-(width-box['width'])) < 2 and box['y']>=-1,box
  button=dialog.get_by_role('button',name='一键配置中转',exact=False); assert button.evaluate('(el)=>{let r=el.getBoundingClientRect();return el.contains(document.elementFromPoint(r.x+r.width/2,r.y+r.height/2))}')
  no_overflow(page); page.screenshot(path=str(OUT/f'nested-relay-{width}.png')); assert not errors,errors; page.close()
  print(width,'inbound IP strategy, relay access, PROXY, CIDR normalization and nested drawer OK',flush=True)

  page,f,errors=open_page(browser,'/templates',width)
  page.get_by_role('button',name='+ 新建模板',exact=True).click(); page.get_by_role('dialog').locator('.field').filter(has=page.get_by_text('名称',exact=True)).locator('input').fill('IPv6 template')
  page.get_by_label('出站 IP 策略',exact=True).select_option('ipv6_only'); page.get_by_role('button',name='保存',exact=True).click(); expect(page.get_by_role('dialog')).to_have_count(0)
  assert f.last('/templates')[2]['items'][0]['outboundIPStrategy']=='ipv6_only'; assert not errors,errors; page.close(); print(width,'template strategy persistence OK',flush=True)

 page,f,errors=open_page(browser,'/nodes',390); page.get_by_role('button',name='回程与定时检测',exact=True).click(); page.get_by_label('IPv4 检测目标',exact=True).fill('1.1.1.1'); page.get_by_role('switch',name='定时回程检测',exact=True).click(); f.schedule_conflict=True; page.get_by_role('button',name='保存定时策略',exact=True).click(); expect(page.get_by_text('回程定时配置已变化，请刷新后重试',exact=True)).to_be_visible(); assert sum(1 for w in f.writes if w[1]=='/nodes/n1/route-schedule')==1; assert not errors,errors; page.close(); print('schedule conflict is displayed without blind retry OK',flush=True)

 page,f,errors=open_page(browser,'/inbounds',390); page.get_by_role('button',name='中转',exact=True).click(); page.get_by_role('button',name='整组停用',exact=True).click(); f.impact_conflict=True; page.get_by_role('button',name='确认执行',exact=True).click(); expect(page.get_by_text('预览后配置已变化，请重新预览',exact=True)).to_be_visible(); expect(page.get_by_role('button',name='确认执行',exact=True)).to_have_count(0); assert len([w for w in f.writes if w[1].endswith('/state')])==1; assert not errors,errors; page.close(); print('relay stale preview closes and requires a new preview OK',flush=True)

 page,f,errors=open_page(browser,'/settings#notifications',390)
 f.notification['policy']['channels']=None
 f.notification['policy']['maintenance']=[dict(id='api-created',nodeIds=None,channels=None,startsAt=now,endsAt=(datetime.now(timezone.utc)+timedelta(hours=1)).isoformat())]
 page.get_by_role('button',name='重新加载',exact=True).click(); expect(page.get_by_role('button',name='保存通知策略',exact=True).first).to_be_disabled()
 control=route_changed_control(page); expect(control).to_have_value('off'); control.select_option('all')
 page.get_by_role('button',name='保存通知策略',exact=True).first.click(); expect(page.get_by_text('设置已保存',exact=True)).to_be_visible()
 assert f.last('/notifications/policy')[2]['maintenance'][0]['channels']==[]; assert f.last('/notifications/policy')[2]['maintenance'][0]['nodeIds']==[]
 assert not errors,errors; page.close(); print('API-created null channel and maintenance scopes render and round-trip OK',flush=True)

 page,f,errors=open_page(browser,'/nodes',390); f.scoped=True; page.reload(); expect(page.get_by_role('heading',name='我的节点',exact=True)).to_be_visible(); expect(page.get_by_role('button',name='测试点策略',exact=True)).to_have_count(0); expect(page.get_by_role('button',name='回程与定时检测',exact=True)).to_have_count(0); assert not any('probe-policy' in path or 'route-schedule' in path or 'route-traces' in path for _,path in f.requests); assert not errors,errors; page.close(); print('node operator has no new administrator controls or requests OK',flush=True)

 # Exercise the shared binary request path under the secret admin prefix,
 # including the existing sudo re-verification retry.
 page=browser.new_page(viewport=dict(width=390,height=1000),locale='en-US'); f=Fixture(); errors=[]; page.on('pageerror',lambda e:errors.append(str(e)))
 page.add_init_script("window.__POLARIS_BASE__='/secret/';localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang','en')")
 first=[True]
 def secret_route(route):
  path=urlparse(route.request.url).path
  if path=='/secret/api/v1/tools/route-asn/database' and first[0]:
   first[0]=False; route.fulfill(status=403,content_type='application/json',body=json.dumps(dict(error='sudo required',sudoRequired=True))); return
  # Fixture receives normalized URL through a tiny adapter, keeping actual
  # browser request bodies/headers intact.
  class Req:
   def __getattr__(self,k): return getattr(route.request,k)
   @property
   def url(self): return route.request.url.replace('/secret/api/v1/','/api/v1/')
  class R:
   request=Req()
   def fulfill(self,**kwargs): route.fulfill(**kwargs)
  f.route(R())
 page.route('**/secret/api/v1/**',secret_route)
 page.goto(BASE_URL+'/secret/settings#network'); expect(page.get_by_role('heading',name='Return-route ASN attribution',exact=True)).to_be_visible()
 page.get_by_label('Upload ASN MMDB',exact=True).set_input_files(dict(name='asn.mmdb',mimeType='application/octet-stream',buffer=b'MMDB fixture bytes')); page.get_by_role('button',name='Upload database',exact=True).click()
 page.get_by_placeholder('Login password',exact=True).fill('fixture-password'); page.get_by_role('button',name='Confirm',exact=True).click(); expect(page.get_by_role('switch',name='Enable ASN annotations',exact=True)).to_be_enabled()
 assert not first[0]; assert f.last('/tools/route-asn/database')[3]['authorization']=='Bearer fixture'; no_overflow(page); assert not errors,errors; page.close(); print('English UI, secret admin prefix and binary sudo retry OK',flush=True)
 browser.close()

with sync_playwright() as p:
 browser=p.chromium.launch(executable_path=CHROMIUM,headless=True,args=['--no-sandbox','--disable-dev-shm-usage'])
 original_comparison=copy.deepcopy(trace['comparison'])
 trace['comparison']=dict(state='changed',previousId=19,changedTtls=[1])
 page,f,errors=open_page(browser,'/nodes',390)
 page.get_by_role('button',name='回程与定时检测',exact=True).click()
 page.get_by_role('button',name='详情',exact=True).click()
 expect(page.get_by_role('columnheader',name='上次路径',exact=True)).to_be_visible()
 expect(page.get_by_role('cell',name='8.8.8.8',exact=True)).to_be_visible()
 expect(page.get_by_role('dialog').get_by_role('cell',name='1.1.1.1',exact=True)).to_be_visible()
 page.screenshot(path=str(OUT/'route-comparison-390.png')); no_overflow(page); assert not errors,errors; page.close()
 trace['comparison']=original_comparison
 original_address=relay['address']; relay['address']='2001:db8::1'
 page,f,errors=open_page(browser,'/inbounds',390); page.get_by_role('button',name='中转',exact=True).click()
 expect(page.locator('.copybox code')).to_have_text('[2001:db8::1]:20443')
 assert not errors,errors; page.close(); relay['address']=original_address
 print('previous/current route comparison and IPv6 relay share formatting OK',flush=True)
 browser.close()
