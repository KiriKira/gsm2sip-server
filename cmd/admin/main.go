package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kirikira/gsm2sip-server/internal/db"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := db.OpenAndMigrate(ctx, databaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "database initialization failed:", err)
		os.Exit(1)
	}
	defer database.Close()
	switch os.Args[1] {
	case "owner":
		if len(os.Args) < 3 || os.Args[2] != "create" {
			usage()
			os.Exit(2)
		}
		ownerCreate(ctx, database, os.Args[3:])
	case "pairing-code":
		if len(os.Args) < 3 || os.Args[2] != "create" {
			usage()
			os.Exit(2)
		}
		pairingCodeCreate(ctx, database, os.Args[3:])
	default:
		usage()
		os.Exit(2)
	}
}

func ownerCreate(ctx context.Context, database interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, args []string) {
	flags := flag.NewFlagSet("owner create", flag.ContinueOnError)
	name := flags.String("name", "", "owner display name")
	if err := flags.Parse(args); err != nil || strings.TrimSpace(*name) == "" || len(strings.TrimSpace(*name)) > 100 {
		fmt.Fprintln(os.Stderr, "usage: gsm2sip-admin owner create --name NAME")
		os.Exit(2)
	}
	id := newUUID()
	if _, err := database.ExecContext(ctx, `INSERT INTO owners(id,display_name) VALUES ($1,$2)`, id, strings.TrimSpace(*name)); err != nil {
		fmt.Fprintln(os.Stderr, "owner creation failed:", err)
		os.Exit(1)
	}
	fmt.Println(id)
}

func pairingCodeCreate(ctx context.Context, database interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, args []string) {
	flags := flag.NewFlagSet("pairing-code create", flag.ContinueOnError)
	ownerID := flags.String("owner-id", "", "owner UUID")
	role := flags.String("role", "", "gateway or client")
	ttlText := flags.String("ttl", "10m", "expiry, from 1m to 1h")
	if err := flags.Parse(args); err != nil || !isUUID(*ownerID) || *role != "gateway" && *role != "client" {
		fmt.Fprintln(os.Stderr, "usage: gsm2sip-admin pairing-code create --owner-id UUID --role gateway|client [--ttl 10m]")
		os.Exit(2)
	}
	ttl, err := time.ParseDuration(*ttlText)
	if err != nil || ttl < time.Minute || ttl > time.Hour {
		fmt.Fprintln(os.Stderr, "--ttl must be between 1m and 1h")
		os.Exit(2)
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		fmt.Fprintln(os.Stderr, "secure random generation failed")
		os.Exit(1)
	}
	code := base64.RawURLEncoding.EncodeToString(secret[:])
	hash := sha256.Sum256([]byte(code))
	expires := time.Now().UTC().Add(ttl)
	if _, err := database.ExecContext(ctx, `
		INSERT INTO pairing_codes(id,owner_id,role,code_hash,expires_at)
		VALUES ($1,$2,$3,$4,$5)`, newUUID(), *ownerID, *role, hash[:], expires); err != nil {
		fmt.Fprintln(os.Stderr, "pairing code creation failed:", err)
		os.Exit(1)
	}
	fmt.Printf("pairing_code=%s\nrole=%s\nexpires_at=%s\n", code, *role, expires.Format(time.RFC3339))
}

func newUUID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

func isUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f' || char >= 'A' && char <= 'F') {
			return false
		}
	}
	return true
}

func usage() {
	fmt.Fprintln(os.Stderr, `Local-only administration:
  gsm2sip-admin owner create --name NAME
  gsm2sip-admin pairing-code create --owner-id UUID --role gateway|client [--ttl 10m]

Requires DATABASE_URL. Pairing-code create prints the one-time secret once.`)
}
