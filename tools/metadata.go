package tools

import "time"

// Metadata for built-in tools lives in one file so the execution contract is
// easy to audit without expanding the minimum Tool interface.  The scheduler
// treats only explicitly read-only tools as safe to run in parallel.

func (t *FileReadTool) Metadata() ToolMetadata {
	return ToolMetadata{ReadOnly: true, Idempotent: true, Risk: RiskLow, DefaultTimeout: 30 * time.Second, ConcurrencyKey: "filesystem"}
}

func (t *FileWriteTool) Metadata() ToolMetadata {
	return ToolMetadata{Risk: RiskMedium, ConcurrencyKey: "filesystem"}
}

func (t *FileEditTool) Metadata() ToolMetadata {
	return ToolMetadata{Risk: RiskMedium, ConcurrencyKey: "filesystem"}
}

func (t *BashTool) Metadata() ToolMetadata {
	return ToolMetadata{Risk: RiskHigh, NetworkAccess: true, ConcurrencyKey: "shell"}
}

func (t *WebFetchTool) Metadata() ToolMetadata {
	return ToolMetadata{ReadOnly: true, Idempotent: true, Risk: RiskLow, DefaultTimeout: 30 * time.Second, NetworkAccess: true, ConcurrencyKey: "network"}
}

func (t *WebSearchTool) Metadata() ToolMetadata {
	return ToolMetadata{ReadOnly: true, Idempotent: true, Risk: RiskLow, DefaultTimeout: 30 * time.Second, NetworkAccess: true, ConcurrencyKey: "network"}
}

func (t *BrowserNavigateTool) Metadata() ToolMetadata {
	// Navigation mutates the shared browser session (current page/history),
	// even though it does not mutate the remote website.
	return ToolMetadata{Idempotent: true, Risk: RiskMedium, DefaultTimeout: 60 * time.Second, NetworkAccess: true, ConcurrencyKey: "browser"}
}

func (t *BrowserContentTool) Metadata() ToolMetadata {
	return ToolMetadata{ReadOnly: true, Idempotent: true, Risk: RiskLow, DefaultTimeout: 30 * time.Second, NetworkAccess: true, ConcurrencyKey: "browser"}
}

func (t *BrowserActionTool) Metadata() ToolMetadata {
	return ToolMetadata{Risk: RiskHigh, DefaultTimeout: 60 * time.Second, NetworkAccess: true, ConcurrencyKey: "browser"}
}

func (t *MemoryStoreTool) Metadata() ToolMetadata {
	return ToolMetadata{Risk: RiskMedium, ConcurrencyKey: "memory"}
}

func (t *MemoryRecallTool) Metadata() ToolMetadata {
	return ToolMetadata{ReadOnly: true, Idempotent: true, Risk: RiskLow, DefaultTimeout: 10 * time.Second, ConcurrencyKey: "memory"}
}

func (t *MemoryForgetTool) Metadata() ToolMetadata {
	return ToolMetadata{Risk: RiskHigh, ConcurrencyKey: "memory"}
}

func (t *CronAddTool) Metadata() ToolMetadata {
	return ToolMetadata{Risk: RiskHigh, ConcurrencyKey: "cron"}
}

func (t *CronRemoveTool) Metadata() ToolMetadata {
	return ToolMetadata{Risk: RiskHigh, ConcurrencyKey: "cron"}
}

func (t *CronListTool) Metadata() ToolMetadata {
	return ToolMetadata{ReadOnly: true, Idempotent: true, Risk: RiskLow, DefaultTimeout: 10 * time.Second, ConcurrencyKey: "cron"}
}

func (t *CronRunTool) Metadata() ToolMetadata {
	return ToolMetadata{Risk: RiskHigh, ConcurrencyKey: "cron"}
}

func (t *CronPauseTool) Metadata() ToolMetadata {
	return ToolMetadata{Idempotent: true, Risk: RiskMedium, DefaultTimeout: 10 * time.Second, ConcurrencyKey: "cron"}
}

func (t *CronResumeTool) Metadata() ToolMetadata {
	return ToolMetadata{Idempotent: true, Risk: RiskMedium, DefaultTimeout: 10 * time.Second, ConcurrencyKey: "cron"}
}

func (t *GlobTool) Metadata() ToolMetadata {
	return ToolMetadata{ReadOnly: true, Idempotent: true, Risk: RiskLow, DefaultTimeout: 30 * time.Second, ConcurrencyKey: "filesystem"}
}

func (t *GrepTool) Metadata() ToolMetadata {
	return ToolMetadata{ReadOnly: true, Idempotent: true, Risk: RiskLow, DefaultTimeout: 30 * time.Second, ConcurrencyKey: "filesystem"}
}

func (t *TreeTool) Metadata() ToolMetadata {
	return ToolMetadata{ReadOnly: true, Idempotent: true, Risk: RiskLow, DefaultTimeout: 30 * time.Second, ConcurrencyKey: "filesystem"}
}

func (t *FinishTool) Metadata() ToolMetadata {
	return ToolMetadata{Risk: RiskLow, ConcurrencyKey: "agent"}
}

func (t *AskUserTool) Metadata() ToolMetadata {
	return ToolMetadata{Risk: RiskMedium, ConcurrencyKey: "interaction"}
}
