package config

import "strings"

// Progress event categories are deliberately stable and coarse-grained. The
// agent never sees notification policy decisions; runtimes use these values
// when deciding which progress events to render in an IM channel.
const (
	ProgressLifecycle = "lifecycle"
	ProgressPlan      = "plan"
	ProgressWaiting   = "waiting"
	ProgressTool      = "tool"
	ProgressSubagent  = "subagent"
	ProgressCron      = "cron"
)

const (
	ProgressPresetQuiet    = "quiet"
	ProgressPresetBalanced = "balanced"
	ProgressPresetVerbose  = "verbose"
)

const defaultProgressThrottleMS = 750

var progressPresetChoices = []Choice{
	{Value: ProgressPresetQuiet, Label: "Quiet", Description: "Only scheduled-task outcomes."},
	{Value: ProgressPresetBalanced, Label: "Balanced", Description: "Plans and scheduled-task outcomes without per-tool noise."},
	{Value: ProgressPresetVerbose, Label: "Verbose", Description: "All supported progress categories."},
}

var progressCategoryChoices = []Choice{
	{Value: ProgressLifecycle, Label: "Lifecycle", Description: "Run lifecycle events."},
	{Value: ProgressPlan, Label: "Plan", Description: "Plan and pipeline milestones."},
	{Value: ProgressWaiting, Label: "Waiting", Description: "Requests that require user input."},
	{Value: ProgressTool, Label: "Tool", Description: "Selected tool activity."},
	{Value: ProgressSubagent, Label: "Sub-agent", Description: "Delegated-agent activity."},
	{Value: ProgressCron, Label: "Scheduled task", Description: "Scheduled-task outcomes."},
}

// ProgressPresetChoices returns display metadata for supported notification
// presets without exposing the package's backing slice.
func ProgressPresetChoices() []Choice { return cloneChoices(progressPresetChoices) }

// ProgressCategoryChoices returns the stable progress event categories.
func ProgressCategoryChoices() []Choice { return cloneChoices(progressCategoryChoices) }

// NotificationConfig controls structured progress events delivered through IM
// channels. Include and Exclude contain stable category names (for example
// "tool" or "plan"); an empty Include lets the selected preset decide.
// Channel overrides are keyed by channel type: matrix, telegram, or discord.
type NotificationConfig struct {
	Preset     string                               `toml:"preset"`
	Include    []string                             `toml:"include"`
	Exclude    []string                             `toml:"exclude"`
	ThrottleMS int                                  `toml:"throttle_ms"`
	Channels   map[string]NotificationChannelConfig `toml:"channels"`
}

// ProgressConfig is an alias kept for callers that want to name the policy
// after its purpose rather than its configuration section.
type ProgressConfig = NotificationConfig

// NotificationChannelConfig overrides the notification policy for one IM
// channel without changing the event schema or Agent tool definitions.
type NotificationChannelConfig struct {
	Preset     string   `toml:"preset"`
	Include    []string `toml:"include"`
	Exclude    []string `toml:"exclude"`
	ThrottleMS int      `toml:"throttle_ms"`
}

// DefaultNotificationConfig returns the balanced policy used when the
// [notifications] section is absent or incomplete.
func DefaultNotificationConfig() NotificationConfig {
	return NotificationConfig{Preset: ProgressPresetBalanced, ThrottleMS: defaultProgressThrottleMS}
}

// EffectiveForChannel resolves a channel-specific override over the global
// policy. It returns a normalized copy, so callers may safely mutate it.
func (c NotificationConfig) EffectiveForChannel(channel string) NotificationConfig {
	base := c
	if base.Preset == "" {
		base.Preset = ProgressPresetBalanced
	}
	if base.ThrottleMS <= 0 {
		base.ThrottleMS = defaultProgressThrottleMS
	}
	base.Preset = normalizeProgressPreset(base.Preset)
	if override, ok := c.Channels[strings.ToLower(strings.TrimSpace(channel))]; ok {
		if override.Preset != "" {
			base.Preset = normalizeProgressPreset(override.Preset)
		}
		if override.Include != nil {
			base.Include = append([]string(nil), override.Include...)
		}
		if override.Exclude != nil {
			base.Exclude = append([]string(nil), override.Exclude...)
		}
		if override.ThrottleMS > 0 {
			base.ThrottleMS = override.ThrottleMS
		}
	}
	base.Include = normalizeProgressCategories(base.Include)
	base.Exclude = normalizeProgressCategories(base.Exclude)
	return base
}

// Allows reports whether a category should be delivered under this policy.
// Explicit excludes win. Explicit includes then act as an allowlist; if no
// include is configured, the preset supplies the default category set.
func (c NotificationConfig) Allows(category string) bool {
	category = strings.ToLower(strings.TrimSpace(category))
	if category == "" {
		return false
	}
	for _, excluded := range c.Exclude {
		if categoryMatches(excluded, category) {
			return false
		}
	}
	if len(c.Include) > 0 {
		for _, included := range c.Include {
			if categoryMatches(included, category) {
				return true
			}
		}
		return false
	}
	return presetAllows(normalizeProgressPreset(c.Preset), category)
}

func categoryMatches(pattern, category string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "*" || pattern == category {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(category, strings.TrimSuffix(pattern, "*"))
	}
	return false
}

func presetAllows(preset, category string) bool {
	switch normalizeProgressPreset(preset) {
	case ProgressPresetQuiet:
		return category == ProgressCron
	case ProgressPresetVerbose:
		return category == ProgressLifecycle || category == ProgressPlan || category == ProgressWaiting ||
			category == ProgressTool || category == ProgressSubagent || category == ProgressCron
	default: // balanced: useful milestones, without per-tool spam
		return category == ProgressPlan || category == ProgressCron
	}
}

func normalizeProgressPreset(preset string) string {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case ProgressPresetQuiet, ProgressPresetVerbose:
		return strings.ToLower(strings.TrimSpace(preset))
	default:
		return ProgressPresetBalanced
	}
}

func normalizeProgressCategories(categories []string) []string {
	if categories == nil {
		return nil
	}
	result := make([]string, 0, len(categories))
	seen := make(map[string]struct{}, len(categories))
	for _, category := range categories {
		category = strings.ToLower(strings.TrimSpace(category))
		if category == "" {
			continue
		}
		if _, ok := seen[category]; ok {
			continue
		}
		seen[category] = struct{}{}
		result = append(result, category)
	}
	return result
}
