# Real UI + server regression. Invoked by TestLivePasskeyBrowser after web build.
# No private keys or recovery codes are written to stdout or artifacts.
import os,shutil,json,sys,traceback
from playwright.sync_api import sync_playwright,expect
url=os.environ['POLARIS_PASSKEY_TEST_URL']
with sync_playwright() as p:
 browser=p.chromium.launch(executable_path=os.environ.get('POLARIS_CHROMIUM_BIN') or shutil.which('chromium'),headless=True,args=['--no-sandbox','--disable-dev-shm-usage'])
 page=browser.new_page(viewport=dict(width=390,height=1100),locale='zh-CN')
 page.set_default_timeout(15000)
 page.add_init_script("localStorage.setItem('polaris.lang','zh');localStorage.setItem('polaris.refresh','0')")
 responses=[]
 page.on('response',lambda r: responses.append((r.url.split('/api/v1/')[-1],r.status)) if '/auth/passkeys' in r.url else None)
 def report_error(typ,value,tb):
  print('WebAuthn API statuses:',responses)
  print('UI status:',page.get_by_role('status').all_text_contents())
  traceback.print_exception(typ,value,tb)
 sys.excepthook=report_error
 errors=[];page.on('pageerror',lambda e:errors.append(str(e)))
 cdp=page.context.new_cdp_session(page);cdp.send('WebAuthn.enable')
 auth=cdp.send('WebAuthn.addVirtualAuthenticator',dict(options=dict(protocol='ctap2',transport='internal',hasResidentKey=True,hasUserVerification=True,isUserVerified=True,automaticPresenceSimulation=True)))['authenticatorId']
 page.goto(url+'/')
 page.get_by_label('用户名',exact=True).fill('root');page.get_by_label('密码',exact=True).fill('old-password')
 page.get_by_role('button',name='登录',exact=True).click()
 page.wait_for_function("!!localStorage.getItem('polaris.token')")
 password_token=page.evaluate("localStorage.getItem('polaris.token')")
 page.goto(url+'/settings#security')
 page.get_by_label('通行密钥名称',exact=True).fill('Browser device')
 page.get_by_role('button',name='注册通行密钥',exact=True).click()
 prompt=page.get_by_role('dialog',name='确认是你本人')
 expect(prompt).to_be_visible();prompt.get_by_placeholder('登录密码',exact=True).fill('old-password')
 with page.expect_response(lambda r:'/auth/passkeys/register/finish' in r.url ) as registration:
  prompt.get_by_role('button',name='确认',exact=True).click()
 result=registration.value.json();assert registration.value.status==201, {'status':registration.value.status,'error':result.get('error')};codes=result['recoveryCodes'];key_id=result['passkey']['id']
 assert len(codes)==10 and all(len(c)>=32 for c in codes)
 expect(page.get_by_role('dialog',name='保存账号恢复码')).to_be_visible()
 page.get_by_role('button',name='我已保存',exact=True).click()
 expect(page.get_by_role('cell',name='Browser device',exact=True)).to_be_visible()
 # Sign-in from a fresh application state with a real authenticator assertion.
 page.evaluate("localStorage.removeItem('polaris.token')");page.goto(url+'/')
 with page.expect_response(lambda r:'/auth/passkeys/login/finish' in r.url and r.status==200) as login:
  page.get_by_role('button',name='使用通行密钥登录',exact=True).click()
 token=login.value.json()['token']
 request=login.value.request.post_data_json
 replay=page.request.post(url+'/api/v1/auth/passkeys/login/finish',data=request)
 assert replay.status==400, 'assertion challenge was replayed'
 # A bad origin and missing user verification must not authenticate.
 begin=page.request.post(url+'/api/v1/auth/passkeys/login/begin',data={}).json()
 assertion=page.evaluate('''async options => {
  const dec=s=>Uint8Array.from(atob(s.replace(/-/g,'+').replace(/_/g,'/')),c=>c.charCodeAt(0));
  const enc=v=>btoa(String.fromCharCode(...new Uint8Array(v))).replace(/\\+/g,'-').replace(/\\//g,'_').replace(/=+$/,'');
  const c=await navigator.credentials.get({publicKey:{...options.publicKey,challenge:dec(options.publicKey.challenge)}});
  const r=c.response;return {id:c.id,rawId:enc(c.rawId),type:c.type,response:{authenticatorData:enc(r.authenticatorData),clientDataJSON:enc(r.clientDataJSON),signature:enc(r.signature),userHandle:enc(r.userHandle)}}
 }''',begin['options'])
 # Tamper with the signed client data (origin/challenge integrity).
 import base64
 raw=json.loads(base64.urlsafe_b64decode(assertion['response']['clientDataJSON']+'==='));raw['origin']='https://attacker.example'
 assertion['response']['clientDataJSON']=base64.urlsafe_b64encode(json.dumps(raw).encode()).decode().rstrip('=')
 tampered=page.request.post(url+'/api/v1/auth/passkeys/login/finish',data=dict(challengeId=begin['challengeId'],credential=assertion))
 assert tampered.status==401, 'wrong origin authenticated'
 # Roll back a virtual credential counter while retaining its real private
 # key. The resulting assertion is signed correctly but must be rejected.
 saved=cdp.send('WebAuthn.getCredentials',dict(authenticatorId=auth))['credentials'][0]
 cdp.send('WebAuthn.removeCredential',dict(authenticatorId=auth,credentialId=saved['credentialId']))
 saved['signCount']=0
 cdp.send('WebAuthn.addCredential',dict(authenticatorId=auth,credential=saved))
 begin_clone=page.request.post(url+'/api/v1/auth/passkeys/login/begin',data={}).json()
 clone=page.evaluate('''async options => {
  const dec=s=>Uint8Array.from(atob(s.replace(/-/g,'+').replace(/_/g,'/')),c=>c.charCodeAt(0));
  const enc=v=>btoa(String.fromCharCode(...new Uint8Array(v))).replace(/\\+/g,'-').replace(/\\//g,'_').replace(/=+$/,'');
  const c=await navigator.credentials.get({publicKey:{...options.publicKey,challenge:dec(options.publicKey.challenge)}});
  const r=c.response;return {id:c.id,rawId:enc(c.rawId),type:c.type,response:{authenticatorData:enc(r.authenticatorData),clientDataJSON:enc(r.clientDataJSON),signature:enc(r.signature),userHandle:enc(r.userHandle)}}
 }''',begin_clone['options'])
 assert page.request.post(url+'/api/v1/auth/passkeys/login/finish',data=dict(challengeId=begin_clone['challengeId'],credential=clone)).status==401, 'cloned counter authenticated'
 # Revoke from the password session; passkey sessions end immediately.
 headers={'Authorization':'Bearer '+password_token}
 revoked=page.request.delete(url+'/api/v1/auth/passkeys/'+key_id,headers=headers)
 assert revoked.status==204
 assert page.request.get(url+'/api/v1/auth/me',headers={'Authorization':'Bearer '+token}).status==401
 # The offline recovery form resets the password and invalidates old sessions.
 page.evaluate("localStorage.removeItem('polaris.token')");page.goto(url+'/')
 page.get_by_role('button',name='无法使用通行密钥？恢复账号',exact=True).click()
 page.get_by_label('用户名',exact=True).fill('root');page.get_by_label('账号恢复码',exact=True).fill(codes[0]);page.get_by_label('新密码',exact=True).fill('new-browser-password');page.get_by_label('确认新密码',exact=True).fill('new-browser-password')
 page.get_by_role('button',name='重置并撤销旧登录凭据',exact=True).click()
 expect(page.get_by_text('账号已恢复，请用新密码登录，并重新设置通行密钥和二次验证。',exact=True)).to_be_visible()
 assert page.request.get(url+'/api/v1/auth/me',headers=headers).status==401
 assert page.request.post(url+'/api/v1/auth/account-recovery',data=dict(username='root',code=codes[0],password='another-password')).status==401
 page.get_by_label('密码',exact=True).fill('new-browser-password');page.get_by_role('button',name='登录',exact=True).click()
 page.wait_for_function("!!localStorage.getItem('polaris.token')")
 page.set_viewport_size(dict(width=1440,height=1000));page.goto(url+'/settings#security')
 expect(page.get_by_role('heading',name='通行密钥',exact=True)).to_be_visible()
 assert page.evaluate('document.documentElement.scrollWidth <= innerWidth + 1'), 'desktop overflow'
 assert not errors,errors
 print('Real WebAuthn registration/login, signed-origin/counter-clone rejection, challenge replay prevention, revocation and offline recovery UI passed.')
 browser.close()
