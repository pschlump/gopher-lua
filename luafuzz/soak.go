package luafuzz

// The chunk-resumable soak state (M6e, m6 plan §4.5/D6).
//
// A 7-night soak on a laptop that moves between networks daily must
// survive: Ctrl-C, lid-close sleep, kill -9, reboot, and IP changes —
// several times per day. Design:
//
//   - The case stream is a pure function of (master_seed, index)
//     (gen.go). Nothing generated is ever needed twice; progress is
//     exactly "the next index".
//
//   - Every completed case appends one line to a per-session chunk
//     journal (chunks/sNNNN.jsonl) — append-only, fsync'd lazily.
//     Findings append to findings/index.jsonl and additionally write
//     their .lua + .txt immediately, so a crash can never lose one.
//
//   - soak.json is a full state snapshot rewritten atomically
//     (tmp+rename) every Checkpoint interval, at every finding, and at
//     clean shutdown. On load, the snapshot is authoritative for
//     everything before State.NextCase; any journal lines with idx ≥
//     NextCase (cases finished after the last snapshot) are replayed
//     on top — no double counting, no lost cases.
//
//   - A "night" is a local calendar date with soak work on it. A night
//     qualifies at NightMinWall of active fuzz time and is clean when
//     it surfaced no finding. The 7-night gate = 7 consecutive
//     qualifying clean nights (dates with no work neither extend nor
//     reset the streak — the laptop was asleep or at work).
//
//   - Sessions (chunks) record start/end, wall/case/exec counts, and
//     the network the chunk ran on (an IP→label map; the soak itself
//     binds nothing — the IP is bookkeeping only, so a mid-session
//     network move is recorded, not fatal).

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// State file/dir layout under the soak dir:
//
//	soak.json          snapshot (authoritative below NextCase)
//	chunks/sNNNN.jsonl per-session case journals
//	findings/          index.jsonl + cNNNNNNN-<CLASS>.{lua,txt}
//	tmp/               per-case engine working dir
//	lock               single-writer lock (pid)

const stateFile = "soak.json"
const stateVersion = 1

// Totals accumulates over the whole soak.
type Totals struct {
	Cases    int64   `json:"cases"`
	Execs    int64   `json:"execs"` // engine runs (the ×10⁷ accounting unit)
	WallMs   float64 `json:"wall_ms"`
	Findings int64   `json:"findings"`
}

// SigInfo is one distinct finding signature (dedup unit for the
// "zero NEW classes" night rule — a repeat of a known signature is
// counted, not new).
type SigInfo struct {
	Class     string `json:"class"`
	FirstCase int    `json:"first_case"`
	FirstDay  string `json:"first_day"`
	Count     int64  `json:"count"`
	Note      string `json:"note,omitempty"`
}

// NightInfo is one local-date bucket.
type NightInfo struct {
	Cases    int64   `json:"cases"`
	Execs    int64   `json:"execs"`
	WallMs   float64 `json:"wall_ms"`
	Findings int64   `json:"findings"`
	NewSigs  int64   `json:"new_sigs"`
	Traps    int64   `json:"traps"`
}

// NetPoint records the network context at a moment in a session.
type NetPoint struct {
	Time string `json:"time"`
	IP   string `json:"ip"`
	Net  string `json:"net"`
}

// Session is one invocation chunk.
type Session struct {
	ID        int64      `json:"id"`
	Start     string     `json:"start"`
	End       string     `json:"end,omitempty"`
	Engines   string     `json:"engines"`
	Host      string     `json:"host"`
	Nets      []NetPoint `json:"nets"`
	Cases     int64      `json:"cases"`
	Execs     int64      `json:"execs"`
	WallMs    float64    `json:"wall_ms"`
	Findings  int64      `json:"findings"`
	CleanExit bool       `json:"clean_exit"`
}

// State is the full soak snapshot.
type State struct {
	Version    int                  `json:"version"`
	MasterSeed int64                `json:"master_seed"`
	NextCase   int                  `json:"next_case"`
	Created    string               `json:"created"`
	Updated    string               `json:"updated"`
	Totals     Totals               `json:"totals"`
	Classes    map[string]int64     `json:"classes"`
	Signatures map[string]SigInfo   `json:"signatures"`
	Nights     map[string]NightInfo `json:"nights"`
	Sessions   []Session            `json:"sessions"`
}

