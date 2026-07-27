#!/usr/bin/env python3
"""Report where the hook actually fired, and what the model did next.

replay.py answers "what would this block". This answers the question that
matters more once the hook is installed: when it did block, did the model
recover? Each firing is printed with the tool calls that followed it, so a
model that re-scoped correctly is visible at a glance — as is one that argued
with the wall.

    python3 tools/replay/firings.py

Firings are grouped by session and labelled with the agent that made the call,
which is what exposed the bug this hook was fixed for: warnings landing in one
agent's context while a sibling agent got escalated over them.

The "escalated, reason not delivered" rows are the ones to watch. An "ask" that
reaches no human comes back to the model as a bare permission denial with the
hook's explanation stripped out; the row is inferred, since by definition the
transcript no longer contains the hook's own text.
"""

import argparse
import collections
import json
import os
import pathlib
import re
import sys

# Openings of the four messages the hook can produce.
HOOK_MARKERS = ("Search scoped to", "Global search of")

GENERIC_DENIAL = (
    "Permission for this tool use was denied",
    "The user doesn't want to proceed with this tool use",
)

# A search command with a bare wide root, used only to attribute a generic
# permission denial to this hook. Deliberately loose: it never decides
# anything, it just labels a row for a human to read.
WIDE_SEARCH = re.compile(
    r"\b(find|grep|egrep|fgrep|rg|ag|fd|fdfind)\b[^;|&\n]*?"
    r"(?<![\w/.-])(/|~|\$HOME|\$\{HOME\})(?=[\s\"']|$)"
)


def load(path):
    entries = []
    try:
        for line in path.read_text(errors="replace").splitlines():
            if line.strip():
                try:
                    entries.append(json.loads(line))
                except ValueError:
                    pass
    except OSError:
        pass
    return entries


def tool_uses(entry):
    content = entry.get("message", {}).get("content")
    if not isinstance(content, list):
        return
    for block in content:
        if isinstance(block, dict) and block.get("type") == "tool_use":
            yield block


def describe(name, tool_input):
    command = tool_input.get("command")
    if command:
        return " ".join(str(command).split())
    for key in ("path", "file_path", "pattern"):
        if tool_input.get(key):
            return f"{name}({key}={tool_input[key]})"
    return name


def identify(path, root):
    """Split a transcript path into (session, agent label)."""
    parts = pathlib.Path(path).relative_to(root).parts
    session = parts[1].removesuffix(".jsonl") if len(parts) > 1 else "?"
    stem = pathlib.Path(path).stem
    return session, "main" if stem == session else stem


def scan(path, root, follow):
    entries = load(path)
    uses = {}
    for index, entry in enumerate(entries):
        for block in tool_uses(entry):
            uses[block.get("id")] = (index, block.get("name"), block.get("input") or {})

    session, agent = identify(path, root)
    found = []
    for entry in entries:
        content = entry.get("message", {}).get("content")
        if not isinstance(content, list):
            continue
        for block in content:
            if not isinstance(block, dict) or block.get("type") != "tool_result":
                continue
            use = uses.get(block.get("tool_use_id"))
            if not use:
                continue
            index, name, tool_input = use
            result = json.dumps(block.get("content", ""), ensure_ascii=False)
            command = describe(name, tool_input)

            if any(m in result for m in HOOK_MARKERS):
                kind = "blocked"
            elif any(m in result for m in GENERIC_DENIAL) and WIDE_SEARCH.search(command):
                kind = "escalated, reason not delivered"
            else:
                continue

            after = []
            for later in entries[index + 1 :]:
                for block2 in tool_uses(later):
                    after.append(describe(block2.get("name"), block2.get("input") or {}))
                if len(after) >= follow:
                    break
            found.append(
                {
                    "when": entry.get("timestamp", ""),
                    "session": session,
                    "agent": agent,
                    "kind": kind,
                    "command": command,
                    "after": after[:follow],
                }
            )
    return found


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument(
        "--transcripts",
        default=os.path.expanduser("~/.claude/projects"),
        help="directory of Claude Code transcripts (default: ~/.claude/projects)",
    )
    ap.add_argument(
        "--follow", type=int, default=2, help="tool calls to show after each firing"
    )
    ap.add_argument("--width", type=int, default=120, help="truncate printed commands")
    args = ap.parse_args()

    root = pathlib.Path(args.transcripts)
    if not root.is_dir():
        sys.exit(f"not a directory: {root}")

    firings = []
    for path in root.rglob("*.jsonl"):
        firings.extend(scan(path, root, args.follow))
    if not firings:
        print(f"no firings found under {root}")
        return

    def cut(text):
        return text[: args.width] if args.width else text

    by_session = collections.defaultdict(list)
    for firing in firings:
        by_session[firing["session"]].append(firing)

    tally = collections.Counter()
    for session, items in sorted(by_session.items(), key=lambda kv: kv[1][0]["when"]):
        items.sort(key=lambda f: f["when"])
        print("=" * 96)
        print(f"session {session}")
        for firing in items:
            tally[firing["kind"]] += 1
            print(
                f"  {firing['when'][:19]}  {firing['kind']}"
                f"  [{firing['agent']}]\n    {cut(firing['command'])}"
            )
            for call in firing["after"]:
                print(f"      then: {cut(call)}")
    print("=" * 96)
    for kind, count in tally.most_common():
        print(f"{kind}: {count}")


if __name__ == "__main__":
    main()
