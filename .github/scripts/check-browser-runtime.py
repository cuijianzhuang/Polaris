"""Use the pinned image's browsers; never install OS packages during tests."""

import importlib.metadata
import os
from pathlib import Path

from playwright.sync_api import sync_playwright


required = Path("web/scripts/browser-requirements.txt").read_text().strip()
assert required == f"playwright=={importlib.metadata.version('playwright')}", required
with sync_playwright() as playwright:
    # Fail immediately if the image and Python package were updated separately.
    for engine in (playwright.chromium, playwright.webkit):
        browser = engine.launch()
        browser.close()
    with open(os.environ["GITHUB_ENV"], "a") as output:
        output.write(f"POLARIS_CHROMIUM_BIN={playwright.chromium.executable_path}\n")
print("Chromium and WebKit are ready; no browser/system dependency download needed")
