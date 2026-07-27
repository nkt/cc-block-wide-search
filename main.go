// PreToolUse hook: keep searches inside the project instead of sweeping the
// whole disk or the home directory.
//
// A first attempt is denied with an explanation. Repeating the same search root
// escalates to "ask", so a genuinely intended global search costs one
// confirmation instead of being walled off.
//
// "Already warned" is tracked per agent, not per session: subagents share their
// parent's session id, so session-wide state made a subagent's first attempt
// look like a repeat of a warning it never saw. Escalation also has to stay a
// "deny" inside a subagent — there is nobody there to answer an "ask", and the
// resulting refusal reaches the model stripped of this hook's reason, which is
// strictly worse than the explanation it replaces.
//
// This is a guardrail against wasted time, not a security boundary — anything
// that hides the path from a plain reading of the command (variables, command
// substitution inside double quotes) will pass straight through.
//
// Reads the hook payload as JSON on stdin, writes a decision as JSON on stdout,
// and always exits 0: a crashing hook would be worse than a missed search.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type hookIn struct {
	SessionID string `json:"session_id"`
	// AgentID is set only inside a subagent; the main thread leaves it empty.
	AgentID   string `json:"agent_id"`
	ToolName  string `json:"tool_name"`
	Cwd       string `json:"cwd"`
	ToolInput struct {
		Path    string `json:"path"`
		Command string `json:"command"`
	} `json:"tool_input"`
}

// denyBody explains the refusal. Naming the caches is not a detail: every
// escalation in the recorded history was a hunt for a dependency jar, which
// really is outside the project and really does have an address narrower than
// a filesystem root.
const denyBody = "Search scoped to %q — that is the whole disk or your home directory, not the project (%s). " +
	"You almost certainly want the project instead: use a relative path, or name the subdirectory you need. " +
	"If the target genuinely lives outside the project — a dependency jar, a build cache — name that directory " +
	"rather than a root: ~/.gradle/caches, ~/.m2, /opt/homebrew. "

const mainTail = "If this search must be global after all, run it again and it will be put to the user for confirmation."

const subTail = "There is no confirmation prompt inside a subagent, so a root will stay refused here: " +
	"pick a concrete directory, or report the limitation in your result and let the main thread ask the user."

const askFmt = "Global search of %q requested again after a scope warning. " +
	"Confirm with the user that searching outside the project is intended."

const subRepeatFmt = "Global search of %q was already refused in this agent. " +
	"Retrying cannot escalate to the user from inside a subagent. " +
	"Name a concrete directory (~/.gradle/caches, ~/.m2, the project itself), " +
	"or state in your result that the answer needs a search outside the project."

// searchCmds are the commands whose arguments name a place to walk.
var searchCmds = map[string]bool{
	"find": true, "grep": true, "egrep": true, "fgrep": true,
	"rg": true, "ag": true, "fd": true, "fdfind": true,
}

// cmdPrefixes wrap another command without changing which one is being run.
var cmdPrefixes = map[string]bool{
	"sudo": true, "xargs": true, "env": true, "time": true, "command": true,
	"nohup": true, "nice": true,
}

// valueFlags consume the following argument, so that argument is neither the
// search pattern nor a path. Missing an entry here is harmless: the value gets
// mistaken for the pattern, which only matters if it is literally "/" or "~".
var valueFlags = map[string]bool{
	"-e": true, "-f": true, "-m": true, "-A": true, "-B": true, "-C": true,
	"-d": true, "-g": true, "-t": true, "-T": true, "-E": true, "-x": true,
	"-X": true, "-j": true, "-M": true, "--include": true, "--exclude": true,
	"--glob": true, "--iglob": true, "--type": true, "--type-not": true,
	"--max-count": true, "--max-depth": true, "--search-path": true,
	"--threads": true, "--sort": true, "--pre": true, "--colors": true,
	"--binary-files": true, "--regexp": true, "--file": true,
}

// patternFlags supply the pattern directly, so no positional pattern follows.
var patternFlags = map[string]bool{
	"-e": true, "-f": true, "--regexp": true, "--file": true,
}

