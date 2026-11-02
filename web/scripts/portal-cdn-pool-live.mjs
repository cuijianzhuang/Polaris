import assert from 'node:assert/strict'
const {chromium} = await import(process.env.POLARIS_PLAYWRIGHT_MODULE || 'playwright')
const base = process.env.POLARIS_PORTAL_TEST_URL
const browser = await chromium.launch({headless:true, executablePath:process.env.POLARIS_CHROMIUM_BIN || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', args:['--no-sandbox']})
try {
 for (const width of [390,1440]) {
  const context = await browser.newContext({viewport:{width,height:1000}})
  await context.addCookies([{name:'polaris_u', value:process.env.POLARIS_PORTAL_TEST_COOKIE, url:base+'/u', httpOnly:true,sameSite:'Lax'}])
  const page=await context.newPage(), errors=[]
  page.on('pageerror',e=>errors.push(e.message))
  await page.goto(base+'/u#cdn-preferences')
  await page.locator('#cdn-add-address').fill('BEST.EXAMPLE.COM.')
  await page.locator('#cdn-add').click()
  await page.locator('#cdn-import').setInputFiles({name:'addresses.txt',mimeType:'text/plain',buffer:Buffer.from('104.20.9.9,104.20.8.8')})
  await page.waitForFunction(()=>document.querySelector('#cdn-addresses').value.includes('104.20.8.8'))
  await Promise.all([page.waitForEvent('load'),page.locator('#cdn-save').click()])
  assert.equal(await page.locator('#cdn-addresses').inputValue(),'best.example.com\n104.20.9.9\n104.20.8.8')
  let row=page.locator('#cdn-pool-items > div').filter({hasText:'104.20.9.9'})
  await Promise.all([page.waitForEvent('load'),row.getByRole('button',{name:'禁用',exact:true}).click()])
  assert.match(await row.innerText(),/已禁用/)
  let sub=await context.request.get(base+'/sub/alice-region-subscription-token?format=clash')
  assert(!(await sub.text()).includes('104.20.9.9'))
  await page.locator('#cdn-pair').click()
  await page.waitForFunction(()=>document.querySelector('#cdn-command').value.includes('--token'))
  const command=await page.locator('#cdn-command').inputValue()
  assert(command.includes('-interval 1h'))
  const token=command.split("--token '")[1].split("'")[0]
  const api=command.split("--api '")[1].split("'")[0]
  const report=await context.request.post(api+'/cdn-profiles/regional/preferred-ips',{headers:{Authorization:'Bearer '+token},data:{source:'personal',port:443,scheme:'https',ips:[{ip:'104.20.7.7'}]}})
  assert.equal(report.status(),200)
  await page.reload()
  assert((await page.locator('#cdn-addresses').inputValue()).includes('best.example.com'))
  assert.match(await page.locator('#cdn-pool-items').innerText(),/104.20.7.7.*自动/)
  await Promise.all([page.waitForEvent('load'),page.locator('#cdn-revoke').click()])
  assert.equal(await page.locator('#cdn-command').inputValue(),'')
  assert.deepEqual(errors,[])
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1))
  // Reset the same fixture before the next viewport.
  await context.request.post(base+'/u/cdn-preferences',{headers:{'X-Polaris-Portal':'1'},data:{profileId:'regional',addresses:[],disabled:[]}})
  await context.close()
 }
 console.log('Personal pool browser checks passed at 390 and 1440 px')
} finally {await browser.close()}
