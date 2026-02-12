package root

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"github.com/docker/cagent/pkg/config"
	"github.com/docker/cagent/pkg/teamloader"
	"github.com/docker/cagent/pkg/telemetry"
	"github.com/docker/cagent/pkg/workflow"
)

func newWorkflowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Run graph-based workflows",
		GroupID: "core",
	}

	cmd.AddCommand(newWorkflowRunCmd())
	return cmd
}

type workflowRunFlags struct {
	inputs    []string
	runConfig config.RuntimeConfig
}

func newWorkflowRunCmd() *cobra.Command {
	var flags workflowRunFlags

	cmd := &cobra.Command{
		Use:   "run <agent-file> <graph-file>",
		Short: "Execute a .graph workflow",
		Long:  "Load an agent team and execute a .graph workflow file",
		Example: `  cagent workflow run ./agent.yaml ./pipeline.graph
  cagent workflow run ./agent.yaml ./review.graph --input source_code="$(cat main.go)"
  cagent workflow run ./agent.yaml ./pipeline.graph --input prompt="Review this code"`,
		Args: cobra.ExactArgs(2),
		RunE: flags.runWorkflow,
	}

	cmd.Flags().StringArrayVar(&flags.inputs, "input", nil, "Seed graph input as type=content (repeatable)")
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *workflowRunFlags) runWorkflow(cmd *cobra.Command, args []string) error {
	telemetry.TrackCommand("workflow run", args)

	ctx := cmd.Context()
	agentFile := args[0]
	graphFile := args[1]

	// Parse inputs.
	inputs := make(map[string]string)
	for _, kv := range f.inputs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("invalid input %q: expected type=content", kv)
		}
		inputs[k] = v
	}

	// Load agent team.
	agentSource, err := config.Resolve(agentFile, f.runConfig.EnvProvider())
	if err != nil {
		return fmt.Errorf("resolve agent: %w", err)
	}

	loadResult, err := teamloader.LoadWithConfig(ctx, agentSource, &f.runConfig)
	if err != nil {
		return fmt.Errorf("load agents: %w", err)
	}
	defer func() {
		cleanupCtx := context.WithoutCancel(ctx)
		if err := loadResult.Team.StopToolSets(cleanupCtx); err != nil {
			slog.Error("Failed to stop tool sets", "error", err)
		}
	}()

	// Run workflow.
	exec := workflow.New(loadResult.Team,
		workflow.WithLogger(slog.Default()),
	)

	fmt.Fprintf(cmd.OutOrStdout(), "Running workflow: %s\n", graphFile)

	result, err := exec.RunFile(ctx, graphFile, inputs)
	if err != nil {
		return fmt.Errorf("workflow failed: %w", err)
	}

	// Print outputs.
	for nodeType, content := range result.Outputs {
		fmt.Fprintf(cmd.OutOrStdout(), "\n--- %s ---\n%s\n", nodeType, string(content))
	}

	// Print summary.
	fmt.Fprintf(cmd.OutOrStdout(), "\nWorkflow complete: %d steps run", result.StepsRun)
	if result.TotalCost > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), ", cost $%.4f", result.TotalCost)
	}
	fmt.Fprintln(cmd.OutOrStdout())

	if len(result.StepErrors) > 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "\nStep errors:")
		for step, stepErr := range result.StepErrors {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s: %v\n", step, stepErr)
		}
	}

	return nil
}
