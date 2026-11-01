"""Schedule settings, persisted progress and polling with the real React UI."""
import copy,json,os,shutil
from datetime import datetime,timezone,timedelta
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright,expect

scope={'__file__':str(Path(__file__).with_name('cdn-address-tools-ui-smoke.py'))}
exec(Path(scope['__file__']).read_text().rsplit('\nwith sync_playwright() as p:',1)[0],scope)
Base=scope['Fixture'];APIRequests=scope['APIRequests']
BASE=os.environ.get('POLARIS_WEB_TEST_URL','http://127.0.0.1:5187').rstrip('/')
OUT=Path(os.environ.get('POLARIS_CDN_ARTIFACTS','/tmp/polaris-cdn-ui'));OUT.mkdir(parents=True,exist_ok=True)

class Fixture(Base):
 def __init__(self):
  super().__init__();self.reads=0;self.schedule=dict(intervalMinutes=0,running=False,checked=0,total=0,failed=0,partial=0)
 def route(self,route):
  req=route.request;path=urlparse(req.url).path.removeprefix('/api/v1')
  if path=='/cdn-profiles/cf/check-schedule':
   body=req.post_data_json;self.writes.append((req.method,path,body,req.headers))
   assert req.method=='PUT' and set(body)=={'intervalMinutes'},body
   interval=body['intervalMinutes'];assert interval==0 or 5<=interval<=1440
   self.schedule=dict(intervalMinutes=interval,running=False,checked=0,total=0,failed=0,partial=0,nextAt=(datetime.now(timezone.utc)+timedelta(minutes=1)).isoformat() if interval else None)
   return route.fulfill(json=self.schedule)
  if path=='/cdn-profiles/cf/address-tools':
   self.reads+=1
   class WithSchedule:
    request=req
    def fulfill(proxy,**kwargs):
     kwargs['json']['schedule']=copy.deepcopy(self.schedule)
     return route.fulfill(**kwargs)
   return super().route(WithSchedule())
  return super().route(route)

with sync_playwright() as p:
 for engine in ['chromium','webkit']:
  browser=p.chromium.launch(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage']) if engine=='chromium' else p.webkit.launch()
  for width in [390,1440]:
   for lang in ['zh','en']:
    f=Fixture();page=browser.new_page(viewport=dict(width=width,height=1000));page.set_default_timeout(10000)
    page.clock.install();requests=APIRequests(page)
    page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.lang',%s)"%json.dumps(lang))
    errors=[];page.on('pageerror',lambda e,errors=errors:errors.append(str(e)));page.route('**/api/v1/**',f.route)
    page.goto(BASE+'/chains');page.get_by_role('tab',name='CDN 配置档' if lang=='zh' else 'CDN profile',exact=True).click()
    card=page.get_by_role('region',name='优选地址与订阅预览' if lang=='zh' else 'Preferred addresses and subscription preview')
    expect(card).to_contain_text('104.16.1.1');requests.settle_preview(page,card)
    # Disabled by default: no diagnostic requests or background preview polling.
    count=f.reads;page.clock.fast_forward(11000);page.wait_for_timeout(100)
    assert f.reads==count and not f.writes
    button=card.get_by_role('button',name='定时检测' if lang=='zh' else 'Scheduled checks',exact=True)
    button.click();dialog=page.get_by_role('dialog');interval=dialog.get_by_label('自动检测周期' if lang=='zh' else 'Automatic check interval')
    expect(interval).to_have_value('0');interval.select_option('15')
    dialog.get_by_role('button',name='保存' if lang=='zh' else 'Save',exact=True).click();expect(dialog).to_have_count(0)
    expect(card.locator('.cdn-check-schedule')).to_contain_text('每 15 分钟' if lang=='zh' else 'every 15 minutes')
    requests.settle_preview(page,card)
    assert f.last('/cdn-profiles/cf/check-schedule')[2]==dict(intervalMinutes=15)
    assert f.last('/cdn-profiles/cf/check-schedule')[3].get('authorization')=='Bearer fixture'
    # Polling updates progress and the failed-address selection without a reload.
    f.failures.add(('ep443','104.16.1.1'))
    f.schedule.update(running=True,checked=3,total=9,failed=1,lastAt=datetime.now(timezone.utc).isoformat())
    count=f.reads;page.clock.fast_forward(11000)
    summary=card.locator('.cdn-check-schedule');expect(summary).to_contain_text('3 / 9')
    expect(summary).to_contain_text('定时检测进行中' if lang=='zh' else 'Scheduled checks running')
    expect(card.locator('[data-address="104.16.1.1"]')).to_contain_text('检测失败，已排除' if lang=='zh' else 'Check failed; excluded')
    assert f.reads>count
    requests.settle_preview(page,card)
    # An open settings form must retain the operator's edits across live refreshes.
    button.click();interval.select_option('30');page.clock.fast_forward(11000)
    expect(interval).to_have_value('30')
    dialog.get_by_role('button',name='取消' if lang=='zh' else 'Cancel',exact=True).click();expect(dialog).to_have_count(0)
    page.reload();page.get_by_role('tab',name='CDN 配置档' if lang=='zh' else 'CDN profile',exact=True).click()
    expect(summary).to_contain_text('3 / 9');button.click();expect(interval).to_have_value('15')
    dialog.get_by_role('button',name='取消' if lang=='zh' else 'Cancel',exact=True).click()
    card.scroll_into_view_if_needed();assert page.evaluate('document.documentElement.scrollWidth<=innerWidth+1')
    page.screenshot(path=str(OUT/f'{engine}-{width}-{lang}-schedule.png'))
    # Disabling retains verdicts and stops automatic polling.
    button.click();interval.select_option('0');dialog.get_by_role('button',name='保存' if lang=='zh' else 'Save',exact=True).click()
    expect(dialog).to_have_count(0);expect(summary).to_have_count(0);requests.settle_preview(page,card)
    count=f.reads;page.clock.fast_forward(11000);page.wait_for_timeout(100);assert f.reads==count
    expect(card.locator('[data-address="104.16.1.1"]')).to_contain_text('检测失败，已排除' if lang=='zh' else 'Check failed; excluded')
    assert len(f.writes)==2 and all(w[1].endswith('/check-schedule') for w in f.writes),f.writes
    assert not errors,(engine,width,lang,errors)
    page.close();print(engine,width,lang,'schedule persistence, live progress, exclusion and disable OK',flush=True)
  browser.close()
