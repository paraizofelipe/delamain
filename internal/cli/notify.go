package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/notify"
)

// testNotifier is what `notify --test` needs from the notifier on top of Notify.
type testNotifier interface {
	notify.Notifier
	Backend() string
	Backends() []string
	Send(ctx context.Context, m notify.Message) error
	SendTo(ctx context.Context, kind string, m notify.Message) error
}

// viaAll, viaDesktop and viaNtfy are the values of --via.
const (
	viaAll     = "all"
	viaDesktop = "desktop"
	viaNtfy    = notify.NtfyKind
)

func newNotifyCmd(load func() (config.Config, error)) *cobra.Command {
	var test bool
	var event, via string
	cmd := &cobra.Command{
		Use:   "notify --test [--event <event>] [--via all|desktop|ntfy]",
		Short: "Send a sample notification",
		Long: `Sends the sample toast of an event (default pr_opened) through the configured backends (the
desktop one and, when notify.ntfy is enabled, the ntfy topic), whatever notify.enabled and
notify.events say, without actions. --via picks one of them. Fails when no backend is available.`,
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
			kinds := n.Backends()
			if len(kinds) == 0 {
				return fmt.Errorf("no notification backend (notify.backend %s resolved to none, notify.ntfy off)", cfg.Notify.Backend)
			}
			switch via {
			case viaAll:
				if err := n.Send(c.Context(), m); err != nil {
					return fmt.Errorf("%s toast: %w", event, err)
				}
			case viaDesktop, viaNtfy:
				kind := via
				if via == viaDesktop {
					kind = n.Backend()
				}
				if !slices.Contains(kinds, kind) {
					return fmt.Errorf("no %s notification backend (configured: %s)", via, strings.Join(kinds, ", "))
				}
				kinds = []string{kind}
				if err := n.SendTo(c.Context(), kind, m); err != nil {
					return fmt.Errorf("%s toast via %s: %w", event, kind, err)
				}
			default:
				return fmt.Errorf("--via %q is not one of all|desktop|ntfy", via)
			}
			fmt.Fprintf(c.OutOrStdout(), "%s toast sent via %s\n", event, strings.Join(kinds, ", "))
			return nil
		},
	}
	cmd.Flags().BoolVar(&test, "test", false, "send the sample toast")
	cmd.Flags().StringVar(&event, "event", string(notify.EventPROpened), "event whose sample toast is sent")
	cmd.Flags().StringVar(&via, "via", viaAll, "backend to send through: all, desktop or ntfy")
	return cmd
}
