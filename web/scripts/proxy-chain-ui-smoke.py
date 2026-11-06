# Protocol-chain discovery, honest probe status and withdrawal controls.
import ast
from pathlib import Path
source = Path(__file__).with_name('operations-ui-smoke.py')
tree = ast.parse(source.read_text())
exec(compile(ast.Module(body=[n for n in tree.body if not isinstance(n, ast.With)], type_ignores=[]), str(source), 'exec'))

class ProxyFixture(Fixture):
    def __init__(self):
        super().__init__()
        self.entry = dict(copy.deepcopy(ib), tag='BWG-HK-anytls', core='singbox', port=8443, protocol=dict(type='anytls'), security=dict(type='tls'), outboundTag='', userCount=0)
        self.landing = dict(copy.deepcopy(ib), id='i2', nodeId='n2', nodeName='TW', tag='TW-vless-reality', outboundTag='')
        self.nodes = [dict(copy.deepcopy(node), name='BWG-HK', lastError='', cores=[dict(id='c1', nodeId='n1', type='singbox', version='1.13.5', status='running', installedVersions=[])]), dict(copy.deepcopy(node), id='n2', name='TW', lastError='')]
        self.chain = dict(id='protocol-path', name='hk-to-tw', mode='proxy', hops=[dict(nodeId='n1', inboundId='i1'), dict(nodeId='n2', inboundId='i2')], remark='', probe=[dict(nodeId='n1', ok=True, latencyMs=16), dict(nodeId='n2', ok=True, latencyMs=3)], probedAt=now)
        self.active = True
    def route(self, route):
        req = route.request
        path = urlparse(req.url).path.removeprefix('/api/v1')
        if path == '/nodes':
            return route.fulfill(json=self.nodes)
        if path == '/inbounds':
            return route.fulfill(json=[self.entry, self.landing])
        if path == '/chains':
            return route.fulfill(json=[self.chain] if self.active else [])
        if path == '/capabilities':
            return route.fulfill(json=caps + [dict(core='singbox', version='1.13.5', combos=[dict(protocol='anytls', transport='tcp', security='tls')], features=[], hotUsers=False)])
        if path == '/inbounds/i1/endpoints':
            return route.fulfill(json=[dict(id='ep', inboundId='i1', kind='relay', relayChainId='protocol-path', address='', port=0, sni='', host='', path='', remark='')] if self.active else [])
        if path == '/chains/protocol-path' and req.method == 'DELETE':
            self.active = False
            return route.fulfill(status=204)
        return super().route(route)

with sync_playwright() as p:
    browser = p.chromium.launch(executable_path=CHROMIUM, headless=True, args=['--no-sandbox', '--disable-dev-shm-usage'])
    for width in [390, 1440]:
        for lang in ['zh', 'en']:
            text = lambda zh, en: zh if lang == 'zh' else en
            f = ProxyFixture()
            page = browser.new_page(viewport=dict(width=width, height=1000))
            page.set_default_timeout(10000)
            errors = []
            page.on('pageerror', lambda e: errors.append(str(e)))
            page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.refresh','0')" % json.dumps(lang))
            page.route('**/api/v1/**', f.route)
            page.goto(BASE_URL + '/chains')
            card = page.get_by_role('article', name='hk-to-tw')
            expect(card.get_by_text(text('● 网络检查通过', '● Network checks passed'), exact=True)).to_be_visible()
            expect(card.get_by_text(text('入口尚无授权用户，请检查入口入站的用户分组或直接授权。', 'The entry has no authorized users. Check its user groups or direct grants.'), exact=True)).to_be_visible()
            expect(card).to_contain_text(text('未验证客户端入口、代理认证', 'do not verify client access, proxy authentication'))
            card.get_by_role('link', name=text('打开入口入站', 'Open entry inbound'), exact=True).click()
            expect(page).to_have_url(BASE_URL + '/inbounds?node=n1&inbound=i1')
            dialog = page.get_by_role('dialog', name=text('编辑入站 · BWG-HK-anytls', 'Edit inbound · BWG-HK-anytls'), exact=True)
            expect(dialog).to_be_visible()
            expect(dialog.get_by_text(text('协议中转绑定', 'Protocol relay binding'), exact=True)).to_be_visible()
            no_overflow(page)
            page.keyboard.press('Escape')
            expect(page.get_by_role('dialog')).to_have_count(0)
            page.goto(BASE_URL + '/chains')
            card.get_by_role('link', name=text('打开落地入站', 'Open landing inbound'), exact=True).click()
            expect(page.get_by_role('dialog', name=text('编辑入站 · TW-vless-reality', 'Edit inbound · TW-vless-reality'), exact=True)).to_be_visible()
            f.nodes[1]['desiredRev'] = 2
            page.goto(BASE_URL + '/chains')
            expect(card.get_by_text(text('等待配置应用', 'Waiting for configuration to apply'), exact=True)).to_be_visible()
            f.nodes[1]['desiredRev'] = 1
            f.entry['outboundTag'] = 'direct'
            page.reload()
            expect(card.get_by_text(text('路由冲突', 'Routing conflict'), exact=True)).to_be_visible()
            f.entry['outboundTag'] = ''
            f.chain['probe'][1].update(ok=False, skipped=True)
            page.reload()
            expect(card.get_by_text(text('未完成网络检查', 'Network checks incomplete'), exact=True)).to_be_visible()
            f.chain['configurationIssues'] = ['sing-box 1.13.5: sing-box does not support xhttp outbound transport for vless']
            page.reload()
            expect(card.locator('.note.bad')).to_contain_text('xhttp')
            expect(card.get_by_text(text('配置不兼容', 'Incompatible configuration'), exact=True)).to_have_count(2)
            expect(card.get_by_text(text('● 网络检查通过', '● Network checks passed'), exact=True)).to_have_count(0)
            del f.chain['configurationIssues']
            f.chain['published'] = False
            page.reload()
            expect(card.get_by_text(text('未发布到订阅', 'Not published to subscriptions'), exact=True)).to_be_visible()
            card.get_by_role('button', name=text('删除', 'Delete'), exact=True).click()
            page.get_by_role('button', name=text('确定', 'OK'), exact=True).click()
            expect(card).to_have_count(0)
            page.goto(BASE_URL + '/inbounds?node=n1&inbound=i1')
            expect(page.get_by_text(text('暂无发布入口，订阅中隐藏此入站', 'No published entry. This inbound is hidden from subscriptions.'), exact=True)).to_be_visible()
            no_overflow(page)
            assert not errors, errors
            page.close()
            print(width, lang, 'entry/landing navigation, honest network/deployment status, conflict warning and withdrawal passed', flush=True)
    browser.close()
