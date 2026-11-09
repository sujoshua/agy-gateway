package session

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/sujoshua/agy-gateway/internal/openai"
)

// The runtime does not fail a turn whose account quota is used up: it
// reports the error as the turn's only message text and ends the turn
// normally. That text is recognized here so the client gets a 429.
const quotaHeader = "Usage Limit Reached\n\nYou have reached your current quota for this period."

var (
	// The delay keeps only its two largest units and is rounded down.
	resetInRE = regexp.MustCompile(`Your limit will reset in ([^.]+)\.`)
	resetOnRE = regexp.MustCompile(`Your limit will reset on ([^.]+ UTC)\.`)
	unitRE    = regexp.MustCompile(`^(\d+) (day|hour|minute|second)s?$`)
)

var unitSize = map[string]time.Duration{"day": 24 * time.Hour, "hour": time.Hour, "minute": time.Minute, "second": time.Second}

// isQuotaMessage reports whether a message chunk is the runtime's report of
// an exhausted quota rather than model output.
func isQuotaMessage(text string) bool { return strings.HasPrefix(text, quotaHeader) }

// quotaReset returns when the quota in a quota message resets, or the zero
// time when the message does not say. A delay is rounded down by the
// runtime, so the result may be early, never late.
func quotaReset(text string, now time.Time) time.Time {
	if m := resetInRE.FindStringSubmatch(text); m != nil {
		var d time.Duration
		for part := range strings.SplitSeq(m[1], ", ") {
			u := unitRE.FindStringSubmatch(part)
			if u == nil {
				return time.Time{}
			}
			n, _ := strconv.Atoi(u[1])
			d += time.Duration(n) * unitSize[u[2]]
		}
		return now.Add(d)
	}
	if m := resetOnRE.FindStringSubmatch(text); m != nil {
		if t, err := time.Parse("Jan 2, 2006 15:04 UTC", m[1]); err == nil {
			return t
		}
	}
	return time.Time{}
}

// limitUntil records that the quota is exhausted until t. Requests fail
// fast with a 429 until then instead of reaching the runtime.
func (m *Manager) limitUntil(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.After(m.limited) {
		m.limited = t
	}
}

// Limited returns when the exhausted quota resets, or the zero time when
// requests are not being refused.
func (m *Manager) Limited() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Now().Before(m.limited) {
		return m.limited
	}
	return time.Time{}
}

// Quota is the account's quota as the runtime reports it.
type Quota struct {
	Groups []QuotaGroup `json:"groups"`
	// Note is set when the runtime reports no limits, for example "No
	// quota limits reported.".
	Note string `json:"note,omitempty"`
	// Error is set when the runtime could not fetch the quota.
	Error string `json:"error,omitempty"`
	// Text is the runtime's report as it was received.
	Text string `json:"text"`
}

// QuotaGroup is a set of models that share limits.
type QuotaGroup struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Buckets     []QuotaBucket `json:"buckets"`
}

// QuotaBucket is one limit. Remaining is given as a percentage or as units,
// whichever the runtime reports; both are absent when it reports neither.
type QuotaBucket struct {
	Name             string `json:"name"`
	RemainingPercent *int   `json:"remaining_percent,omitempty"`
	RemainingUnits   *int64 `json:"remaining_units,omitempty"`
	ResetsAt         *int64 `json:"resets_at,omitempty"`
	Disabled         bool   `json:"disabled,omitempty"`
	// Status is the runtime's wording, for example "91% remaining".
	Status string `json:"status"`
}

// exhausted reports whether nothing of the bucket is left.
func (b *QuotaBucket) exhausted() bool {
	return !b.Disabled && (b.RemainingPercent != nil && *b.RemainingPercent == 0 || b.RemainingUnits != nil && *b.RemainingUnits == 0)
}

// usageCommand is answered by the runtime itself, without a model call.
const usageCommand = "/usage"

var (
	groupRE   = regexp.MustCompile(`^- \*\*(.+)\*\*$`)
	descRE    = regexp.MustCompile(`^ {2}\*\((.*)\)\*$`)
	bucketRE  = regexp.MustCompile(`^ {2}- \*\*(.+?):\*\* (.*)$`)
	resetsRE  = regexp.MustCompile(` \(resets (.+)\)$`)
	percentRE = regexp.MustCompile(`^(\d+)% remaining$`)
	unitsRE   = regexp.MustCompile(`^([\d,]+) units remaining$`)
)

