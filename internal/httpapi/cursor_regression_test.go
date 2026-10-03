package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/kirikira/gsm2sip-server/internal/db"
)

type crFeedPrincipal struct {
	ownerID   string
	gatewayID string
	clientID  string
	token     string
}

func crOpenPostgres(t *testing.T) *sql.DB {
	t.Helper()
	rawURL := os.Getenv("TEST_DATABASE_URL")
	if rawURL == "" {
		t.Skip("TEST_DATABASE_URL is required for PostgreSQL regression tests")
	}

	baseConfig, err := pgx.ParseConfig(rawURL)
	if err != nil {
		t.Fatal("parse TEST_DATABASE_URL")
	}
	baseDB := stdlib.OpenDB(*baseConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := baseDB.PingContext(ctx); err != nil {
		_ = baseDB.Close()
		t.Fatal("connect to PostgreSQL test database")
	}

	schema := "cr_" + strings.ReplaceAll(newUUID(), "-", "")
	if _, err := baseDB.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = baseDB.Close()
		t.Fatal("create isolated PostgreSQL schema")
	}
	isolatedConfig := baseConfig.Copy()
	if isolatedConfig.RuntimeParams == nil {
		isolatedConfig.RuntimeParams = make(map[string]string)
	}
	isolatedConfig.RuntimeParams["search_path"] = schema
	testDB := stdlib.OpenDB(*isolatedConfig)
	testDB.SetMaxOpenConns(8)
	t.Cleanup(func() {
		_ = testDB.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := baseDB.ExecContext(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop isolated PostgreSQL schema: %v", err)
		}
		_ = baseDB.Close()
	})
	if err := testDB.PingContext(ctx); err != nil {
		t.Fatal("connect with isolated PostgreSQL schema")
	}
	if err := db.Migrate(ctx, testDB); err != nil {
		t.Fatalf("migrate isolated PostgreSQL schema: %v", err)
	}
	return testDB
}

func crCreateFeedPrincipal(t *testing.T, database *sql.DB) crFeedPrincipal {
	t.Helper()
	ctx := context.Background()
	principal := crFeedPrincipal{
		ownerID:   newUUID(),
		gatewayID: newUUID(),
		clientID:  newUUID(),
		token:     "cr-access-" + newUUID(),
	}
	_, err := database.ExecContext(ctx, `INSERT INTO owners(id,display_name) VALUES ($1,'cursor-test')`, principal.ownerID)
	if err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	_, err = database.ExecContext(ctx, `INSERT INTO devices(id,owner_id,role,name) VALUES
		($1,$3,'gateway','cursor-gateway'),($2,$3,'client','cursor-client')`, principal.gatewayID, principal.clientID, principal.ownerID)
	if err != nil {
		t.Fatalf("insert devices: %v", err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO gateways(device_id) VALUES ($1)`, principal.gatewayID); err != nil {
		t.Fatalf("insert gateway: %v", err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO sessions(id,device_id,access_hash,access_expires_at,refresh_hash,refresh_expires_at)
		VALUES ($1,$2,$3,now()+interval '1 hour',$4,now()+interval '1 day')`,
		newUUID(), principal.clientID, tokenHash(principal.token), tokenHash(principal.token+"-refresh")); err != nil {
		t.Fatalf("insert client session: %v", err)
	}
	return principal
}

func crInsertMessage(t *testing.T, database *sql.DB, principal crFeedPrincipal, direction, body, status string) string {
	t.Helper()
	messageID := newUUID()
	_, err := database.ExecContext(context.Background(), `INSERT INTO messages
		(id,owner_id,gateway_id,direction,body,status,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,now(),now())`, messageID, principal.ownerID, principal.gatewayID, direction, body, status)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}
	return messageID
}

