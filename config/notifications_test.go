package config

import "testing"

func TestNotificationPresetsFilterProgressCategories(t *testing.T) {
	quiet := DefaultNotificationConfig()
	quiet.Preset = ProgressPresetQuiet
	if quiet.Allows(ProgressTool) {
		t.Fatal("quiet preset should suppress tool events")
	}
	if !quiet.Allows(ProgressLifecycle) || !quiet.Allows(ProgressCron) {
		t.Fatal("quiet preset should retain lifecycle and cron events")
	}

	balanced := DefaultNotificationConfig()
	if balanced.Allows(ProgressTool) || !balanced.Allows(ProgressPlan) {
		t.Fatal("balanced preset should show milestones but suppress per-tool events")
	}

	verbose := DefaultNotificationConfig()
	verbose.Preset = ProgressPresetVerbose
	if !verbose.Allows(ProgressTool) || !verbose.Allows(ProgressSubagent) {
		t.Fatal("verbose preset should include detailed categories")
	}
}

func TestNotificationIncludeExcludeAndChannelOverride(t *testing.T) {
	cfg := NotificationConfig{
		Preset:     ProgressPresetVerbose,
		Include:    []string{"tool", "plan"},
		Exclude:    []string{"tool"},
		ThrottleMS: 1000,
		Channels: map[string]NotificationChannelConfig{
			"matrix": {Preset: ProgressPresetQuiet, ThrottleMS: 250},
		},
	}
	if cfg.Allows(ProgressTool) || !cfg.Allows(ProgressPlan) || cfg.Allows(ProgressCron) {
		t.Fatal("include/exclude filtering is incorrect")
	}
	channelCfg := NotificationConfig{
		Preset:     ProgressPresetVerbose,
		ThrottleMS: 1000,
		Channels: map[string]NotificationChannelConfig{
			"matrix": {Preset: ProgressPresetQuiet, ThrottleMS: 250},
		},
	}
	matrix := channelCfg.EffectiveForChannel("MATRIX")
	if matrix.Preset != ProgressPresetQuiet || matrix.ThrottleMS != 250 {
		t.Fatalf("matrix override = %#v", matrix)
	}
	if matrix.Allows(ProgressLifecycle) == false || matrix.Allows(ProgressTool) {
		t.Fatal("matrix quiet override has incorrect filtering")
	}
}

func TestNotificationConfigDefaultsAndUnknownPreset(t *testing.T) {
	cfg := NotificationConfig{}
	effective := cfg.EffectiveForChannel("telegram")
	if effective.Preset != ProgressPresetBalanced || effective.ThrottleMS <= 0 {
		t.Fatalf("effective defaults = %#v", effective)
	}
	cfg.Preset = "not-a-preset"
	if cfg.EffectiveForChannel("telegram").Preset != ProgressPresetBalanced {
		t.Fatal("unknown preset should fall back to balanced")
	}
}
