# Selecting a relay endpoint must not silently bind another landing or lose the draft.
import ast
from pathlib import Path
source = Path(__file__).with_name('operations-ui-smoke.py')
tree = ast.parse(source.read_text())
exec(compile(ast.Module(body=[n for n in tree.body if not isinstance(n, ast.With)], type_ignores=[]), str(source), 'exec'))
ib.update(core='singbox', protocol=dict(type='anytls'), transport=dict(type='tcp'), security=dict(type='tls'), tag='BWG-HK-anytls')
node.update(address='landing.example.com', cores=[dict(id='c1', nodeId='n1', type='singbox', version='1.13.5', status='running', installedVersions=[])])
caps[:] = [dict(core='singbox', version='1.13.5', combos=[dict(protocol='anytls', transport='tcp', security='tls')], features=[], hotUsers=False)]

class EndpointFixture(Fixture):
    def __init__(self):
        super().__init__()
        self.endpoints = []
        self.fail = True
        base = dict(relay, managed=True)
        self.chains = [dict(base, id='valid', name='hk-current'), dict(base, id='foreign', name='hk-tw', hops=[dict(nodeId='n2', port=20444), dict(nodeId='n1', inboundId='tw-inbound')]), dict(base, id='disabled', name='disabled-route', disabled=True), dict(base, id='wrong-node', name='wrong-node', hops=[dict(nodeId='n2', port=20445), dict(nodeId='n2', inboundId='i1')])]
    def route(self, route):
        req = route.request
        path = urlparse(req.url).path.removeprefix('/api/v1')
        if path == '/chains':
            return route.fulfill(json=self.chains)
        if path == '/inbounds/i1/endpoints':
            if req.method == 'POST':
                self.writes.append((req.method, path, req.post_data_json, req.headers))
                if self.fail:
                    self.fail = False
                    return route.fulfill(status=400, json=dict(error='Fixture endpoint save failed'))
                self.endpoints.append(dict(req.post_data_json, id='saved', inboundId='i1'))
                return route.fulfill(status=201, json=self.endpoints[-1])
            return route.fulfill(json=self.endpoints)
        return super().route(route)

with sync_playwright() as p:
    browser = p.chromium.launch(executable_path=CHROMIUM, headless=True, args=['--no-sandbox', '--disable-dev-shm-usage'])
    for width in [390, 1440]:
        for lang in ['zh', 'en']:
            text = lambda zh, en: zh if lang == 'zh' else en
            f = EndpointFixture()
            page = browser.new_page(viewport=dict(width=width, height=900))
            page.set_default_timeout(10000)
            page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.refresh','0')" % json.dumps(lang))
            errors = []
            page.on('pageerror', lambda e: errors.append(str(e)))
            page.route('**/api/v1/**', f.route)
            page.goto(BASE_URL + '/inbounds')
            page.get_by_text('BWG-HK-anytls', exact=True).click()
            dialog = page.get_by_role('dialog')
            save = dialog.get_by_role('button', name=text('保存并下发', 'Save and push'), exact=True)
            expect(save).to_be_enabled()
            dialog.locator('.seg').get_by_role('button', name=text('中转', 'Relay'), exact=True).click()
            select = dialog.get_by_role('combobox', name=text('中转链路', 'Relay chain'), exact=True)
            expect(select.locator('option')).to_have_count(2)
            assert select.locator('option').all_text_contents()[-1] == 'hk-current'
            select.select_option('valid')
            expect(dialog.get_by_text('TLS SNI: landing.example.com', exact=True)).to_be_visible()
            expect(save).to_be_disabled()
            expect(dialog.get_by_role('status')).to_contain_text(text('请先添加入口', 'Add the endpoint'))
            # Cancel releases the parent save and discards the unsubmitted draft.
            dialog.get_by_role('button', name=text('取消添加', 'Cancel addition'), exact=True).click()
            expect(save).to_be_enabled()
            dialog.get_by_role('button', name=text('+ 添加入口', '+ Add entry'), exact=True).click()
            select.select_option('valid')
            add = dialog.get_by_role('button', name=text('添加入口', 'Add entry'), exact=True)
            add.click()
            expect(page.get_by_text('Fixture endpoint save failed', exact=True)).to_be_visible()
            expect(select).to_have_value('valid')
            expect(save).to_be_disabled()
            add.click()
            expect(dialog.get_by_text(text('中转', 'Relay') + ' · hk-current', exact=True)).to_be_visible()
            expect(save).to_be_enabled()
            assert f.last('/inbounds/i1/endpoints')[2]['relayChainId'] == 'valid'
            # An endpoint already published by the managed route is not offered twice.
            dialog.get_by_role('button', name=text('+ 添加入口', '+ Add entry'), exact=True).click()
            expect(select.locator('option')).to_have_count(1)
            expect(dialog.get_by_role('button', name=text('添加入口', 'Add entry'), exact=True)).to_be_disabled()
            no_overflow(page)
            assert not errors, errors
            page.close()
            print(width, lang, 'relay compatibility, TLS preview, draft guard, retry and duplicate filtering passed', flush=True)
    browser.close()
