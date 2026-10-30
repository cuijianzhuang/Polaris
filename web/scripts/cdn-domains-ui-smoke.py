# CDN domain editor regression with synthetic API data. Backend persistence,
# authorization and refresh concurrency are exercised separately in Go tests.
import copy, json, os, shutil
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright, expect

fixtures = {}
exec(Path(__file__).with_name('operations-ui-smoke.py').read_text().split('with sync_playwright() as p:')[0], fixtures)
BaseFixture = fixtures['Fixture']
BASE = os.environ.get('POLARIS_WEB_TEST_URL', 'http://127.0.0.1:5187').rstrip('/')
OUT = Path(os.environ.get('POLARIS_CDN_ARTIFACTS', '/tmp/polaris-cdn-ui'))
OUT.mkdir(parents=True, exist_ok=True)

class Fixture(BaseFixture):
    def __init__(self):
        super().__init__()
        self.profile = dict(id='cf', name='CF fixture', provider='cloudflare', originDomain='cdn.example.com', tlsMode='full_strict', ports=[443],
            preferredIps=['104.16.1.1', 'source.example.com', 'keep.example.com'], pinnedHosts=['keep.example.com'], ipSourceUrl='https://source.example.com/list',
            ipTopN=1, expandPerIp=False, autoDns=False, caddyFronting=False, hasToken=False, tokenSource='', zoneId='', lastError='', inbounds=0,
            nodeCheck=False, staleHours=24, badIps=[], carrierSplit=False, reports=[], shared=[], thirdParty=[], probes=[], serves=[])
        self.other = dict(copy.deepcopy(self.profile), id='other', name='Other fixture', pinnedHosts=['other.example.com'], preferredIps=['104.18.1.1','other.example.com'])
    def route(self, route):
        req = route.request
        path = urlparse(req.url).path.removeprefix('/api/v1')
        if path == '/cdn-shodan':
            return route.fulfill(json=dict(enabled=False, query='', defaultQuery='', pages=1, keySet=False, count=0, total=0, at='', error='', sample=[]))
        if path == '/cdn-profiles':
            return route.fulfill(json=[self.profile, self.other])
        if path == '/cdn-profiles/cf/pinned-hosts':
            body = req.post_data_json
            self.writes.append((req.method, path, body, req.headers))
            hosts = list(dict.fromkeys(h.strip().lower().rstrip('.') for h in body['pinnedHosts'] if h.strip()))
            if any('://' in h or ':' in h for h in hosts):
                return route.fulfill(status=400, json={'error':'Invalid manual domain'})
            old = self.profile['pinnedHosts']
            self.profile['preferredIps'] = list(dict.fromkeys([h for h in self.profile['preferredIps'] if h not in old] + hosts))
            self.profile['pinnedHosts'] = hosts
            return route.fulfill(json=self.profile)
        if path == '/cdn-profiles/cf/refresh-ips':
            self.profile['preferredIps'] = ['104.17.1.1'] + self.profile['pinnedHosts']
            return route.fulfill(json={'ips': self.profile['preferredIps']})
        if path == '/cdn-profiles/cf' and req.method == 'PUT':
            body = req.post_data_json
            self.writes.append((req.method, path, body, req.headers))
            self.profile.update(body)
            self.profile['preferredIps'] = list(dict.fromkeys(body['preferredIps'] + body['pinnedHosts']))
            return route.fulfill(json=self.profile)
        return super().route(route)

