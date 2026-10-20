// Package cli wires the delamain cobra commands.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/doctor"
	"github.com/paraizo/delamain/internal/herdr"
	"github.com/paraizo/delamain/internal/notify"
	"github.com/paraizo/delamain/internal/omp/rpc"
)

// Version is set at build time with -ldflags "-X github.com/paraizo/delamain/internal/cli.Version=...".
var Version = "dev"

// ErrChecksFailed signals a non-zero exit without an extra error message.
var ErrChecksFailed = errors.New("checks failed")

func NewRoot() *cobra.Command {
	var cfgPath string
	root := &cobra.Command{
		Use:           "delamain",
		Short:         "Autonomous orchestrator for development agents (omp; herdr optional)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&cfgPath, "config", config.DefaultPath(), "config file")
	load := func() (config.Config, error) { return config.Load(cfgPath, os.Getenv) }

	root.AddCommand(
		&cobra.Command{
			Use:   "version",
			Short: "Print the version",
			Run:   func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), Version) },
		},
		&cobra.Command{
			Use:   "config",
			Short: "Print the effective configuration (file + env)",
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := load()
				if err != nil {
					return err
				}
				return yaml.NewEncoder(cmd.OutOrStdout()).Encode(cfg)
			},
		},
		&cobra.Command{
			Use:   "doctor",
			Short: "Check omp (RPC mode), git, prompts, tools, forge auth and, when enabled, herdr",
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := load()
				if err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
				defer cancel()
				h := herdr.New(herdr.ExecRunner{Binary: cfg.Herdr.Binary, Session: cfg.Herdr.Session})
				set, err := loadPrompts(cfg)
				if err != nil {
					return err
				}
				checks := doctor.Run(ctx, cfg, doctor.Deps{
					LookPath: exec.LookPath, Version: binVersion, OmpRPC: ompRPCReady, ClaudeAuth: claudeAuth, Herdr: h,
					Prompts: set, PromptsTarget: promptsTarget(cfg),
					ForgeAuth: func(ctx context.Context, dir string) (doctor.ForgeIdentity, error) { return forgeAuth(ctx, cfg, dir) },
					Notify:    func(ctx context.Context) notify.Probe { return notify.Diagnose(ctx, cfg.Notify) },
				})
				if err := printChecks(cmd.OutOrStdout(), checks); err != nil {
					return err
				}
				if doctor.Failed(checks) {
					return ErrChecksFailed
				}
				return nil
			},
		},
		newPromptsCmd(load),
		newRunCmd(load),
		newDaemonCmd(load),
		newRecoverCmd(load),
		newNotifyCmd(load),
		newListCmd(load),
		newStatusCmd(load),
		newLogsCmd(load),
		newUpgradeCmd(),
	)
	return root
}

// claudeAuth asks claude whether it has credentials; the account itself is never printed.
func claudeAuth(ctx context.Context, bin string) error {
	out, err := exec.CommandContext(ctx, bin, "auth", "status", "--json").Output() //nolint:gosec // configured binary
	var st struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if jerr := json.Unmarshal(out, &st); jerr != nil {
		return errors.Join(errors.New("unreadable auth status"), err)
	}
	if !st.LoggedIn {
		return errors.New("not logged in")
	}
	return nil
}

// ompRPCReady starts omp in RPC mode without a session and stops it once it is ready.
func ompRPCReady(ctx context.Context, bin string) error {
	var stderr bytes.Buffer
	c, err := rpc.Start(ctx, rpc.Options{
		Binary: bin, Dir: os.TempDir(), Args: []string{"--no-session"},
		Stderr: &stderr, ReadyTimeout: 15 * time.Second, CloseTimeout: 5 * time.Second,
	})
	if err != nil {
		if msg, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n"); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return c.Close()
}

func binVersion(ctx context.Context, bin string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func printChecks(w io.Writer, checks []doctor.Check) error {
	icon := map[doctor.Status]string{doctor.OK: "✅", doctor.Warn: "⚠️ ", doctor.Fail: "❌"}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range checks {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", icon[c.Status], c.Name, c.Detail)
	}
	return tw.Flush()
}
