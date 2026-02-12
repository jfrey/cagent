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
		Use:     "workflow",
		Short:   "Run graph-based workflows",
		GroupID: "core",
	}

	cmd.AddCommand(newWorkflowRunCmd())
	return cmd
}

type workflowRunFlags struct {
	inputs    []string
	agents    string
	runConfig config.RuntimeConfig
}

func newWorkflowRunCmd() *cobra.Command {
	var flags workflowRunFlags

	cmd := &cobra.Command{
		Use:   "run <graph-file>",
		Short: "Execute a .cgt workflow",
		Long: `Execute a .cgt workflow file. Agent, model, and provider definitions
can be embedded directly in the .cgt file. Use --agents to provide
additional model/agent configuration from a YAML file (e.g., for
custom API endpoints or credentials).`,
		Example: `  cagent workflow run ./pipeline.cgt --input topic="Docker containers"
  cagent workflow run ./review.cgt --agents ./models.yaml --input prompt="Review this"
  cagent workflow run ./research.cgt --input question="What is the best search API?"`,
		Args: cobra.ExactArgs(1),
		RunE: flags.runWorkflow,
	}

	cmd.Flags().StringArrayVar(&flags.inputs, "input", nil, "Seed graph input as type=content (repeatable)")
	cmd.Flags().StringVar(&flags.agents, "agents", "", "Agent/model configuration YAML file (optional if .cgt defines models)")
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *workflowRunFlags) runWorkflow(cmd *cobra.Command, args []string) error {
	telemetry.TrackCommand("workflow run", nil)

	ctx := cmd.Context()
	graphFile := args[0]

	// Parse inputs.
	inputs := make(map[string]string)
	for _, kv := range f.inputs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("invalid input %q: expected type=content", kv)
		}
		inputs[k] = v
	}

	// Load agent team — either from --agents file or we'll build one from the .cgt file later.
	var loadResult *teamloader.LoadResult
	if f.agents != "" {
		agentSource, err := config.Resolve(f.agents, f.runConfig.EnvProvider())
		if err != nil {
			return fmt.Errorf("resolve agents: %w", err)
		}

		result, err := teamloader.LoadWithConfig(ctx, agentSource, &f.runConfig)
		if err != nil {
			return fmt.Errorf("load agents: %w", err)
		}
		loadResult = result
	}

	// Build executor options.
	var execOpts []workflow.Option
	execOpts = append(execOpts, workflow.WithLogger(slog.Default()))

	if loadResult != nil {
		defer func() {
			cleanupCtx := context.WithoutCancel(ctx)
			if err := loadResult.Team.StopToolSets(cleanupCtx); err != nil {
				slog.Error("Failed to stop tool sets", "error", err)
			}
		}()
	}

	// Run workflow. If no --agents file, pass nil team — the executor will
	// build agents from the .cgt file's definitions via AgentParams.
	var exec *workflow.Executor
	if loadResult != nil {
		exec = workflow.New(loadResult.Team, execOpts...)
	} else {
		exec = workflow.New(nil, execOpts...)
	}

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
