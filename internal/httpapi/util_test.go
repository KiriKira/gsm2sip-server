package httpapi

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestCommandDigestMatchesWireFixture(t *testing.T) {
	data, err := os.ReadFile("../../fixtures/api/command-list-response.json")
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Commands []Command `json:"commands"`
	}
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Commands) != 1 {
		t.Fatalf("fixture has %d commands, want one", len(page.Commands))
	}
	if got := commandDigest(page.Commands[0]); got != page.Commands[0].PayloadSHA256 {
		t.Fatalf("command digest = %s, fixture = %s", got, page.Commands[0].PayloadSHA256)
	}
}

func TestCommandDigestUsesImmutableFieldsOnly(t *testing.T) {
	command := Command{
		CommandID: "e6000000-0000-4000-8000-000000000001",
		MessageID: "e5000000-0000-4000-8000-000000000001",
		GatewayID: "e2000000-0000-4000-8000-000000000001",
		SIMID:     "e3000000-0000-4000-8000-000000000001", MappingRevision: 4,
		To: "+8613800000000", Text: "中文与 emoji 📱",
		ExpiresAt: time.Date(2026, 10, 3, 12, 5, 0, 0, time.FixedZone("test", 3600)),
		State:     "queued", CreatedAt: time.Now(),
	}
	first := commandDigest(command)
	command.State = "dispatching"
	command.CreatedAt = time.Now().Add(time.Hour)
	if second := commandDigest(command); second != first {
		t.Fatalf("mutable command fields changed digest: %s != %s", first, second)
	}
	command.Text += " changed"
	if second := commandDigest(command); second == first {
		t.Fatal("changing immutable SMS text did not change digest")
	}
}

func TestPartStateCanAdvanceIsMonotonic(t *testing.T) {
	cases := []struct {
		current, next string
		want          bool
	}{
		{"dispatching", "submitted", true},
		{"dispatching", "unknown", true},
		{"unknown", "delivered", true},
		{"submitted", "delivered", true},
		{"delivered", "dispatching", false},
		{"delivered", "failed", false},
		{"failed", "delivered", false},
		{"expired", "submitted", false},
		{"unknown", "dispatching", false},
		{"dispatching", "dispatching", false},
	}
	for _, tc := range cases {
		t.Run(tc.current+"_to_"+tc.next, func(t *testing.T) {
			if got := partStateCanAdvance(tc.current, tc.next); got != tc.want {
				t.Fatalf("partStateCanAdvance(%q, %q) = %t, want %t", tc.current, tc.next, got, tc.want)
			}
		})
	}
}
