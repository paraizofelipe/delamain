package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/notify"
)

func newNotifyCmd(load func() (config.Config, error)) *cobra.Command {
	var test bool
	var event string
	cmd := &cobra.Command{
		Use:   "notify --test [--event <event>]",
		Short: "Send a sample desktop notification to check how it looks",
		Long: `Sends the sample toast of an event (default pr_opened) through the resolved backend, without
buttons. Fails when notifications are off or no notification server was found.
Events: ` + strings.Join(config.NotifyEvents, ", ") + ".",
		RunE: func(c *cobra.Command, _ []string) error {
			if !test {
				return errors.New("nothing to do: use --test")
			}
			cfg, err := load()
			if err != nil {
				return err
			}
			m, ok := notify.Example(notify.Event(event))
			if !ok {
				return fmt.Errorf("unknown event %q: use one of %s", event, strings.Join(config.NotifyEvents, ", "))
			}
			n := newNotifier(cfg.Notify, notify.Options{StateDir: cfg.Paths.StateDir, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))})
			defer func() { _ = n.Close() }()
			backend := notify.BackendOf(n)
			if backend == config.NotifyNone {
				return errors.New("backend resolved to none: notify.enabled is false, notify.backend is none or no notification server was found (see delamain doctor)")
			}
			m.Actions = nil
			if err := n.Notify(c.Context(), m); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "%s sample sent via %s\n", event, backend)
			return nil
		},
	}
	cmd.Flags().BoolVar(&test, "test", false, "send the sample toast")
	cmd.Flags().StringVar(&event, "event", string(notify.EventPROpened), "event whose sample is sent")
	return cmd
}