with sync_playwright() as p:
    for engine in ['chromium', 'webkit']:
        options = dict(headless=True)
        if engine == 'chromium':
            options.update(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'), args=['--no-sandbox','--disable-dev-shm-usage'])
        browser = getattr(p, engine).launch(**options)
        for width in [390, 1440]:
            for lang in ['zh', 'en']:
                f = Fixture()
                page = browser.new_page(viewport=dict(width=width, height=1000), locale='zh-CN' if lang=='zh' else 'en-US')
                page.set_default_timeout(10000)
                page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.refresh','0');localStorage.setItem('polaris.lang',%s)" % json.dumps(lang))
                errors=[]
                page.on('pageerror', lambda e: errors.append(str(e)))
                page.route('**/api/v1/**', f.route)
                page.goto(BASE + '/chains')
                page.get_by_role('tab', name='CDN 配置档' if lang=='zh' else 'CDN profile', exact=True).click()
                card = page.get_by_role('region', name='手动优选域名' if lang=='zh' else 'Manually maintained domains', exact=True)
                expect(card).to_contain_text('keep.example.com')
                expect(card).not_to_contain_text('source.example.com')
                expect(card).to_contain_text('当前订阅只用回源域名' if lang=='zh' else 'Subscriptions currently use only the origin domain')
                edit = '维护域名' if lang=='zh' else 'Edit domains'
                save = '保存' if lang=='zh' else 'Save'
                card.get_by_role('button',name=edit,exact=True).click()
                dialog=page.get_by_role('dialog')
                textarea=dialog.get_by_label('手动优选域名' if lang=='zh' else 'Manually maintained domains',exact=True)
                expect(textarea).to_have_value('keep.example.com')
                textarea.fill('https://bad.example.com')
                dialog.get_by_role('button',name=save,exact=True).click()
                expect(page.get_by_text('Invalid manual domain',exact=True)).to_be_visible()
                expect(textarea).to_have_value('https://bad.example.com')
                textarea.fill(' Keep.Example.com.\nnew.example.com\nkeep.example.com')
                # Simulate a report updating the automatic list while the dialog is open.
                f.profile['preferredIps']=['104.19.1.1','keep.example.com']
                dialog.get_by_role('button',name=save,exact=True).click()
                expect(dialog).to_have_count(0)
                expect(card).to_contain_text('new.example.com')
                saved=f.last('/cdn-profiles/cf/pinned-hosts')[2]
                assert set(saved)=={'pinnedHosts'}, saved
                assert f.profile['preferredIps']==['104.19.1.1','keep.example.com','new.example.com']
                # Refresh modifies automatic addresses without dropping manual domains.
                page.get_by_role('button',name='刷新' if lang=='zh' else 'Refresh',exact=True).click()
                expect(page.get_by_text('104.17.1.1',exact=True)).to_be_visible()
                expect(card).to_contain_text('new.example.com')
                assert page.evaluate('document.documentElement.scrollWidth <= innerWidth + 1')
                card.scroll_into_view_if_needed()
                page.screenshot(path=str(OUT/f'{engine}-{width}-{lang}.png'))
                # Explicit empty array clears, and reopening shows the persisted empty list.
                card.get_by_role('button',name=edit,exact=True).click()
                dialog.get_by_label('手动优选域名' if lang=='zh' else 'Manually maintained domains',exact=True).fill('')
                dialog.get_by_role('button',name=save,exact=True).click()
                expect(dialog).to_have_count(0)
                expect(card).to_contain_text('尚未添加手动优选域名' if lang=='zh' else 'No manually maintained domains yet')
                assert f.last('/cdn-profiles/cf/pinned-hosts')[2]=={'pinnedHosts':[]}
                # The full editor separates persisted pins and temporary results, including copy.
                page.get_by_role('button',name='编辑' if lang=='zh' else 'Edit',exact=True).last.click()
                expect(page.locator('#cdn-pinned-hosts')).to_have_value('')
                expect(page.locator('#cdn-preferred-ips')).to_have_value('104.17.1.1')
                page.get_by_label('从其他配置档复制优选 IP' if lang=='zh' else 'Copy preferred IPs from another profile',exact=True).select_option('other')
                expect(page.locator('#cdn-pinned-hosts')).to_have_value('other.example.com')
                expect(page.locator('#cdn-preferred-ips')).to_have_value('104.18.1.1')
                page.get_by_role('dialog').get_by_role('button',name=save,exact=True).click()
                expect(page.get_by_role('dialog')).to_have_count(0)
                assert f.last('/cdn-profiles/cf')[2]['pinnedHosts']==['other.example.com']
                expect(card).to_contain_text('other.example.com')
                assert not errors, errors
                page.close()
                print(engine,width,lang,'manual domains: edit, validation, refresh, clear, copy OK',flush=True)
        browser.close()
