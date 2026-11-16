# Existing inbound policies apply independently of the inbound form. Uses
# mocked API responses, not real forwarding or deployed-node acceptance tests.
# Start Vite, then run with Python Playwright and POLARIS_CHROMIUM_BIN set:
# POLARIS_WEB_TEST_URL=http://127.0.0.1:5187 python3 web/scripts/relay-policy-save-ui-smoke.py
import ast
import re

from pathlib import Path

source = Path(__file__).with_name('operations-ui-smoke.py')
tree = ast.parse(source.read_text())
exec(compile(ast.Module(body=[n for n in tree.body if not isinstance(n, ast.With)], type_ignores=[]), str(source), 'exec'))


class PolicyFixture(Fixture):
    def __init__(self):
        super().__init__()
        self.inbound = dict(copy.deepcopy(ib), relayOnly=False)
        self.defer_policy = True
        self.pending_policy = None
        self.reject_inbound = True
        self.relay_reads = 0

    def record(self, req, path):
        self.requests.append((req.method, path))
        if req.method in ['POST', 'PUT', 'DELETE']:
            self.writes.append((req.method, path, req.post_data_json, req.headers))

    def complete_policy(self, route, policy, reject=False):
        if reject:
            return route.fulfill(status=400, json=dict(error='Fixture policy rejected'))
        self.inbound['relayAccess'] = copy.deepcopy(policy)
        if policy['enabled']:
            self.inbound['relayOnly'] = True
        route.fulfill(json=policy)

    def release_policy(self, reject=False):
        assert self.pending_policy is not None
        route, policy = self.pending_policy
        self.pending_policy = None
        self.complete_policy(route, policy, reject)

    def route(self, route):
        req = route.request
        path = urlparse(req.url).path.removeprefix('/api/v1')
        if path == '/inbounds':
            self.record(req, path)
            if req.method == 'POST':
                created = dict(req.post_data_json, id='created')
                return route.fulfill(status=201, json=dict(inbound=created, core=created['core']))
            return route.fulfill(json=[self.inbound])
        if path == '/inbounds/i1/endpoints':
            self.record(req, path)
            return route.fulfill(json=[dict(id='direct', inboundId='i1', kind='direct')])
        if path == '/inbounds/i1/relay-access':
            self.record(req, path)
            assert req.method == 'PUT'
            policy = req.post_data_json
            if self.defer_policy:
                self.pending_policy = (route, policy)
                return
            return self.complete_policy(route, policy)
        if path == '/inbounds/i1' and req.method == 'PUT':
            self.record(req, path)
            body = req.post_data_json
            # Model the server's separate policy contract: the generic save
            # cannot apply an edited policy, including after a prior apply.
            assert 'relayAccess' not in body, body
            if self.reject_inbound:
                self.reject_inbound = False
                return route.fulfill(status=400, json=dict(error='Fixture inbound rejected'))
            self.inbound.update(body)
            return route.fulfill(json=dict(inbound=self.inbound, core=self.inbound['core']))
        if path == '/inbounds/i1/relays':
            self.record(req, path)
            self.relay_reads += 1
            pending = dict(copy.deepcopy(relay), name=f'Pending policy path {self.relay_reads}', state='pending', nodes=[dict(nodeId='n2', name='Tokyo', state='pending', desiredRev=2, appliedRev=1)])
            return route.fulfill(json=[pending])
        return super().route(route)


def policy_page(browser, width, lang):
    fixture = PolicyFixture()
    page = browser.new_page(viewport=dict(width=width, height=1000))
    page.set_default_timeout(10000)
    page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.refresh','0')" % json.dumps(lang))
    errors = []
    page.on('pageerror', lambda e: errors.append(str(e)))
    page.route('**/api/v1/**', fixture.route)
    page.goto(BASE_URL + '/inbounds')
    return page, fixture, errors


