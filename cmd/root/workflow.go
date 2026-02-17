package root

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/docker/cagent-graph/pkg/compiler"
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
	cmd.AddCommand(newWorkflowResumeCmd())
	cmd.AddCommand(newWorkflowTagCmd())
	cmd.AddCommand(newWorkflowListCmd())
	cmd.AddCommand(newWorkflowPruneCmd())
	return cmd
}

type workflowRunFlags struct {
	inputs       []string
	agents       string
	dbPath       string
	ephemeral    bool
	workflowName string
	runConfig    config.RuntimeConfig
}

const defaultWorkflowDB = ".cagent/workflows.db" // relative to home dir

// getDefaultWorkflowDB returns the absolute path to the default workflow database.
func getDefaultWorkflowDB() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home directory: %w", err)
	}
	return filepath.Join(homeDir, defaultWorkflowDB), nil
}

func newWorkflowRunCmd() *cobra.Command {
	var flags workflowRunFlags

	cmd := &cobra.Command{
		Use:   "run <graph-file>",
		Short: "Execute a .cagent workflow",
		Long: `Execute a .cagent workflow file. Agent, model, and provider definitions
can be embedded directly in the .cagent file. Use --agents to provide
additional model/agent configuration from a YAML file (e.g., for
custom API endpoints or credentials).`,
		Example: `  cagent workflow run ./pipeline.cagent --input topic="Docker containers"
  cagent workflow run ./review.cagent --agents ./models.yaml --input prompt="Review this"
  cagent workflow run ./research.cagent --input question="What is the best search API?"`,
		Args: cobra.ExactArgs(1),
		RunE: flags.runWorkflow,
	}

	cmd.Flags().StringArrayVar(&flags.inputs, "input", nil, "Seed graph input as type=content (repeatable)")
	cmd.Flags().StringVar(&flags.agents, "agents", "", "Agent/model configuration YAML file (optional if .cagent defines models)")
	cmd.Flags().StringVar(&flags.dbPath, "db", "", "Path to SQLite graph database (default: ~/.cagent/workflows.db)")
	cmd.Flags().BoolVar(&flags.ephemeral, "ephemeral", false, "Use ephemeral database (deleted after run, disables resume/list/tag)")
	cmd.Flags().StringVarP(&flags.workflowName, "workflow", "w", "", "Workflow name to execute (default: auto-select by filename or last workflow)")
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *workflowRunFlags) runWorkflow(cmd *cobra.Command, args []string) error {
	telemetry.TrackCommand("workflow run", nil)

	ctx := cmd.Context()
	graphFile := args[0]

	// Use default database unless --ephemeral or --db specified
	if f.dbPath == "" && !f.ephemeral {
		homeDir, err := os.UserHomeDir()
		if err == nil {
			f.dbPath = filepath.Join(homeDir, defaultWorkflowDB)
			// Ensure .cagent directory exists
			if err := os.MkdirAll(filepath.Dir(f.dbPath), 0755); err != nil {
				return fmt.Errorf("create workflow database directory: %w", err)
			}
		}
	}

	// Parse inputs.
	inputs := make(map[string]string)
	for _, kv := range f.inputs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("invalid input %q: expected type=content", kv)
		}
		inputs[k] = v
	}

	// Load agent team — either from --agents file or we'll build one from the .cagent file later.
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
	execOpts = append(execOpts, workflow.WithRunConfig(&f.runConfig))
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
	// build agents from the .cagent file's definitions via AgentParams.
	var exec *workflow.Executor
	if loadResult != nil {
		exec = workflow.New(loadResult.Team, execOpts...)
	} else {
		exec = workflow.New(nil, execOpts...)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Running workflow: %s\n", graphFile)

	// Use the new workflow selection in executor
	result, err := exec.RunFileWithSelection(ctx, graphFile, f.workflowName, inputs)
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
		Short: "Validate a .cagent workflow file",
		Long: `Parse and validate a .cagent workflow file without executing it.
Checks for syntax errors, type mismatches, and other issues.`,
		Example: `  cagent workflow lint ./pipeline.cagent
  cagent workflow lint ./review.cagent`,
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
		Long: `Parse a .cagent workflow file and display detailed information including:
- Workflow configuration (model, retries, etc.)
- Node types and their content types
- Steps with their agents, inputs, and outputs
- Agent, provider, and model definitions`,
		Example: `  cagent workflow describe ./pipeline.cagent
  cagent workflow describe ./review.cagent`,
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
		Long: `Generate a new .cagent workflow file with a basic template structure.
If no filename is provided, creates 'workflow.cagent' in the current directory.`,
		Example: `  cagent workflow init
  cagent workflow init pipeline.cagent
  cagent workflow init research/analysis.cagent`,
		Args: cobra.MaximumNArgs(1),
		RunE: runWorkflowInit,
	}

	return cmd
}

