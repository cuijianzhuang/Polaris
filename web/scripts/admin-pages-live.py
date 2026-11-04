# Real UI + server smoke test, invoked by TestLiveAdminPagesBrowser after
# `npm run build`. Signs in, opens every admin page at desktop and phone
# width (and once in English), and fails on a page crash, an uncaught error,
# a server error from the API, or a page that renders no heading.
import os, re, shutil, sys
from playwright.sync_api import sync_playwright

url = os.environ['POLARIS_PAGES_TEST_URL'].rstrip('/')
sub = os.environ.get('POLARIS_PAGES_TEST_SUB', '')
# Optional: save a full-page screenshot of every page into this directory.
shots = os.environ.get('POLARIS_PAGES_SHOTS', '')
routes = ['/', '/stats', '/nodes', '/map', '/probe', '/cores', '/forwards', '/inbounds', '/chains', '/templates',
          '/users', '/shop', '/tickets', '/plans', '/sub-templates', '/certificates', '/websites', '/settings', '/audit',
          '/convert', '/domains', '/telegram', '/costs', '/extensions', '/license',
          # Every settings tab, and the new-user drawer.
          '/settings#security', '/settings#update', '/settings#backup', '/settings#network', '/settings#notifications',
          '/settings#mail', '/settings#appearance', '/settings#system', '/users?new=1',
          '/extensions?tab=market']
# Messages that are not page faults: requests cancelled by navigation, the
# live-update socket closing when the page changes, and 502-504 answers,
# which the panel uses for an unreachable upstream (GitHub release lookups
# have no network in tests). A 500 is a server bug and fails the run.
benign = re.compile(r'ERR_ABORTED|net::ERR_|WebSocket|websocket|status of (404|502|503|504)')

