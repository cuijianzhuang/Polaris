import assert from 'node:assert/strict'
import {mkdir} from 'node:fs/promises'
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
  const automaticRow=page.locator('.cdn-item').filter({hasText:'104.20.7.7'})
  assert.equal(await automaticRow.getByText('自动',{exact:true}).count(),1)
  // Pinning an automatic address is an explicit manual edit; row operations
  // cannot silently submit that unsaved draft.
  await automaticRow.getByRole('button',{name:'保留为手动',exact:true}).click()
  assert(await page.locator('#cdn-draft').innerText().then(t=>t.includes('未保存')))
  assert(!(await row.getByRole('button',{name:'启用',exact:true}).isEnabled()))
  await Promise.all([page.waitForEvent('load'),page.locator('#cdn-save').click()])
  assert((await page.locator('#cdn-addresses').inputValue()).includes('104.20.7.7'))
  await page.locator('.cdn-item').filter({hasText:'104.20.8.8'}).getByRole('button',{name:'移除',exact:true}).click()
  await Promise.all([page.waitForEvent('load'),page.locator('#cdn-save').click()])
  assert(!(await page.locator('#cdn-addresses').inputValue()).includes('104.20.8.8'))
  // Reject over-limit imports without replacing the saved list.
  const tooMany=Array.from({length:33},(_,i)=>'candidate'+i+'.example.com').join('\n')
  await page.locator('#cdn-import').setInputFiles({name:'large.txt',mimeType:'text/plain',buffer:Buffer.from(tooMany)})
  await page.locator('#cdn-preferences-error').waitFor({state:'visible'})
  assert(!(await page.locator('#cdn-addresses').inputValue()).includes('candidate0'))
  assert(!(await page.locator('#cdn-save').isEnabled()))
  const checked=[]
  let pauseChecks=false, releaseCheck, signalStart
  await page.route('**/u/cdn-pool',async route=>{
   const body=route.request().postDataJSON()
   if(body.action!=='check')return route.continue()
   checked.push(body.address)
   if(pauseChecks)await new Promise(resolve=>{releaseCheck=resolve;signalStart()})
   return route.fulfill({json:{ok:true,check:{ok:body.address!=='best.example.com',at:new Date().toISOString(),message:body.address==='best.example.com'?'连接检测失败：示例诊断':'TCP／TLS 连接检测通过'}}})
  })
  await page.locator('#cdn-check-all').click()
  await page.waitForFunction(()=>document.querySelector('#cdn-check-progress').textContent.includes('检测完成'))
  assert(!checked.includes('104.20.9.9'))
  assert(checked.includes('best.example.com')&&checked.includes('104.20.7.7'))
  assert.match(await page.locator('.cdn-item').filter({hasText:'best.example.com'}).innerText(),/示例诊断/)
  assert.match(await page.locator('#cdn-summary').innerText(),/1 个检测失败/)
  if(process.env.POLARIS_CDN_ARTIFACTS){
   await mkdir(process.env.POLARIS_CDN_ARTIFACTS,{recursive:true})
   await page.locator('#cdn-preferences').scrollIntoViewIfNeeded()
   await page.locator('#cdn-preferences').screenshot({path:process.env.POLARIS_CDN_ARTIFACTS+'/personal-pool-'+width+'.png'})
  }
  // Stop waits for the in-flight address and does not dispatch the next one.
  const beforeStop=checked.length
  pauseChecks=true
  const started=new Promise(resolve=>{signalStart=resolve})
  await page.locator('#cdn-check-all').click()
  await started
  await page.locator('#cdn-stop-checks').click()
  releaseCheck()
  await page.waitForFunction(()=>document.querySelector('#cdn-check-progress').textContent.includes('已停止'))
  assert.equal(checked.length,beforeStop+1)
  pauseChecks=false
  await page.locator('#cdn-port').selectOption('2053')
  await Promise.all([page.waitForEvent('load'),page.locator('#cdn-revoke').click()])
  assert.equal(await page.locator('#cdn-port').inputValue(),'2053')
  assert.equal(await page.locator('#cdn-command').inputValue(),'')
  assert.deepEqual(errors,[])
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1))
  // Reset the same fixture before the next viewport.
  await context.request.post(base+'/u/cdn-preferences',{headers:{'X-Polaris-Portal':'1'},data:{profileId:'regional',addresses:[],disabled:[]}})
  await context.close()
 }
 console.log('Personal pool browser checks passed at 390 and 1440 px')
} finally {await browser.close()}
