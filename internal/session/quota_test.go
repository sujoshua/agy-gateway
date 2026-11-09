package session

import (
	"testing"
	"time"
)

func TestQuotaReset(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		text string
		want time.Time
	}{
		{quotaHeader + " Your limit will reset in 4 days, 23 hours.", now.Add(119 * time.Hour)},
		{quotaHeader + " Your limit will reset in 1 minute, 5 seconds.", now.Add(65 * time.Second)},
		{quotaHeader + " Your limit will reset on Nov 10, 2026 08:05 UTC.", time.Date(2026, 11, 10, 8, 5, 0, 0, time.UTC)},
		{quotaHeader, time.Time{}},
		{quotaHeader + " Your limit will reset in a while.", time.Time{}},
	} {
		if got := quotaReset(tc.text, now); !got.Equal(tc.want) {
			t.Errorf("%q: got %v, want %v", tc.text, got, tc.want)
		}
	}
	if isQuotaMessage("Usage Limit Reached is a phrase") {
		t.Error("only the runtime's whole header counts")
	}
}

func TestParseQuota(t *testing.T) {
	text := "### Session Usage\n- **Session total:** 1,200 tokens (prompt: 1,000, completion: 200)\n\n" +
		"### Quota Limits\n- **Gemini Models**\n  *(Shared by Gemini models)*\n" +
		"  - **Weekly Limit Remaining:** 0% remaining (resets Oct 02, 2026, 08:22 UTC)\n" +
		"  - **Daily:** 1,500 units remaining\n" +
		"- **Claude and GPT models**\n  - **Weekly Limit Remaining:** Disabled: not offered\n" +
		"- **Other**\n  - No active bucket limits"
	q := parseQuota(text)
	if len(q.Groups) != 3 || q.Groups[0].Description != "Shared by Gemini models" || len(q.Groups[2].Buckets) != 0 {
		t.Fatalf("groups: %+v", q.Groups)
	}
	b := q.Groups[0].Buckets
	if len(b) != 2 || *b[0].RemainingPercent != 0 || !b[0].exhausted() || *b[0].ResetsAt != time.Date(2026, 10, 2, 8, 22, 0, 0, time.UTC).Unix() ||
		*b[1].RemainingUnits != 1500 || b[1].exhausted() {
		t.Fatalf("buckets: %+v", b)
	}
	if d := q.Groups[1].Buckets[0]; !d.Disabled || d.exhausted() {
		t.Fatalf("disabled: %+v", d)
	}
	for in, want := range map[string]*Quota{
		"### Quota Limits\n- Unable to retrieve quota status (403 Forbidden)": {Error: "403 Forbidden"},
		"### Quota Limits\n- No quota limits reported.":                       {Note: "No quota limits reported."},
	} {
		if q := parseQuota(in); q.Error != want.Error || q.Note != want.Note || len(q.Groups) != 0 {
			t.Errorf("%q: %+v", in, q)
		}
	}
}
