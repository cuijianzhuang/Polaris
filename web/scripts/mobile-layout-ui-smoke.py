"""Reachable mobile controls, unclipped map details and responsive data cards."""
import copy, json, os, shutil
from pathlib import Path
from urllib.parse import urlparse
from datetime import datetime, timezone
from playwright.sync_api import sync_playwright, expect

scope = {'__file__': str(Path(__file__).with_name('cdn-domains-ui-smoke.py'))}
exec(Path(scope['__file__']).read_text().split('\nwith sync_playwright() as p:')[0], scope)
BASE = os.environ.get('POLARIS_WEB_TEST_URL', 'http://127.0.0.1:5187').rstrip('/')
OUT = Path(os.environ.get('POLARIS_MOBILE_ARTIFACTS', '/tmp/polaris-mobile-layout'))
OUT.mkdir(parents=True, exist_ok=True)

class Fixture(scope['Fixture']):
    def __init__(self):
        super().__init__()
        host = 'edge-' + 'a' * 45 + '.preferred.cdn.example.com'
        self.profile.update(name='hk-cdn', originDomain=host, serves=[dict(node='BWG-HK long display name', tag='ws', host=host)])
        self.other.update(name='sg-cdn')
        self.nodes = []
        for i, (flag, lat, lng) in enumerate([('US', 37.7, -122.4), ('HK', 22.3, 114.2), ('SG', 1.3, 103.8), ('TW', 25, 121.5)]):
            node = copy.deepcopy(scope['fixtures']['node'])
            node.update(id=f'n{i+1}', name=flag+' long regional node name / '+('edge-'*12), flag=flag, region=flag, lat=lat, lng=lng,
                        address='2001:db8:1234:5678:abcd:ef01:2345:6789', domain=host)
            self.nodes.append(node)
    def route(self, route):
        path = urlparse(route.request.url).path.removeprefix('/api/v1')
        if path == '/nodes': return route.fulfill(json=self.nodes)
        if path == '/license': return route.fulfill(json=dict(edition='pro', state=dict(plan='pro', features=['themes'], warnings=[], limits={}), usage={}))
        if path == '/users': return route.fulfill(json=dict(items=[], total=0, summary=dict(all=0)))
        if path == '/probe':
            now = datetime.now(timezone.utc).isoformat()
            return route.fulfill(json=dict(at=now, probeEnabled=True, statusPage=False, showTraffic=True, targets=[], nodes=[dict(
                id=n['id'], name=n['name'], flag=n['flag'], region=n['region'], online=True, status='online', cpu=1, mem=2, disk=0,
                diskTotal=0, uptime=900, rxBps=0, txBps=0, load1=0, netUsed=0, netLimit=0, lastSeen=now,
                telemetryFresh=True, latency={}, probeTargets=[]) for n in self.nodes]))
        return super().route(route)

def no_overflow(page):
    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth + 1'), 'page overflowed'

def reachable(control):
    control.scroll_into_view_if_needed()
    assert control.evaluate('''e => {
        const r=e.getBoundingClientRect(), hit=document.elementFromPoint(r.x+r.width/2,r.y+r.height/2);
        return r.left>=0 && r.right<=innerWidth+1 && r.top>=0 && r.bottom<=innerHeight+1 && e.contains(hit);
    }'''), 'control obscured by navigation or clipping'

with sync_playwright() as p:
    for engine in ['chromium', 'webkit']:
        opts = dict(headless=True)
        if engine == 'chromium': opts.update(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'), args=['--no-sandbox','--disable-dev-shm-usage'])
        browser = getattr(p, engine).launch(**opts)
        for width, height, lang, style, mode in [(320,568,'zh','default','light'), (390,844,'zh','midnight','light'),
                (430,932,'en','default','dark'), (760,390,'en','warm','light'), (768,1024,'en','midnight','dark'), (1280,900,'zh','blackgold','dark')]:
            label = f'{engine}-{width}-{lang}-{style}'
            f = Fixture()
            page = browser.new_page(viewport=dict(width=width,height=height),is_mobile=width<=760,has_touch=width<=1024,locale='zh-CN' if lang=='zh' else 'en-US')
            page.set_default_timeout(10000)
            page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.theme',%s)" % (json.dumps(lang),json.dumps(json.dumps(dict(style=style,mode=mode,accent='ice')))))
            errors=[];page.on('pageerror',lambda e: errors.append(str(e)));page.route('**/api/v1/**',f.route)
            for path in ['/', '/users', '/probe']:
                page.goto(BASE+path);page.locator('.refresh-picker').wait_for()
                controls=page.locator('.refresh-picker > *')
                boxes=controls.evaluate_all('es => es.map(e=>{const r=e.getBoundingClientRect();return {height:r.height,left:r.left,right:r.right,top:r.top,bottom:r.bottom}})')
                assert abs(boxes[0]['height']-boxes[1]['height'])<1,(label,path,boxes)
                if width<=760:
                    assert min(b['height'] for b in boxes)>=44,(label,path,boxes)
                    assert all(b['left']>=0 and b['right']<=width for b in boxes),(label,path,boxes)
                for control in controls.all(): reachable(control)
                no_overflow(page)
                if path=='/':page.screenshot(path=str(OUT/(label+'-dashboard.png')),animations='disabled')
            page.goto(BASE+'/map');page.locator('.nm-row').filter(has_text=f.nodes[0]['name']).click()
            dialog=page.get_by_role('dialog',name=f.nodes[0]['name'],exact=True);expect(dialog).to_be_visible()
            # Both the close control and the navigation link remain reachable.
            close=dialog.get_by_role('button',name='关闭' if lang=='zh' else 'Close',exact=True)
            reachable(close)
            link=dialog.get_by_role('link');reachable(link)
            if 'nm-pop-inline' not in (dialog.get_attribute('class') or ''):
                assert dialog.evaluate('e=>{const r=e.getBoundingClientRect(),p=e.parentElement.getBoundingClientRect();return r.top>=p.top-1&&r.bottom<=p.bottom+1&&r.left>=p.left-1&&r.right<=p.right+1}'), 'map popup clipped'
            else:
                assert dialog.evaluate("e=>!e.closest('.nm')"), 'mobile details trapped inside map'
            no_overflow(page);page.screenshot(path=str(OUT/(label+'-map.png')),animations='disabled')
            page.keyboard.press('Escape');expect(dialog).to_have_count(0)
            expect(page.locator('.nm-row').filter(has_text=f.nodes[0]['name'])).to_be_focused()
            page.goto(BASE+'/chains');page.get_by_role('tab',name='CDN 配置档' if lang=='zh' else 'CDN profile',exact=True).click()
            row=page.locator('.cdn-page .rtable tr.sel');expect(row).to_be_visible()
            if width<=760:
                assert row.locator('td').evaluate_all('es=>es.every(e=>e.scrollWidth<=e.clientWidth+2)'), 'CDN value clipped'
                for control in row.locator('button').all():
                    box=control.bounding_box();assert box['height']>=44 and box['width']>=44,(label,box)
                    reachable(control)
            row.evaluate("e=>e.scrollIntoView({block:'center'})")
            no_overflow(page);page.screenshot(path=str(OUT/(label+'-cdn.png')),animations='disabled')
            assert not errors,(label,errors)
            assert not f.writes,(label,f.writes)
            page.close();print(label+' header controls, map details, CDN values and actions passed',flush=True)
        browser.close()
