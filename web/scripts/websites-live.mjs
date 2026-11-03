import assert from 'node:assert/strict'
import { mkdir, readFile } from 'node:fs/promises'
const pw = await import(process.env.POLARIS_PLAYWRIGHT_MODULE || 'playwright')
const base = process.env.POLARIS_WEBSITES_TEST_URL
const auth = { Authorization: 'Bearer ' + process.env.POLARIS_WEBSITES_TEST_TOKEN }
for (const engine of ['chromium', 'webkit']) {
 const browser = await pw[engine].launch({ headless: true, ...(engine === 'chromium' ? { executablePath: process.env.POLARIS_CHROMIUM_BIN || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', args: ['--no-sandbox'] } : {}) })
 try {
  for (const width of [390, 1440]) {
   for (const lang of ['zh', 'en']) {
    const context = await browser.newContext({ viewport: { width, height: 1100 } })
    await context.addInitScript(({ token, lang }) => { localStorage.setItem('polaris.token', token); localStorage.setItem('polaris.lang', lang); localStorage.setItem('polaris.refresh', '0') }, { token: process.env.POLARIS_WEBSITES_TEST_TOKEN, lang })
    const previous = await context.request.get(base + '/api/v1/websites', { headers: auth }).then(r => r.json())
    for (const view of previous.sites) assert.equal((await context.request.delete(base + '/api/v1/websites/' + view.site.id, { headers: auth })).status(), 204)
    const page = await context.newPage(), errors = []
    page.on('pageerror', e => errors.push(e.message))
    await page.goto(base + '/websites')
    await page.getByRole('button', { name: lang === 'zh' ? '+ 网站入口' : '+ Website entry', exact: true }).click()
    await page.locator('#website-node').selectOption('web-node')
    await page.locator('#website-domain').fill('site.example.com')
    await page.locator('#website-cert').selectOption('web-cert')
    await page.locator('#website-archive').setInputFiles({name:'invalid.txt',mimeType:'text/plain',buffer:Buffer.from('not a ZIP')})
    await page.getByRole('alert').filter({hasText:lang==='zh'?'请选择不超过':'Choose a ZIP'}).waitFor()
    const chooser = page.waitForEvent('filechooser')
    await page.getByRole('button',{name:lang==='zh'?'选择 ZIP':'Choose ZIP',exact:true}).click()
    await (await chooser).setFiles(process.env.POLARIS_WEBSITES_TEST_ZIP)
    await page.getByRole('status').filter({ hasText: lang === 'zh' ? '网站包已就绪' : 'Bundle ready' }).waitFor()
    assert.equal(await page.locator('.website-upload-copy strong').innerText(), 'blog.zip')
    if(width===390 && lang==='zh') {
      const longName='my-static-blog-'.repeat(12)+'.zip'
      const bytes=await readFile(process.env.POLARIS_WEBSITES_TEST_ZIP)
      const transfer=await page.evaluateHandle(({data,name})=>{const dt=new DataTransfer();dt.items.add(new File([Uint8Array.from(atob(data),c=>c.charCodeAt(0))],name,{type:'application/zip'}));return dt},{data:bytes.toString('base64'),name:longName})
      await page.dispatchEvent('.website-upload','drop',{dataTransfer:transfer})
      await page.waitForFunction(name=>document.querySelector('.website-upload-copy strong')?.textContent===name,longName)
      assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1),'long upload name overflows')
      await page.locator('#website-archive').setInputFiles(process.env.POLARIS_WEBSITES_TEST_ZIP)
      await page.waitForFunction(()=>document.querySelector('.website-upload-copy strong')?.textContent==='blog.zip')
      await transfer.dispose()
    }
    await page.locator('#website-panel').fill('http://127.0.0.1:8080')
    await page.locator('#website-sub').selectOption('/sub')
    await page.locator('#website-portal').selectOption('/u')
    const preview = page.getByRole('button', { name: lang === 'zh' ? '检查冲突并预览' : 'Check conflicts & preview', exact: true })
    const save = page.getByRole('button', { name: lang === 'zh' ? '保存并发布' : 'Save & publish', exact: true })
    await preview.click()
    await page.locator('#website-route-web-ws').waitFor()
    await page.locator('#website-route-web-ws').fill('/sub/conflict')
    assert(await save.isDisabled(), 'editing must invalidate preview')
    await preview.click()
    await page.getByRole('alert').filter({ hasText: '路径冲突' }).waitFor()
    assert(await save.isDisabled(), 'conflicting paths must block saving')
    await page.locator('#website-route-web-ws').fill('/private-ws')
    await preview.click()
    await save.waitFor({ state: 'visible' })
    await page.waitForFunction(() => [...document.querySelectorAll('button')].some(e => ['保存并发布', 'Save & publish'].includes(e.textContent.trim()) && !e.disabled))
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `${engine} ${width} ${lang} overflow`)
    if (process.env.POLARIS_CDN_ARTIFACTS) { await mkdir(process.env.POLARIS_CDN_ARTIFACTS, { recursive: true }); await page.locator('.website-bundle').screenshot({path:`${process.env.POLARIS_CDN_ARTIFACTS}/upload-${engine}-${width}-${lang}.png`}); await page.evaluate(() => scrollTo(0, 0)); await page.screenshot({ path: `${process.env.POLARIS_CDN_ARTIFACTS}/website-${engine}-${width}-${lang}.png`, fullPage: true }) }
    await save.click()
    await page.locator('#website-node').waitFor({ state: 'detached' })
    const listing = await context.request.get(base + '/api/v1/websites', { headers: auth }).then(r => r.json())
    assert.equal(listing.sites.length, 1)
    const saved = listing.sites[0]
    assert.equal(saved.fileCount, 2)
    assert.equal(saved.site.files, null)
    await page.getByRole('button', { name: lang === 'zh' ? '编辑' : 'Edit', exact: true }).click()
    await page.locator('#website-mode').selectOption('proxy')
    await page.locator('#website-upstream').fill('http://127.0.0.1:3000')
    await preview.click()
    await page.waitForFunction(() => [...document.querySelectorAll('button')].some(e => ['保存并发布', 'Save & publish'].includes(e.textContent.trim()) && !e.disabled))
    await save.click()
    await page.locator('#website-node').waitFor({ state: 'detached' })
    const edited = await context.request.get(base + '/api/v1/websites', { headers: auth }).then(r => r.json())
    assert.equal(edited.sites[0].site.mode, 'proxy')
    assert.equal(edited.sites[0].fileCount, 2)
    assert.deepEqual(errors, [])
    await context.close()
    console.log(engine, width, lang, 'static upload, conflict preview, node path edit, publish and local blog mode passed')
   }
  }
 } finally { await browser.close() }
}
