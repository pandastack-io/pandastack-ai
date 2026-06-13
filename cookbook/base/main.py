"""base template — the universal apps runtime.

`base` ships Node 22, Python 3.12, Go, and Bun via mise. It's the default for
apps and general-purpose workloads. This example verifies the runtimes are on
PATH and runs a tiny program in each that's installed.

EXPECTED: base smoke OK
"""

from __future__ import annotations

from pandastack import Sandbox


# (label, "detect" command, "run" command, expected substring)
CHECKS = [
    ("python", "command -v python3", "python3 -c 'print(2 + 2)'", "4"),
    ("node", "command -v node", "node -e 'console.log(6 * 7)'", "42"),
    ("go", "command -v go", "go version", "go version"),
    ("bun", "command -v bun", "bun --version", "."),
]


def main() -> None:
    with Sandbox.create(template="base") as sb:
        ran = []
        for label, detect, run, expect in CHECKS:
            d = sb.exec(detect, timeout_seconds=10)
            if d.exit_code != 0 or not d.stdout.strip():
                print(f"SKIP: {label} not on PATH")
                continue
            r = sb.exec(run, timeout_seconds=30)
            assert r.exit_code == 0, f"{label}: {r.stderr}"
            assert expect in r.stdout, f"{label}: expected {expect!r} in {r.stdout!r}"
            ran.append(label)
            print(f"{label}: {r.stdout.strip().splitlines()[0]}")
        assert ran, "no runtime found on the base template"
        print(f"EXPECTED: base smoke OK  ({', '.join(ran)})")


if __name__ == "__main__":
    main()
