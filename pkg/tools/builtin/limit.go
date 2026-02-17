package builtin

const (
	// TODO: Consider reducing to 1MB or making this configurable per-tool/per-workflow
	// to prevent memory exhaustion and context window overflow. See LOOKOUT_REVIEW.md.
	maxOutputSize = 10000000 // 10MB

	maxFiles = 100
)

func limitOutput(output string) string {
	if len(output) > maxOutputSize {
		return output[:maxOutputSize] + "\n\n[Output truncated: exceeded 30,000 character limit]"
	}
	return output
}
