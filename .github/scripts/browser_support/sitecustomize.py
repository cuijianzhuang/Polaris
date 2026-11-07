# Opt-in for CI and its Go-launched Python browser subprocesses.
import os
if os.environ.get('POLARIS_BROWSER_DIAGNOSTICS')=='1':
    from diagnostics import install
    install()
