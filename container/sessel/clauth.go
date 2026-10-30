// clauth (~/.clauth) manages multi-account pools and automatic private->work
// fallback for Claude Code and Codex. sessel reads status.json for live quota
// windows and session attribution, and walks Codex back to the preferred
// private profile once its windows reset.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var clauthBin = "clauth"

func clauthHome() string {
	if h := os.Getenv("CLAUTH_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".clauth")
}

type clauthWindow struct {
	Label          string  `json:"label"`
	UtilizationPct float64 `json:"utilization_pct"`
	ResetsAt       string  `json:"resets_at"`
}

type clauthFallback struct {
	Position  int     `json:"position"`
	Threshold float64 `json:"threshold"`
	Armed     bool    `json:"armed"`
}

type clauthProfile struct {
	Name        string          `json:"name"`
	Active      bool            `json:"active"`
	Tier        string          `json:"tier"`
	Provider    string          `json:"provider"`
	Harness     string          `json:"harness"`
	AuthStatus  string          `json:"auth_status"`
	FetchStatus string          `json:"fetch_status"`
	FetchedAt   string          `json:"fetched_at"`
	Stale       bool            `json:"stale"`
	Fallback    *clauthFallback `json:"fallback"`
	Windows     []clauthWindow  `json:"windows"`
}

type clauthStatus struct {
	ActiveProfile      string          `json:"active_profile"`
	ActiveCodexProfile string          `json:"active_codex_profile"`
	CodexFallbackChain []string        `json:"codex_fallback_chain"`
	RefreshIntervalMs  int64           `json:"refresh_interval_ms"`
	Profiles           []clauthProfile `json:"profiles"`
	TPS                struct {
		Sessions map[string]struct {
			Account string `json:"account"`
		} `json:"sessions"`
	} `json:"tps"`
}

func loadClauthStatus() *clauthStatus {
	b, err := os.ReadFile(filepath.Join(clauthHome(), "status.json"))
	if err != nil {
		return nil
	}
	var st clauthStatus
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	return &st
}

// loadClauthSessionProfiles maps session UUIDs to their clauth profile name
// across session_profiles.json, tps_attribution.json, conversations/*.json,
// and status.json's live TPS table.
func loadClauthSessionProfiles(st *clauthStatus) map[string]string {
	out := map[string]string{}
	home := clauthHome()

	if b, err := os.ReadFile(filepath.Join(home, "session_profiles.json")); err == nil {
		var m map[string]string
		if json.Unmarshal(b, &m) == nil {
			for k, v := range m {
				if v != "" && v != "*" {
					out[k] = v
				}
			}
		}
	}

	if b, err := os.ReadFile(filepath.Join(home, "tps_attribution.json")); err == nil {
		var attr struct {
			Entries []struct {
				Session string `json:"session"`
				Member  string `json:"member"`
			} `json:"entries"`
		}
		if json.Unmarshal(b, &attr) == nil {
			for _, e := range attr.Entries {
				if e.Session != "" && e.Member != "" && e.Member != "*" {
					out[e.Session] = e.Member
				}
			}
		}
	}

	if files, err := filepath.Glob(filepath.Join(home, "conversations", "*.json")); err == nil {
		for _, f := range files {
			uuid := strings.TrimSuffix(filepath.Base(f), ".json")
			if !uuidRe.MatchString(uuid) {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var c struct {
				Profile     string `json:"profile"`
				LastProfile string `json:"last_profile"`
			}
			if json.Unmarshal(b, &c) == nil {
				if c.LastProfile != "" && c.LastProfile != "*" {
					out[uuid] = c.LastProfile
				} else if c.Profile != "" && c.Profile != "*" {
					out[uuid] = c.Profile
				}
			}
		}
	}

	if st != nil {
		for uuid, s := range st.TPS.Sessions {
			if s.Account != "" && s.Account != "*" {
				out[uuid] = s.Account
			}
		}
	}
	return out
}

// clauthProfileOverflowed reports whether p is currently serving as a non-primary
// fallback account (e.g. overflowed from private onto work).
func (st *clauthStatus) profileOverflowed(p clauthProfile) bool {
	if p.Harness == "codex" {
		return len(st.CodexFallbackChain) > 0 && st.CodexFallbackChain[0] != "" && p.Name != st.CodexFallbackChain[0]
	}
	return p.Fallback != nil && p.Fallback.Position > 1
}

// clauthWindowSummary formats the primary 5h and 7d window percentages for a profile.
func clauthWindowSummary(windows []clauthWindow) string {
	var parts []string
	for _, w := range windows {
		if w.Label == "5h" || w.Label == "7d" {
			parts = append(parts, fmt.Sprintf("%.0f%%", w.UtilizationPct))
		}
	}
	return strings.Join(parts, "/")
}

// clauthWindowDetail formats all windows with labels and reset times for the stats pane.
func clauthWindowDetail(windows []clauthWindow) string {
	if len(windows) == 0 {
		return "no windows yet"
	}
	var parts []string
	for _, w := range windows {
		s := fmt.Sprintf("%s %.0f%%", w.Label, w.UtilizationPct)
		if t, ok := parseTS(w.ResetsAt); ok && time.Until(t) > 0 {
			s += " (resets in " + span(time.Until(t)) + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

// reconcileCodexPreferred switches Codex back to the first profile in
// codex_fallback_chain once its usage windows have reset below 90%. clauth's
// daemon walks Claude back automatically via preferred=true, but hardcodes
// preferred=false on Codex chain snapshots.
func reconcileCodexPreferred(dry bool) error {
	st := loadClauthStatus()
	if st == nil || len(st.CodexFallbackChain) < 2 {
		return nil
	}
	preferred := st.CodexFallbackChain[0]
	if preferred == "" || st.ActiveCodexProfile == "" || st.ActiveCodexProfile == preferred {
		return nil
	}
	var pref *clauthProfile
	for i := range st.Profiles {
		p := &st.Profiles[i]
		if p.Harness == "codex" && p.Name == preferred {
			pref = p
			break
		}
	}
	if pref == nil || pref.AuthStatus != "ok" || pref.FetchStatus != "Fresh" || pref.Stale || len(pref.Windows) == 0 {
		return nil
	}
	for _, w := range pref.Windows {
		if w.UtilizationPct >= 90.0 {
			return nil
		}
	}
	fmt.Printf("clauth  codex %s -> %s (preferred window clear)\n", st.ActiveCodexProfile, preferred)
	if dry {
		return nil
	}
	path, err := exec.LookPath(clauthBin)
	if err != nil {
		return nil
	}
	if out, err := exec.Command(path, "switch", preferred).CombinedOutput(); err != nil {
		return fmt.Errorf("clauth switch %s: %s", preferred, strings.TrimSpace(string(out)))
	}
	return nil
}

// pctStyler colours a usage figure by how close it is to the cap. Only the
// TUI style implements it; plain output stays uncoloured.
type pctStyler interface {
	Pct(pct float64, s string) string
}

// claudeChain is the Claude fallback chain in position order.
func (cs *clauthStatus) claudeChain() []string {
	var ps []clauthProfile
	for _, p := range cs.Profiles {
		if p.Harness == AgentClaude && p.Fallback != nil && p.Fallback.Position > 0 {
			ps = append(ps, p)
		}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Fallback.Position < ps[j].Fallback.Position })
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

func usageBar(pct float64, width int) string {
	n := int(pct/100*float64(width) + 0.5)
	n = max(0, min(width, n))
	return strings.Repeat("█", n) + strings.Repeat("░", width-n)
}

func resetsIn(ts string) string {
	t, ok := parseTS(ts)
	if !ok {
		return ""
	}
	if d := time.Until(t); d > 0 {
		return "resets in " + span(d)
	}
	return "reset due"
}

// renderAccountsBody is the accounts view: every clauth profile per harness,
// in fallback order, with a bar and reset time for each usage window.
func renderAccountsBody(cs *clauthStatus, width int, st style) string {
	if cs == nil {
		return st.Dim("clauth is not set up: there is no ~/.clauth/status.json.\n" +
			"Run `clauth login <name>` and start `clauth daemon`, see the README.")
	}
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	pct := func(p float64, s string) string {
		if ps, ok := st.(pctStyler); ok {
			return ps.Pct(p, s)
		}
		return s
	}
	barW := max(10, min(24, width-40))

	sections := []struct {
		harness, title string
		chain          []string
	}{
		{AgentClaude, "Claude Code", cs.claudeChain()},
		{AgentCodex, "Codex", cs.CodexFallbackChain},
	}
	for _, sec := range sections {
		var ps []clauthProfile
		for _, p := range cs.Profiles {
			if p.Harness == sec.harness {
				ps = append(ps, p)
			}
		}
		if len(ps) == 0 {
			continue
		}
		pos := map[string]int{}
		for i, n := range sec.chain {
			pos[n] = i + 1
		}
		sort.SliceStable(ps, func(i, j int) bool {
			pi, pj := pos[ps[i].Name], pos[ps[j].Name]
			if pi == 0 {
				pi = 1 << 20
			}
			if pj == 0 {
				pj = 1 << 20
			}
			return pi < pj
		})

		if b.Len() > 0 {
			w("\n")
		}
		w("%s\n", st.Heading("── "+sec.title+" ──"))
		if len(sec.chain) > 0 {
			w("%s %s\n", st.Label("chain"), strings.Join(sec.chain, " → "))
			w("%s\n", st.Dim("      the first is preferred; the next takes over when it runs out"))
		}
		for _, p := range ps {
			mark, role := "○", "standby"
			if p.Active {
				mark, role = "●", "active"
				if cs.profileOverflowed(p) && len(sec.chain) > 0 {
					role = "active, overflowed from " + sec.chain[0]
				}
			}
			plan := p.Tier
			if plan == "" {
				plan = p.Provider
			}
			var notes []string
			if n := pos[p.Name]; n > 0 {
				notes = append(notes, fmt.Sprintf("#%d in chain", n))
			}
			if p.Fallback != nil && p.Fallback.Threshold > 0 {
				notes = append(notes, fmt.Sprintf("switches at %.0f%%", p.Fallback.Threshold))
			}
			switch {
			case p.AuthStatus != "" && p.AuthStatus != "ok" && p.AuthStatus != "unknown":
				notes = append(notes, "login "+p.AuthStatus)
			case p.FetchedAt != "":
				if t, ok := parseTS(p.FetchedAt); ok {
					u := "updated " + age(t) + " ago"
					if p.Stale {
						u += " (stale)"
					}
					notes = append(notes, u)
				}
			}
			head := fmt.Sprintf("%s %-15s %s", mark, p.Name, role)
			if p.Active {
				head = st.Who(head)
			}
			w("\n%s  %s\n", head, st.Dim(plan))
			if len(notes) > 0 {
				w("  %s\n", st.Dim(strings.Join(notes, " · ")))
			}
			if len(p.Windows) == 0 {
				w("  %s\n", st.Dim("no usage reported yet; the daemon fetches it on its next pass"))
				continue
			}
			for _, win := range p.Windows {
				label := fmt.Sprintf("%-9s", win.Label)
				w("  %s %s %s  %s\n", st.Label(label),
					pct(win.UtilizationPct, usageBar(win.UtilizationPct, barW)),
					pct(win.UtilizationPct, fmt.Sprintf("%3.0f%%", win.UtilizationPct)),
					st.Dim(resetsIn(win.ResetsAt)))
			}
		}
	}
	every := "90s"
	if cs.RefreshIntervalMs > 0 {
		every = fmt.Sprintf("%ds", cs.RefreshIntervalMs/1000)
	}
	w("\n%s\n", st.Dim("From ~/.clauth/status.json, refreshed by the clauth daemon every "+every+
		". `clauth` on the laptop logs accounts in and out."))
	return b.String()
}