func runWorkflowInit(cmd *cobra.Command, args []string) error {
	telemetry.TrackCommand("workflow init", nil)

	filename := "workflow.cagent"
	if len(args) > 0 {
		filename = args[0]
	}

	// Check if file already exists
	if _, err := os.Stat(filename); err == nil {
		return fmt.Errorf("file already exists: %s", filename)
	}

	template := `agent analyzer {
  model "gpt-4o"
  skills true

  # Common toolsets - comment out what you don't need
  toolset shell {}
  toolset filesystem {}
  toolset think {}

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
		Use:   "status [db-path]",
		Short: "Show status of a persisted workflow",
		Long: `Query a workflow database and show execution status including:
- Number of nodes created
- Completed vs pending steps
- Node types and counts

Defaults to ~/.cagent/workflows.db if no path specified.`,
		Example: `  cagent workflow status
  cagent workflow status ./workflow.db
  cagent workflow status /tmp/my-workflow.db`,
		Args: cobra.MaximumNArgs(1),
		RunE: runWorkflowStatus,
	}

	return cmd
}

func runWorkflowStatus(cmd *cobra.Command, args []string) error {
	telemetry.TrackCommand("workflow status", nil)

	// Use default database if not specified
	var dbPath string
	if len(args) > 0 {
		dbPath = args[0]
	} else {
		var err error
		dbPath, err = getDefaultWorkflowDB()
		if err != nil {
			return err
		}
	}

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

	// Warn if results may be truncated
	if len(resp.Nodes) >= 1000 {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: showing first 1000 nodes (results may be truncated)\n")
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
// Uses the public compiler API (no runner needed).
func compileWorkflowFile(graphFile string) ([]engine.Workflow, error) {
	return compiler.CompileFile(graphFile)
}

func openGraphDB(dbPath string) (engine.Graph, error) {
	// Create a runner with nil agentFn since we're only querying, not executing
	r, err := runner.New(dbPath, nil)
	if err != nil {
		return nil, err
	}
	return r.Graph(), nil
}

func newWorkflowResumeCmd() *cobra.Command {
	var namespace string
	var clean bool
	var allowChanges bool
	var workflowFile string

	cmd := &cobra.Command{
		Use:   "resume [db-path]",
		Short: "Resume an interrupted workflow",
		Long: `Resume an interrupted or failed workflow from its last completed step.
Automatically detects resumable workflows in the database.
Defaults to ~/.cagent/workflows.db if no path specified.`,
		Example: `  cagent workflow resume --workflow pipeline.cagent
  cagent workflow resume ./workflow.db --namespace research-pipeline-123
  cagent workflow resume --clean --workflow pipeline.cagent`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflowResume(cmd, args, namespace, clean, allowChanges, workflowFile)
		},
	}

	cmd.Flags().StringVar(&namespace, "namespace", "", "Specific namespace to resume")
	cmd.Flags().BoolVar(&clean, "clean", false, "Clean partial outputs before resuming")
	cmd.Flags().BoolVar(&allowChanges, "allow-changes", false, "Allow resuming if workflow definition changed")
	cmd.Flags().StringVar(&workflowFile, "workflow", "", "Workflow definition file (required)")

	return cmd
}

func runWorkflowResume(cmd *cobra.Command, args []string, namespace string, clean, allowChanges bool, workflowFile string) error {
	telemetry.TrackCommand("workflow resume", nil)

	if workflowFile == "" {
		return fmt.Errorf("--workflow flag is required")
	}

	// Use default database if not specified
	var dbPath string
	if len(args) > 0 {
		dbPath = args[0]
	} else {
		var err error
		dbPath, err = getDefaultWorkflowDB()
		if err != nil {
			return err
		}
	}

	ctx := cmd.Context()

	// Create runner
	r, err := runner.New(dbPath, nil) // agentFn will be set later
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer r.Close()

	// Compile workflow
	if err := r.CompileFile(workflowFile); err != nil {
		return fmt.Errorf("compile workflow: %w", err)
	}

	// If namespace not specified, detect resumable runs
	if namespace == "" {
		runs, err := r.ListWorkflowRuns(ctx)
		if err != nil {
			return fmt.Errorf("list runs: %w", err)
		}

		resumable := make([]*runner.WorkflowState, 0)
		for _, run := range runs {
			if run.CanResume {
				resumable = append(resumable, run)
			}
		}

		if len(resumable) == 0 {
			return fmt.Errorf("no resumable workflows found in %s", dbPath)
		}

		if len(resumable) > 1 {
			fmt.Fprintf(cmd.ErrOrStderr(), "Multiple resumable workflows found:\n")
			for i, run := range resumable {
				fmt.Fprintf(cmd.ErrOrStderr(), "  %d. %s (%d/%d steps)\n",
					i+1, run.Namespace, len(run.CompletedSteps), run.TotalSteps)
			}
			return fmt.Errorf("specify --namespace <ns> to choose one")
		}

		namespace = resumable[0].Namespace
	}

	// Check state
	state, err := r.GetWorkflowState(ctx, namespace)
	if err != nil {
		return fmt.Errorf("get workflow state: %w", err)
	}

	if !state.CanResume {
		return fmt.Errorf("cannot resume: %s", state.Reason)
	}

	// Show what will happen
	fmt.Fprintf(cmd.OutOrStdout(), "Resuming workflow: %s\n", state.WorkflowName)
	fmt.Fprintf(cmd.OutOrStdout(), "  Namespace: %s\n", namespace)
	fmt.Fprintf(cmd.OutOrStdout(), "  Completed: %d/%d steps\n", len(state.CompletedSteps), state.TotalSteps)
	if len(state.PartialSteps) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "  Partial: %d step(s) with incomplete outputs\n", len(state.PartialSteps))
		if clean {
			fmt.Fprintf(cmd.OutOrStdout(), "  Action: Cleaning partial outputs before resuming\n")
		}
	}
	if state.WorkflowModified {
		fmt.Fprintf(cmd.OutOrStdout(), "  ⚠ Warning: Workflow definition has changed since this run started\n")
		if !allowChanges {
			return fmt.Errorf("use --allow-changes to resume modified workflow")
		}
	}

	// Resume
	result, err := r.Resume(ctx, namespace, &runner.ResumeOptions{
		CleanPartialOutputs: clean,
		AllowWorkflowChange: allowChanges,
	})
	if err != nil {
		return fmt.Errorf("resume failed: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "\n✓ Workflow resumed successfully\n")
	fmt.Fprintf(cmd.OutOrStdout(), "  Steps executed: %d\n", result.StepsRun)
	if result.TotalCost > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "  Cost: $%.2f\n", result.TotalCost)
	}

	return nil
}

func newWorkflowTagCmd() *cobra.Command {
	var listFlag bool
	var findTag string

	cmd := &cobra.Command{
		Use:   "tag [db-path] <namespace> <tag...>",
		Short: "Tag workflow runs for organization",
		Long: `Add tags to workflow runs for grouping and filtering.
Tags help organize workflows by feature, sprint, team, etc.
Defaults to ~/.cagent/workflows.db if no db-path specified.`,
		Example: `  cagent workflow tag research-123 feature search sprint-12
  cagent workflow tag research-123 --list
  cagent workflow tag --find feature`,
		Args: cobra.MinimumNArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflowTag(cmd, args, listFlag, findTag)
		},
	}

	cmd.Flags().BoolVar(&listFlag, "list", false, "List tags for a namespace")
	cmd.Flags().StringVar(&findTag, "find", "", "Find namespaces with a tag")

	return cmd
}

func runWorkflowTag(cmd *cobra.Command, args []string, listFlag bool, findTag string) error {
	telemetry.TrackCommand("workflow tag", nil)

	// Determine if first arg is db-path or namespace
	// If it looks like a path (.db, .sqlite, /), treat as db-path
	// Otherwise treat as namespace and use default DB
	var dbPath string
	var namespace string
	var tags []string

	if len(args) > 0 && (strings.HasSuffix(args[0], ".db") || strings.HasSuffix(args[0], ".sqlite") || strings.Contains(args[0], "/")) {
		// First arg is explicit db-path
		dbPath = args[0]
		if len(args) > 1 {
			namespace = args[1]
			tags = args[2:]
		}
	} else {
		// Use default DB, first arg is namespace
		var err error
		dbPath, err = getDefaultWorkflowDB()
		if err != nil {
			return err
		}
		if len(args) > 0 {
			namespace = args[0]
			tags = args[1:]
		}
	}

	ctx := cmd.Context()

	// Open runner (provides access to namespace tagging)
	r, err := runner.New(dbPath, nil)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer r.Close()

	if listFlag {
		if namespace == "" {
			return fmt.Errorf("namespace required with --list")
		}

		nsTags, err := r.GetNamespaceTags(ctx, namespace)
		if err != nil {
			return fmt.Errorf("get tags: %w", err)
		}

		if len(nsTags) == 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "No tags for %s\n", namespace)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "Tags for %s: %s\n", namespace, strings.Join(nsTags, ", "))
		}
		return nil
	}

	if findTag != "" {
		namespaces, err := r.ListNamespacesByTags(ctx, []string{findTag})
		if err != nil {
			return fmt.Errorf("find namespaces: %w", err)
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Namespaces with tag '%s':\n", findTag)
		for _, ns := range namespaces {
			fmt.Fprintf(cmd.OutOrStdout(), "  - %s\n", ns)
		}
		return nil
	}

	// Set tags
	if namespace == "" {
		return fmt.Errorf("namespace required")
	}
	if len(tags) == 0 {
		return fmt.Errorf("at least one tag required")
	}

	if err := r.SetNamespaceTags(ctx, namespace, tags); err != nil {
		return fmt.Errorf("set tags: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "✓ Tagged %s with: %s\n", namespace, strings.Join(tags, ", "))
	return nil
}

func newWorkflowListCmd() *cobra.Command {
	var statusFilter string
	var tagFilter string

	cmd := &cobra.Command{
		Use:   "list [db-path]",
		Short: "List all workflow runs",
		Long: `List all workflow runs in a database with their status.
Filter by status (running, completed, failed) or tags.
Defaults to ~/.cagent/workflows.db if no path specified.`,
		Example: `  cagent workflow list
  cagent workflow list --status running
  cagent workflow list workflow.db --tag feature`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflowList(cmd, args, statusFilter, tagFilter)
		},
	}

	cmd.Flags().StringVar(&statusFilter, "status", "", "Filter by status (running, completed, failed)")
	cmd.Flags().StringVar(&tagFilter, "tag", "", "Filter by tag")

	return cmd
}

func runWorkflowList(cmd *cobra.Command, args []string, statusFilter, tagFilter string) error {
	telemetry.TrackCommand("workflow list", nil)

	// Use default database if not specified
	var dbPath string
	if len(args) > 0 {
		dbPath = args[0]
	} else {
		var err error
		dbPath, err = getDefaultWorkflowDB()
		if err != nil {
			return err
		}
	}

	ctx := cmd.Context()

	// Open runner
	r, err := runner.New(dbPath, nil)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer r.Close()

	// List runs based on status filter
	var runs []*runner.WorkflowRunInfo
	if statusFilter != "" {
		var status runner.WorkflowRunStatus
		switch statusFilter {
		case "running":
			status = runner.WorkflowRunStatusRunning
		case "completed":
			status = runner.WorkflowRunStatusCompleted
		case "failed":
			status = runner.WorkflowRunStatusFailed
		default:
			return fmt.Errorf("invalid status: %s (use running, completed, or failed)", statusFilter)
		}
		runs, err = r.ListAllWorkflowRuns(ctx, status)
	} else {
		// List all - get all status types
		runningRuns, err := r.ListAllWorkflowRuns(ctx, runner.WorkflowRunStatusRunning)
		if err != nil {
			return fmt.Errorf("list running workflows: %w", err)
		}

		completedRuns, err := r.ListAllWorkflowRuns(ctx, runner.WorkflowRunStatusCompleted)
		if err != nil {
			return fmt.Errorf("list completed workflows: %w", err)
		}

		failedRuns, err := r.ListAllWorkflowRuns(ctx, runner.WorkflowRunStatusFailed)
		if err != nil {
			return fmt.Errorf("list failed workflows: %w", err)
		}

		runs = append(runs, runningRuns...)
		runs = append(runs, completedRuns...)
		runs = append(runs, failedRuns...)
	}

	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}

	// Filter by tag if specified
	if tagFilter != "" {
		namespaces, err := r.ListNamespacesByTags(ctx, []string{tagFilter})
		if err != nil {
			return fmt.Errorf("filter by tag: %w", err)
		}

		nsMap := make(map[string]bool)
		for _, ns := range namespaces {
			nsMap[ns] = true
		}

		filtered := make([]*runner.WorkflowRunInfo, 0)
		for _, run := range runs {
			if nsMap[run.Namespace] {
				filtered = append(filtered, run)
			}
		}
		runs = filtered
	}

	// Display
	if len(runs) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No workflow runs found\n")
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Workflow Runs in %s:\n\n", dbPath)
	for _, run := range runs {
		fmt.Fprintf(cmd.OutOrStdout(), "Namespace: %s\n", run.Namespace)
		fmt.Fprintf(cmd.OutOrStdout(), "  Workflow: %s\n", run.WorkflowName)
		fmt.Fprintf(cmd.OutOrStdout(), "  Status: %s\n", run.Status)
		fmt.Fprintf(cmd.OutOrStdout(), "  Started: %s\n", run.StartedAt)

		if run.CompletedAt != "" {
			// Parse timestamps to calculate duration
			startTime, err1 := time.Parse(time.RFC3339, run.StartedAt)
			endTime, err2 := time.Parse(time.RFC3339, run.CompletedAt)
			if err1 == nil && err2 == nil {
				duration := endTime.Sub(startTime)
				fmt.Fprintf(cmd.OutOrStdout(), "  Finished: %s (%s)\n", run.CompletedAt, duration)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "  Finished: %s\n", run.CompletedAt)
			}
		}

		if run.Error != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "  Error: %s\n", run.Error)
		}

		// Show tags if any
		tags, _ := r.GetNamespaceTags(ctx, run.Namespace)
		if len(tags) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "  Tags: %s\n", strings.Join(tags, ", "))
		}

		fmt.Fprintln(cmd.OutOrStdout())
	}

	return nil
}

func newWorkflowPruneCmd() *cobra.Command {
	var olderThan string
	var namespace string
	var all bool

	cmd := &cobra.Command{
		Use:   "prune [db-path]",
		Short: "Clean up old workflow runs",
		Long: `Remove old workflow runs from the database to free up space.
Defaults to ~/.cagent/workflows.db if no path specified.`,
		Example: `  cagent workflow prune --older-than 30d
  cagent workflow prune --namespace research-pipeline-123
  cagent workflow prune --all
  cagent workflow prune workflow.db --older-than 7d`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkflowPrune(cmd, args, olderThan, namespace, all)
		},
	}

	cmd.Flags().StringVar(&olderThan, "older-than", "", "Delete runs older than duration (e.g., 30d, 7d, 24h)")
	cmd.Flags().StringVar(&namespace, "namespace", "", "Delete specific namespace")
	cmd.Flags().BoolVar(&all, "all", false, "Delete all workflow runs (use with caution)")

	return cmd
}

func runWorkflowPrune(cmd *cobra.Command, args []string, olderThan, namespace string, all bool) error {
	telemetry.TrackCommand("workflow prune", nil)

	// Use default database if not specified
	var dbPath string
	if len(args) > 0 {
		dbPath = args[0]
	} else {
		var err error
		dbPath, err = getDefaultWorkflowDB()
		if err != nil {
			return err
		}
	}

	ctx := cmd.Context()

	// Open runner
	r, err := runner.New(dbPath, nil)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer r.Close()

	if namespace != "" {
		// Delete specific namespace
		graph := r.Graph()
		if _, err := graph.Delete(ctx, &graphpb.DeleteRequest{
			Namespace: namespace,
		}); err != nil {
			return fmt.Errorf("delete namespace: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "✓ Deleted namespace: %s\n", namespace)
		return nil
	}

	if all {
		// Confirm before deleting everything
		fmt.Fprintf(cmd.ErrOrStderr(), "⚠ This will delete ALL workflow runs. Continue? [y/N] ")
		var response string
		fmt.Scanln(&response)
		if response != "y" && response != "Y" {
			return fmt.Errorf("cancelled")
		}

		// Delete all by removing the database file
		r.Close()
		if err := os.Remove(dbPath); err != nil {
			return fmt.Errorf("delete database: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "✓ Deleted all workflow runs from %s\n", dbPath)
		return nil
	}

	if olderThan != "" {
		// Parse duration
		duration, err := time.ParseDuration(olderThan)
		if err != nil {
			// Try adding hours if no unit specified
			if _, err := time.ParseDuration(olderThan + "h"); err == nil {
				duration, _ = time.ParseDuration(olderThan + "h")
			} else {
				return fmt.Errorf("invalid duration %q (use: 30d, 7d, 24h)", olderThan)
			}
		}

		cutoff := time.Now().Add(-duration)

		// List all runs and delete old ones
		runs, err := r.ListAllWorkflowRuns(ctx, "")
		if err != nil {
			return fmt.Errorf("list runs: %w", err)
		}

		deleted := 0
		graph := r.Graph()
		for _, run := range runs {
			startTime, err := time.Parse(time.RFC3339, run.StartedAt)
			if err != nil {
				continue
			}

			if startTime.Before(cutoff) {
				if _, err := graph.Delete(ctx, &graphpb.DeleteRequest{
					Namespace: run.Namespace,
				}); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to delete %s: %v\n", run.Namespace, err)
					continue
				}
				deleted++
			}
		}

		fmt.Fprintf(cmd.OutOrStdout(), "✓ Deleted %d workflow run(s) older than %s\n", deleted, olderThan)
		return nil
	}

	return fmt.Errorf("specify --older-than, --namespace, or --all")
}
