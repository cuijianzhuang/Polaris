"""Real portal forms, private address persistence and user isolation. No probes needed."""
import os, shutil
from pathlib import Path
from playwright.sync_api import sync_playwright, expect
base=os.environ['POLARIS_PORTAL_TEST_URL']
out=Path(os.environ.get('POLARIS_CDN_ARTIFACTS','/tmp/polaris-portal-cdn'));out.mkdir(parents=True,exist_ok=True)
with sync_playwright() as p:
 for engine in os.environ.get('POLARIS_TEST_ENGINES','chromium,webkit').split(','):
  options=dict(headless=True)
  if engine=='chromium':options.update(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),args=['--no-sandbox','--disable-dev-shm-usage'])
  browser=getattr(p,engine).launch(**options)
  for width in [390,1440]:
   for theme in ['light','dark']:
    context=browser.new_context(viewport=dict(width=width,height=1000),color_scheme=theme)
    context.add_cookies([dict(name='polaris_u',value=os.environ['POLARIS_PORTAL_TEST_COOKIE'],url=base+'/u',httpOnly=True,sameSite='Lax')])
    page=context.new_page();page.set_default_timeout(10000)
    errors=[];page.on('pageerror',lambda e:errors.append(str(e)))
    # Start each scenario with the shared Shanghai Telecom pool.
    for path,body in [('cdn-preferences',dict(profileId='regional',addresses=[])),('region',dict(region='CN-31')),('carrier',dict(carrier='ct'))]:
     if path=='cdn-preferences': body['version']=context.request.get(base+'/u/cdn-state?profileId=regional').json()['version']
     result=context.request.post(base+'/u/'+path,data=body,headers={'X-Polaris-Portal':'1'});assert result.status==200
    page.goto(base+'/u')
    panel=page.locator('#cdn-preferences')
    page.locator('#cdn-preferences > summary').click();expect(page.get_by_label('个人优选地址（一行一个，最多 32 个）')).to_be_visible()
    expect(page.locator('#cdn-profile')).to_have_value('regional')
    assert 'foreign' not in page.locator('#cdn-profile').inner_text()
    # WebKit can leak selected text outside a sized select's anonymous box.
    # Exercise a long label as well as the real entry label without changing its value.
    entry=page.locator('#cdn-endpoint')
    expect(entry.locator('option')).not_to_have_count(0)
    original=entry.evaluate('(e)=>e.selectedOptions[0].textContent')
    entry.evaluate('(e)=>e.selectedOptions[0].textContent="long-entry-".repeat(30)')
    page.screenshot(path=str(out/f'{engine}-{width}-{theme}-long-entry.png'),full_page=True)
    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth+1'),f'{engine} {width} {theme}: long entry label overflows'
    assert entry.evaluate('(e)=>e.selectedOptions[0].textContent').endswith('long-entry-')
    entry.evaluate('(e,text)=>e.selectedOptions[0].textContent=text',original)
    text=page.locator('#cdn-addresses');text.fill('https://bad.example.com')
    page.get_by_role('button',name='保存个人优选地址',exact=True).click()
    expect(page.locator('#cdn-preferences-error')).to_be_visible();expect(text).to_have_value('https://bad.example.com')
    text.fill('104.20.9.9\nBEST.EXAMPLE.COM.')
    with page.expect_navigation():page.get_by_role('button',name='保存个人优选地址',exact=True).click()
    expect(page.locator('#cdn-addresses')).to_have_value('104.20.9.9\nbest.example.com')
    expect(page.locator('#cdn-selection')).to_contain_text('个人优选')
    alice_response=context.request.get(base+'/sub/alice-region-subscription-token?format=clash');assert alice_response.status==200,f'subscription HTTP {alice_response.status}';alice=alice_response.text()
    bob=context.request.get(base+'/sub/bob-region-subscription-token?format=clash').text()
    assert '104.20.9.9' in alice and 'best.example.com' in alice
    assert '104.20.9.9' not in bob and '104.16.2.1' in bob
    overflow=page.evaluate('''() => ({viewport:innerWidth, document:document.documentElement.scrollWidth, scrollX,
      elements:[...document.querySelectorAll('body *')]
      .filter(e => e.getClientRects().length && (e.getBoundingClientRect().right + scrollX > innerWidth + 1 || e.scrollWidth > e.clientWidth + 1))
      .slice(0, 25).map(e => ({tag:e.tagName,id:e.id,css:e.className,text:e.textContent.slice(0, 90),width:e.getBoundingClientRect().width,scroll:e.scrollWidth,right:e.getBoundingClientRect().right}))})''')
    page.screenshot(path=str(out/f'{engine}-{width}-{theme}-personal.png'),full_page=True)
    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth+1'),f'{engine} {width} {theme}: overflowing controls {overflow}'
    # Clearing restores shared selection; selecting a different region changes output.
    if not page.locator('#cdn-bulk-edit').evaluate('(el) => el.open'): page.locator('#cdn-bulk-edit > summary').click()
    page.locator('#cdn-addresses').fill('')
    with page.expect_navigation():page.get_by_role('button',name='保存个人优选地址',exact=True).click()
    expect(page.locator('#cdn-addresses')).to_have_value('')
    with page.expect_navigation():page.locator('[data-portal-region]').select_option('CN-44')
    expect(page.locator('[data-portal-region]')).to_have_value('CN-44')
    alice_response=context.request.get(base+'/sub/alice-region-subscription-token?format=clash');assert alice_response.status==200,f'subscription HTTP {alice_response.status}';alice=alice_response.text()
    assert '104.16.2.1' in alice and '104.16.1.1' not in alice
    assert not errors,errors
    context.close();print(engine,width,theme,'private edit/validation/clear, region choice, subscription and user isolation OK',flush=True)
  browser.close()
