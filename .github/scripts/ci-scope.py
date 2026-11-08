"""Conservative PR test selection. Releases, unknown files and shared code run all."""
import json, os, re, subprocess
from pathlib import Path

SUITES = {
 'settings':'probe-refresh-ui-smoke.py appearance-ui-smoke.py license-method-ui-smoke.py ui-details-ui-smoke.py settings-workflow-smoke.py restore-drill-ui-smoke.py admin-roles-ui-smoke.py',
 'settings-state':'settings-state-ui-smoke.py mobile-layout-ui-smoke.py drafts-refresh-ui-smoke.py',
 'cdn-addresses':'cdn-address-tools-ui-smoke.py cdn-schedule-ui-smoke.py cdn-domains-ui-smoke.py cdn-check-ports-ui-smoke.py',
 'cdn-tunnels':'cdn-tunnel-ui-smoke.py cdn-tunnel-agent-ui-smoke.py cdn-tunnel-remote-ui-smoke.py cdn-scheme-ui-smoke.py',
 'nodes-users':'rollout-result-ui-smoke.py nodes-browser-ui-smoke.py user-online-ui-smoke.py user-state-ui-smoke.py relay-endpoint-ui-smoke.py proxy-chain-ui-smoke.py account-navigation-ui-smoke.py traffic-meter-ui-smoke.py',
}

def select(paths, event='pull_request'):
    flags=dict.fromkeys(['go','postgres','redis','pro','protocols','web','portal','full'],False)
    suites=set()
    def full():
        return {**dict.fromkeys(flags,True),'matrix':{'include':[{'suite':s,'scripts':scripts} for s,scripts in SUITES.items()]},'browser':True}
    if event!='pull_request' or not paths:return full()
    for path in paths:
        if path.startswith('docs/') or path.endswith('.md') or path in {'.gitignore','LICENSE'}:continue
        if path.startswith('web/scripts/') and path.endswith('.py'):
            matches={suite for suite,scripts in SUITES.items() if Path(path).name in scripts.split()}
            if not matches:return full()
            suites.update(matches);flags['web']=True;continue
        if path.startswith('web/src/pages/') and path.endswith(('.tsx','.css')):
            name=Path(path).name
            flags['web']=True
            if name in {'Nodes.tsx','Cores.tsx','Users.tsx','Stats.tsx','Costs.tsx'}:suites.update(['nodes-users','settings-state']);continue
            if name in {'Chains.tsx','Inbounds.tsx','Forwards.tsx'}:suites.update(['cdn-addresses','cdn-tunnels','nodes-users']);continue
            if name in {'Settings.tsx','Telegram.tsx','SubTemplates.tsx'} or '/settings/' in path:
                suites.update(['settings','settings-state']);flags['portal']=True;continue
        # Generated protocol files, Go, dependencies, shared UI, workflows and
        # unmapped paths are deliberately broad. Renames/deletions count too.
        return full()
    return {**flags,'matrix':{'include':[{'suite':s,'scripts':SUITES[s]} for s in SUITES if s in suites]},'browser':bool(suites)}

def main():
    event=os.environ.get('CI_SCOPE_EVENT','workflow_call')
    base=os.environ.get('CI_SCOPE_BASE','')
    paths=[]
    if event=='pull_request':
        try:
            if not re.fullmatch(r'[0-9a-f]{40}',base):raise ValueError('missing base')
            paths=subprocess.check_output(['git','diff','--name-only','-z',base,'HEAD']).decode().rstrip('\0').split('\0')
        except (ValueError,subprocess.CalledProcessError,UnicodeError):event='full-fallback'
    result=select(paths,event)
    with open(os.environ['GITHUB_OUTPUT'],'a') as output:
        for key,value in result.items():output.write(key+'='+json.dumps(value,separators=(',',':'))+'\n')
    print('Selected checks:',json.dumps(result,ensure_ascii=False))
if __name__=='__main__':main()