// heredocStart matches a here-document introducer, capturing its delimiter.
// Bit shifts ("1 << 2") do not match: a delimiter cannot start with a digit.
// Go's RE2 has no backreferences, so the closing quote is matched loosely.
var heredocStart = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)

// stripHeredocs drops here-document bodies. Their contents are data being
// written to a file, not commands about to run — a script containing the text
// "find /" is not a disk-wide search.
func stripHeredocs(cmd string) string {
	if !strings.Contains(cmd, "<<") {
		return cmd
	}
	lines := strings.Split(cmd, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		// A herestring (<<<) introduces no body.
		for _, m := range heredocStart.FindAllStringSubmatch(strings.ReplaceAll(lines[i], "<<<", ""), -1) {
			for i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != m[1] {
				i++
			}
			if i+1 < len(lines) {
				i++ // consume the closing delimiter
			}
		}
	}
	return strings.Join(out, "\n")
}

// splitSegments breaks a shell command into per-invocation segments of words,
// honouring single quotes, double quotes and backslash escapes, and stripping
// quotes from the result. Command-substitution and subshell punctuation starts
// a new segment so the inner command is inspected too.
//
// Quote awareness is what keeps a pattern like "QR / external" from being read
// as three words one of which is the filesystem root.
func splitSegments(cmd string) [][]string {
	const (
		plain = iota
		single
		double
	)

	var (
		segs    [][]string
		cur     []string
		w       strings.Builder
		hasWord bool
		state   = plain
	)

	flushWord := func() {
		if hasWord {
			cur = append(cur, w.String())
			w.Reset()
			hasWord = false
		}
	}
	flushSeg := func() {
		flushWord()
		if len(cur) > 0 {
			segs = append(segs, cur)
			cur = nil
		}
	}

	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch state {
		case single:
			if c == '\'' {
				state = plain
			} else {
				w.WriteRune(c)
				hasWord = true
			}
		case double:
			switch {
			case c == '"':
				state = plain
			case c == '\\' && i+1 < len(rs):
				i++
				w.WriteRune(rs[i])
				hasWord = true
			default:
				w.WriteRune(c)
				hasWord = true
			}
		default:
			switch {
			case c == '\'':
				state = single
				hasWord = true
			case c == '"':
				state = double
				hasWord = true
			case c == '\\' && i+1 < len(rs):
				i++
				w.WriteRune(rs[i])
				hasWord = true
			case c == ' ' || c == '\t' || c == '\r':
				flushWord()
			case strings.ContainsRune(";|&()`\n", c):
				flushSeg()
			default:
				w.WriteRune(c)
				hasWord = true
			}
		}
	}
	flushSeg()
	return segs
}

// searchPaths returns the arguments of a search invocation that name a place to
// walk. find takes its paths first, before the expression; the pattern-first
// tools (grep, rg, fd, ag) take everything after the pattern.
func searchPaths(name string, args []string) []string {
	needPattern := name != "find"

	var paths []string
	endOfFlags := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !endOfFlags {
			if a == "--" {
				endOfFlags = true
				continue
			}
			if strings.HasPrefix(a, "-") && a != "-" {
				if name == "find" {
					return paths // find's expression begins; no more paths
				}
				base, inline := a, strings.Contains(a, "=")
				if eq := strings.Index(a, "="); eq >= 0 {
					base = a[:eq]
				}
				if patternFlags[base] {
					needPattern = false
				}
				if valueFlags[base] && !inline {
					i++ // the next argument belongs to this flag
				}
				continue
			}
		}
		if needPattern {
			needPattern = false
			continue // this positional is the search pattern, not a path
		}
		paths = append(paths, a)
	}
	return paths
}

// wideRoots is what this hook refuses to search: the filesystem root, the
// user's home, and the directory holding all homes. Anything narrower — a
// sibling repo, ~/Projects — is the user's business.
func wideRoots(home string) map[string]bool {
	set := map[string]bool{"/": true}
	if home == "" || home == "/" {
		return set
	}
	set[filepath.Clean(home)] = true
	if parent := filepath.Dir(filepath.Clean(home)); parent != "/" && parent != "." {
		set[parent] = true
	}
	return set
}

