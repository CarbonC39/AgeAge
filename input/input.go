// Package input holds normalized user input shared by transports and tools.
package input

import "ageage/llm"

// UserInput contains no open streams and can be submitted to a new turn or an
// existing ask_user request. Attachment warnings are also present in Parts.
type UserInput struct {
	Text     string
	Parts    []llm.ContentPart
	Warnings []string
}
