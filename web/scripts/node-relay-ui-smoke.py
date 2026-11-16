# Browser regression for manual external relay metadata in the node editor.
# Uses operations-ui-smoke fixtures, not deployed forwarding acceptance tests.
# Start Vite, then run with Python Playwright and POLARIS_CHROMIUM_BIN set:
# POLARIS_WEB_TEST_URL=http://127.0.0.1:5187 python3 web/scripts/node-relay-ui-smoke.py
import ast
import re
from pathlib import Path

source = Path(__file__).with_name('operations-ui-smoke.py')
tree = ast.parse(source.read_text())
exec(compile(ast.Module(body=[n for n in tree.body if not isinstance(n, ast.With)], type_ignores=[]), str(source), 'exec'))

ARTIFACTS = Path(os.environ.get('POLARIS_NODE_RELAY_ARTIFACTS', '/tmp/polaris-node-relay-ui'))
ARTIFACTS.mkdir(parents=True, exist_ok=True)


class NodeRelayFixture(Fixture):
    def __init__(self, null_targets=False):
        super().__init__()
        self.nodes = [copy.deepcopy(node), dict(copy.deepcopy(node), id='n2', name='Tokyo', address='203.0.113.2', inboundCount=0)]
        if null_targets:
            self.nodes[0]['relayFor'] = None
        self.reject_save = False

    def route(self, route):
        req = route.request
        path = urlparse(req.url).path.removeprefix('/api/v1')
        if path == '/nodes':
            self.requests.append((req.method, path))
            return route.fulfill(json=self.nodes)
        if path == '/nodes/n1' and req.method == 'PUT':
            self.requests.append((req.method, path))
            body = req.post_data_json
            self.writes.append((req.method, path, body, req.headers))
            if self.reject_save:
                self.reject_save = False
                return route.fulfill(status=400, json=dict(error='Fixture relay metadata rejected'))
            self.nodes[0].update(copy.deepcopy(body))
            return route.fulfill(json=self.nodes[0])
        if path == '/nodes/n1/net' and req.method == 'PUT':
            self.requests.append((req.method, path))
            self.writes.append((req.method, path, req.post_data_json, req.headers))
            return route.fulfill(json=req.post_data_json)
        return super().route(route)


def relay_page(browser, width, lang, null_targets=False):
    fixture = NodeRelayFixture(null_targets)
    page = browser.new_page(viewport=dict(width=width, height=1000), locale='zh-CN' if lang == 'zh' else 'en-US')
    page.set_default_timeout(10000)
    page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.refresh','0')" % json.dumps(lang))
    errors = []
    page.on('pageerror', lambda error: errors.append(str(error)))
    page.route('**/api/v1/**', fixture.route)
    page.goto(BASE_URL + '/nodes?id=n1')
    return page, fixture, errors


def editor_controls(page, lang):
    zh = lang == 'zh'
    page.get_by_role('button', name='编辑' if zh else 'Edit', exact=True).click()
    editor = page.get_by_role('dialog', name='编辑节点' if zh else 'Edit node', exact=True)
    expect(editor).to_be_visible()
    group = editor.get_by_role('group', name='外部中转目标' if zh else 'External relay targets', exact=True)
    expect(group.get_by_role('checkbox')).to_have_count(1)
    expect(group.get_by_role('checkbox', name=re.compile('Hong Kong'))).to_have_count(0)
    expect(editor.locator('#n-relay-hint')).to_contain_text('不会配置转发' if zh else 'does not configure forwarding')
    expect(editor.locator('#n-relay-hint')).to_contain_text('用户 IP' if zh else 'user IPs')
    expect(group).to_have_attribute('aria-describedby', 'n-relay-hint')
    checkbox = group.get_by_role('checkbox', name=re.compile('Tokyo'))
    save = editor.get_by_role('button', name='保存' if zh else 'Save', exact=True)
    return editor, group, checkbox, save


