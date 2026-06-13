#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
from pathlib import Path
root = Path(__file__).resolve().parents[1]
include_dirs = [root / p for p in ("agent", "api", "dashboard", "sdks")]
skip_parts = {"node_modules", ".venv", "venv", "dist", ".next", "build", "coverage", "vendor", ".git"}
skip_suffixes = (".pb.go", ".gen.go", ".generated.go", ".min.js")
headers = {
    ".go": "// SPDX-License-Identifier: Apache-2.0\n",
    ".js": "// SPDX-License-Identifier: Apache-2.0\n",
    ".jsx": "// SPDX-License-Identifier: Apache-2.0\n",
    ".ts": "// SPDX-License-Identifier: Apache-2.0\n",
    ".tsx": "// SPDX-License-Identifier: Apache-2.0\n",
    ".mjs": "// SPDX-License-Identifier: Apache-2.0\n",
    ".cjs": "// SPDX-License-Identifier: Apache-2.0\n",
    ".py": "# SPDX-License-Identifier: Apache-2.0\n",
}
count = 0
for base in include_dirs:
    if not base.exists():
        continue
    for path in base.rglob("*"):
        if not path.is_file():
            continue
        if any(part in skip_parts for part in path.parts):
            continue
        if path.name.endswith(skip_suffixes):
            continue
        header = headers.get(path.suffix)
        if not header:
            continue
        text = path.read_text(errors="ignore")
        if "SPDX-License-Identifier:" in text[:400]:
            continue
        if text.startswith("#!"):
            first, rest = text.split("\n", 1)
            path.write_text(first + "\n" + header + rest)
        else:
            path.write_text(header + text)
        count += 1
print(f"added SPDX headers to {count} files")
