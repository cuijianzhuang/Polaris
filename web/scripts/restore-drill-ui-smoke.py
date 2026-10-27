# Restore drill interaction regression. All API data is synthetic; no live
# database, restore, credentials or service restart is involved.
import json, os, shutil
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright, expect

source = Path(__file__).with_name('settings-workflow-smoke.py')
scope = {'__file__': str(source)}
exec(source.read_text().split('\nwith sync_playwright() as p:')[0], scope)
BaseFixture = scope['Fixture']
BASE = os.environ.get('POLARIS_WEB_TEST_URL', 'http://127.0.0.1:5187').rstrip('/')

class Fixture(BaseFixture):
    def __init__(self):
        super().__init__()
        self.history, self.calls, self.restore_calls = [], 0, 0
        self.name = 'polaris-20261027-010101.tar.gz.enc'
    def route(self, route):
        path = urlparse(route.request.url).path
        if path == '/api/v1/backup':
            return route.fulfill(content_type='application/json',body=json.dumps(dict(supported=True,db='sqlite',dbSize=10,dir='/fixture',encrypted=True,targets=[],files=[dict(name=self.name,size=1024,created='2026-10-27T01:01:01Z',encrypted=True)])))
        if path == '/api/v1/backup/drills':
            return route.fulfill(content_type='application/json',body=json.dumps(self.history))
        if path.endswith('/restore'):
            self.restore_calls += 1
            return route.fulfill(status=500,body='must not restore')
        if path.endswith('/drill'):
            self.calls += 1
            body = route.request.post_data_json
            passed = body.get('passphrase') == 'synthetic-backup-pass'
            r = dict(id=str(self.calls),source=self.name,status='passed' if passed else 'failed',startedAt='2026-10-27T01:02:01Z',backupAt='2026-10-27T01:01:01Z',durationMs=450,checks=[dict(name='archive',state='passed' if passed else 'failed')])
            if passed:
                r['checks'].append(dict(name='live_acceptance',state='skipped',detail='Interactive login was not checked.'))
            else:
                r.update(error='Backup passphrase required',needPassphrase=True)
            self.history.insert(0,r)
            return route.fulfill(content_type='application/json',body=json.dumps(r))
        return super().route(route)

with sync_playwright() as p:
    for engine in os.environ.get('POLARIS_SETTINGS_BROWSERS','chromium,webkit').split(','):
        browser = (p.chromium.launch(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage']) if engine=='chromium' else p.webkit.launch())
        for width in [390,1440]:
            for lang in ['zh','en']:
                f = Fixture()
                page = browser.new_page(viewport=dict(width=width,height=900),locale='zh-CN' if lang=='zh' else 'en-US',is_mobile=width<760,has_touch=width<760)
                page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.refresh','0')" % json.dumps(lang))
                errors=[]
                page.on('pageerror',lambda e:errors.append(str(e)))
                page.route('**/api/v1/**',f.route)
                page.goto(BASE+'/settings#backup')
                page.get_by_role('button',name='恢复演练' if lang=='zh' else 'Restore drill',exact=True).click()
                modal = page.get_by_role('dialog')
                expect(modal).to_be_visible()
                modal.locator('input[type=password]').fill('synthetic-backup-pass')
                modal.get_by_role('button',name='恢复演练' if lang=='zh' else 'Restore drill',exact=True).click()
                expect(modal).to_have_count(0)
                history = page.get_by_role('region',name='恢复演练记录' if lang=='zh' else 'Restore drill history',exact=True)
                expect(history.locator('details')).to_have_count(2)
                history.locator('summary').first.click()
                expect(history).to_contain_text('未验证' if lang=='zh' else 'Not checked')
                assert f.calls == 2 and f.restore_calls == 0
                assert page.evaluate('document.documentElement.scrollWidth <= innerWidth + 1'), 'page overflow'
                assert not errors,errors
                page.close()
                print(engine,width,lang,'drill passphrase, retained history, scope and no restart passed',flush=True)
        browser.close()
