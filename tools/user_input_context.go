package tools

import (
	"context"

	"ageage/input"
)

type userInputHandlerContextKey struct{}

// WithUserInputHandler associates a callback for submitting typed answers to
// the current Agent turn. AskUserTool invokes it only after receiving an answer.
func WithUserInputHandler(ctx context.Context, handler func(input.UserInput)) context.Context {
	return context.WithValue(ctx, userInputHandlerContextKey{}, handler)
}

func userInputHandlerFromContext(ctx context.Context) func(input.UserInput) {
	if ctx == nil {
		return nil
	}
	handler, _ := ctx.Value(userInputHandlerContextKey{}).(func(input.UserInput))
	return handler
}
