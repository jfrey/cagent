package root

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/docker/cagent-graph/pkg/engine"
	graphpb "github.com/docker/cagent-graph/pkg/graph/v1"
	"github.com/docker/cagent-graph/pkg/runner"
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
	cmd.AddCommand(newWorkflowLintCmd())
	cmd.AddCommand(newWorkflowDescribeCmd())
	cmd.AddCommand(newWorkflowInitCmd())
	cmd.AddCommand(newWorkflowStatusCmd())
	return cmd
}

type workflowRunFlags struct {
	inputs    []string
	agents    string
	dbPath    string
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
	cmd.Flags().StringVar(&flags.dbPath, "db", "", "Path to SQLite graph database (persists between runs; default: ephemeral)")
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

	// Build executor options with a stderr logger that shows debug output.
	workflowLogger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var execOpts []workflow.Option
	execOpts = append(execOpts, workflow.WithLogger(workflowLogger))
	if f.dbPath != "" {
		execOpts = append(execOpts, workflow.WithDBPath(f.dbPath))
	}

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

func newWorkflowLintCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lint <graph-file>",
		Short: "Validate a .cgt workflow file",
		Long: `Parse and validate a .cgt workflow file without executing it.
Checks for syntax errors, type mismatches, and other issues.`,
		Example: `  cagent workflow lint ./pipeline.cgt
  cagent workflow lint ./review.cgt`,
		Args: cobra.ExactArgs(1),
		RunE: runWorkflowLint,
	}

	return cmd
}

func runWorkflowLint(cmd *cobra.Command, args []string) error {
	telemetry.TrackCommand("workflow lint", nil)

	graphFile := args[0]

	fmt.Fprintf(cmd.OutOrStdout(), "Linting workflow: %s\n", graphFile)

	// Use runner to compile (it handles DSL internally)
	workflows, err := compileWorkflowFile(graphFile)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "✓ Workflow is valid\n")
	fmt.Fprintf(cmd.OutOrStdout(), "  Found %d workflow(s)\n", len(workflows))
	for i, wf := range workflows {
		fmt.Fprintf(cmd.OutOrStdout(), "    %d. %s\n", i+1, wf.Name)
	}

	return nil
}

func newWorkflowDescribeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "describe <graph-file>",
		Short: "Show detailed information about a workflow",
		Long: `Parse a .cgt workflow file and display detailed information including:
- Workflow configuration (model, retries, etc.)
- Node types and their content types
- Steps with their agents, inputs, and outputs
- Agent, provider, and model definitions`,
		Example: `  cagent workflow describe ./pipeline.cgt
  cagent workflow describe ./review.cgt`,
		Args: cobra.ExactArgs(1),
		RunE: runWorkflowDescribe,
	}

	return cmd
}

func runWorkflowDescribe(cmd *cobra.Command, args []string) error {
	telemetry.TrackCommand("workflow describe", nil)

	graphFile := args[0]

	workflows, err := compileWorkflowFile(graphFile)
	if err != nil {
		return fmt.Errorf("parse workflow: %w", err)
	}

	for i, wf := range workflows {
		if i > 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "\n"+strings.Repeat("─", 80))
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Workflow: %s\n", wf.Name)
		fmt.Fprintln(cmd.OutOrStdout())

		// Configuration
		fmt.Fprintln(cmd.OutOrStdout(), "Configuration:")
		if wf.Config.Model != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "  Model: %s\n", wf.Config.Model)
		}
		if wf.Config.FallbackModel != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "  Fallback Model: %s\n", wf.Config.FallbackModel)
		}
		if wf.Config.MaxRetries > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "  Max Retries: %d\n", wf.Config.MaxRetries)
		}
		fmt.Fprintln(cmd.OutOrStdout())

		// Node Types
		if len(wf.Types) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "Node Types (%d):\n", len(wf.Types))
			for name, typeInfo := range wf.Types {
				contentType := typeInfo.ContentType
				if contentType == "" {
					contentType = "text/plain"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  • %s (%s)\n", name, contentType)
			}
			fmt.Fprintln(cmd.OutOrStdout())
		}

		// Steps
		if len(wf.Steps) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "Steps (%d):\n", len(wf.Steps))
			for j, step := range wf.Steps {
				fmt.Fprintf(cmd.OutOrStdout(), "  %d. Agent: %s\n", j+1, step.Agent)
				if len(step.Reads) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "     Reads: %s\n", strings.Join(step.Reads, ", "))
				}
				if len(step.Writes) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "     Writes: %s\n", strings.Join(step.Writes, ", "))
				}
				if step.Model != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "     Model: %s\n", step.Model)
				}
				if step.Timeout > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "     Timeout: %s\n", step.Timeout)
				}
				if step.Condition != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "     Condition: %s\n", step.Condition)
				}
				if step.Loop != nil {
					if step.Loop.Condition != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "     Loop: condition=%s", step.Loop.Condition)
						if step.Loop.Max > 0 {
							fmt.Fprintf(cmd.OutOrStdout(), ", max=%d", step.Loop.Max)
						}
						fmt.Fprintln(cmd.OutOrStdout())
					} else if step.Loop.Max > 0 {
						fmt.Fprintf(cmd.OutOrStdout(), "     Loop: max=%d\n", step.Loop.Max)
					}
				}
				if step.Parallel != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "     Parallel: %s\n", step.Parallel.Expression)
				}
			}
			fmt.Fprintln(cmd.OutOrStdout())
		}

		// Agents
		if len(wf.Agents) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "Agents (%d):\n", len(wf.Agents))
			for name := range wf.Agents {
				fmt.Fprintf(cmd.OutOrStdout(), "  • %s\n", name)
			}
			fmt.Fprintln(cmd.OutOrStdout())
		}

		// Providers
		if len(wf.Providers) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "Providers (%d):\n", len(wf.Providers))
			for name := range wf.Providers {
				fmt.Fprintf(cmd.OutOrStdout(), "  • %s\n", name)
			}
			fmt.Fprintln(cmd.OutOrStdout())
		}

		// Models
		if len(wf.Models) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "Models (%d):\n", len(wf.Models))
			for name := range wf.Models {
				fmt.Fprintf(cmd.OutOrStdout(), "  • %s\n", name)
			}
		}
	}

	return nil
}

func newWorkflowInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init [filename]",
		Short: "Create a new workflow file from a template",
		Long: `Generate a new .cgt workflow file with a basic template structure.
If no filename is provided, creates 'workflow.cgt' in the current directory.`,
		Example: `  cagent workflow init
  cagent workflow init pipeline.cgt
  cagent workflow init research/analysis.cgt`,
		Args: cobra.MaximumNArgs(1),
		RunE: runWorkflowInit,
	}

	return cmd
}

func runWorkflowInit(cmd *cobra.Command, args []string) error {
	telemetry.TrackCommand("workflow init", nil)

	filename := "workflow.cgt"
	if len(args) > 0 {
		filename = args[0]
	}

	// Check if file already exists
	if _, err := os.Stat(filename); err == nil {
		return fmt.Errorf("file already exists: %s", filename)
	}

	template := `agent analyzer {
  model "gpt-4o"
  instruction "You are a helpful AI assistant that analyzes input and provides detailed insights."
}

workflow example {
  config {
    model "gpt-4o"
    max_retries 3
  }

  graph {
    type user_input { content text/plain }
    type analysis { content text/markdown }
    type summary { content text/plain }
  }

  seed user_input from inputs

  step analyzer {
    reads [user_input]
    writes [analysis]
  }

  step analyzer {
    reads [analysis]
    writes [summary]
  }
}
`

	if err := os.WriteFile(filename, []byte(template), 0644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "✓ Created workflow file: %s\n", filename)
	fmt.Fprintln(cmd.OutOrStdout(), "\nNext steps:")
	fmt.Fprintf(cmd.OutOrStdout(), "  1. Edit the workflow: vim %s\n", filename)
	fmt.Fprintf(cmd.OutOrStdout(), "  2. Validate syntax: cagent workflow lint %s\n", filename)
	fmt.Fprintf(cmd.OutOrStdout(), "  3. Run the workflow: cagent workflow run %s --input user_input=\"your input here\"\n", filename)

	return nil
}

func newWorkflowStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <db-path>",
		Short: "Show status of a persisted workflow",
		Long: `Query a workflow database and show execution status including:
- Number of nodes created
- Completed vs pending steps
- Node types and counts

Requires a workflow that was run with the --db flag.`,
		Example: `  cagent workflow status ./workflow.db
  cagent workflow status /tmp/my-workflow.db`,
		Args: cobra.ExactArgs(1),
		RunE: runWorkflowStatus,
	}

	return cmd
}

func runWorkflowStatus(cmd *cobra.Command, args []string) error {
	telemetry.TrackCommand("workflow status", nil)

	dbPath := args[0]

	// Check if database exists
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("database not found: %s", dbPath)
	}

	// Open the graph store
	ctx := cmd.Context()
	graph, err := openGraphDB(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer graph.Close()

	// Query all nodes using Get (no filters = all nodes)
	resp, err := graph.Get(ctx, &graphpb.GetRequest{
		Limit: 1000, // reasonable limit for status overview
	})
	if err != nil {
		return fmt.Errorf("query nodes: %w", err)
	}

	// Analyze nodes
	nodesByType := make(map[string]int)
	nodesWithContent := 0
	totalNodes := len(resp.Nodes)

	for _, node := range resp.Nodes {
		nodesByType[node.Type]++
		if len(node.Content) > 0 {
			nodesWithContent++
		}
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Workflow Database: %s\n\n", dbPath)
	fmt.Fprintf(cmd.OutOrStdout(), "Total Nodes: %d\n", totalNodes)
	fmt.Fprintf(cmd.OutOrStdout(), "Nodes with Content: %d\n", nodesWithContent)
	fmt.Fprintf(cmd.OutOrStdout(), "Empty Nodes: %d\n", totalNodes-nodesWithContent)
	fmt.Fprintln(cmd.OutOrStdout())

	if len(nodesByType) > 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Nodes by Type:")
		for nodeType, count := range nodesByType {
			fmt.Fprintf(cmd.OutOrStdout(), "  • %s: %d\n", nodeType, count)
		}
	}

	return nil
}

// compileWorkflowFile parses a workflow file and returns the compiled workflows.
// Creates a temporary in-memory runner to leverage the DSL compiler.
func compileWorkflowFile(graphFile string) ([]engine.Workflow, error) {
	// Create temp runner with in-memory DB (we only need compilation, not execution)
	r, err := runner.New(":memory:", nil)
	if err != nil {
		return nil, fmt.Errorf("create runner: %w", err)
	}
	defer r.Close()

	if err := r.CompileFile(graphFile); err != nil {
		return nil, err
	}

	return r.Workflows(), nil
}

func openGraphDB(dbPath string) (engine.Graph, error) {
	// Create a runner with nil agentFn since we're only querying, not executing
	r, err := runner.New(dbPath, nil)
	if err != nil {
		return nil, err
	}
	return r.Graph(), nil
}