def check_layout(page, editor, group):
    group.scroll_into_view_if_needed()
    no_overflow(page)
    assert editor.evaluate('(el) => el.scrollWidth <= el.clientWidth + 1'), 'node editor overflows horizontally'
    assert group.evaluate('(el) => el.scrollWidth <= el.clientWidth + 1'), 'relay target group overflows horizontally'


with sync_playwright() as p:
    browser = p.chromium.launch(executable_path=CHROMIUM, headless=True, args=['--no-sandbox', '--disable-dev-shm-usage'])
    for width in [390, 1440]:
        for lang in ['zh', 'en']:
            page, f, errors = relay_page(browser, width, lang)
            editor, group, checkbox, save = editor_controls(page, lang)
            # Older API responses omit relayFor entirely; the editor starts empty.
            assert 'relayFor' not in f.nodes[0]
            expect(checkbox).not_to_be_checked()
            checkbox.check()
            check_layout(page, editor, group)
            page.screenshot(path=str(ARTIFACTS / f'node-relay-{width}-{lang}-selected.png'))
            save.click()
            expect(editor).to_have_count(0)
            assert f.last('/nodes/n1')[0] == 'PUT'
            assert f.last('/nodes/n1')[2]['relayFor'] == ['n2'], f.writes
            assert f.last('/nodes/n1')[3]['authorization'] == 'Bearer fixture'
            assert [(write[0], write[1]) for write in f.writes] == [('PUT', '/nodes/n1'), ('PUT', '/nodes/n1/net')], f.writes
            assert not any('/relays' in path or '/chains' in path for _, path, _, _ in f.writes), f.writes

            # Reload from saved metadata, then clear it through the same update.
            page.reload()
            editor, group, checkbox, save = editor_controls(page, lang)
            expect(checkbox).to_be_checked()
            checkbox.uncheck()
            save.click()
            expect(editor).to_have_count(0)
            assert f.last('/nodes/n1')[2]['relayFor'] == [], f.writes
            page.reload()
            editor, group, checkbox, save = editor_controls(page, lang)
            expect(checkbox).not_to_be_checked()

            # Rejected saves keep both relay selection and unrelated draft fields.
            editor.locator('#n-name').fill('Unsaved node draft')
            editor.locator('#n-region').fill('Draft region')
            checkbox.check()
            f.reject_save = True
            writes_before = len(f.writes)
            save.click()
            expect(page.get_by_text('Fixture relay metadata rejected', exact=True)).to_be_visible()
            expect(save).to_be_enabled()
            expect(editor).to_be_visible()
            expect(checkbox).to_be_checked()
            expect(editor.locator('#n-name')).to_have_value('Unsaved node draft')
            expect(editor.locator('#n-region')).to_have_value('Draft region')
            assert len(f.writes) == writes_before + 1, f.writes
            assert f.last('/nodes/n1')[2]['relayFor'] == ['n2'], f.writes
            assert f.nodes[0]['relayFor'] == [] and f.nodes[0]['name'] == 'Hong Kong', f.nodes[0]
            check_layout(page, editor, group)
            page.screenshot(path=str(ARTIFACTS / f'node-relay-{width}-{lang}-failed-draft.png'))
            save.click()
            expect(editor).to_have_count(0)
            assert f.nodes[0]['relayFor'] == ['n2'] and f.nodes[0]['name'] == 'Unsaved node draft', f.nodes[0]
            assert not errors, (width, lang, errors)
            no_overflow(page)
            page.close()

            # Explicit null has the same empty selection as an omitted field.
            page, f, errors = relay_page(browser, width, lang, null_targets=True)
            editor, group, checkbox, save = editor_controls(page, lang)
            assert f.nodes[0]['relayFor'] is None
            expect(checkbox).not_to_be_checked()
            check_layout(page, editor, group)
            save.click()
            expect(editor).to_have_count(0)
            assert f.last('/nodes/n1')[2]['relayFor'] == [], f.writes
            assert not errors, (width, lang, errors)
            page.close()
            print(width, lang, 'node relay selection, save/read, clear, failed draft/retry, absent/null and overflow passed', flush=True)
    browser.close()
