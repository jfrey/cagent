package teamloader

import (
	"context"
	"encoding/json"
	"fmt"

	graphagent "github.com/docker/cagent-graph/pkg/agent"
	"github.com/docker/cagent/pkg/config"
	"github.com/docker/cagent/pkg/config/latest"
	"github.com/docker/cagent/pkg/tools"
)

// ConvertGraphToolsets converts toolset definitions from .cgt files
// to cagent toolsets using the default toolset registry.
//
// This enables workflow agents defined in .cgt files to have access to
// the same toolsets (shell, filesystem, MCP, etc.) as agents defined in
// agent.yaml files.
func ConvertGraphToolsets(
	ctx context.Context,
	defs []graphagent.ToolsetDef,
	workDir string,
	runConfig *config.RuntimeConfig,
) ([]tools.ToolSet, []string) {
	if len(defs) == 0 {
		return nil, nil
	}

	registry := NewDefaultToolsetRegistry()

	var toolsets []tools.ToolSet
	var warnings []string

	for _, tsDef := range defs {
		// Convert ToolsetDef to latest.Toolset
		ts := latest.Toolset{
			Type: tsDef.Type,
		}

		// Unmarshal properties into the appropriate latest.Toolset fields
		if len(tsDef.Properties) > 0 {
			// Unmarshal the JSON properties into the toolset struct
			// latest.Toolset has all the fields for all toolset types
			if err := json.Unmarshal(tsDef.Properties, &ts); err != nil {
				warning := fmt.Sprintf("toolset %s: invalid properties: %v", ts.Type, err)
				warnings = append(warnings, warning)
				continue
			}
			// Ensure Type is preserved (unmarshal might override it)
			ts.Type = tsDef.Type
		}

		// Create toolset using registry (handles type-specific logic)
		created, err := registry.CreateTool(ctx, ts, workDir, runConfig)
		if err != nil {
			warning := fmt.Sprintf("toolset %s failed: %v", ts.Type, err)
			warnings = append(warnings, warning)
			continue
		}

		toolsets = append(toolsets, created)
	}

	return toolsets, warnings
}
