"""Curated benchmark tasks for LSP-assisted Python bug fixing."""

from __future__ import annotations

from copy import deepcopy


def _noise_files(prefix: str) -> dict[str, str]:
    return {
        f"/workspace/{prefix}_metrics.py": "def average(values):\n    return sum(values) / len(values) if values else 0\n\ndef clamp(value, low, high):\n    return max(low, min(high, value))\n",
        f"/workspace/{prefix}_strings.py": "def slugify(text):\n    return text.lower().replace(' ', '-')\n\ndef title_words(words):\n    return ' '.join(w.title() for w in words)\n",
        f"/workspace/{prefix}_config.py": "FEATURE_FLAGS = {'beta': False, 'audit': True}\nDEFAULT_REGION = 'us-east-1'\n",
    }


def get_tasks() -> list[dict]:
    tasks = [
        {
            "id": "01_renamed_helper",
            "files": {
                "/workspace/utils.py": "def calc_tax_rate(state):\n    rates = {'CA': 0.0825, 'NY': 0.08875}\n    return rates.get(state, 0.05)\n",
                "/workspace/main.py": "from utils import calc_tax_rate\n\ndef invoice_total(subtotal, state):\n    return round(subtotal * (1 + calculate_tax_rate(state)), 2)\n",
                "/workspace/test_main.py": "from main import invoice_total\n\ndef test_invoice_total_uses_state_tax():\n    assert invoice_total(100, 'CA') == 108.25\n",
                **_noise_files("noise01"),
            },
            "failing_test": "pytest -x /workspace/test_main.py",
            "prompt": "The test in /workspace/test_main.py is failing. Read it, find and fix the bug, then call submit().",
            "expected_fix_hint": "main.py calls calculate_tax_rate but utils.py defines calc_tax_rate",
            "dry_run_fix": {"/workspace/main.py": "from utils import calc_tax_rate\n\ndef invoice_total(subtotal, state):\n    return round(subtotal * (1 + calc_tax_rate(state)), 2)\n"},
        },
        {
            "id": "02_wrong_attribute",
            "files": {
                "/workspace/models.py": "class User:\n    def __init__(self, first, last):\n        self.first = first\n        self.last = last\n        self.full_name = f'{first} {last}'\n",
                "/workspace/greeter.py": "from models import User\n\ndef greet(user: User):\n    return f'Hello, {user.fullname}!'\n",
                "/workspace/test_main.py": "from models import User\nfrom greeter import greet\n\ndef test_greeting_uses_full_name():\n    assert greet(User('Ada', 'Lovelace')) == 'Hello, Ada Lovelace!'\n",
                **_noise_files("noise02"),
            },
            "failing_test": "pytest -x /workspace/test_main.py",
            "prompt": "The test in /workspace/test_main.py is failing. Read it, find and fix the bug, then call submit().",
            "expected_fix_hint": "class User has full_name; code uses user.fullname",
            "dry_run_fix": {"/workspace/greeter.py": "from models import User\n\ndef greet(user: User):\n    return f'Hello, {user.full_name}!'\n"},
        },
        {
            "id": "03_moved_symbol",
            "files": {
                "/workspace/helpers/__init__.py": "",
                "/workspace/helpers/date_utils.py": "from datetime import date\n\ndef format_date(value: date):\n    return value.strftime('%Y-%m-%d')\n",
                "/workspace/reports.py": "from helpers import format_date\n\ndef render_report(run_date):\n    return 'Report date: ' + format_date(run_date)\n",
                "/workspace/test_main.py": "from datetime import date\nfrom reports import render_report\n\ndef test_report_date_format():\n    assert render_report(date(2024, 2, 3)) == 'Report date: 2024-02-03'\n",
                **_noise_files("noise03"),
            },
            "failing_test": "pytest -x /workspace/test_main.py",
            "prompt": "The test in /workspace/test_main.py is failing. Read it, find and fix the bug, then call submit().",
            "expected_fix_hint": "from helpers import format_date but format_date lives in helpers.date_utils",
            "dry_run_fix": {"/workspace/reports.py": "from helpers.date_utils import format_date\n\ndef render_report(run_date):\n    return 'Report date: ' + format_date(run_date)\n"},
        },
        {
            "id": "04_wrong_arg_order",
            "files": {
                "/workspace/cart.py": "def add_item(qty: int, name: str):\n    if not isinstance(qty, int):\n        raise TypeError('qty must be int')\n    return {'name': name, 'qty': qty}\n\ndef build_cart():\n    return [add_item('apple', 3)]\n",
                "/workspace/test_main.py": "from cart import build_cart\n\ndef test_cart_item_shape():\n    assert build_cart() == [{'name': 'apple', 'qty': 3}]\n",
                **_noise_files("noise04"),
            },
            "failing_test": "pytest -x /workspace/test_main.py",
            "prompt": "The test in /workspace/test_main.py is failing. Read it, find and fix the bug, then call submit().",
            "expected_fix_hint": "add_item(qty:int, name:str) is called as add_item('apple', 3)",
            "dry_run_fix": {"/workspace/cart.py": "def add_item(qty: int, name: str):\n    if not isinstance(qty, int):\n        raise TypeError('qty must be int')\n    return {'name': name, 'qty': qty}\n\ndef build_cart():\n    return [add_item(3, 'apple')]\n"},
        },
        {
            "id": "05_typo_in_constant",
            "files": {
                "/workspace/settings.py": "HTTP_TIMEOUT_SECONDS = 15\nRETRY_COUNT = 2\n",
                "/workspace/client.py": "from settings import HTTP_TIMEOUT_SECONDS\n\ndef request_options():\n    return {'timeout': HTTP_TIMEOUT_SECS, 'retries': 2}\n",
                "/workspace/test_main.py": "from client import request_options\n\ndef test_request_options_timeout():\n    assert request_options() == {'timeout': 15, 'retries': 2}\n",
                **_noise_files("noise05"),
            },
            "failing_test": "pytest -x /workspace/test_main.py",
            "prompt": "The test in /workspace/test_main.py is failing. Read it, find and fix the bug, then call submit().",
            "expected_fix_hint": "HTTP_TIMEOUT_SECONDS is defined; code uses HTTP_TIMEOUT_SECS",
            "dry_run_fix": {"/workspace/client.py": "from settings import HTTP_TIMEOUT_SECONDS\n\ndef request_options():\n    return {'timeout': HTTP_TIMEOUT_SECONDS, 'retries': 2}\n"},
        },
    ]
    return deepcopy(tasks)


def select_tasks(selector: str | None) -> list[dict]:
    tasks = get_tasks()
    if not selector:
        return tasks
    wanted = {part.strip() for part in selector.split(",") if part.strip()}
    return [task for task in tasks if task["id"] in wanted or task["id"].split("_", 1)[0] in wanted]
