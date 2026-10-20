package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/notify"
)

// testNotifier is what `notify --test` needs from the notifier on top of Notify.
type testNotifier interface {
	notify.Notifier
	Backend() string
	Send(ctx context.Context, m notify.Message) error
}

func newNotifyCmd(load func() (config.Config, error)) *cobra.Command {
	var test bool
	var event string
	cmd := &cobra.Command{
		Use:   "notify --test [--event <event>]",
		Short: "Send a sample desktop notification",
		Long: `Sends the sample toast of an event (default pr_opened) through the configured backend,
whatever notify.enabled and notify.events say, without actions. Fails when no backend is
available (backend none).`,
		RunE: func(c *cobra.Command, _ []string) error {
			if !test {
				return errors.New("use --test to send a sample notification")
			}
			cfg, err := load()
			if err != nil {
				return err
			}
			m, err := notify.Sample(notify.Event(event))
			if err != nil {
				return err
			}
			nc := cfg.Notify
			nc.Enabled, nc.Events = true, config.NotifyEvents
			n, ok := newNotifier(nc, notify.Options{StateDir: cfg.Paths.StateDir}).(testNotifier)
			if !ok {
				return errors.New("this notifier cannot send a test toast")
			}
			defer func() { _ = n.Close() }()
			if n.Backend() == config.NotifyNone {
				return fmt.Errorf("no desktop notification backend (notify.backend %s resolved to none)", cfg.Notify.Backend)
			}
			if err := n.Send(c.Context(), m); err != nil {
				return fmt.Errorf("%s toast via %s: %w", event, n.Backend(), err)
			}
			fmt.Fprintf(c.OutOrStdout(), "%s toast sent via %s\n", event, n.Backend())
			return nil
		},
	}
	cmd.Flags().BoolVar(&test, "test", false, "send the sample toast")
	cmd.Flags().StringVar(&event, "event", string(notify.EventPROpened), "event whose sample toast is sent")
	return cmd
}
