"""code-interpreter template — Python with pandas/numpy/matplotlib pre-installed.

Demonstrates: running a multi-line Python script, reading the produced JSON
back to the host, and confirming a real numpy computation worked inside the
sandbox.

EXPECTED: code-interpreter smoke OK
"""

from __future__ import annotations

import json

from pandastack import Sandbox


SCRIPT = """
import json, numpy as np, pandas as pd

df = pd.DataFrame({"x": np.arange(10), "y": np.arange(10) ** 2})
summary = {
    "rows": len(df),
    "sum_y": int(df["y"].sum()),
    "numpy_version": np.__version__,
}
with open("/tmp/out.json", "w") as f:
    json.dump(summary, f)
print(json.dumps(summary))
""".strip()


def main() -> None:
    with Sandbox.create(template="code-interpreter") as sb:
        sb.filesystem.write("/tmp/script.py", SCRIPT.encode())
        out = sb.exec("python3 /tmp/script.py", timeout_seconds=60)
        assert out.exit_code == 0, out.stderr
        result = json.loads(out.stdout.strip())
        assert result["rows"] == 10
        assert result["sum_y"] == 285  # 0+1+4+9+...+81
        print(f"EXPECTED: code-interpreter smoke OK  (numpy={result['numpy_version']})")


if __name__ == "__main__":
    main()
