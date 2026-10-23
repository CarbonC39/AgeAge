package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"ageage/agent"
)

const failedInboxFilename = "failed-inbox.json"

// sealAndDrainRun waits for every accepted normalization and atomically seals
// admission, so an error path can persist the complete ordered inbox safely.
func sealAndDrainRun(run *agent.RunHandle) []agent.UserInput {
	if run == nil {
		return nil
	}
	var inputs []agent.UserInput
	for {
		inputs = append(inputs, run.Drain(context.Background())...)
		if run.TryFinish() {
			return inputs
		}
	}
}

// persistFailedInbox atomically appends inputs to a private session-local file.
// A later run can recover the exact normalized text and attachment parts even
// when the session history could not be loaded.
func persistFailedInbox(sessionDir string, inputs []agent.UserInput) error {
	if len(inputs) == 0 {
		return nil
	}
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}
	path := filepath.Join(sessionDir, failedInboxFilename)
	all := make([]agent.UserInput, 0, len(inputs))
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &all); err != nil {
			return fmt.Errorf("decode existing failed inbox: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read existing failed inbox: %w", err)
	}
	all = append(all, inputs...)
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return fmt.Errorf("encode failed inbox: %w", err)
	}
	tmp, err := os.CreateTemp(sessionDir, ".failed-inbox-*")
	if err != nil {
		return fmt.Errorf("create failed inbox temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("protect failed inbox: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write failed inbox: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync failed inbox: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close failed inbox: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install failed inbox: %w", err)
	}
	return nil
}
