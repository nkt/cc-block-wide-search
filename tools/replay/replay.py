#!/usr/bin/env python3
"""Replay historical Bash calls from Claude Code transcripts through the hook.

The only real risk this hook carries is the false positive: a command like
`rg -c "QR / external wallet" src/i18n` reads as a search of `/` to a naive
parser, and blocking it would be worse than never having installed the hook at
all. Reasoning about the parser does not find those — replaying a few thousand
real commands does. Every case in TestRegressionsFromHistory came from this.

Run it after any change to the parsing, and read the blocked list by hand: the
number is not the answer, the commands are. Anything in there you would have
wanted to run is a bug.

    python3 tools/replay/replay.py --hook ./block-wide-search

Payloads are built without a session id on purpose, so the hook records no
state and every call is judged as a first attempt.
"""

import argparse
import json
import os
import pathlib
import re
import subprocess
import sys


def transcripts(root):
    yield from pathlib.Path(root).rglob("*.jsonl")


def bash_calls(path):
    """Yield (cwd, command) for every Bash tool call in one transcript."""
    try:
        text = path.read_text(errors="replace")
    except OSError:
        return
    # Cheap pre-filter: most lines are prose and never touch the JSON parser.
    if '"Bash"' not in text:
        return
    for line in text.splitlines():
        if '"Bash"' not in line:
            continue
        try:
            entry = json.loads(line)
        except ValueError:
            continue
        cwd = entry.get("cwd")
        content = entry.get("message", {}).get("content")
        if not cwd or not isinstance(content, list):
            continue
        for block in content:
            if not isinstance(block, dict) or block.get("type") != "tool_use":
                continue
            if block.get("name") != "Bash":
                continue
            command = (block.get("input") or {}).get("command")
            if isinstance(command, str) and command:
                yield cwd, command


ROOT = re.compile(r'^\w[\w ]* (?:scoped to|search of) "([^"]*)"')


def verdict(hook, cwd, command):
    """Return (decision, offending root), or None when the hook stays silent.

    The root is pulled back out of the reason because a long command truncated
    for display can easily hide the very token that triggered the block, which
    turns an obvious true positive into a suspicious-looking one.
    """
    payload = json.dumps(
        {"tool_name": "Bash", "cwd": cwd, "tool_input": {"command": command}}
    )
    out = subprocess.run(
        [hook], input=payload, capture_output=True, text=True
    ).stdout.strip()
    if not out:
        return None
    try:
        decision = json.loads(out)["hookSpecificOutput"]
    except (ValueError, KeyError):
        return None
    match = ROOT.match(decision.get("permissionDecisionReason", ""))
    return decision["permissionDecision"], match.group(1) if match else "?"


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument(
        "--transcripts",
        default=os.path.expanduser("~/.claude/projects"),
        help="directory of Claude Code transcripts (default: ~/.claude/projects)",
    )
    ap.add_argument(
        "--hook",
        default=os.path.expanduser("~/.claude/hooks/bin/block-wide-search"),
        help="hook binary to replay against",
    )
    ap.add_argument(
        "--width", type=int, default=140, help="truncate printed commands (0 = full)"
    )
    ap.add_argument(
        "--json", action="store_true", help="emit blocked calls as JSON lines"
    )
    args = ap.parse_args()

    if not os.path.exists(args.hook):
        sys.exit(f"hook binary not found: {args.hook}")

    total = 0
    # Judging is per (cwd, command): the same command in the same directory
    # cannot get two different answers, and history repeats itself a lot.
    seen = {}
    for path in transcripts(args.transcripts):
        for cwd, command in bash_calls(path):
            total += 1
            seen.setdefault((cwd, command), 0)
            seen[(cwd, command)] += 1

    if not total:
        sys.exit(f"no Bash calls found under {args.transcripts}")

    blocked = []
    for cwd, command in seen:
        result = verdict(args.hook, cwd, command)
        if result:
            decision, root = result
            blocked.append((decision, root, cwd, command, seen[(cwd, command)]))

    blocked_calls = sum(b[-1] for b in blocked)
    print(f"transcripts:      {args.transcripts}")
    print(f"Bash calls:       {total}")
    print(f"unique commands:  {len(seen)}")
    print(f"blocked calls:    {blocked_calls}  ({blocked_calls / total * 100:.3f}%)")
    print(f"unique blocked:   {len(blocked)}")

    if not blocked:
        return
    print("\n=== blocked commands — read these, do not just count them ===")
    print("    the root in brackets is what tripped it; if that is not in the")
    print("    visible part of the command, widen with --width 0\n")
    for decision, root, cwd, command, n in sorted(blocked, key=lambda b: -b[-1]):
        if args.json:
            print(
                json.dumps(
                    {
                        "decision": decision,
                        "root": root,
                        "cwd": cwd,
                        "command": command,
                        "seen": n,
                    }
                )
            )
            continue
        flat = " ".join(command.split())
        if args.width:
            flat = flat[: args.width]
        print(f"  [{decision} {root}] x{n:<4} {flat}")


if __name__ == "__main__":
    main()
