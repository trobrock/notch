package intercom

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/trobrock/notch/internal/extension"
)

// Keep durable session identity checks at the host boundary. The transport
// never gets permission to append to or wake a different active session.
type hostAdapter struct{ extension.Host }

func (h hostAdapter) AppendSessionEntryFor(ctx context.Context, id, kind string, data any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return h.Host.AppendSessionEntry(id, kind, data)
}
func (h hostAdapter) SessionEntries(ctx context.Context, kind string) ([]json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return h.Host.SessionEntries(kind)
}
func (h hostAdapter) FollowUpFor(ctx context.Context, id, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if guarded, ok := h.Host.(interface{ FollowUpFor(string, string) error }); ok {
		return guarded.FollowUpFor(id, text)
	}
	return errors.New("this host does not support session-guarded wakeups")
}
func (h hostAdapter) Notify(_ context.Context, text, level string) error {
	h.Host.Notify(text, level)
	return nil
}
func (h hostAdapter) SetStatus(_ context.Context, key, text string) error {
	h.Host.SetStatus(key, text)
	return nil
}