// parseQuota reads the quota section of the runtime's /usage report.
func parseQuota(text string) *Quota {
	q := &Quota{Groups: []QuotaGroup{}, Text: text}
	_, section, _ := strings.Cut(text, "### Quota Limits\n")
	var g *QuotaGroup
	for line := range strings.SplitSeq(section, "\n") {
		if m := groupRE.FindStringSubmatch(line); m != nil {
			q.Groups = append(q.Groups, QuotaGroup{Name: m[1], Buckets: []QuotaBucket{}})
			g = &q.Groups[len(q.Groups)-1]
			continue
		}
		if g == nil {
			if msg, ok := strings.CutPrefix(line, "- Unable to retrieve quota status ("); ok {
				q.Error = strings.TrimSuffix(msg, ")")
			} else if msg, ok := strings.CutPrefix(line, "- "); ok {
				q.Note = msg
			}
			continue
		}
		if m := descRE.FindStringSubmatch(line); m != nil {
			g.Description = m[1]
		} else if m := bucketRE.FindStringSubmatch(line); m != nil {
			g.Buckets = append(g.Buckets, parseBucket(m[1], m[2]))
		}
	}
	return q
}

func parseBucket(name, status string) QuotaBucket {
	b := QuotaBucket{Name: name, Status: status}
	if strings.HasPrefix(status, "Disabled") {
		b.Disabled = true
		return b
	}
	if m := resetsRE.FindStringSubmatchIndex(status); m != nil {
		if t, err := time.Parse("Jan 02, 2006, 15:04 UTC", status[m[2]:m[3]]); err == nil {
			at := t.Unix()
			b.ResetsAt = &at
		}
		status = status[:m[0]]
	}
	if m := percentRE.FindStringSubmatch(status); m != nil {
		n, _ := strconv.Atoi(m[1])
		b.RemainingPercent = &n
	} else if m := unitsRE.FindStringSubmatch(status); m != nil {
		n, _ := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
		b.RemainingUnits = &n
	}
	return b
}

// Quota asks the runtime for the account's quota. The answer is cached for
// quotaCacheTTL. A report with no exhausted bucket lifts an earlier 429.
func (m *Manager) Quota(ctx context.Context) (*Quota, *openai.Error) {
	m.quotaMu.Lock()
	defer m.quotaMu.Unlock()
	if m.quota != nil && time.Since(m.quotaAt) < quotaCacheTTL {
		return m.quota, nil
	}
	text, err := m.usage(ctx)
	if err != nil {
		return nil, err
	}
	q := parseQuota(text)
	if q.Error == "" && len(q.Groups) > 0 {
		left := true
		for _, g := range q.Groups {
			for i := range g.Buckets {
				if g.Buckets[i].exhausted() {
					left = false
				}
			}
		}
		if left {
			m.mu.Lock()
			m.limited = time.Time{}
			m.mu.Unlock()
		}
	}
	m.quota, m.quotaAt = q, time.Now()
	return q, nil
}

const quotaCacheTTL = 30 * time.Second

// usage runs the /usage command in a session of its own, which is closed
// afterwards. The command never reaches the model.
func (m *Manager) usage(ctx context.Context) (string, *openai.Error) {
	conn, err := m.opt.Runtime.Conn()
	if err != nil {
		return "", openai.Unavailable("%v", err)
	}
	dir, err := m.opt.Runtime.NewWorkDir()
	if err != nil {
		return "", openai.Internal("cannot create a session directory")
	}
	resp, err := conn.NewSession(ctx, sdk.NewSessionRequest{Cwd: dir, McpServers: []sdk.McpServer{}})
	if err != nil {
		m.opt.Runtime.RemoveWorkDir(dir)
		return "", upstreamError(err)
	}
	// busy keeps the session out of request matching and eviction.
	s := &Session{m: m, id: string(resp.SessionId), workDir: dir, q: newQueue(), busy: true, lastUsed: time.Now()}
	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()
	defer m.close(s, "quota query done")

	s.startTurn(m.ctx, []sdk.ContentBlock{sdk.TextBlock(usageCommand)})
	var text strings.Builder
	for {
		ev, ok := s.q.pop()
		if !ok {
			select {
			case <-s.q.wake:
				continue
			case <-ctx.Done():
				return "", openai.Timeout("timed out waiting for the quota report")
			}
		}
		switch ev.kind {
		case evText, evQuota:
			text.WriteString(ev.text)
		case evDone:
			if ev.err != nil {
				return "", upstreamError(ev.err)
			}
			return text.String(), nil
		default:
		}
	}
}
