package builtin

const (
	maxOutputSize = 10000000 // 10MB

	maxFiles = 100
)

func limitOutput(output string) string {
	if len(output) > maxOutputSize {
		return output[:maxOutputSize] + "\n\n[Output truncated: exceeded 30,000 character limit]"
	}
	return output
}
