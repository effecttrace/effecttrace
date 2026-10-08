#!/usr/bin/env python3
"""Validate the EffectTrace website: local links and assets resolve, data
files are valid JSON, and no page loads third-party scripts, fonts,
analytics or trackers."""
import json
import re
import sys
from html.parser import HTMLParser
from pathlib import Path

ALLOWED_EXTERNAL_LINK_PREFIXES = ("https://", "mailto:")
FORBIDDEN = [
    (re.compile(r"fonts\.googleapis|fonts\.gstatic", re.I), "Google Fonts"),
    (re.compile(r"google-analytics|googletagmanager|gtag\(|plausible|segment\.com|hotjar|clarity\.ms", re.I), "analytics"),
    (re.compile(r"document\.cookie", re.I), "cookies"),
    (re.compile(r"\.innerHTML\s*=", re.I), "innerHTML assignment (use textContent / DOM APIs)"),
    (re.compile(r"\beval\(|new Function\(", re.I), "dynamic code evaluation"),
]


class Refs(HTMLParser):
    def __init__(self):
        super().__init__()
        self.refs = []
        self.scripts = []
        self.ids = set()

    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if "id" in a:
            self.ids.add(a["id"])
        for key in ("href", "src"):
            if key in a and a[key]:
                self.refs.append((tag, key, a[key]))
        if tag == "script" and a.get("src"):
            self.scripts.append(a["src"])
        if tag == "link" and a.get("rel") == "stylesheet" and a.get("href", "").startswith("http"):
            self.scripts.append(a["href"])


def main(root: Path) -> int:
    if not root.is_dir():
        print(f"site directory {root} not found")
        return 1
    errors = []
    pages = sorted(root.rglob("*.html"))
    if not pages:
        errors.append("no HTML pages found")
    for page in pages:
        text = page.read_text(encoding="utf-8")
        p = Refs()
        p.feed(text)
        for src in p.scripts:
            if src.startswith(("http://", "https://", "//")):
                errors.append(f"{page}: third-party script or stylesheet {src}")
        for tag, key, ref in p.refs:
            if ref.startswith(ALLOWED_EXTERNAL_LINK_PREFIXES):
                if tag in ("script", "img", "link", "iframe") and key == "src" or (tag == "link" and key == "href" and not ref.startswith("https://github.com")):
                    errors.append(f"{page}: external resource {ref}")
                continue
            if ref.startswith("#"):
                if ref[1:] and ref[1:] not in p.ids:
                    errors.append(f"{page}: missing anchor {ref}")
                continue
            target = (page.parent / ref.split("#")[0].split("?")[0]).resolve()
            if ref.split("#")[0] and not target.exists():
                errors.append(f"{page}: broken link {ref}")
        for rx, what in FORBIDDEN:
            if rx.search(text):
                errors.append(f"{page}: forbidden {what}")
        if 'name="viewport"' not in text:
            errors.append(f"{page}: missing viewport meta")
        if "<html lang=" not in text:
            errors.append(f"{page}: missing lang attribute")
    for js in sorted(root.rglob("*.js")):
        text = js.read_text(encoding="utf-8")
        for rx, what in FORBIDDEN:
            if rx.search(text):
                errors.append(f"{js}: forbidden {what}")
        if re.search(r"fetch\(\s*['\"]https?://", text):
            errors.append(f"{js}: fetches a third-party URL")
    for data in sorted((root / "data").rglob("*.json")) if (root / "data").is_dir() else []:
        try:
            json.loads(data.read_text(encoding="utf-8"))
        except json.JSONDecodeError as e:
            errors.append(f"{data}: invalid JSON: {e}")
    for e in errors:
        print("  " + e)
    print(f"site-check: {len(pages)} pages, {len(errors)} problem(s)")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main(Path(sys.argv[1] if len(sys.argv) > 1 else "../effecttrace.github.io")))
