"""Bounded failure evidence from real Playwright contexts, before teardown."""
import json, os, re, sys, time, zipfile, tempfile, shutil
from pathlib import Path
from urllib.parse import urlsplit,parse_qs

contexts={}

def safe_url(value):
    try:
        url=urlsplit(value)
        path=re.sub(r'(/(?:sub|pair|token)/)[^/]+',r'\1<redacted>',url.path)
        host=url.hostname or ''
        if ':' in host:host='['+host+']'
        if url.port is not None:host+=':'+str(url.port)
        return dict(origin=url.scheme+'://'+host,path=path,queryKeys=sorted(parse_qs(url.query)))
    except ValueError:return dict(origin='',path='<invalid>',queryKeys=[])

def register(context):
    if context in contexts:return
    data={'pages':{},'trace':False};contexts[context]=data
    try:context.tracing.start(screenshots=False,snapshots=False,sources=False);data['trace']=True
    except Exception as error:data['traceError']='start:'+type(error).__name__

def page_opened(page):
    register(page.context)
    pages=contexts[page.context]['pages']
    if page in pages:return
    events=[];pages[page]=events
    def event(kind,url='',**extra):
        events.append(dict(kind=kind,url=safe_url(url),**extra))
        if len(events)>500:del events[0]
    page.on('request',lambda request:event('request',request.url,method=request.method))
    page.on('response',lambda response:event('response',response.url,status=response.status))
    page.on('requestfailed',lambda request:event('requestfailed',request.url,method=request.method))
    # Headers, cookies, request bodies and query values are never copied to
    # the network log. Full traces are restricted to synthetic CI fixtures.
    page.on('pageerror',lambda error:event('pageerror',page.url))

def sanitize_trace(path):
    # Keep action timing and call sites, but exclude form values, evaluated
    # arguments, response bodies, DOM snapshots and network headers.
    cleaned=path.with_suffix('.sanitized.zip')
    with zipfile.ZipFile(path) as source,zipfile.ZipFile(cleaned,'w',zipfile.ZIP_DEFLATED) as target:
        for item in source.infolist():
            if item.filename.endswith('.stacks'):
                target.writestr(item.filename,source.read(item));continue
            if item.filename.endswith('.network'):
                target.writestr(item.filename,'');continue
            if not item.filename.endswith('.trace'):continue
            lines=[]
            for line in source.read(item).decode().splitlines():
                event=json.loads(line)
                if event.get('type') in {'log','stdout','stderr','console'}:continue
                allowed={'type','version','origin','browserName','channel','platform','wallTime','monotonicTime','callId','startTime','endTime','apiName','class','method','parentId','stepId','stack','error','contextId','options'}
                event={key:value for key,value in event.items() if key in allowed}
                if isinstance(event.get('options'),dict):
                    safe_options={'viewport','screen','isMobile','deviceScaleFactor','hasTouch','colorScheme','locale','timezoneId'}
                    event['options']={key:value for key,value in event['options'].items() if key in safe_options}
                if 'error' in event:event['error']={'name':'BrowserActionError','message':'See the CI assertion and redacted screenshot'}
                lines.append(json.dumps(event))
            target.writestr(item.filename,'\n'.join(lines)+'\n')
    cleaned.replace(path)

def capture():
    root=Path(os.environ.get('POLARIS_BROWSER_ARTIFACTS','/tmp/polaris-browser-artifacts'))
    root=root/(re.sub(r'[^a-zA-Z0-9_-]','_',Path(sys.argv[0]).stem)+'-'+str(os.getpid())+'-'+str(time.time_ns()))
    root.mkdir(parents=True,exist_ok=True)
    for i,(context,data) in enumerate(list(contexts.items())):
        for j,(page,events) in enumerate(list(data['pages'].items())):
            (root/f'{i}-{j}-network.json').write_text(json.dumps(events,ensure_ascii=False,indent=2))
            try:
                page.add_style_tag(content='input,textarea,pre,code,.mono,.copybox,.copy-text,.qr,[data-sensitive]{visibility:hidden!important}')
                page.screenshot(path=str(root/f'{i}-{j}-failure.png'),full_page=True,timeout=5000)
            except Exception:pass
        if data['trace']:
            try:
                with tempfile.TemporaryDirectory(prefix='polaris-raw-trace-') as scratch:
                    trace=Path(scratch)/'trace.zip'
                    context.tracing.stop(path=str(trace));sanitize_trace(trace)
                    shutil.copyfile(trace,root/f'{i}-trace.zip')
            except Exception as error:data['traceError']='stop:'+type(error).__name__
            data['trace']=False
        if data.get('traceError'):(root/f'{i}-trace-error.json').write_text(json.dumps({'error':data['traceError']}))
    (root/'failure.json').write_text(json.dumps({'script':Path(sys.argv[0]).name,'contexts':len(contexts)}))

def install():
    try:
        from playwright.sync_api import Browser,BrowserContext,Page
        from playwright.sync_api._context_manager import PlaywrightContextManager
    except ImportError:return
    original_context=Browser.new_context;original_page=Browser.new_page;original_new_page=BrowserContext.new_page
    original_close=BrowserContext.close;original_page_close=Page.close;original_browser_close=Browser.close;original_exit=PlaywrightContextManager.__exit__
    def new_context(self,*args,**kwargs):
        context=original_context(self,*args,**kwargs);register(context);return context
    def new_page(self,*args,**kwargs):
        page=original_page(self,*args,**kwargs);page_opened(page);return page
    def context_page(self,*args,**kwargs):
        register(self);page=original_new_page(self,*args,**kwargs);page_opened(page);return page
    def close(self,*args,**kwargs):
        data=contexts.pop(self,None)
        if data and data['trace']:
            try:self.tracing.stop()
            except Exception:pass
        return original_close(self,*args,**kwargs)
    def page_close(self,*args,**kwargs):
        context=self.context
        result=original_page_close(self,*args,**kwargs)
        data=contexts.get(context)
        if data:
            data['pages'].pop(self,None)
            if not data['pages']:
                contexts.pop(context,None)
                if data['trace']:
                    try:context.tracing.stop()
                    except Exception:pass
        return result
    def browser_close(self,*args,**kwargs):
        result=original_browser_close(self,*args,**kwargs)
        for context in list(contexts):
            if context.browser is self:contexts.pop(context,None)
        return result
    def context_exit(self,exc_type,exc,tb):
        if exc_type is not None:
            try:capture()
            except Exception as error:print('Browser evidence capture failed:',type(error).__name__,file=sys.stderr)
        return original_exit(self,exc_type,exc,tb)
    Browser.new_context=new_context;Browser.new_page=new_page;BrowserContext.new_page=context_page
    BrowserContext.close=close;Page.close=page_close;Browser.close=browser_close;PlaywrightContextManager.__exit__=context_exit