func (s *State) dayKey(t time.Time) string { return t.Format("2006-01-02") }

// Options configures a soak run (or status read).
type Options struct {
	Dir          string            // soak state dir
	MasterSeed   int64             // seed for a NEW soak (0 → time-derived; must match on resume)
	Engines      []string          // engine spec per run (also recorded per session)
	Deadline     time.Duration     // per-case engine deadline (safety net)
	MaxCases     int               // stop after this many cases this chunk (0 = no cap)
	MaxDur       time.Duration     // stop after this much wall time this chunk (0 = no cap)
	Checkpoint   time.Duration     // snapshot interval
	NightMinWall time.Duration     // min active fuzz time for a night to qualify
	Nets         map[string]string // ip → label (bookkeeping only)
	Now          func() time.Time  // clock injection (tests)
}

const (
	DefaultDeadline   = 1500 * time.Millisecond
	DefaultCheckpoint = 30 * time.Second
	DefaultNightMin   = 30 * time.Minute
	sleepDropGap      = 2 * time.Minute // case wall above this + DL-TIE ⇒ machine slept mid-case
)

// ---- journal ----

type journalLine struct {
	I   int     `json:"i"`
	Cls string  `json:"cls"`
	Ms  float64 `json:"ms"`
	X   int     `json:"x"` // execs
	T   string  `json:"t"`
}

type chunkWriter struct {
	mu       sync.Mutex
	f        *os.File
	path     string
	lastSync time.Time
}

func (w *chunkWriter) append(l journalLine, forceSync bool) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.f.Write(append(b, '\n')); err != nil {
		return err
	}
	if forceSync || time.Since(w.lastSync) > 250*time.Millisecond {
		if err := w.f.Sync(); err != nil {
			return err
		}
		w.lastSync = time.Now()
	}
	return nil
}

func (w *chunkWriter) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.f.Sync(); err != nil {
		return err
	}
	return w.f.Close()
}

// ---- load / save ----

// LoadState reads the soak state and replays chunk journals on top of
// the snapshot (idempotently — journal lines with idx < NextCase are
// already baked into the snapshot and skipped).
func LoadState(dir string, now func() time.Time) (*State, error) {
	if now == nil {
		now = time.Now
	}
	raw, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("corrupt %s: %w", stateFile, err)
	}
	if s.Version != stateVersion {
		return nil, fmt.Errorf("state version %d ≠ %d", s.Version, stateVersion)
	}
	if err := s.replayJournals(dir); err != nil {
		return nil, err
	}
	return &s, nil
}

// replayJournals applies every chunk-journal line with idx ≥ NextCase
// (a crash between snapshot writes leaves at most the current chunk's
// tail unreplayed) and every findings-index line likewise.
func (s *State) replayJournals(dir string) error {
	// findings first so signatures exist when the chunk lines that
	// produced them are applied
	fidx, err := os.ReadFile(filepath.Join(dir, "findings", "index.jsonl"))
	if err == nil {
		for _, line := range strings.Split(strings.TrimRight(string(fidx), "\n"), "\n") {
			if line == "" {
				continue
			}
			var f FindingRecord
			if json.Unmarshal([]byte(line), &f) != nil {
				continue // torn tail write — skip
			}
			if f.Idx >= s.NextCase {
				s.registerSig(f.Class, f.Sig, f.Idx, f.Day, f.Note)
			}
		}
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "chunks", "*.jsonl"))
	sort.Strings(paths)
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			if line == "" {
				continue
			}
			var l journalLine
			if json.Unmarshal([]byte(line), &l) != nil {
				continue // torn tail write — the case is re-run, not lost
			}
			if l.I < s.NextCase {
				continue // already in the snapshot
			}
			s.applyOutcome(l.I, Class(l.Cls), l.Ms, l.X, l.T)
			s.NextCase = l.I + 1
		}
	}
	return nil
}

// applyOutcome updates every derived bucket for one completed case.
// day is RFC3339 local time (empty → derived at replay from T).
func (s *State) applyOutcome(idx int, cls Class, ms float64, execs int, dayISO string) {
	dk := dayISO
	if t, err := time.Parse(time.RFC3339, dayISO); err == nil {
		dk = t.Format("2006-01-02")
	}
	n := s.Nights[dk]
	n.Cases++
	n.Execs += int64(execs)
	n.WallMs += ms
	if isFindingClass(cls) {
		n.Findings++
		if cls == ClassTrap {
			n.Traps++
		}
	}
	s.Nights[dk] = n

	s.Classes[string(cls)]++
	s.Totals.Cases++
	s.Totals.Execs += int64(execs)
	s.Totals.WallMs += ms
}