with sync_playwright() as p:
    browser = p.chromium.launch(executable_path=CHROMIUM, headless=True, args=['--no-sandbox', '--disable-dev-shm-usage'])
    for width in [390, 1440]:
        for lang in ['zh', 'en']:
            text = lambda zh, en: zh if lang == 'zh' else en
            page, f, errors = policy_page(browser, width, lang)
            page.locator('td.rt-head').click()
            editor = page.get_by_role('dialog')
            remark = editor.locator('#ib-remark')
            remark.fill('Keep this unsaved remark')
            editor.locator('#ib-port').fill('8443')
            restrict = editor.get_by_role('switch', name=text('只允许中转机访问', 'Restrict access to relay nodes'), exact=True)
            restrict.click()
            proxy = editor.get_by_role('combobox', name=text('真实客户端 IP（PROXY 协议）', 'Real client IP (PROXY protocol)'), exact=True)
            proxy.select_option('2')
            editor.get_by_text(text('自定义中转来源地址', 'Custom relay source addresses'), exact=True).click()
            sources = editor.get_by_role('textbox', name='Tokyo CIDR', exact=True)
            sources.fill(' 203.0.113.9/32 \n\n 2001:db8::9/128\n')
            save = editor.get_by_role('button', name=text('保存并下发', 'Save and push'), exact=True)
            apply = editor.get_by_role('button', name=text('应用访问策略并下发', 'Apply access policy & deploy'), exact=True)
            expect(save).to_be_disabled()
            expect(apply).to_be_enabled()
            assert not f.writes, f.writes

            apply.click()
            expect(apply).to_be_disabled()
            expect(save).to_be_disabled()
            expect(proxy).to_be_disabled()
            assert f.pending_policy is not None
            assert [(w[0], w[1]) for w in f.writes] == [('PUT', '/inbounds/i1/relay-access')]
            expected = dict(enabled=True, proxyProtocol=2, sourceOverrides={'n2': ['203.0.113.9/32', '2001:db8::9/128']})
            assert f.writes[-1][2] == expected, f.writes
            # A failed policy apply never claims the form or policy was saved.
            f.release_policy(reject=True)
            expect(editor.get_by_role('alert')).to_contain_text('Fixture policy rejected')
            expect(editor.locator('.note.ok-note')).to_have_count(0)
            expect(apply).to_be_enabled()
            expect(save).to_be_disabled()
            expect(remark).to_have_value('Keep this unsaved remark')
            expect(editor.locator('#ib-port')).to_have_value('8443')
            expect(proxy).to_have_value('2')

            f.defer_policy = False
            apply.click()
            saved_note = editor.locator('.note.ok-note')
            expect(saved_note).to_contain_text(text('访问策略已保存，等待节点应用', 'Access policy saved, awaiting node application'))
            expect(editor).to_be_visible()
            expect(remark).to_have_value('Keep this unsaved remark')
            expect(editor.locator('#ib-port')).to_have_value('8443')
            expect(sources).to_have_value('203.0.113.9/32\n2001:db8::9/128')
            expect(save).to_be_enabled()
            expect(apply).to_be_disabled()
            assert len(f.writes) == 2 and all(w[1].endswith('/relay-access') for w in f.writes), f.writes
            assert f.inbound['port'] == 443 and f.inbound['remark'] == '', f.inbound
            assert f.inbound['relayOnly'] is True

            # A separate inbound failure must retain the successfully saved
            # policy and the unrelated draft rather than resubmit the policy.
            save.click()
            expect(page.get_by_text('Fixture inbound rejected', exact=True)).to_be_visible()
            expect(save).to_be_enabled()
            expect(saved_note).to_be_visible()
            expect(remark).to_have_value('Keep this unsaved remark')
            assert f.writes[-1][1] == '/inbounds/i1'
            assert 'relayAccess' not in f.writes[-1][2]
            assert f.writes[-1][2]['relayOnly'] is True
            save.click()
            expect(page.get_by_text(text('已保存并下发', 'Saved and pushed'), exact=True)).to_be_visible()
            assert f.inbound['port'] == 8443 and f.inbound['remark'] == 'Keep this unsaved remark'
            assert len([w for w in f.writes if w[1].endswith('/relay-access')]) == 2

            # Reverting a later policy edit releases generic Save without
            # another policy request; a whitespace-only source edit does too.
            proxy.select_option('1')
            expect(save).to_be_disabled()
            proxy.select_option('2')
            expect(save).to_be_enabled()
            sources.fill(' 203.0.113.9/32\n\n2001:db8::9/128 \n')
            expect(save).to_be_enabled()
            expect(apply).to_be_disabled()
            remark.fill('Draft survives relay polling')
            page.clock.install()
            editor.get_by_role('button', name=text('中转管理', 'Relay management'), exact=True).click()
            manager = page.get_by_role('dialog', name=text('中转管理 · reality', 'Relay management · reality'), exact=True)
            expect(manager.get_by_text(re.compile(r'^Pending policy path \d+$'))).to_be_visible()
            expect(manager).to_contain_text('rev 1/2')
            # Advance the existing poll interval; another response is visible
            # while the underlying unsaved inbound draft remains intact.
            initial_reads = f.relay_reads
            with page.expect_response(lambda r: urlparse(r.url).path.endswith('/inbounds/i1/relays')):
                page.clock.run_for(10100)
            assert f.relay_reads > initial_reads
            expect(manager.get_by_text(f'Pending policy path {f.relay_reads}', exact=True)).to_be_visible()
            manager.get_by_role('button', name=text('关闭', 'Close'), exact=True).last.click()
            expect(remark).to_have_value('Draft survives relay polling')
            no_overflow(page)
            assert not errors, errors
            page.close()

            # Creation continues to send its policy with the single create
            # request; there is no partially completed create-then-apply flow.
            page, f, errors = policy_page(browser, width, lang)
            page.get_by_role('button', name=text('+ 新建入站', '+ New inbound'), exact=True).click()
            editor = page.get_by_role('dialog')
            editor.locator('#ib-tag').fill('new-policy-inbound')
            editor.locator('#ib-port').fill('8443')
            editor.get_by_role('switch', name=text('只允许中转机访问', 'Restrict access to relay nodes'), exact=True).click()
            editor.get_by_role('combobox', name=text('真实客户端 IP（PROXY 协议）', 'Real client IP (PROXY protocol)'), exact=True).select_option('1')
            expect(editor.get_by_role('button', name=text('应用访问策略并下发', 'Apply access policy & deploy'), exact=True)).to_have_count(0)
            editor.get_by_role('button', name=text('保存并下发', 'Save and push'), exact=True).click()
            expect(editor).to_have_count(0)
            assert len(f.writes) == 1 and f.writes[0][:2] == ('POST', '/inbounds'), f.writes
            assert f.writes[0][2]['relayAccess'] == dict(enabled=True, proxyProtocol=1, sourceOverrides={})
            no_overflow(page)
            assert not errors, errors
            page.close()
            print(width, lang, 'policy/inbound separation, failure retry, dirty draft, relay polling and atomic creation passed', flush=True)
    browser.close()
