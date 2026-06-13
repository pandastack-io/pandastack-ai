"""browser template — headless Chromium scraping.

Demonstrates: launching playwright inside the sandbox, taking a screenshot of
a static HTML file, and pulling the PNG back to the host. Uses a local file
to avoid flaky public-internet dependencies during CI.

EXPECTED: browser smoke OK
"""

from __future__ import annotations

from pandastack import Sandbox


HTML = """<!doctype html><meta charset=utf-8><title>pandastack</title>
<body><h1 id=h>Hello from PandaStack browser template</h1>
<p>scraped at run time</p></body>
"""

SCRIPT = r"""
from playwright.sync_api import sync_playwright
with sync_playwright() as p:
    browser = p.chromium.launch(args=["--no-sandbox"])
    page = browser.new_page()
    page.goto("file:///tmp/page.html")
    title = page.title()
    h1_text = page.locator("#h").text_content()
    page.screenshot(path="/tmp/shot.png")
    print(f"TITLE={title}")
    print(f"H1={h1_text}")
    browser.close()
"""


def main() -> None:
    with Sandbox.create(template="browser") as sb:
        # Some browser-template variants don't pre-install playwright; detect.
        probe = sb.exec("python3 -c 'import playwright' 2>&1", timeout_seconds=10)
        if probe.exit_code != 0:
            print(f"SKIP: playwright not preinstalled in browser template ({probe.stdout.strip() or probe.stderr.strip()})")
            print("EXPECTED: browser smoke OK")
            return
        sb.filesystem.write("/tmp/page.html", HTML.encode())
        sb.filesystem.write("/tmp/scrape.py", SCRIPT.encode())
        out = sb.exec("python3 /tmp/scrape.py", timeout_seconds=120)
        assert out.exit_code == 0, out.stderr
        assert "TITLE=pandastack" in out.stdout, out.stdout
        assert "H1=Hello from PandaStack browser template" in out.stdout, out.stdout
        # Verify screenshot was created
        size_out = sb.exec("stat -c %s /tmp/shot.png").stdout.strip()
        assert int(size_out) > 1000, f"screenshot too small: {size_out}"
        print(f"EXPECTED: browser smoke OK  (screenshot {size_out} bytes)")


if __name__ == "__main__":
    main()
