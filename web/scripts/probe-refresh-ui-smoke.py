# Regression for real React polling behavior with deliberately held API requests.
# Start Vite, then run with Python playwright and Chromium installed.
import json, os, shutil
from datetime import datetime, timezone
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright, expect

BASE = os.environ.get('POLARIS_WEB_TEST_URL', 'http://127.0.0.1:5187')

def overview(cpu):
    now = datetime.now(timezone.utc).isoformat()
    node = dict(id='live', name='Refresh fixture', region='HK', flag='HK', online=True, status='online',
                cpu=cpu, mem=42, rxBps=123456, txBps=456789, uptime=3600, load1=1, disk=20, diskTotal=100,
                netUsed=0, netLimit=0, lastSeen=now, telemetryFresh=True, ipv4=True, ipv6=False, latency={}, probeTargets=[])
    return dict(nodes=[node], targets=[], at=now, probeEnabled=True, statusPage=False, showTraffic=True)

class Fixture:
    def __init__(self):
        self.hold = False
        self.pending = []
        self.probes = 0
        self.metrics = 0
        self.latency = 0
        self.cpu = 12
    def route(self, route):
        path = urlparse(route.request.url).path.removeprefix('/api/v1')
        result = []
        if path == '/auth/me': result = dict(username='admin')
        elif path == '/info': result = dict(title='Polaris', version='test', edition='community')
        elif path == '/license': result = dict(edition='community', state=dict(plan='community', features=[], warnings=[], limits={}), usage={})
        elif path == '/nodes': result = []
        elif path == '/probe':
            self.probes += 1
            if self.hold:
                self.pending.append(route)
                return
            result = overview(self.cpu)
        elif path.endswith('/metrics'):
            self.metrics += 1
            result = dict(points=[])
        elif path.endswith('/latency'):
            self.latency += 1
            result = dict(points=[], targets=[])
        elif path == '/settings': result = {}
        route.fulfill(content_type='application/json', body=json.dumps(result))
    def release(self, cpu):
        self.cpu = cpu
        route = self.pending.pop(0)
        route.fulfill(content_type='application/json', body=json.dumps(overview(cpu)))

with sync_playwright() as p:
    browser = p.chromium.launch(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),
                                headless=True, args=['--no-sandbox', '--disable-dev-shm-usage'])
    page = browser.new_page(locale='zh-CN', viewport=dict(width=1440, height=1000))
    page.set_default_timeout(8000)
    page.clock.install()
    page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang','zh');localStorage.setItem('polaris.refresh','60000')")
    f = Fixture()
    page.route('**/api/v1/**', f.route)
    errors = []
    page.on('pageerror', lambda e: errors.append(str(e)))
    page.goto(BASE + '/probe')
    expect(page.get_by_label('自动刷新频率')).to_have_value('2000')
    expect(page.locator('.pv-grid')).to_contain_text('12.0%')
    # A response slower than a polling period is still accepted, without overlap.
    f.hold = True
    page.clock.run_for(2200)
    page.wait_for_timeout(30)
    assert len(f.pending) == 1, len(f.pending)
    calls = f.probes
    page.clock.run_for(2500)
    assert f.probes == calls, 'a slow request overlapped a second poll'
    f.release(83)
    expect(page.locator('.pv-grid')).to_contain_text('83.0%')
    # A stalled request expires; its last good values remain visible.
    page.clock.run_for(2200)
    page.wait_for_timeout(30)
    assert len(f.pending) == 1, len(f.pending)
    page.clock.run_for(8500)
    expect(page.get_by_role('alert')).to_be_visible()
    expect(page.locator('.pv-grid')).to_contain_text('83.0%')
    f.hold = False
    f.cpu = 27
    calls = f.probes
    # Abort reaches fetch through the browser's network task queue. Advancing
    # the mock clock does not guarantee that this queue or React has settled;
    # a poll tick may therefore still see the previous request in flight.
    # Observe a fresh request within a bounded number of polling periods.
    for _ in range(5):
        page.clock.run_for(2200)
        page.wait_for_timeout(50)
        if f.probes > calls:
            break
    assert f.probes > calls, 'polling never resumed after the stalled request'
    expect(page.locator('.pv-grid')).to_contain_text('27.0%')
    # The retained drawer refreshes resource and latency history independently.
    page.locator('.pv-card').click()
    expect(page.get_by_text('资源历史', exact=True)).to_be_visible()
    page.wait_for_timeout(50)
    metrics, latency = f.metrics, f.latency
    page.clock.run_for(5200)
    page.wait_for_timeout(50)
    assert f.metrics > metrics and f.latency == latency, (f.metrics, f.latency)
    page.clock.run_for(5200)
    page.wait_for_timeout(50)
    assert f.latency > latency
    page.get_by_role('button', name='关闭', exact=True).click()
    assert not errors, errors
    # Hidden tabs do not poll; returning triggers an immediate fresh read.
    page.evaluate("Object.defineProperty(document, 'hidden', {configurable:true,get:()=>window.probeTestHidden});window.probeTestHidden=true;document.dispatchEvent(new Event('visibilitychange'))")
    page.wait_for_timeout(50)
    calls = f.probes
    page.clock.run_for(6500)
    assert f.probes == calls, 'hidden page still polled'
    f.cpu = 61
    page.evaluate("window.probeTestHidden=false;document.dispatchEvent(new Event('visibilitychange'))")
    expect(page.locator('.pv-grid')).to_contain_text('61.0%')
    # Manual mode is independent of the legacy global setting and is persisted.
    page.get_by_label('自动刷新频率').select_option('0')
    page.wait_for_timeout(100)
    calls = f.probes
    page.clock.run_for(6500)
    assert f.probes == calls, 'manual mode still polled the overview'
    assert page.evaluate("localStorage.getItem('polaris.probe.refresh')") == '0'
    assert not errors, errors
    # Release the intercepted request whose client-side signal timed out.
    for route in f.pending:
        try: route.abort()
        except Exception: pass  # Chromium may already have disposed the request.
    page.unroute_all(behavior='ignoreErrors')
    browser.close()
    print('Probe refresh: 2s default, serialized slow responses, timeout recovery, 5s/10s drawer histories and manual mode passed')
