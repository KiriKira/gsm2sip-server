package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"rsc.io/qr"
)

const (
	joinScheme     = "mahogsm"
	joinMaxEntries = 8
)

type joinEntry struct{ server, code string }

type entryList []joinEntry

func (l *entryList) String() string { return fmt.Sprint(len(*l)) }

func (l *entryList) Set(text string) error {
	server, code, ok := cutLast(text, "=")
	if !ok {
		return errors.New("expected URL=CODE")
	}
	normalized, err := normalizeServer(server)
	if err != nil {
		return err
	}
	if code == "" || len(code) > 128 || strings.ContainsAny(code, " \t\r\n#") {
		return errors.New("the code must be a single token")
	}
	*l = append(*l, joinEntry{normalized, code})
	return nil
}

func cutLast(text, sep string) (string, string, bool) {
	index := strings.LastIndex(text, sep)
	if index < 0 {
		return "", "", false
	}
	return text[:index], text[index+len(sep):], true
}

// normalizeServer accepts the address phones use for this server.
func normalizeServer(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("server must be a plain http:// or https:// address: %q", raw)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// joinLink lists every node, highest priority first. Each phone pairs with
// the first one and adds the rest as nodes, under its own role.
func joinLink(entries []joinEntry) string {
	var builder strings.Builder
	builder.WriteString(joinScheme + "://join?v=2")
	for _, entry := range entries {
		builder.WriteString("&n=")
		builder.WriteString(url.QueryEscape(entry.server + "#" + entry.code))
	}
	return builder.String()
}

func joinCreate(ctx context.Context, database *sql.DB, args []string) {
	flags := flag.NewFlagSet("join create", flag.ContinueOnError)
	server := flags.String("url", os.Getenv("PUBLIC_API_URL"), "address the phones use for this server (default $PUBLIC_API_URL)")
	ownerID := flags.String("owner-id", "", "owner UUID; optional when the server has at most one owner")
	name := flags.String("name", "Home", "name of the owner created when the server has none")
	ttlText := flags.String("ttl", "15m", "expiry, from 1m to 1h")
	var also entryList
	flags.Var(&also, "also", "another node's join code as URL=CODE, in priority order; repeatable")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		joinUsage()
	}
	self, err := normalizeServer(*server)
	if err != nil {
		fmt.Fprintln(os.Stderr, "--url:", err)
		joinUsage()
	}
	ttl, err := time.ParseDuration(*ttlText)
	if err != nil || ttl < time.Minute || ttl > time.Hour {
		fmt.Fprintln(os.Stderr, "--ttl must be between 1m and 1h")
		os.Exit(2)
	}
	entries := append([]joinEntry{{server: self}}, also...)
	if len(entries) > joinMaxEntries {
		fmt.Fprintf(os.Stderr, "at most %d nodes per link\n", joinMaxEntries)
		os.Exit(2)
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		key := strings.ToLower(entry.server)
		if seen[key] {
			fmt.Fprintln(os.Stderr, "each server may appear only once")
			os.Exit(2)
		}
		seen[key] = true
	}
	owner, err := resolveOwner(ctx, database, strings.TrimSpace(*ownerID), strings.TrimSpace(*name))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		fmt.Fprintln(os.Stderr, "secure random generation failed")
		os.Exit(1)
	}
	code := base64.RawURLEncoding.EncodeToString(secret[:])
	hash := sha256.Sum256([]byte(code))
	expires := time.Now().UTC().Add(ttl)
	if _, err := database.ExecContext(ctx, `INSERT INTO join_codes(id,owner_id,code_hash,expires_at) VALUES ($1,$2,$3,$4)`,
		newUUID(), owner, hash[:], expires); err != nil {
		fmt.Fprintln(os.Stderr, "join code creation failed:", err)
		os.Exit(1)
	}
	entries[0].code = code
	link := joinLink(entries)
	fmt.Printf("join_code=%s\nowner_id=%s\nexpires_at=%s\nlink=%s\n\n", code, owner, expires.Format(time.RFC3339), link)
	if err := printQR(os.Stdout, link); err != nil {
		fmt.Fprintln(os.Stderr, "QR code unavailable:", err)
	}
	fmt.Println("\nScan with the old phone (Homura) and the main phone (Madoka); each joins once.")
}

func joinUsage() {
	fmt.Fprintln(os.Stderr, "usage: mahogsm-admin join create --url https://PUBLIC-ADDRESS [--owner-id UUID] [--name Home] [--ttl 15m] [--also URL=CODE ...]")
	os.Exit(2)
}

// resolveOwner picks the owner for a join code: the given one, the only one,
// or a new one on an empty server.
func resolveOwner(ctx context.Context, database *sql.DB, ownerID, name string) (string, error) {
	if ownerID != "" {
		if !isUUID(ownerID) {
			return "", errors.New("--owner-id must be a UUID")
		}
		var exists bool
		if err := database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM owners WHERE id=$1)`, ownerID).Scan(&exists); err != nil {
			return "", fmt.Errorf("owner lookup failed: %w", err)
		}
		if !exists {
			return "", errors.New("no owner has that ID")
		}
		return ownerID, nil
	}
	rows, err := database.QueryContext(ctx, `SELECT id::text FROM owners ORDER BY created_at LIMIT 2`)
	if err != nil {
		return "", fmt.Errorf("owner lookup failed: %w", err)
	}
	var owners []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", fmt.Errorf("owner lookup failed: %w", err)
		}
		owners = append(owners, id)
	}
	rows.Close()
	switch len(owners) {
	case 1:
		return owners[0], nil
	case 0:
		if name == "" || len(name) > 100 {
			return "", errors.New("--name must be 1 to 100 characters")
		}
		id := newUUID()
		if _, err := database.ExecContext(ctx, `INSERT INTO owners(id,display_name) VALUES ($1,$2)`, id, name); err != nil {
			return "", fmt.Errorf("owner creation failed: %w", err)
		}
		return id, nil
	default:
		return "", errors.New("this server has several owners; pass --owner-id")
	}
}

// printQR draws the code with half-block characters, two modules per line,
// with a quiet zone so phone cameras pick it up from a terminal.
func printQR(out io.Writer, text string) error {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return err
	}
	const quiet = 2
	size := code.Size
	dark := func(x, y int) bool {
		return x >= 0 && y >= 0 && x < size && y < size && code.Black(x, y)
	}
	var builder strings.Builder
	for y := -quiet; y < size+quiet; y += 2 {
		for x := -quiet; x < size+quiet; x++ {
			top, bottom := dark(x, y), dark(x, y+1)
			// Light modules are drawn, dark ones left blank: terminals are
			// usually dark, so this reads as dark modules on a light field.
			switch {
			case !top && !bottom:
				builder.WriteString("█")
			case !top:
				builder.WriteString("▀")
			case !bottom:
				builder.WriteString("▄")
			default:
				builder.WriteString(" ")
			}
		}
		builder.WriteString("\n")
	}
	_, err = io.WriteString(out, builder.String())
	return err
}
