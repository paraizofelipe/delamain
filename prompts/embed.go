// Package prompts embeds the default prompts of delamain.
//
// The .md files are the source of truth and are meant to be edited directly:
// see manifest.yaml for the layout. `delamain prompts lint` validates changes,
// and `paths.prompts_dir` (or DELAMAIN_PROMPTS_DIR) overrides any file without rebuilding.
package prompts

import "embed"

//go:embed manifest.yaml roles agents skills stages
var FS embed.FS
