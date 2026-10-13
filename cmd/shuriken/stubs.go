package main

import (
	"context"
	"errors"

	ghc "github.com/sos-academy/shuriken/internal/github"
	"github.com/sos-academy/shuriken/internal/llm"
)

// stubModel stands in for the Anthropic client when running dry without an API
// key, so the webhook, normalization and routing paths can be exercised
// offline.
type stubModel struct{}

func (stubModel) Review(context.Context, llm.Request) (*llm.Result, error) {
	return &llm.Result{
		Output: llm.ReviewOutput{
			Summary: "(dry run: no model was called, so there are no findings)",
		},
	}, nil
}

// stubClients stands in for the GitHub App client factory when running dry
// without App credentials.
type stubClients struct{}

func (stubClients) For(int64) (ghc.Client, error) {
	return nil, errors.New("dry run without GitHub App credentials: set SHURIKEN_GITHUB_APP_ID and SHURIKEN_GITHUB_PRIVATE_KEY to exercise GitHub reads")
}
