"""Offline retry policy tests: python3 -m unittest discover -s web/scripts -p 'test_browser_retry.py'."""
import os
from pathlib import Path
import runpy
import sys
import tempfile
from types import ModuleType
import unittest
from unittest.mock import Mock, patch

from browser_retry import is_webkit_infrastructure_error, run_engine_with_retry


class PlaywrightError(Exception):
    pass


class PlaywrightTimeout(PlaywrightError):
    pass


ERROR_TYPES = dict(playwright_error=PlaywrightError, playwright_timeout=PlaywrightTimeout)
# Exact heading from the CI failure on PR #346.
INTERNAL = 'Page.goto: WebKit encountered an internal error'


class BrowserRetryTests(unittest.TestCase):
    def test_exact_webkit_infrastructure_headings_only(self):
        for heading in (INTERNAL, 'Page.goto: An internal error was encountered',
                        'BrowserType.launch: WebKit encountered an internal error', 'Page.goto: Internal error',
                        'BrowserType.launch: Internal error',
                        'BrowserType.launch: An internal error was encountered'):
            with self.subTest(heading=heading):
                self.assertTrue(is_webkit_infrastructure_error(
                    'webkit', PlaywrightError(heading + '\nCall log:\n  - navigating'), **ERROR_TYPES))

    def test_regressions_and_other_engines_are_never_retryable(self):
        errors = [
            AssertionError(INTERNAL), RuntimeError(INTERNAL), PlaywrightTimeout(INTERNAL),
            PlaywrightTimeout('Page.goto: Timeout 30000ms exceeded.'),
            PlaywrightError('Page.goto: Target page, context or browser has been closed'),
            PlaywrightError('BrowserType.launch: Target page, context or browser has been closed'),
            PlaywrightError("BrowserType.launch: Executable doesn't exist"),
            PlaywrightError('Page.goto: net::ERR_CONNECTION_REFUSED'),
            PlaywrightError('Locator.click: Internal error'),
            PlaywrightError('Page.goto: Internal error loading application'),
            PlaywrightError('Page.goto: timeout\nCall log:\nInternal error'),
        ]
        for error in errors:
            with self.subTest(error=repr(error)):
                self.assertFalse(is_webkit_infrastructure_error('webkit', error, **ERROR_TYPES))
        for engine in ('chromium', 'firefox'):
            self.assertFalse(is_webkit_infrastructure_error(engine, PlaywrightError(INTERNAL), **ERROR_TYPES))

    def run_retry(self, launch, scenario, engine='webkit'):
        with patch('browser_retry.sys.stderr'):
            run_engine_with_retry(engine, launch, scenario, **ERROR_TYPES)

    def test_restart_discards_browser_and_reacquires_fixture(self):
        first, second = Mock(), Mock()
        launch = Mock(side_effect=[first, second])
        fixtures = iter([object(), object()])
        seen = []

        def scenario(browser):
            # The AI scenario fetches /fixture inside each invocation, never
            # outside the retry loop; model that ownership here.
            seen.append((browser, next(fixtures)))
            if browser is first:
                raise PlaywrightError(INTERNAL)

        self.run_retry(launch, scenario)
        self.assertEqual([browser for browser, _ in seen], [first, second])
        self.assertIsNot(seen[0][1], seen[1][1])
        self.assertEqual(launch.call_count, 2)
        first.close.assert_called_once_with()
        second.close.assert_called_once_with()

    def test_other_engines_do_not_restart(self):
        for engine in ('chromium', 'firefox'):
            with self.subTest(engine=engine):
                error = PlaywrightError(INTERNAL)
                launch = Mock(return_value=Mock())
                with self.assertRaises(PlaywrightError) as caught:
                    self.run_retry(launch, Mock(side_effect=error), engine=engine)
                self.assertIs(caught.exception, error)
                launch.assert_called_once_with()

    def test_launch_internal_failure_retries_once(self):
        browser = Mock()
        launch = Mock(side_effect=[PlaywrightError('BrowserType.launch: Internal error'), browser])
        scenario = Mock()
        self.run_retry(launch, scenario)
        scenario.assert_called_once_with(browser)
        self.assertEqual(launch.call_count, 2)
        browser.close.assert_called_once_with()

    def test_retry_exhaustion_preserves_second_error(self):
        browsers = [Mock(), Mock()]
        launch = Mock(side_effect=browsers)
        original, final = PlaywrightError(INTERNAL), PlaywrightError(INTERNAL)
        scenario = Mock(side_effect=[original, final])
        with self.assertRaises(PlaywrightError) as caught:
            self.run_retry(launch, scenario)
        self.assertIs(caught.exception, final)
        self.assertEqual(launch.call_count, 2)
        browsers[0].close.assert_called_once_with()
        browsers[1].close.assert_not_called()

    def test_nonretryable_failure_preserves_context_for_diagnostics(self):
        for error in (AssertionError(INTERNAL), PlaywrightTimeout(INTERNAL),
                      PlaywrightError('Page.goto: Target page, context or browser has been closed')):
            with self.subTest(error=repr(error)):
                browser = Mock()
                browser.close.side_effect = PlaywrightError(INTERNAL)
                launch = Mock(return_value=browser)
                with self.assertRaises(type(error)) as caught:
                    self.run_retry(launch, Mock(side_effect=error))
                self.assertIs(caught.exception, error)
                launch.assert_called_once_with()
                browser.close.assert_not_called()

    def test_retry_cleanup_failure_does_not_replace_internal_error(self):
        first, second = Mock(), Mock()
        first.close.side_effect = RuntimeError('driver already gone')
        launch = Mock(side_effect=[first, second])
        scenario = Mock(side_effect=[PlaywrightError(INTERNAL), None])
        self.run_retry(launch, scenario)
        self.assertEqual(launch.call_count, 2)
        second.close.assert_called_once_with()

    def test_ai_script_fetches_fresh_fixture_and_context_after_restart(self):
        playwright = ModuleType('playwright')
        sync_api = ModuleType('playwright.sync_api')
        sync_api.Error, sync_api.TimeoutError = PlaywrightError, PlaywrightTimeout
        sync_api.expect = Mock()
        runtime = Mock()
        manager = Mock(__enter__=Mock(return_value=runtime), __exit__=Mock(return_value=False))
        sync_api.sync_playwright = Mock(return_value=manager)
        browsers = [Mock(), Mock()]
        runtime.webkit.launch.side_effect = browsers
        final = AssertionError('stop after verifying fresh fixture and context')
        for browser, error in zip(browsers, [PlaywrightError(INTERNAL), final]):
            browser.new_context.return_value.new_page.return_value.goto.side_effect = error
        requests = [Mock(), Mock()]
        for index, request in enumerate(requests):
            request.post.return_value.json.return_value = {'username': f'fresh-{index}'}
        runtime.request.new_context.side_effect = requests
        with tempfile.TemporaryDirectory() as artifacts, patch.dict(os.environ, {
            'POLARIS_TEST_ENGINES': 'webkit', 'POLARIS_AI_TEST_URL': 'http://local',
            'POLARIS_AI_TEST_CONTROL': 'http://control', 'POLARIS_AI_ARTIFACTS': artifacts,
        }), patch.dict(sys.modules, {'playwright': playwright, 'playwright.sync_api': sync_api}), \
                patch('browser_retry.sys.stderr'):
            with self.assertRaises(AssertionError) as caught:
                runpy.run_path(str(Path(__file__).with_name('ai-assistant-live.py')))
        self.assertIs(caught.exception, final)
        self.assertEqual(runtime.webkit.launch.call_count, 2)
        self.assertEqual(runtime.request.new_context.call_count, 2)
        for request in requests:
            request.post.assert_called_once_with('http://control/fixture')
            request.dispose.assert_called_once_with()
        for browser in browsers:
            browser.new_context.assert_called_once_with(viewport={'width': 320, 'height': 960}, locale='zh-CN')
        browsers[0].close.assert_called_once_with()
        browsers[1].close.assert_not_called()

    def test_teardown_does_not_retry(self):
        error = PlaywrightError(INTERNAL)
        browser = Mock()
        browser.close.side_effect = error
        launch = Mock(return_value=browser)
        with self.assertRaises(PlaywrightError) as caught:
            self.run_retry(launch, Mock())
        self.assertIs(caught.exception, error)
        launch.assert_called_once_with()

    def test_success_does_not_retry(self):
        browser = Mock()
        launch, scenario = Mock(return_value=browser), Mock()
        self.run_retry(launch, scenario)
        launch.assert_called_once_with()
        scenario.assert_called_once_with(browser)
        browser.close.assert_called_once_with()


if __name__ == '__main__':
    unittest.main()
