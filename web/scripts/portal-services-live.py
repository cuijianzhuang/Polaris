"""Real checkout, receipt refresh and tickets; no external payment or messaging."""
import os, re, shutil
from pathlib import Path
from urllib.parse import urlsplit
from playwright.sync_api import sync_playwright, expect

base = os.environ['POLARIS_PORTAL_TEST_URL']
out = Path(os.environ.get('POLARIS_CDN_ARTIFACTS', '/tmp/polaris-portal-services'))
out.mkdir(parents=True, exist_ok=True)

def fits(page):
    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth+1'), 'horizontal overflow'

with sync_playwright() as p:
    for engine in os.environ.get('POLARIS_TEST_ENGINES', 'chromium,webkit').split(','):
        options = dict(headless=True)
        if engine == 'chromium':
            options.update(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'), args=['--no-sandbox','--disable-dev-shm-usage'])
        browser = getattr(p, engine).launch(**options)
        for width in [360, 1440]:
            for theme in ['light','dark']:
                context = browser.new_context(viewport=dict(width=width,height=1000), color_scheme=theme)
                context.add_cookies([dict(name='polaris_u',value=os.environ['POLARIS_PORTAL_TEST_COOKIE'],domain=urlsplit(base).hostname,path='/u',httpOnly=True,sameSite='Lax')])
                page = context.new_page()
                page.set_default_timeout(10000)
                errors = []
                page.on('pageerror', lambda error: errors.append(str(error)))
                page.goto(base+'/u/shop')
                expect(page.get_by_role('heading',name='购买套餐',exact=True)).to_be_visible()
                expect(page.locator('.p-plan')).to_have_count(4)
                assert 'PRIVATE-OTHER-PURCHASE' not in page.content()
                fits(page)
                page.screenshot(path=str(out/f'services-shop-{engine}-{width}-{theme}.png'),full_page=True)
                plan = page.locator('.p-plan[data-id="switch-plan"]')
                page.locator('#coupon-section > summary').click()
                page.locator('#coupon').fill('SAVE10')
                page.locator('#capply').click()
                expect(page.locator('#cnote')).to_contain_text('已使用优惠码 SAVE10')
                expect(plan.locator('[data-price]')).to_have_text('¥0.90')
                # Changing a previously applied code must discard its old quote.
                page.locator('#coupon').fill('INVALID')
                expect(plan.locator('[data-price]')).to_have_text('¥1.00')
                page.locator('#capply').click()
                expect(page.locator('#cnote')).to_contain_text('无效')
                plan.locator('[data-checkout]').click()
                expect(page.locator('#checkout')).to_be_visible()
                expect(page.locator('.p-checkout-context')).to_contain_text('当前账号：alice')
                expect(page.locator('#checkout-coupon')).to_be_empty()
                fits(page)
                page.keyboard.press('Escape')
                expect(page.locator('#checkout')).not_to_be_visible()
                plan.locator('[data-checkout]').click()
                with page.expect_navigation():
                    page.locator('.p-checkout-payments .buy[data-method="usdt"]').first.click()
                expect(page.locator('#st')).to_have_text('待支付')
                expect(page.get_by_role('heading',name='完成付款')).to_be_visible()
                fits(page)
                page.screenshot(path=str(out/f'services-order-{engine}-{width}-{theme}.png'),full_page=True)
                page.locator('#check-order').click()
                expect(page.locator('#order-update')).to_contain_text('状态已更新')
                if width == 360 and theme == 'light':
                    # Real manual confirmation and real order polling, on the
                    # isolated test backend; no gateway response is mocked.
                    order_id = page.url.rsplit('/',1)[-1]
                    response = context.request.post(os.environ['POLARIS_SERVICES_ADMIN_API']+'/orders/'+order_id+'/fulfil',headers={'Authorization':'Bearer '+os.environ['POLARIS_SERVICES_ADMIN_TOKEN']},data={})
                    assert response.status == 200, 'test payment confirmation failed'
                    expect(page.locator('#st')).to_have_text('已支付',timeout=15000)
                    expect(page.locator('#delivered-sub')).to_be_visible()
                    assert context.request.post(base+'/u/shop/order/'+order_id+'/cancel',headers={'X-Polaris-Portal':'1'}).status == 400
                else:
                    page.once('dialog',lambda dialog:dialog.accept())
                    with page.expect_navigation():
                        page.locator('#cancel').click()
                    expect(page.locator('#st')).to_have_text('已取消')
                # A saved paid receipt is rendered with the same navigation.
                page.goto(base+'/u/shop/order/ownpaidorder')
                expect(page.locator('#delivered-sub')).to_be_visible()
                fits(page)
                assert context.request.get(base+'/u/shop/order/privateorder').status == 404
                page.goto(base+'/u/tickets')
                expect(page.get_by_role('heading',name='我的工单',exact=True)).to_be_visible()
                assert 'PRIVATE-OTHER-TICKET' not in page.content()
                page.locator('#ticket-compose > summary').click()
                page.locator('#send').click()
                expect(page.locator('#err')).to_have_text('请先填写问题描述')
                page.locator('#subject').fill('连接排查 '+engine+' '+str(width)+' '+theme)
                payload = '节点连接超时\n<script>window.__ticketAttack=true</script>\n'+('long-hostname-'*18)
                page.locator('#body').fill(payload)
                with page.expect_navigation():
                    page.locator('#send').click()
                expect(page.locator('.p-message-body')).to_have_text(payload)
                assert page.evaluate('window.__ticketAttack') is None
                page.locator('#body').fill('补充：已更新订阅，请协助检查。')
                with page.expect_navigation():
                    page.locator('#send').click()
                expect(page.locator('.p-message-body')).to_have_count(2)
                fits(page)
                page.screenshot(path=str(out/f'services-ticket-{engine}-{width}-{theme}.png'),full_page=True)
                page.once('dialog',lambda dialog:dialog.accept())
                with page.expect_navigation():
                    page.locator('#close').click()
                expect(page.locator('.p-notice')).to_contain_text('已关闭')
                page.locator('#body').fill('问题仍未解决，重新打开工单。')
                with page.expect_navigation():
                    page.locator('#send').click()
                expect(page.locator('#close')).to_be_visible()
                assert context.request.post(base+'/u/tickets/2/reply',data={'body':'not mine'},headers={'X-Polaris-Portal':'1'}).status == 404
                page.goto(base+'/u/forwards')
                expect(page.get_by_role('heading',name='我的转发',exact=True)).to_be_visible()
                fits(page)
                page.goto(base+'/u/shop/order/unknownorder')
                expect(page.get_by_role('heading',name='订单不存在',exact=True)).to_be_visible()
                fits(page)
                assert not errors, errors
                context.close()
                print(engine,width,theme,'checkout, quote reset, payment status, cancel, receipts and ticket lifecycle OK',flush=True)
        guest = browser.new_context(viewport=dict(width=360,height=1000))
        page = guest.new_page()
        page.goto(base+'/u/shop')
        expect(page.locator('.p-notice')).to_contain_text('未登录直接购买会创建新的账号')
        page.locator('.p-plan[data-id="switch-plan"] [data-checkout]').click()
        expect(page.locator('.p-checkout-context')).to_contain_text('购买后将创建新账号')
        page.keyboard.press('Escape')
        page.locator('#lgo').click()
        expect(page.locator('#lerr')).to_contain_text('请填写正确')
        page.locator('#lid').fill('privateorder')
        page.locator('#lkey').fill('c'*32)
        with page.expect_navigation():
            page.locator('#lgo').click()
        expect(page.locator('#delivered-sub')).to_be_visible()
        expect(page.locator('.p-sidebar a[href="/u#orders"]')).to_have_count(0)
        fits(page)
        # Quoting a USDT-only checkout must retain the correct currency, and
        # a code-only product without a price must not break the quote update.
        headers = {'Authorization':'Bearer '+os.environ['POLARIS_SERVICES_ADMIN_TOKEN']}
        settings = os.environ['POLARIS_SERVICES_ADMIN_API']+'/settings'
        assert guest.request.put(settings,data={'shop_epay_enabled':'false'},headers=headers).status == 200
        page.goto(base+'/u/shop')
        page.locator('#coupon-section > summary').click()
        page.locator('#coupon').fill('SAVE10')
        page.locator('#capply').click()
        expect(page.locator('#cnote')).to_contain_text('已使用优惠码 SAVE10')
        expect(page.locator('.p-plan[data-id="switch-plan"] [data-price]')).to_have_text('0.9 USDT')
        fits(page)
        assert guest.request.put(settings,data={'shop_epay_enabled':'true'},headers=headers).status == 200
        guest.close()
        browser.close()
