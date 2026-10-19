package notify

import "context"

// macosBackend shows toasts in the Notification Center.
type macosBackend struct {
	opts Options
}

func newMacOS(o Options) *macosBackend { return &macosBackend{opts: o} }

func (m *macosBackend) Notify(context.Context, Message) error { return nil }

func (m *macosBackend) Close() error { return nil }
