"""One isolated engine restart for narrowly identified WebKit driver failures."""
import re
import sys


def is_webkit_infrastructure_error(engine, error, *, playwright_error, playwright_timeout):
    if (engine != 'webkit' or not isinstance(error, playwright_error)
            or isinstance(error, (AssertionError, playwright_timeout))):
        return False
    # Only the actual Playwright error heading counts. Call logs may include
    # arbitrary page text, including words like "internal error" or "crash".
    heading = str(error).splitlines()[0] if str(error) else ''
    return bool(re.fullmatch(
        r'(?:Page\.goto|BrowserType\.launch): (?:WebKit encountered an internal error|An internal error was encountered|Internal error)',
        heading,
    ))


def run_engine_with_retry(engine, launch, scenario, *, playwright_error, playwright_timeout):
    """Restart only this engine once; scenario must acquire fresh fixture data."""
    for attempt in range(2):
        browser = None
        try:
            browser = launch()
            scenario(browser)
        except Exception as error:
            if attempt or not is_webkit_infrastructure_error(
                engine, error, playwright_error=playwright_error,
                playwright_timeout=playwright_timeout,
            ):
                # Leave final failure contexts available to the outer
                # sync_playwright diagnostics hook before driver shutdown.
                raise
            # Keep the original failure if closing a failed browser also fails.
            if browser is not None:
                try:
                    browser.close()
                except Exception:
                    pass
            print(f'{engine}: infrastructure failure; restarting engine once: {error}', file=sys.stderr)
        else:
            # Teardown errors are never retryable, even with a matching message.
            browser.close()
            return
