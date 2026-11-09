"""Metering evidence distinguishes sampling, stale and failed collections."""
import json,os,shutil,copy
from pathlib import Path
from datetime import datetime,timezone,timedelta
from playwright.sync_api import sync_playwright,expect
source=Path(__file__).with_name('nodes-browser-ui-smoke.py');scope={'__file__':str(source)}
exec(source.read_text().split('\nwith sync_playwright() as p:')[0],scope)
BASE=os.environ.get('POLARIS_WEB_TEST_URL','http://127.0.0.1:5187').rstrip('/')
with sync_playwright() as p:
 for engine in ['chromium','webkit']:
  opts=dict(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN')or shutil.which('chromium'),args=['--no-sandbox'])if engine=='chromium'else{}
  browser=getattr(p,engine).launch(**opts)
  for width in [390,1440]:
   f=scope['Fixture'](1);node=f.nodes[0];node['cores'][0].update(type='mihomo',status='running',trafficMeter=dict(mode='connection_sampled',intervalSeconds=1,lastSuccess=datetime.now(timezone.utc).isoformat(),consecutiveFailures=0))
   page=browser.new_page(viewport=dict(width=width,height=1000));page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang','zh');localStorage.setItem('polaris.refresh','0')")
   page.route('**/api/v1/**',f.route);page.goto(BASE+'/cores')
   meter=page.locator('.meter-health');expect(meter).to_contain_text('活动连接采样');expect(meter).to_contain_text('采集正常');expect(meter).to_contain_text('不等同于精确账单');expect(meter).to_contain_text('采样间隔 1 秒')
   node['cores'][0]['trafficMeter']['consecutiveFailures']=1
   page.reload();expect(meter).to_contain_text('最近采集失败')
   node['cores'][0]['trafficMeter'].update(consecutiveFailures=0,lastSuccess=(datetime.now(timezone.utc)-timedelta(minutes=10)).isoformat())
   page.reload();expect(meter).to_contain_text('采集结果较旧')
   node['cores'][0]['trafficMeter'].update(mode='kernel_counter',lastSuccess=datetime.now(timezone.utc).isoformat())
   page.reload();expect(meter).to_contain_text('内核用户计数');expect(meter).not_to_contain_text('活动连接采样');expect(meter).to_contain_text('采集正常')
   node['cores'][0].pop('trafficMeter')
   page.reload();expect(meter).to_contain_text('采集状态未上报')
   assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1')
   page.close();print(engine,width,'meter precision, last collection, failure, stale and legacy states passed',flush=True)
  browser.close()
