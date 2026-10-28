# Real UI + server smoke test, invoked by TestLiveAdminPagesBrowser after
# `npm run build`. Signs in, opens every admin page at desktop and phone
# width (and once in English), and fails on a page crash, an uncaught error,
# a server error from the API, or a page that renders no heading.
import os, re, shutil, sys
from playwright.sync_api import sync_playwright

url = os.environ['POLARIS_PAGES_TEST_URL'].rstrip('/')
sub = os.environ.get('POLARIS_PAGES_TEST_SUB', '')
routes = ['/', '/stats', '/nodes', '/map', '/probe', '/cores', '/forwards', '/inbounds', '/chains', '/templates',
          '/users', '/shop', '/tickets', '/plans', '/sub-templates', '/certificates', '/settings', '/audit',
          '/convert', '/domains', '/telegram', '/costs', '/extensions', '/license',
          # Every settings tab, and the new-user drawer.
          '/settings#security', '/settings#update', '/settings#backup', '/settings#network', '/settings#notifications',
          '/settings#mail', '/settings#appearance', '/settings#system', '/users?new=1']
# Messages that are not page faults: requests cancelled by navigation, the
# live-update socket closing when the page changes, and 502-504 answers,
# which the panel uses for an unreachable upstream (GitHub release lookups
# have no network in tests). A 500 is a server bug and fails the run.
benign = re.compile(r'ERR_ABORTED|net::ERR_|WebSocket|websocket|status of (404|502|503|504)')

failures = []
with sync_playwright() as p:
    browser = p.chromium.launch(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),
                                headless=True, args=['--no-sandbox', '--disable-dev-shm-usage'])

    def visit_all(width, lang):
        page = browser.new_page(viewport=dict(width=width, height=1000), locale='zh-CN' if lang == 'zh' else 'en-US')
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
            overflow = page.evaluate('document.documentElement.scrollWidth - window.innerWidth')
            if width < 500 and overflow > 4:
                culprits = page.evaluate('''() => {
                    const clipped = (e) => { for (let p = e.parentElement; p; p = p.parentElement) { const o = getComputedStyle(p).overflowX; if (o !== 'visible') return true } return false }
                    const name = (e) => e.tagName.toLowerCase() + (typeof e.className === 'string' && e.className ? '.' + e.className.trim().split(/\\s+/).join('.') : '')
                    return [...document.querySelectorAll('body *')]
                      .filter(e => e.getBoundingClientRect().right > window.innerWidth + 4 && !clipped(e) && getComputedStyle(e).position !== 'fixed')
                      .slice(0, 4).map(e => name(e) + ' "' + (e.textContent || '').trim().slice(0, 30) + '"')
                }''')
                failures.append(f'{label}: page scrolls sideways by {overflow}px: {culprits}')
            print(f'ok {label}' if not (crashed or headings == 0 or problems) else f'FAIL {label}')
        # The editor of an existing user.
        problems.clear()
        page.goto(url + '/users')
        page.wait_for_load_state('networkidle')
        page.get_by_role('button', name='编辑' if lang == 'zh' else 'Edit', exact=True).first.click()
        page.wait_for_load_state('networkidle')
        label = f'user drawer @{width}px {lang}'
        if page.locator('[role=dialog], .drawer').count() == 0:
            failures.append(f'{label}: drawer did not open')
        failures.extend(f'{label}: {msg}' for msg in problems)
        print(f'ok {label}' if not problems else f'FAIL {label}')
        page.close()

    visit_all(1440, 'zh')
    visit_all(390, 'zh')
    visit_all(1440, 'en')

    if sub:
        page = browser.new_page(viewport=dict(width=390, height=1000), locale='zh-CN')
        errors = []
        page.on('pageerror', lambda e: errors.append(str(e)))
        page.goto(sub)
        if page.locator('.card').count() == 0:
            failures.append('subscription page: nothing rendered')
        failures.extend(f'subscription page: {e}' for e in errors)
        print('ok subscription page' if not errors else 'FAIL subscription page')
    browser.close()

if failures:
    print('\n'.join(failures))
    sys.exit(1)