func TestCursorCommitOrderAndRollbackDoNotCreateGaps(t *testing.T) {
	database := crOpenPostgres(t)
	principal := crCreateFeedPrincipal(t, database)
	message1 := crInsertMessage(t, database, principal, "outbound", "first", "queued")
	message2 := crInsertMessage(t, database, principal, "outbound", "second", "queued")
	messageRolledBack := crInsertMessage(t, database, principal, "outbound", "rolled back", "queued")
	message3 := crInsertMessage(t, database, principal, "outbound", "third", "queued")
	server := New(database, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	first, err := beginDurable(ctx, database)
	if err != nil {
		t.Fatalf("begin first transaction: %v", err)
	}
	defer first.Rollback()
	if err := lockOwnerEventCursor(ctx, first, principal.ownerID); err != nil {
		t.Fatalf("lock initial cursor: %v", err)
	}
	if err := server.addClientEvent(ctx, first, principal.ownerID, message1, "message.created"); err != nil {
		t.Fatalf("append first event: %v", err)
	}

	started := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		second, err := beginDurable(ctx, database)
		if err != nil {
			secondDone <- err
			return
		}
		defer second.Rollback()
		close(started)
		if err := lockOwnerEventCursor(ctx, second, principal.ownerID); err != nil {
			secondDone <- err
			return
		}
		if err := server.addClientEvent(ctx, second, principal.ownerID, message2, "message.created"); err != nil {
			secondDone <- err
			return
		}
		secondDone <- second.Commit()
	}()
	<-started
	blocked := false
	select {
	case err := <-secondDone:
		t.Fatalf("second event transaction passed the uncommitted owner cursor lock: %v", err)
	case <-time.After(150 * time.Millisecond):
		blocked = true
	}
	if err := first.Commit(); err != nil {
		t.Fatalf("commit first event transaction: %v", err)
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("commit second event transaction: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("second event transaction did not finish after the first commit")
	}
	if !blocked {
		t.Fatal("the second event transaction did not wait for the first commit")
	}

	rolledBack, err := beginDurable(ctx, database)
	if err != nil {
		t.Fatalf("begin rollback transaction: %v", err)
	}
	if err := lockOwnerEventCursor(ctx, rolledBack, principal.ownerID); err != nil {
		t.Fatalf("lock cursor for rollback transaction: %v", err)
	}
	if err := server.addClientEvent(ctx, rolledBack, principal.ownerID, messageRolledBack, "message.created"); err != nil {
		_ = rolledBack.Rollback()
		t.Fatalf("append event in rollback transaction: %v", err)
	}
	if err := rolledBack.Rollback(); err != nil {
		t.Fatalf("roll back event transaction: %v", err)
	}

	last, err := beginDurable(ctx, database)
	if err != nil {
		t.Fatalf("begin post-rollback transaction: %v", err)
	}
	if err := lockOwnerEventCursor(ctx, last, principal.ownerID); err != nil {
		_ = last.Rollback()
		t.Fatalf("lock cursor after rollback: %v", err)
	}
	if err := server.addClientEvent(ctx, last, principal.ownerID, message3, "message.created"); err != nil {
		_ = last.Rollback()
		t.Fatalf("append event after rollback: %v", err)
	}
	if err := last.Commit(); err != nil {
		t.Fatalf("commit event after rollback: %v", err)
	}

	rows, err := database.QueryContext(ctx, `SELECT cursor,event_json FROM server_events WHERE owner_id=$1 ORDER BY cursor`, principal.ownerID)
	if err != nil {
		t.Fatalf("query committed events: %v", err)
	}
	defer rows.Close()
	wantMessages := []string{message1, message2, message3}
	for index, expectedMessage := range wantMessages {
		if !rows.Next() {
			t.Fatalf("event %d is missing: %v", index+1, rows.Err())
		}
		var cursor int64
		var eventJSON []byte
		if err := rows.Scan(&cursor, &eventJSON); err != nil {
			t.Fatalf("scan committed event: %v", err)
		}
		if cursor != int64(index+1) {
			t.Errorf("committed cursor = %d, want %d", cursor, index+1)
		}
		var event struct {
			Message struct {
				MessageID string `json:"message_id"`
			} `json:"message"`
		}
		if err := json.Unmarshal(eventJSON, &event); err != nil {
			t.Fatalf("decode committed event: %v", err)
		}
		if event.Message.MessageID != expectedMessage {
			t.Errorf("event at cursor %d references message %s, want %s", cursor, event.Message.MessageID, expectedMessage)
		}
	}
	if rows.Next() {
		t.Fatal("rolled back event unexpectedly appeared in the feed")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read committed events: %v", err)
	}
}

func TestEventFeedIsOwnerScopedAndUsesCurrentMessageProjection(t *testing.T) {
	database := crOpenPostgres(t)
	ownerA := crCreateFeedPrincipal(t, database)
	ownerB := crCreateFeedPrincipal(t, database)
	messageA := crInsertMessage(t, database, ownerA, "outbound", "current owner A", "delivered")
	messageB := crInsertMessage(t, database, ownerB, "outbound", "owner B private", "queued")
	crInsertHistoricalEvent(t, database, ownerA, messageA, "queued", "old snapshot A", 1)
	crInsertHistoricalEvent(t, database, ownerB, messageB, "queued", "old snapshot B", 1)
	if _, err := database.ExecContext(context.Background(), `UPDATE messages SET status='delivered' WHERE id=$1`, messageA); err != nil {
		t.Fatalf("advance current message projection: %v", err)
	}

	handler := New(database, nil).Handler()
	for _, test := range []struct {
		name      string
		principal crFeedPrincipal
		wantID    string
		forbidID  string
		wantState string
		wantText  string
	}{
		{name: "owner A", principal: ownerA, wantID: messageA, forbidID: messageB, wantState: "delivered", wantText: "current owner A"},
		{name: "owner B", principal: ownerB, wantID: messageB, forbidID: messageA, wantState: "queued", wantText: "owner B private"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
			request.Header.Set("Authorization", "Bearer "+test.principal.token)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("GET /v1/events status = %d, body = %s", response.Code, response.Body.String())
			}
			var page struct {
				Items []struct {
					Message MessageDetail `json:"message"`
				} `json:"items"`
				NextCursor     *string `json:"next_cursor"`
				ResyncRequired bool    `json:"resync_required"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatalf("decode event page: %v", err)
			}
			if page.ResyncRequired || len(page.Items) != 1 {
				t.Fatalf("owner %s received %d events (resync=%t), want exactly one", test.name, len(page.Items), page.ResyncRequired)
			}
			message := page.Items[0].Message
			if message.MessageID != test.wantID || message.MessageID == test.forbidID {
				t.Errorf("owner %s received message %s, want only %s", test.name, message.MessageID, test.wantID)
			}
			if message.Status != test.wantState || message.Text != test.wantText {
				t.Errorf("owner %s event projection has status=%q text=%q, want status=%q text=%q", test.name, message.Status, message.Text, test.wantState, test.wantText)
			}
			if !bytes.Contains(response.Body.Bytes(), []byte(test.wantID)) {
				t.Errorf("owner %s response omitted its message identifier", test.name)
			}
			if bytes.Contains(response.Body.Bytes(), []byte(test.forbidID)) {
				t.Errorf("owner %s response disclosed another owner's message identifier", test.name)
			}
		})
	}
}

func crInsertHistoricalEvent(t *testing.T, database *sql.DB, principal crFeedPrincipal, messageID, staleStatus, staleText string, cursor int64) {
	t.Helper()
	eventID := newUUID()
	createdAt := time.Now().UTC()
	body, err := json.Marshal(map[string]any{
		"event_id":    eventID,
		"type":        "message.created",
		"occurred_at": createdAt,
		"message": map[string]any{
			"message_id": messageID,
			"status":     staleStatus,
			"text":       staleText,
		},
	})
	if err != nil {
		t.Fatalf("encode historical event: %v", err)
	}
	_, err = database.ExecContext(context.Background(), `INSERT INTO server_events
		(cursor,id,owner_id,event_type,message_id,event_json,created_at)
		VALUES ($1,$2,$3,'message.created',$4,$5,$6)`, cursor, eventID, principal.ownerID, messageID, body, createdAt)
	if err != nil {
		t.Fatalf("insert historical event: %v", err)
	}
}
