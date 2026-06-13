#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
import json, os, re, subprocess
from pathlib import Path
root = Path(__file__).resolve().parents[1]
items = {}
lic_re = re.compile(r"\b(MIT|Apache-2\.0|Apache License|Apache License, Version 2\.0)\b", re.I)
def add(name, version, license_name, source):
    key=(name, version or "")
    if key not in items:
        items[key] = {"license": license_name or "unknown", "sources": set()}
    items[key]["sources"].add(source)
for nm in root.glob("*/node_modules"):
    for pkg_json in nm.glob("**/package.json"):
        if "/node_modules/." in str(pkg_json):
            continue
        try:
            pkg=json.loads(pkg_json.read_text())
        except Exception:
            continue
        name=pkg.get("name")
        if not name: continue
        license_name=pkg.get("license") or ""
        if isinstance(license_name, dict): license_name=license_name.get("type", "")
        license_file=None
        for cand in pkg_json.parent.iterdir():
            if cand.is_file() and cand.name.lower().startswith("license"):
                license_file=cand; break
        text=""
        if license_file:
            try: text=license_file.read_text(errors="ignore")[:20000]
            except Exception: text=""
        if lic_re.search(str(license_name)) or lic_re.search(text):
            label = "Apache-2.0" if re.search("Apache", str(license_name)+text, re.I) else "MIT"
            add(name, pkg.get("version"), label, str(pkg_json.parent.relative_to(root)))
for gomod in root.glob("**/go.mod"):
    if any(part in {"node_modules", ".git", "dist", ".next"} for part in gomod.parts):
        continue
    try:
        out=subprocess.check_output(["go", "list", "-m", "-json", "all"], cwd=gomod.parent, text=True, stderr=subprocess.DEVNULL)
    except Exception:
        continue
    for chunk in out.split("}\n{"):
        if not chunk.startswith("{"): chunk="{"+chunk
        if not chunk.endswith("}"): chunk=chunk+"}"
        try: mod=json.loads(chunk)
        except Exception: continue
        path=mod.get("Path"); ver=mod.get("Version", "")
        if not path or mod.get("Main"): continue
        d=Path(mod.get("Dir", ""))
        text=""
        if d.exists():
            for cand in d.iterdir():
                if cand.is_file() and cand.name.lower().startswith("license"):
                    try: text=cand.read_text(errors="ignore")[:20000]
                    except Exception: text=""
                    break
        if lic_re.search(text):
            label = "Apache-2.0" if re.search("Apache", text, re.I) else "MIT"
            add(path, ver, label, str(gomod.relative_to(root)))
lines=["PandaStack", "Copyright 2026 PandaStack Authors", "", "This product includes third-party software components licensed under permissive licenses.", "", "Third-party attributions", "------------------------"]
for (name, ver), meta in sorted(items.items(), key=lambda x: x[0][0].lower()):
    suffix=f" {ver}" if ver else ""
    lines.append(f"- {name}{suffix} — {meta['license']}")
lines.append("")
(root/"NOTICE").write_text("\n".join(lines))
