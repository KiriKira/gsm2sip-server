package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestJoinLinkListsNodesInOrder(t *testing.T) {
	var also entryList
	if err := also.Set("https://b.example.com/=CODE-B"); err != nil {
		t.Fatal(err)
	}
	link := joinLink(append([]joinEntry{{"https://a.example.com", "CODE-A"}}, also...))
	parsed, err := url.Parse(link)
	if err != nil || parsed.Scheme != "mahogsm" || parsed.Host != "join" {
		t.Fatalf("link %q: %v", link, err)
	}
	query := parsed.Query()
	if query.Get("v") != "2" || query.Get("role") != "" {
		t.Fatalf("query %v", query)
	}
	nodes := query["n"]
	if len(nodes) != 2 || nodes[0] != "https://a.example.com#CODE-A" || nodes[1] != "https://b.example.com#CODE-B" {
		t.Fatalf("nodes %v", nodes)
	}
}

func TestEntriesRejectUnsafeServers(t *testing.T) {
	var list entryList
	for _, bad := range []string{"https://a.example.com", "ftp://a=CODE", "https://user@a=CODE", "https://a/?x=1=CODE", "https://a=CO DE"} {
		if err := list.Set(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestPrintQRDrawsASquareWithQuietZone(t *testing.T) {
	var out strings.Builder
	if err := printQR(&out, "mahogsm://join?v=2&n=https%3A%2F%2Fa.example.com%23CODE"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	width := len([]rune(lines[0]))
	if width < 25 || len(lines) != (width+1)/2 {
		t.Fatalf("%d lines of width %d", len(lines), width)
	}
	if strings.Trim(lines[0], "█") != "" {
		t.Fatal("top quiet zone is not light")
	}
}
