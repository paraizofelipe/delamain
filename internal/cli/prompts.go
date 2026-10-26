package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/prompts"
	embedded "github.com/paraizo/delamain/prompts"
)

// loadPrompts layers the configured override directory (if any) over the embedded defaults.
func loadPrompts(cfg config.Config) (*prompts.Set, error) {
	layers := []fs.FS{embedded.FS}
	if dir := cfg.Paths.PromptsDir; dir != "" {
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("paths.prompts_dir: %w", err)
		}
		layers = append([]fs.FS{os.DirFS(dir)}, layers...)
	}
	return prompts.Load(layers...)
}

func promptsTarget(cfg config.Config) prompts.Target {
	agents := map[string]prompts.AgentModel{}
	for name := range cfg.Agents {
		if model, reasoning := cfg.AgentOverride(name); model != "" || reasoning != "" {
			agents[name] = prompts.AgentModel{Model: model, Reasoning: reasoning}
		}
	}
	target := prompts.Target{
		Harness: string(cfg.Agent.Harness), OmpAgentDir: cfg.Omp.AgentDir,
		RolesDir: cfg.Paths.RolesDir(), ClaudeDir: cfg.Paths.ClaudeDir(), Agents: agents,
	}
	// agents.json embeds these overrides, and every repository shares the install dir: with
	// per-project overrides, concurrent runs would rewrite the file with each other's models
	// between sessions. A directory per override set keeps them apart — equal overrides share
	// one install (map keys marshal sorted, so the digest is stable). Only claude moves: omp
	// discovers its agent dir on its own, so relocating it would hide the agents from it.
	if cfg.Agent.Harness == config.HarnessClaude && len(agents) > 0 {
		b, _ := json.Marshal(agents)
		sum := sha256.Sum256(b)
		target.ClaudeDir = filepath.Join(target.ClaudeDir, hex.EncodeToString(sum[:6]))
	}
	return target
}

func newPromptsCmd(load func() (config.Config, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "prompts", Short: "Validate, install and preview the agents' .md prompts"}

	withSet := func(run func(*cobra.Command, config.Config, *prompts.Set, []string) error) func(*cobra.Command, []string) error {
		return func(c *cobra.Command, args []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			set, err := loadPrompts(cfg)
			if err != nil {
				return err
			}
			return run(c, cfg, set, args)
		}
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "lint",
			Short: "Validate manifest, frontmatter, references and templates",
			RunE: withSet(func(c *cobra.Command, _ config.Config, set *prompts.Set, _ []string) error {
				errs := set.Lint()
				for _, e := range errs {
					fmt.Fprintln(c.OutOrStdout(), "❌", e)
				}
				if len(errs) > 0 {
					return ErrChecksFailed
				}
				fmt.Fprintln(c.OutOrStdout(), "✅ prompts valid")
				return nil
			}),
		},
		&cobra.Command{
			Use:   "install",
			Short: "Install agents, skills and roles for the configured harness (omp or claude)",
			RunE: withSet(func(c *cobra.Command, cfg config.Config, set *prompts.Set, _ []string) error {
				written, err := set.Install(promptsTarget(cfg))
				for _, p := range written {
					fmt.Fprintln(c.OutOrStdout(), "📝", p)
				}
				return err
			}),
		},
		&cobra.Command{
			Use:   "render <stage>",
			Short: "Print a stage's prompt rendered with sample data",
			Args:  cobra.ExactArgs(1),
			RunE: withSet(func(c *cobra.Command, _ config.Config, set *prompts.Set, args []string) error {
				out, err := set.Render(args[0], prompts.SampleStageData())
				if err != nil {
					return err
				}
				fmt.Fprint(c.OutOrStdout(), out)
				return nil
			}),
		},
	)
	return cmd
}