// isWide reports whether tok names one of the roots we refuse to walk.
func isWide(tok, home string, wide map[string]bool) bool {
	switch {
	case tok == "":
		return false
	case tok == "~", tok == "~/", tok == "$HOME", tok == "${HOME}":
		tok = home
	case strings.HasPrefix(tok, "~/"):
		tok = home + "/" + tok[len("~/"):]
	case strings.HasPrefix(tok, "$HOME/"):
		tok = home + "/" + tok[len("$HOME/"):]
	case strings.HasPrefix(tok, "${HOME}/"):
		tok = home + "/" + tok[len("${HOME}/"):]
	}
	if !strings.HasPrefix(tok, "/") {
		return false // relative paths stay inside the project
	}
	return wide[filepath.Clean(tok)]
}

// decide returns the offending search root, and true when the call must not
// proceed unchallenged.
func decide(in hookIn, home string) (string, bool) {
	wide := wideRoots(home)

	switch in.ToolName {
	case "Grep", "Glob":
		if isWide(in.ToolInput.Path, home, wide) {
			return in.ToolInput.Path, true
		}
	case "Bash":
		for _, seg := range splitSegments(stripHeredocs(in.ToolInput.Command)) {
			for len(seg) > 0 && cmdPrefixes[seg[0]] {
				seg = seg[1:]
			}
			if len(seg) == 0 {
				continue
			}
			name := seg[0]
			if i := strings.LastIndex(name, "/"); i >= 0 {
				name = name[i+1:]
			}
			if !searchCmds[name] {
				continue
			}
			for _, p := range searchPaths(name, seg[1:]) {
				if isWide(p, home, wide) {
					return p, true
				}
			}
		}
	}
	return "", false
}

// stateKey identifies the context that gets warned. Subagents inherit their
// parent's session id, so the agent id has to be part of the key: without it,
// three subagents fanned out from one session share a single ledger and two of
// them are escalated over a warning that landed in somebody else's context.
func stateKey(in hookIn) string {
	if in.SessionID == "" {
		return ""
	}
	key := sanitize(in.SessionID)
	if in.AgentID != "" {
		key += "__" + sanitize(in.AgentID)
	}
	return key
}

// warned reports whether this agent was already told about target, recording it
// when it was not. An agent that insists gets escalated rather than walled off
// behind the same message again.
func warned(key, target, home string) bool {
	if key == "" || home == "" {
		return false
	}
	dir := filepath.Join(home, ".claude", "hooks", "state", "wide-search")
	if os.MkdirAll(dir, 0o755) != nil {
		return false
	}
	prune(dir)

	f := filepath.Join(dir, key)
	if data, err := os.ReadFile(f); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if line == target {
				return true
			}
		}
	}
	fh, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	defer fh.Close()
	fmt.Fprintln(fh, target)
	return false
}

// respond turns a blocked search into a permission decision and the reason the
// model will read. Only the main thread can be asked anything, so only there
// does a repeat become an "ask".
func respond(in hookIn, target string, repeat bool) (decision, reason string) {
	inSubagent := in.AgentID != ""
	switch {
	case repeat && !inSubagent:
		return "ask", fmt.Sprintf(askFmt, target)
	case repeat:
		return "deny", fmt.Sprintf(subRepeatFmt, target)
	case inSubagent:
		return "deny", fmt.Sprintf(denyBody+subTail, target, in.Cwd)
	default:
		return "deny", fmt.Sprintf(denyBody+mainTail, target, in.Cwd)
	}
}

// sanitize keeps an id safe to use as a filename.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, s)
}

// prune drops state for sessions that have been idle for a week.
func prune(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func main() {
	var in hookIn
	if err := json.NewDecoder(os.Stdin).Decode(&in); err != nil {
		return // malformed payload: fail open
	}
	if in.Cwd == "" {
		in.Cwd, _ = os.Getwd()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}

	target, blocked := decide(in, home)
	if !blocked {
		return
	}

	decision, reason := respond(in, target, warned(stateKey(in), target, home))

	json.NewEncoder(os.Stdout).Encode(map[string]any{
		"hookSpecificOutput": map[string]string{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       decision,
			"permissionDecisionReason": reason,
		},
	})
}
