package workflow_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	graphagent "github.com/docker/cagent-graph/pkg/agent"
	"github.com/docker/cagent/pkg/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunSourceTwoSteps(t *testing.T) {
	src := []byte(`
agent writer {
  instruction "Transform input into intermediate form."
}

agent summarizer {
  instruction "Summarize intermediate content."
}

workflow test {
  graph {
    type input { content text/plain }
    type intermediate { content text/plain }
    type output { content text/plain }
  }

  seed input from inputs

  step writer {
    reads [input]
    writes [intermediate]
  }

  step summarizer {
    reads [intermediate]
    writes [output]
  }
}
`)

	var callCount atomic.Int32
	var prompts sync.Map

	mockAgentFn := func(_ context.Context, params graphagent.AgentParams) (*graphagent.AgentResult, error) {
		callCount.Add(1)
		// Store the full context: instruction + prompt (engine may split data across both)
		prompts.Store(params.Agent, params.Instruction+"\n"+params.Prompt)
		switch params.Agent {
		case "writer":
			return &graphagent.AgentResult{Output: "intermediate content from writer"}, nil
		case "summarizer":
			return &graphagent.AgentResult{Output: "final summary"}, nil
		default:
			return nil, fmt.Errorf("unexpected agent: %s", params.Agent)
		}
	}

	exec := workflow.New(nil, workflow.WithAgentFunc(mockAgentFn))
	result, err := exec.RunSource(context.Background(), src, map[string]string{
		"input": "seed data",
	})
	require.NoError(t, err)
	assert.Equal(t, 2, result.StepsRun)
	assert.Equal(t, int32(2), callCount.Load())
	assert.Empty(t, result.StepErrors)

	// Writer should receive the seeded input as its prompt.
	writerPrompt, ok := prompts.Load("writer")
	require.True(t, ok)
	assert.Contains(t, writerPrompt.(string), "seed data")

	// Summarizer should receive writer's output as its prompt.
	summarizerPrompt, ok := prompts.Load("summarizer")
	require.True(t, ok)
	assert.Contains(t, summarizerPrompt.(string), "intermediate content from writer")

	// Terminal output should contain the summarizer's result.
	require.Contains(t, result.Outputs, "output")
	assert.Equal(t, "final summary", string(result.Outputs["output"]))
}

func TestRunSourceConcurrentSteps(t *testing.T) {
	src := []byte(`
agent analyzer {
  instruction "Analyze the input."
}

agent reviewer {
  instruction "Review the input."
}

workflow parallel_test {
  graph {
    type input { content text/plain }
    type analysis { content text/plain }
    type review { content text/plain }
  }

  seed input from inputs

  step analyzer {
    reads [input]
    writes [analysis]
  }

  step reviewer {
    reads [input]
    writes [review]
  }
}
`)

	var callCount atomic.Int32

	mockAgentFn := func(_ context.Context, params graphagent.AgentParams) (*graphagent.AgentResult, error) {
		callCount.Add(1)
		return &graphagent.AgentResult{
			Output:  fmt.Sprintf("output from %s", params.Agent),
			CostUSD: 0.01,
		}, nil
	}

	exec := workflow.New(nil, workflow.WithAgentFunc(mockAgentFn))
	result, err := exec.RunSource(context.Background(), src, map[string]string{
		"input": "analyze this",
	})
	require.NoError(t, err)
	assert.Equal(t, 2, result.StepsRun)
	assert.Equal(t, int32(2), callCount.Load())
	assert.InDelta(t, 0.02, result.TotalCost, 0.001)
	assert.Empty(t, result.StepErrors)

	// Both analysis and review are terminal (nothing reads them).
	require.Contains(t, result.Outputs, "analysis")
	assert.Equal(t, "output from analyzer", string(result.Outputs["analysis"]))
	require.Contains(t, result.Outputs, "review")
	assert.Equal(t, "output from reviewer", string(result.Outputs["review"]))
}

func TestRunSourceNoWorkflows(t *testing.T) {
	exec := workflow.New(nil, workflow.WithAgentFunc(
		func(context.Context, graphagent.AgentParams) (*graphagent.AgentResult, error) {
			return nil, fmt.Errorf("should not be called")
		},
	))
	// Empty source should fail to compile, not panic.
	_, err := exec.RunSource(context.Background(), []byte(""), nil)
	require.Error(t, err)
}
