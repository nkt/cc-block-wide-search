# cc-block-wide-search

A `PreToolUse` hook for [Claude Code](https://claude.com/claude-code) that keeps searches inside the
project instead of letting them sweep the whole disk or your home directory.

`find / -iname "*.jar"` on a developer machine walks millions of inodes, takes tens of seconds, and
almost always answers a question that `find . -name "*.jar"` would have answered instantly. This hook
intercepts those calls before they run and tells the model what to do instead.

## What it blocks

Only three roots: `/`, your home directory, and the directory that holds all homes (`/Users`,
`/home`). Everything narrower is your business — a sibling repo, `~/projects`, `~/.gradle/caches`
and `/opt/homebrew` all pass through untouched.

It understands the tools that take a place to walk — `find`, `grep`/`egrep`/`fgrep`, `rg`, `ag`,
`fd`/`fdfind` — plus Claude Code's own `Grep` and `Glob` tools. Command prefixes (`sudo`, `xargs`,
`env`, `time`, …) are unwrapped, absolute binary paths (`/usr/bin/find`) are resolved to their name,
and each `;`/`|`/`&&`/backtick-separated segment is inspected on its own, so a wide search hiding in
the tail of a compound command is still caught.

```
find / -name "*.ts"                       blocked
sudo find ~ -maxdepth 3 -name "id_rsa"    blocked
echo hi && rg foo /Users/you              blocked
Grep(path="$HOME")                        blocked

find . -name "*.ts"                       allowed
rg -n foo ~/projects/other-repo/src       allowed
find ~/.gradle/caches -iname "*.jar"      allowed
ls /                                      allowed  (not a search)
```

## Escalation

A guardrail that cannot be overridden is a guardrail you end up deleting, so a genuinely intended
global search costs one confirmation rather than being walled off:

| | first attempt | same root again |
|---|---|---|
| **main thread** | `deny` + explanation | `ask` — the user confirms |
| **subagent** | `deny` + explanation | `deny` + "escalation isn't available here" |

Two details in that table were learned the hard way, by reading eight days of real transcripts:

**"Already warned" is tracked per agent, not per session.** Subagents inherit their parent's
`session_id`, so a session-wide ledger made a subagent's *first* attempt look like a repeat of a
warning that had landed in some other context — sometimes hours earlier, sometimes in a sibling
agent running in parallel. The hook keys its state on `agent_id`, which the payload sets only inside
a subagent.

**Escalation stays a `deny` inside a subagent.** There is nobody there to answer an `ask`, so it
resolves to a refusal — and that refusal reaches the model as a bare *"Permission for this tool use
was denied. Try a different approach"*, stripped of `permissionDecisionReason`. The escalation was
therefore destroying the one thing that made the block useful. Inside a subagent the second refusal
carries its own explanation instead.

For what it's worth, the transcripts also settled the question the escalation path exists for: in
12 out of 12 cases where the explanation actually reached the model, the very next command was
correctly scoped. Models take the hint the first time. It's the plumbing around the hint that needs
the care.

## Install

Requires Go 1.26+ to build.

```sh
git clone https://github.com/nkt/cc-block-wide-search
cd cc-block-wide-search
go test ./...
go build -trimpath -ldflags="-s -w" -o ~/.claude/hooks/bin/block-wide-search .
```

Then register it in `~/.claude/settings.json`:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Grep|Glob|Bash",
        "hooks": [
          {
            "type": "command",
            "command": "/Users/you/.claude/hooks/bin/block-wide-search",
            "statusMessage": "Checking search scope..."
          }
        ]
      }
    ]
  }
}
```

The path must be absolute — `~` is not expanded there. Runs in about 5 ms per tool call.

## Tuning

Everything worth changing is a map or a constant near the top of `main.go`:

- `searchCmds` — the commands whose arguments name a place to walk.
- `wideRoots` — what counts as too wide. Deliberately narrow; add `~/projects` here if you want
  cross-repo searches challenged too.
- `denyBody` / `subTail` / `askFmt` / `subRepeatFmt` — the text the model reads. `denyBody` names
  `~/.gradle/caches`, `~/.m2` and `/opt/homebrew` as legitimate out-of-project targets, because
  every escalation in the recorded history turned out to be a hunt for a dependency jar. Swap those
  for the caches your stack actually uses.

State lives in `~/.claude/hooks/state/wide-search/<session>__<agent>`, one line per warned root, and
is pruned after seven idle days.

## Scope

This is a guardrail against wasted time, not a security boundary. Anything that hides the path from
a plain reading of the command — a variable, a command substitution, a path assembled at runtime —
passes straight through, by design. The parser is quote-aware so that a pattern like
`rg "QR / external"` is not mistaken for a search of `/`; every case in
`TestRegressionsFromHistory` is a real false positive found by replaying 18783 historical `Bash`
calls through an earlier implementation.

The hook always exits 0 and stays silent on anything it cannot parse. A crashing or overzealous hook
would be worse than a missed search.

## License

MIT
