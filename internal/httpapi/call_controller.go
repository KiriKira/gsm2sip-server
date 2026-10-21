package httpapi

import (
	"context"

	"github.com/kirikira/gsm2sip-server/internal/calls"
)

// CallController is the existing HTTP call-control boundary. Transport startup,
// ARI events, bridges and media belong to the implementation, not to HTTP handlers.
// The v1 DTOs (including the SIP intent URI) deliberately remain unchanged until
// the versioned WebRTC contract has been validated and introduced separately.
type CallController interface {
	Configured() bool
	CreateIntent(context.Context, string, string, calls.CreateIntentRequest, string) (calls.Intent, error)
	CancelIntent(context.Context, string, string, string) error
	ListCalls(context.Context, string, string, string, int) ([]calls.Call, string, error)
	GetCall(context.Context, string, string, string) (calls.Call, error)
	ClientReady(context.Context, string, string, string, string, string) (calls.Call, error)
	RenewExternalMedia(context.Context, string, string, string) error
}

var _ CallController = (*calls.Manager)(nil)
