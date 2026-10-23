// Package event holds semantic runtime events without IM formatting or routing.
package event

import "time"

type Kind string

const (
	RunStarted            Kind = "run_started"
	RunCompleted          Kind = "run_completed"
	RunFailed             Kind = "run_failed"
	RunCancelled          Kind = "run_cancelled"
	ToolStarted           Kind = "tool_started"
	ToolCompleted         Kind = "tool_completed"
	SubagentStarted       Kind = "subagent_started"
	SubagentCompleted     Kind = "subagent_completed"
	Waiting               Kind = "waiting"
	ConfirmationRequested Kind = "confirmation_requested"
	PlanUpdated           Kind = "plan_updated"
	Notification          Kind = "notification"
	Warning               Kind = "warning"
	CronCompleted         Kind = "cron_completed"
)

type Event struct {
	Kind      Kind
	Name      string
	Text      string
	Arguments string // redacted tool arguments; never raw credential substitutions
	Options   []string
	At        time.Time
}
