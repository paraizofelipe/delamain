package cli

import (
	"fmt"
	"io/fs"
	"os"

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
	return prompts.Target{
		Harness: string(cfg.Agent.Harness), OmpAgentDir: cfg.Omp.AgentDir,
		RolesDir: cfg.Paths.RolesDir(), ClaudeDir: cfg.Paths.ClaudeDir(),
	}
}

func newPromptsCmd(load func() (config.Config, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "prompts", Short: "Valida, instala e pré-visualiza os prompts .md dos agentes"}

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
			Short: "Valida manifest, frontmatter, referências e templates",
			RunE: withSet(func(c *cobra.Command, _ config.Config, set *prompts.Set, _ []string) error {
				errs := set.Lint()
				for _, e := range errs {
					fmt.Fprintln(c.OutOrStdout(), "❌", e)
				}
				if len(errs) > 0 {
					return ErrChecksFailed
				}
				fmt.Fprintln(c.OutOrStdout(), "✅ prompts válidos")
				return nil
			}),
		},
		&cobra.Command{
			Use:   "install",
			Short: "Instala agents e skills no omp e os roles no state dir",
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
			Short: "Mostra o prompt de um passo renderizado com dados de exemplo",
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
