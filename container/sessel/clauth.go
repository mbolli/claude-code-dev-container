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
	Harness     string          `json:"harness"`
	AuthStatus  string          `json:"auth_status"`
	FetchStatus string          `json:"fetch_status"`
	Stale       bool            `json:"stale"`
	Fallback    *clauthFallback `json:"fallback"`
	Windows     []clauthWindow  `json:"windows"`
}

type clauthStatus struct {
	ActiveProfile      string          `json:"active_profile"`
	ActiveCodexProfile string          `json:"active_codex_profile"`
	CodexFallbackChain []string        `json:"codex_fallback_chain"`
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