failures = []
with sync_playwright() as p:
    # Optional full audit: POLARIS_PAGES_CASES=360:en,768:en,1024:zh
    # POLARIS_PAGES_ENGINE=webkit and POLARIS_PAGES_TOUCH=1 cover touch tablets.
    engine = os.environ.get('POLARIS_PAGES_ENGINE', 'chromium')
    if engine not in ('chromium', 'webkit'):
        raise ValueError('POLARIS_PAGES_ENGINE must be chromium or webkit')
    browser = (p.chromium.launch(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),
                                 headless=True, args=['--no-sandbox', '--disable-dev-shm-usage'])
               if engine == 'chromium' else p.webkit.launch(headless=True))

    def check_layout(page, label):
        # Document overflow alone misses controls and tables clipped by a card.
        issues = page.evaluate('''() => {
            const issues = [];
            for (const e of document.querySelectorAll('.field .input')) {
                if (!e.getClientRects().length || e.closest('table, .scroll')) continue;
                const field = e.closest('.field').getBoundingClientRect(), r = e.getBoundingClientRect();
                if (r.left < field.left - 2 || r.right > field.right + 2)
                    issues.push('input exceeds field: ' + (e.getAttribute('aria-label') || e.id || e.closest('.field').innerText).slice(0, 60));
            }
            for (const card of document.querySelectorAll('.card.flush')) {
                if (!card.querySelector(':scope > table') || card.scrollWidth <= card.clientWidth + 2) continue;
                if (!['auto', 'scroll'].includes(getComputedStyle(card).overflowX)) issues.push('wide table cannot scroll');
            }
            return issues;
        }''')
        failures.extend(f'{label}: {issue}' for issue in issues)

    def check_dialog(page, label):
        dialog = page.get_by_role('dialog').last
        dialog.evaluate("e => e.querySelectorAll('details').forEach(d => d.open = true)")
        for edge in ('top', 'bottom'):
            dialog.evaluate('(e, edge) => e.scrollTop = edge === "top" ? 0 : e.scrollHeight', edge)
            # Wait for the opening transform to finish before measuring hit targets.
            page.wait_for_function("[...document.querySelectorAll('.overlay *')].every(e => e.getAnimations().every(a => a.playState !== 'running'))")
            issues = dialog.evaluate('''e => {
                const issues = [];
                for (const item of e.querySelectorAll(':scope > .dhead button, :scope > .dfoot button')) {
                    const r = item.getBoundingClientRect();
                    const hit = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
                    if (r.top < -1 || r.bottom > innerHeight + 1 || !item.contains(hit))
                        issues.push('dialog control not reachable: ' + (item.getAttribute('aria-label') || item.innerText));
                    if (innerWidth <= 760 && (r.width < 44 || r.height < 44)) issues.push('dialog control too small');
                }
                return issues;
            }''')
            failures.extend(f'{label} {edge}: {issue}' for issue in issues)
            check_layout(page, label)
        if shots:
            page.screenshot(path=os.path.join(shots, f'dialog-{label.replace(" ", "-")}.png'), animations='disabled')

    def visit_all(width, lang):
        page = browser.new_page(viewport=dict(width=width, height=1000), locale='zh-CN' if lang == 'zh' else 'en-US',
                                is_mobile=width <= 760, has_touch=width <= 760 or os.environ.get('POLARIS_PAGES_TOUCH') == '1')
        page.set_default_timeout(15000)
        page.add_init_script(f"localStorage.setItem('polaris.lang','{lang}');localStorage.setItem('polaris.refresh','0')")
        problems = []
        page.on('pageerror', lambda e: problems.append(f'uncaught: {e}'))
        page.on('console', lambda m: problems.append(f'console: {m.text}') if m.type == 'error' and not benign.search(m.text) else None)
        page.on('response', lambda r: problems.append(f'HTTP {r.status} {r.url}') if '/api/' in r.url and r.status == 500 else None)
        page.goto(url + '/')
        page.locator('#u').fill('root')
        page.locator('#p').fill('old-password')
        page.locator('form button[type=submit]').first.click()
        page.wait_for_function("!!localStorage.getItem('polaris.token')")
        # Authentication mounts the dashboard and starts its requests. Let that
        # landing navigation settle before the first explicit route visit.
        page.wait_for_load_state('networkidle')
        for route in routes:
            problems.clear()
            page.goto(url + route)
            page.wait_for_load_state('networkidle')
            crashed = page.get_by_text('页面出错了' if lang == 'zh' else 'This page hit an error', exact=False).count() > 0
            headings = page.locator('main h1, main h2, main h3, .card h2, h1, h2').count()
            label = f'{route} @{width}px {lang}'
            if crashed:
                failures.append(f'{label}: error boundary shown')
            if headings == 0:
                failures.append(f'{label}: nothing rendered')
            for msg in problems:
                failures.append(f'{label}: {msg}')
            check_layout(page, label)
            if shots:
                name = re.sub(r'[^a-z0-9]+', '-', route.lower()).strip('-') or 'home'
                page.screenshot(path=os.path.join(shots, f'{name}-{width}-{lang}.png'), full_page=True, animations='disabled')
            overflow = page.evaluate('document.documentElement.scrollWidth - window.innerWidth')
            if overflow > 4:
                culprits = page.evaluate('''() => {
                    const clipped = (e) => { for (let p = e.parentElement; p; p = p.parentElement) { const o = getComputedStyle(p).overflowX; if (o !== 'visible') return true } return false }
                    const name = (e) => e.tagName.toLowerCase() + (typeof e.className === 'string' && e.className ? '.' + e.className.trim().split(/\\s+/).join('.') : '')
                    return [...document.querySelectorAll('body *')]
                      .filter(e => e.getBoundingClientRect().right > window.innerWidth + 4 && !clipped(e) && getComputedStyle(e).position !== 'fixed')
                      .slice(0, 4).map(e => name(e) + ' "' + (e.textContent || '').trim().slice(0, 30) + '"')
                }''')
                failures.append(f'{label}: page scrolls sideways by {overflow}px: {culprits}')
            print(f'ok {label}' if not (crashed or headings == 0 or problems) else f'FAIL {label}')
        # Sticky workbench navigation must not cover bulk actions after scrolling.
        page.goto(url + '/inbounds')
        page.wait_for_load_state('networkidle')
        page.set_viewport_size(dict(width=width, height=400))
        page.locator('tbody input[type=checkbox]').first.check()
        clear = page.get_by_role('button', name='取消选择' if lang == 'zh' else 'Clear selection', exact=True)
        clear.wait_for(state='visible')
        page.evaluate('window.scrollTo(0, document.documentElement.scrollHeight)')
        header = page.locator('.mtop' if width <= 760 else '.workspace-bar').bounding_box()
        reachable = clear.evaluate('''e => {
            const r = e.getBoundingClientRect(), hit = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
            return r.top >= 0 && r.bottom <= innerHeight && e.contains(hit);
        }''')
        if not reachable or not header or header['y'] < -1:
            failures.append(f'bulk actions @{width}px {lang}: navigation or clear selection obscured after scrolling')
        clear.click()
        page.set_viewport_size(dict(width=width, height=1000))
        # The editor of an existing user.
        problems.clear()
        page.goto(url + '/users')
        page.wait_for_load_state('networkidle')
        page.get_by_role('button', name='编辑' if lang == 'zh' else 'Edit', exact=True).first.click()
        page.wait_for_load_state('networkidle')
        label = f'user drawer @{width}px {lang}'
        if page.locator('[role=dialog], .drawer').count() == 0:
            failures.append(f'{label}: drawer did not open')
        else:
            page.set_viewport_size(dict(width=width, height=560))
            check_dialog(page, label)
        failures.extend(f'{label}: {msg}' for msg in problems)
        print(f'ok {label}' if not problems else f'FAIL {label}')

        # A few narrow-phone/tablet cases catch clipping even when the regular
        # phone viewport and document-level overflow checks both pass.
        if width == 390 and lang == 'zh':
            for narrow, route in [(360, '/sub-templates'), (360, '/telegram'), (768, '/settings'), (768, '/users')]:
                page.set_viewport_size(dict(width=narrow, height=1000))
                page.goto(url + route)
                page.wait_for_load_state('networkidle')
                check_layout(page, f'{route} @{narrow}px')

        # The routing and DNS editors are tabs, rather than standalone routes.
        page.set_viewport_size(dict(width=width, height=1000))
        page.goto(url + '/chains')
        page.get_by_role('tab', name='出站与路由' if lang == 'zh' else 'Outbounds & routing', exact=True).click()
        page.get_by_role('button', name='+ 规则' if lang == 'zh' else '+ Rule', exact=True).click()
        check_layout(page, f'routing draft @{width}px {lang}')
        page.set_viewport_size(dict(width=width, height=400))
        page.get_by_role('button', name='应用规则预设' if lang == 'zh' else 'Apply rule preset', exact=True).click()
        check_dialog(page, f'routing modal @{width}px {lang}')
        page.get_by_role('dialog').locator('.dhead button').click()
        page.get_by_role('dialog').wait_for(state='hidden')
        page.set_viewport_size(dict(width=width, height=1000))
        page.get_by_role('tab', name='DNS', exact=True).click()
        page.wait_for_load_state('networkidle')
        page.get_by_role('switch', name='启用内核 DNS' if lang == 'zh' else 'Enable core DNS', exact=True).click()
        page.get_by_role('button', name='+ 服务器' if lang == 'zh' else '+ Server', exact=True).click()
        check_layout(page, f'DNS draft @{width}px {lang}')
        failures.extend(f'editors @{width}px {lang}: {msg}' for msg in problems)
        page.close()

    # Extra widths are opt-in for the full layout audit, keeping CI focused.
    cases = os.environ.get('POLARIS_PAGES_CASES', '1440:zh,390:zh,1440:en')
    for case in cases.split(','):
        width, lang = case.split(':')
        visit_all(int(width), lang)

    if sub:
        page = browser.new_page(viewport=dict(width=390, height=1000), locale='zh-CN')
        errors = []
        page.on('pageerror', lambda e: errors.append(str(e)))
        page.goto(sub)
        if page.locator('.card').count() == 0:
            failures.append('subscription page: nothing rendered')
        if page.evaluate('document.documentElement.scrollWidth > innerWidth + 4'):
            failures.append('subscription page: horizontal overflow')
        failures.extend(f'subscription page: {e}' for e in errors)
        print('ok subscription page' if not errors else 'FAIL subscription page')
    browser.close()

if failures:
    print('\n'.join(failures))
    sys.exit(1)
