"""License method switching, mobile hit targets and isolated submission paths."""
import json, os, shutil
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright, expect

source = Path(__file__).with_name('operations-ui-smoke.py')
scope = {'__file__': str(source)}
exec(source.read_text().split('with sync_playwright() as p:')[0], scope)
BASE = os.environ.get('POLARIS_WEB_TEST_URL', 'http://127.0.0.1:5187').rstrip('/')
OUT = Path(os.environ.get('POLARIS_LICENSE_ARTIFACTS', '/tmp/polaris-license-methods'))
OUT.mkdir(parents=True, exist_ok=True)

class Fixture(scope['Fixture']):
    def __init__(self):
        super().__init__()
        self.actions = []
        self.reject_activation = False
        self.license = dict(edition='pro', keys=True, server=True, instance='inst_license_ui_fixture',
            serverStatus='available', state=dict(plan='pro', source='license', features=['themes'], warnings=[], limits={}),
            usage=dict(nodes=2, users=10), license=dict(id='license-ui-fixture', customer='Fixture', plan='pro', offline=False, issuedAt='2026-01-01'))
    def route(self, route):
        request = route.request
        path = urlparse(request.url).path
        if path in ['/api/v1/license', '/api/v1/license/activate']:
            if request.method == 'POST':
                self.actions.append((path, request.post_data_json))
                if path.endswith('/activate') and self.reject_activation:
                    return route.fulfill(status=503, json=dict(error='fixture activation failure'))
            return route.fulfill(json=self.license)
        return super().route(route)

with sync_playwright() as p:
    for engine in ['chromium', 'webkit']:
        browser = (p.chromium.launch(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),
                                    args=['--no-sandbox', '--disable-dev-shm-usage'])
                   if engine == 'chromium' else p.webkit.launch())
        for width in [360, 390, 768, 1440]:
            for lang in ['zh', 'en']:
                for style, mode in [('default', 'light'), ('midnight', 'dark')]:
                    f = Fixture()
                    page = browser.new_page(viewport=dict(width=width, height=900),
                        is_mobile=width <= 760, has_touch=width <= 760, locale='zh-CN' if lang == 'zh' else 'en-US')
                    page.set_default_timeout(10000)
                    page.add_init_script("localStorage.setItem('polaris.token','fixture');localStorage.setItem('polaris.lang',%s);localStorage.setItem('polaris.theme',%s)" %
                        (json.dumps(lang), json.dumps(json.dumps(dict(style=style, mode=mode, accent='auto')))))
                    errors = []
                    page.on('pageerror', lambda e: errors.append(str(e)))
                    page.route('**/api/v1/**', f.route)
                    page.goto(BASE + '/license')
                    group = page.get_by_role('tablist', name='更换许可证' if lang == 'zh' else 'Replace license', exact=True)
                    key = group.get_by_role('tab', name='激活码' if lang == 'zh' else 'Activation key', exact=True)
                    file = group.get_by_role('tab', name='许可证文件' if lang == 'zh' else 'License file', exact=True)
                    panel = page.get_by_role('tabpanel')
                    group.scroll_into_view_if_needed()
                    expect(key).to_have_attribute('aria-selected', 'true')
                    expect(file).to_have_attribute('tabindex', '-1')
                    boxes = group.get_by_role('tab').evaluate_all('''buttons => buttons.map(e => {
                        const r=e.getBoundingClientRect(), hit=document.elementFromPoint(r.x+r.width/2,r.y+r.height/2);
                        return {width:r.width,height:r.height,reachable:e.contains(hit)};
                    })''')
                    assert abs(boxes[0]['width'] - boxes[1]['width']) <= 1, boxes
                    assert all(b['height'] >= (44 if width <= 760 else 40) and b['reachable'] for b in boxes), boxes
                    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth+1')
                    key_input = panel.get_by_role('textbox', name='激活码' if lang == 'zh' else 'Activation key', exact=True)
                    key_input.fill('FAKE-KEY-DO-NOT-ACTIVATE')
                    file.click()
                    expect(panel).to_have_attribute('aria-labelledby', file.get_attribute('id'))
                    text = panel.get_by_role('textbox', name='粘贴许可证' if lang == 'zh' else 'Paste license', exact=True)
                    text.fill('fixture-license-text')
                    if lang == 'zh' and width in [390, 1440]:
                        page.locator('.license-activation').screenshot(path=str(OUT/f'{engine}-{width}-{mode}-file.png'), animations='disabled')
                    file.press('ArrowLeft')
                    expect(key).to_be_focused()
                    expect(key_input).to_have_value('FAKE-KEY-DO-NOT-ACTIVATE')
                    key.press('End')
                    expect(file).to_be_focused()
                    expect(text).to_have_value('fixture-license-text')
                    file.press('Home')
                    expect(key).to_be_focused()
                    key.press('ArrowRight')
                    expect(file).to_have_attribute('aria-selected', 'true')
                    key.click()
                    key.press('Tab')
                    expect(key_input).to_be_focused()
                    assert f.actions == [], f.actions  # Changing methods cannot submit a license.
                    if lang == 'zh' and width in [390, 1440]:
                        page.locator('.license-activation').screenshot(path=str(OUT/f'{engine}-{width}-{mode}-key.png'), animations='disabled')
                    f.reject_activation = True
                    panel.get_by_role('button', name='激活' if lang == 'zh' else 'Activate', exact=True).click()
                    expect(page.locator('.toasts')).to_contain_text('fixture activation failure')
                    expect(key_input).to_have_value('FAKE-KEY-DO-NOT-ACTIVATE')
                    f.reject_activation = False
                    panel.get_by_role('button', name='激活' if lang == 'zh' else 'Activate', exact=True).click()
                    expect(key_input).to_have_value('')
                    assert f.actions[-1] == ('/api/v1/license/activate', dict(key='FAKE-KEY-DO-NOT-ACTIVATE'))
                    file.click()
                    expect(text).to_have_value('fixture-license-text')
                    panel.get_by_role('button', name='安装许可证' if lang == 'zh' else 'Install license', exact=True).click()
                    expect(text).to_have_value('')
                    assert f.actions[-1] == ('/api/v1/license', dict(text='fixture-license-text'))
                    with page.expect_response(lambda r: r.request.method == 'POST' and urlparse(r.url).path == '/api/v1/license') as imported:
                        panel.locator('input[type=file]').set_input_files(dict(name='fixture.lic', mimeType='text/plain', buffer=b' file-license-fixture\n'))
                    assert imported.value.status == 200
                    expect(panel.locator('input[type=file]')).to_be_enabled()
                    assert f.actions[-1] == ('/api/v1/license', dict(text='file-license-fixture'))
                    assert not errors, errors
                    page.close()
                    print(engine, width, lang, mode, 'tabs, drafts, keyboard, activation and file import passed', flush=True)
        browser.close()