// registerSig records (or counts) a finding signature; reports whether
// it is new-to-today (the night-cleanliness input).
func (s *State) registerSig(cls, sig string, idx int, day, note string) bool {
	key := string(cls) + "|" + sig
	prev, known := s.Signatures[key]
	s.Signatures[key] = SigInfo{Class: string(cls), FirstCase: idx, FirstDay: day,
		Count: prev.Count + 1, Note: note}
	if !known {
		s.Totals.Findings++
	}
	dk := day
	if t, err := time.Parse(time.RFC3339, day); err == nil {
		dk = t.Format("2006-01-02")
	}
	if !known {
		n := s.Nights[dk]
		n.NewSigs++
		s.Nights[dk] = n
		return true
	}
	return false
}

// SaveState writes the snapshot atomically.
func SaveState(dir string, s *State) error {
	s.Updated = nowISO(time.Now())
	b, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, stateFile+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, stateFile))
}

func nowISO(t time.Time) string { return t.Format(time.RFC3339) }

// ---- night accounting ----

// NightStatus is one rendered night row.
type NightStatus struct {
	Day       string
	Qualifies bool
	Clean     bool
	Cases     int64
	Execs     int64
	WallMs    float64
	Findings  int64
	NewSigs   int64
}

// NightReport returns nights newest-first plus the current clean
// streak (consecutive qualifying+clean nights walking back from the
// most recent night with work; work-free dates are skipped, an unclean
// or non-qualifying work date stops the walk).
func NightReport(s *State, nightMinWall time.Duration) ([]NightStatus, int) {
	var days []string
	for d := range s.Nights {
		days = append(days, d)
	}
	sort.Strings(days)
	out := make([]NightStatus, 0, len(days))
	for i := len(days) - 1; i >= 0; i-- {
		d := days[i]
		n := s.Nights[d]
		out = append(out, NightStatus{
			Day: d, Cases: n.Cases, Execs: n.Execs, WallMs: n.WallMs,
			Findings: n.Findings, NewSigs: n.NewSigs,
			Qualifies: time.Duration(n.WallMs)*time.Millisecond >= nightMinWall,
			Clean:     n.Findings == 0 && n.NewSigs == 0,
		})
	}
	streak := 0
	for i := len(days) - 1; i >= 0; i-- {
		n := s.Nights[days[i]]
		if n.Cases == 0 {
			continue // no work that date — skip, don't break
		}
		if n.Findings > 0 || n.NewSigs > 0 {
			break // any finding day resets the clock
		}
		if time.Duration(n.WallMs)*time.Millisecond < nightMinWall {
			continue // attempted-but-short (a lunch chunk) — doesn't extend
		}
		streak++
	}
	return out, streak
}

// ---- network bookkeeping ----

// OutboundIP returns the primary outbound IPv4 without sending a
// packet ("offline" when there is no route).
func OutboundIP() string {
	conn, err := net.Dial("udp", "10.255.255.255:1")
	if err != nil {
		return "offline"
	}
	defer conn.Close()
	ua, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "offline"
	}
	return ua.IP.String()
}

// NetLabel maps an IP through the configured ip→label map.
func NetLabel(ip string, nets map[string]string) string {
	if l, ok := nets[ip]; ok {
		return l
	}
	return "other"
}

// ---- single-writer lock ----

// AcquireLock takes dir/lock (O_EXCL); a stale lock whose pid is gone
// is stolen with a warning.
func AcquireLock(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err == nil {
		fmt.Fprintf(f, "%d\n", os.Getpid())
		return f, nil
	}
	// existing lock — is the writer alive?
	raw, _ := os.ReadFile(path)
	var pid int
	fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid)
	if pid > 0 && pid != os.Getpid() && processAlive(pid) {
		return nil, fmt.Errorf("soak already running (pid %d) — %s", pid, path)
	}
	os.Remove(path)
	f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return f, nil
}

func ReleaseLock(f *os.File, dir string) {
	if f != nil {
		f.Close()
		os.Remove(filepath.Join(dir, "lock"))
	}
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
