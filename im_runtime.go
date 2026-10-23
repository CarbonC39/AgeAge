package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"ageage/agent"
	"ageage/channel"
	"ageage/config"
	"ageage/conversation"
	"ageage/event"
	"ageage/llm"
	"ageage/rendering"
	"ageage/tools"
)

// imRuntime exposes semantic input and interaction entrypoints for connectors.
type imRuntime struct {
	Handler  channel.MessageHandler
	Sessions *imSessions
	Answer   func(conversation.Target, string)
}

func newIMRuntime(factory *agent.AgentFactory, channelsByType map[string]channel.Channel) *imRuntime {
	cfg := factory.Config
	sm := agent.NewSessionManager(cfg.AgeAgeDirPath())
	registry, err := agent.OpenSessionRegistry(cfg.AgeAgeDirPath())
	if err != nil {
		fmt.Printf("[session] registry unavailable: %s\n", err)
	}
	sessions := newIMSessions(sm, registry)
	surfaceFor := func(target conversation.Target) channel.Surface {
		return channel.NewSurface(channelsByType[target.ChannelType], target)
	}
	sessionCommands := imSessionCommands{sessions: sessions, surfaceFor: surfaceFor}
	confirmationUsers := map[string][]string{
		config.ChannelMatrix:   cfg.Channels.Matrix.AllowedUsers,
		config.ChannelTelegram: cfg.Channels.Telegram.AllowedUsers,
		config.ChannelDiscord:  cfg.Channels.Discord.AllowedUsers,
	}
	confirmMgr := tools.NewConfirmationManager()
	agents := make(map[string]*agent.Agent)
	activeFeedback := make(map[string]*imRunFeedback)
	var agentMu sync.Mutex
	getAgent := func(target conversation.Target) (*agent.Agent, string, error) {
		id, err := sessions.Resolve(target)
		if err != nil {
			return nil, "", err
		}
		agentMu.Lock()
		if ag := agents[id]; ag != nil {
			agentMu.Unlock()
			return ag, id, nil
		}
		agentMu.Unlock()
		ag := factory.CreateAgent(confirmMgr, "")
		ag.SetCommandPrefix(imCommandPrefix(target.ChannelType))
		ag.SetInteractionScope(interactionScope(target, id))
		ag.SessionDir = sm.SessionDir(id)
		// Async evaluator notices retain the originating surface after a run closes.
		surface := surfaceFor(target.WithoutReply())
		ag.Callbacks.Notify = func(text string) {
			if cfg.Notifications.EffectiveForChannel(target.ChannelType).Allows(config.ProgressLifecycle) {
				_, _ = surface.Send(text)
			}
		}
		ag.Callbacks.AskUser = func(question string, options []string) { _ = surface.Ask(question, options) }
		ag.Callbacks.BackgroundEvent = func(ev event.Event) {
			presenter := rendering.NewRun(surface, cfg.Notifications)
			defer presenter.Close()
			if err := presenter.Handle(ev); err != nil {
				fmt.Printf("[notification] %s\n", err)
			}
		}
		agentMu.Lock()
		defer agentMu.Unlock()
		if existing := agents[id]; existing != nil {
			return existing, id, nil
		}
		agents[id] = ag
		return ag, id, nil
	}
	chatLocks := make(map[string]*sync.Mutex)
	var chatLocksMu sync.Mutex
	getChatMutex := func(key string) *sync.Mutex {
		chatLocksMu.Lock()
		defer chatLocksMu.Unlock()
		if lock := chatLocks[key]; lock != nil {
			return lock
		}
		lock := &sync.Mutex{}
		chatLocks[key] = lock
		return lock
	}
	respond := func(msg channel.IncomingMessage, text string) string {
		if text == "" {
			return ""
		}
		if _, err := surfaceFor(msg.Target()).Reply(text); err == nil {
			return ""
		}
		return text
	}
	handler := func(msg channel.IncomingMessage) string {
		target := msg.Target()
		surface := surfaceFor(target)
		chatKey := target.BindingKey()
		text := strings.TrimSpace(msg.Text)
		textLow := strings.ToLower(text)
		cmd := parseIMCommand(target.ChannelType, text)
		// Media captions are part of a user turn, even when they happen to
		// resemble administrative commands or confirmation replies.
		if len(msg.Attachments) > 0 {
			cmd = imCommand{}
		}
		isConfirmationResponse := len(msg.Attachments) == 0 && (textLow == "y" || textLow == "n" || textLow == "a")
		// Reuse the channel connector's inbound allowlist for confirmation
		// approval. The originating sender remains authorised even when the
		// allowlist is empty.

		confirmMgr.SetAllowedRespondersForScope(tools.InteractionScope{
			ChannelType: target.ChannelType,
			ChannelID:   target.ChannelID,
		}, confirmationUsers[target.ChannelType]...)

		waitingScope := interactionScope(target, sessions.Current(target))
		isPendingAnswer := factory.UserInputMgr.HasPendingScope(waitingScope)

		// In group chats, only respond when the bot is @mentioned or replied to.
		// Confirmation replies and recognized channel commands are directed at the
		// bot by definition; unknown prefixed text still requires a mention.
		if msg.IsGroupChat && !msg.BotMentioned && !isPendingAnswer {
			if !isConfirmationResponse && !isRecognizedIMCommand(cmd, factory.GetSkills()) {
				return ""
			}
		}

		// Confirmation responses bypass the per-chat mutex.
		if isConfirmationResponse {
			agentMu.Lock()
			pendingSession := sessions.Current(target)
			agentMu.Unlock()
			scope := tools.InteractionScope{
				ChannelType: target.ChannelType,
				ChannelID:   target.ChannelID,
				ThreadID:    target.ThreadID,
				SessionID:   pendingSession,
				SenderID:    target.SenderID,
			}
			if len(confirmMgr.GetPendingForScope(scope)) > 0 {
				allowed := (textLow == "y" || textLow == "a")
				if !confirmMgr.RespondForScope(scope, allowed) {
					return respond(msg, "❌ Confirmation could not be matched uniquely or you are not authorised.")
				}
				if !allowed {
					return respond(msg, "❌ Operation denied.")
				}
				return ""
			}
		}

		// stop and session abort bypass the per-chat mutex (which may be held by
		// the agent). Both are exact channel-aware command matches.
		if cmd.is("stop") && cmd.Args == "" || cmd.is("session") && strings.EqualFold(cmd.Args, "abort") {
			agentMu.Lock()
			if sessionID := sessions.Current(target); sessionID != "" {
				if ag, ok := agents[sessionID]; ok {
					ag.Stop()
				}
			}
			feedback := activeFeedback[chatKey]
			agentMu.Unlock()
			factory.UserInputMgr.CancelForScope(interactionScope(target, sessions.Current(target)))
			if feedback != nil {
				feedback.Stop()
			}

			return respond(msg, "🛑 Task stopped.")
		}

		if cmd.is("session") || cmd.is("sessions") {
			sub := ""
			if fields := strings.Fields(cmd.Args); len(fields) > 0 {
				sub = fields[0]
			}
			readOnly := cmd.is("sessions") || sub == "" || strings.EqualFold(sub, "list") || strings.EqualFold(sub, "ls")
			newThread := (strings.EqualFold(sub, "new") || strings.EqualFold(sub, "n")) && surfaceFor(target.Room()).Capabilities().CreateThread
			if !readOnly && !newThread {
				lock := getChatMutex(chatKey)
				if !lock.TryLock() {
					return respond(msg, "A run is active here. Use session list to navigate, or stop it before changing this session.")
				}
				defer lock.Unlock()
			}
			reply, err := sessionCommands.handle(surface, cmd)
			if err != nil {
				return respond(msg, "❌ "+err.Error())
			}
			return respond(msg, reply)
		}

		// If a pipeline node is waiting for user input, route this message to it
		// instead of starting a new agent run.
		agentMu.Lock()
		pendingSession := sessions.Current(target)
		agentMu.Unlock()
		scope := tools.InteractionScope{
			ChannelType: target.ChannelType,
			ChannelID:   target.ChannelID,
			ThreadID:    target.ThreadID,
			SessionID:   pendingSession,
			SenderID:    target.SenderID,
		}
		if len(factory.UserInputMgr.GetPendingForScope(scope)) > 0 {
			agentMu.Lock()
			waitingAgent := agents[pendingSession]
			agentMu.Unlock()
			var tmp *agent.TmpManager
			if waitingAgent != nil {
				tmp = waitingAgent.TmpManager()
			}
			_, reply := answerPendingIMInput(context.Background(), msg, scope, factory.UserInputMgr, factory.Config, tmp)
			return respond(msg, reply)
		}

		_ = surface.Read()

		// All other messages are processed sequentially per chat.
		mu := getChatMutex(chatKey)
		mu.Lock()
		defer mu.Unlock()

		if debugFlag {
			fmt.Printf("\n  ▸ [%s] %s: %s\n", target.ChannelType, target.SenderName, msg.Text)
		}

		// cred commands — never routed through the agent.
		// /cred set and /cred add are blocked in IM to prevent passwords appearing in chat logs.
		if cmd.is("cred") {
			return respond(msg, handleCredChanCmd(msg, factory.CredMgr, text, cmd.Prefix))
		}

		// build [description] — create a skill or pipeline without entering the agent loop.
		// The planner runs isolated; the main conversation history is untouched.
		if cmd.is("build") {
			task := cmd.Args
			feedback := beginSurfaceFeedback(surface)
			buildFailed := true
			agentMu.Lock()
			activeFeedback[chatKey] = feedback
			agentMu.Unlock()
			defer func() { feedback.Close(buildFailed); agentMu.Lock(); delete(activeFeedback, chatKey); agentMu.Unlock() }()
			_, sessionID, getErr := getAgent(target)
			if getErr != nil {
				return respond(msg, "❌ "+getErr.Error())
			}

			history, historyErr := sm.LoadHistory(sessionID)
			if historyErr != nil {
				return respond(msg, fmt.Sprintf("❌ Failed to load session history: %s", historyErr))
			}
			docsDir := filepath.Join(factory.Config.AgeAgeDirPath(), "docs")
			planner := agent.NewPlanner(factory, docsDir, factory.GetStandardToolNames())
			skill, buildErr := planner.CreateSkill(context.Background(), task, history)

			if buildErr != nil {
				return respond(msg, fmt.Sprintf("❌ Build failed: %s", buildErr))
			}
			buildFailed = false
			return respond(msg, fmt.Sprintf("✅ Built `%s` — use `%s%s` to activate.", skill.Name, cmd.Prefix, skill.CommandName()))
		}

		switch {
		case cmd.is("clear") && cmd.Args == "":
			ag, sessionID, getErr := getAgent(target)
			if getErr != nil {
				return respond(msg, "❌ "+getErr.Error())
			}
			if err := sm.WithSessionTransaction(sessionID, func(tx *agent.SessionTransaction) error {
				messages, err := tx.LoadHistory()
				if err != nil {
					return err
				}
				ag.SetMessages(messages)
				ag.ClearHistory()
				return tx.SaveHistory(ag.Messages())
			}); err != nil {
				return respond(msg, fmt.Sprintf("❌ Failed to clear history: %s", err))
			}
			return respond(msg, "🗑️ Conversation history cleared.")

		case cmd.is("summarize") && cmd.Args == "":
			ag, sessionID, getErr := getAgent(target)
			if getErr != nil {
				return respond(msg, "❌ "+getErr.Error())
			}
			var summary string
			err := sm.WithSessionTransaction(sessionID, func(tx *agent.SessionTransaction) error {
				messages, err := tx.LoadHistory()
				if err != nil {
					return err
				}
				ag.SetMessages(messages)
				summary, err = ag.ForceSummarize()
				if err != nil {
					return err
				}
				return tx.SaveHistory(ag.Messages())
			})
			if err != nil {
				return respond(msg, fmt.Sprintf("❌ %s", err))
			}
			return respond(msg, fmt.Sprintf("📋 Summary:\n%s", summary))

		case cmd.is("undo") && cmd.Args == "":
			ag, sessionID, getErr := getAgent(target)
			if getErr != nil {
				return respond(msg, "❌ "+getErr.Error())
			}
			n := 0
			if err := sm.WithSessionTransaction(sessionID, func(tx *agent.SessionTransaction) error {
				messages, err := tx.LoadHistory()
				if err != nil {
					return err
				}
				ag.SetMessages(messages)
				n = ag.RollbackLastTurn()
				if n == 0 {
					return nil
				}
				return tx.SaveHistory(ag.Messages())
			}); err != nil {
				return respond(msg, fmt.Sprintf("❌ Failed to undo: %s", err))
			}
			if n == 0 {
				return respond(msg, "Nothing to undo.")
			}
			return respond(msg, "↩️ Last turn undone.")

		case cmd.is("help") && cmd.Args == "":
			return respond(msg, "Available commands:\n"+
				fmt.Sprintf("%sclear — Clear conversation history (keeps session)\n", cmd.Prefix)+
				fmt.Sprintf("%sbuild [description] — Create a skill or pipeline (uses conversation context)\n", cmd.Prefix)+
				fmt.Sprintf("%sstop — Stop the current task\n", cmd.Prefix)+
				fmt.Sprintf("%ssummarize — Summarize conversation\n", cmd.Prefix)+
				fmt.Sprintf("%sundo — Remove the last turn from history\n", cmd.Prefix)+
				fmt.Sprintf("%sretry [text] — Re-run the last message (optionally modified)\n", cmd.Prefix)+
				fmt.Sprintf("%ssessions — List sessions for this room\n", cmd.Prefix)+
				fmt.Sprintf("%ssession list|ls — List sessions\n", cmd.Prefix)+
				fmt.Sprintf("%ssession new|n [name] — Start a new session\n", cmd.Prefix)+
				fmt.Sprintf("%ssession switch|sw <name> — Switch to a session\n", cmd.Prefix)+
				fmt.Sprintf("%ssession remove|rm <name> — Remove a session (moves to trash)\n", cmd.Prefix)+
				fmt.Sprintf("%scred list|ls — List stored credentials\n", cmd.Prefix)+
				fmt.Sprintf("%shelp — Show this help", cmd.Prefix))
		}

		// retry [modifier] — roll back the last turn and re-run with optional extra text.
		// Must NOT return here; fall through to the agent execution block below.
		retryRequested := cmd.is("retry")
		retryModifier := cmd.Args

		feedback := beginSurfaceFeedback(surface)
		runReporter := rendering.NewRun(surface, factory.Config.Notifications)
		runFailed := true
		agentMu.Lock()
		activeFeedback[chatKey] = feedback
		agentMu.Unlock()
		defer func() {
			_ = runReporter.Close()
			feedback.Close(runFailed)
			agentMu.Lock()
			delete(activeFeedback, chatKey)
			agentMu.Unlock()
		}()
		ag, sessionID, getErr := getAgent(target)
		if getErr != nil {
			return respond(msg, "❌ "+getErr.Error())
		}

		// Serialize the complete mutable Agent lifecycle per logical session.
		// The transaction keeps the lock through Agent.Run and history save;
		// use tx.SaveHistory below to avoid reacquiring the non-reentrant lock.
		tx, unlockSession := sm.BeginSessionTransaction(sessionID)
		defer unlockSession()
		persistedMessages, loadErr := tx.LoadHistory()
		if loadErr != nil {
			return respond(msg, fmt.Sprintf("Agent error: failed to load session history: %s", loadErr))
		}
		ag.SetMessages(persistedMessages)
		ag.SetInteractionScope(tools.InteractionScope{
			ChannelType: target.ChannelType,
			ChannelID:   target.ChannelID,
			ThreadID:    target.ThreadID,
			SessionID:   sessionID,
			SenderID:    target.SenderID,
		})

		restoreCallbacks := installIMRunCallbacks(ag, runReporter, surface)
		defer restoreCallbacks()

		runText := msg.Text
		var retryParts []llm.ContentPart
		if retryRequested {
			lastMsg, ok := ag.LastTurnUserMessage()
			if !ok {
				runFailed = false
				return respond(msg, "Nothing to retry.")
			}
			retried := retryUserInput(lastMsg, retryModifier)
			runText, retryParts = retried.Text, retried.Parts
			ag.RollbackLastTurn()
		}
		input := agent.UserInput{Text: runText, Parts: retryParts}
		if !retryRequested {
			input = normalizeIMInput(context.Background(), msg, factory.Config, ag.TmpManager())
		}
		warningReply := imInputWarnings(input)
		if warningReply != "" {
			warningReply = respond(msg, warningReply)
		}
		var result string
		var err error
		result, err = ag.RunInput(context.Background(), input, nil)
		// Save history after every run (best-effort; ignore errors).
		if saveErr := tx.SaveHistory(ag.Messages()); saveErr != nil && err == nil {
			err = fmt.Errorf("save session history: %w", saveErr)
		}

		runFailed = err != nil
		if warningReply != "" {
			result = warningReply + result
		}
		if deliveryErr := runReporter.Complete(result, err); deliveryErr != nil {
			return respond(msg, fmt.Sprintf("Delivery error: %s", deliveryErr))
		}
		return ""
	}

	answer := func(target conversation.Target, answer string) {
		scope := interactionScope(target, sessions.Current(target))
		low := strings.ToLower(strings.TrimSpace(answer))
		if (low == "y" || low == "n" || low == "a") && len(confirmMgr.GetPendingForScope(scope)) > 0 {
			confirmMgr.RespondForScope(scope, low != "n")
			return
		}
		factory.UserInputMgr.RespondForScope(scope, answer)
	}
	return &imRuntime{Handler: handler, Sessions: sessions, Answer: answer}
}

// startChannels owns platform construction and lifecycle wiring only.
func startChannels(factory *agent.AgentFactory) (*channel.Manager, int, func(tools.CronEntry, string)) {
	cfg := factory.Config
	channelsByType := make(map[string]channel.Channel)
	runtime := newIMRuntime(factory, channelsByType)
	sessions := runtime.Sessions
	surfaceFor := func(target conversation.Target) channel.Surface {
		return channel.NewSurface(channelsByType[target.ChannelType], target)
	}

	manager := channel.NewManager(runtime.Handler)
	registered := 0

	opts := channel.ChannelOptions{
		Parallel: cfg.Channels.Parallel,
	}

	if cfg.Channels.Telegram.Enabled {
		if cfg.Channels.Telegram.BotToken == "" {
			fmt.Println("  ⚠  Telegram: bot_token not set, skipping")
		} else {
			tg := channel.NewTelegram(cfg.Channels.Telegram.BotToken, cfg.Channels.Telegram.AllowedUsers, opts)
			tg.AnswerCallback = func(channelID, threadID, senderID, answer string) {
				runtime.Answer(conversation.Target{ChannelType: config.ChannelTelegram, ChannelID: channelID, ThreadID: threadID, SenderID: senderID}, answer)
			}
			manager.Register(tg)
			channelsByType[config.ChannelTelegram] = tg
			fmt.Println("  ✓  Telegram")
			registered++
		}
	}

	if cfg.Channels.Discord.Enabled {
		if cfg.Channels.Discord.BotToken == "" {
			fmt.Println("  ⚠  Discord: bot_token not set, skipping")
		} else if len(cfg.Channels.Discord.ChannelIDs) == 0 {
			fmt.Println("  ⚠  Discord: no channel_ids configured, skipping")
		} else {
			dc := channel.NewDiscord(cfg.Channels.Discord.BotToken, cfg.Channels.Discord.ChannelIDs, cfg.Channels.Discord.AllowedUsers, opts)
			manager.Register(dc)
			channelsByType[config.ChannelDiscord] = dc
			fmt.Println("  ✓  Discord")
			registered++
		}
	}

	if cfg.Channels.Matrix.Enabled {
		if cfg.Channels.Matrix.AccessToken == "" {
			fmt.Println("  ⚠  Matrix: access_token not set, skipping")
		} else {
			mx := channel.NewMatrix(
				cfg.Channels.Matrix.Homeserver,
				cfg.Channels.Matrix.UserID,
				cfg.Channels.Matrix.AccessToken,
				cfg.Channels.Matrix.RoomIDs,
				cfg.Channels.Matrix.AllowedUsers,
				opts,
			)
			manager.Register(mx)
			channelsByType[config.ChannelMatrix] = mx
			fmt.Println("  ✓  Matrix")
			registered++
		}
	}

	if registered == 0 {
		return nil, 0, nil
	}

	if sessions.registry != nil {
		for _, binding := range sessions.registry.List() {
			manager.TrackConversation(binding.Target())
		}
	}

	cronDeliver := func(entry tools.CronEntry, summary string) {
		channelType, channelID, threadID, ok := cronDeliveryTarget(entry)
		if !ok {
			return
		}
		surface := surfaceFor(conversation.Target{ChannelType: channelType, ChannelID: channelID, ThreadID: threadID})
		run := rendering.NewRun(surface, cfg.Notifications)
		defer run.Close()
		if err := run.Handle(event.Event{Kind: event.CronCompleted, Name: entry.ID, Text: summary}); err != nil {
			fmt.Printf("[cron] %s\n", err)
		}
	}
	return manager, registered, cronDeliver
}

func interactionScope(target conversation.Target, sessionID string) tools.InteractionScope {
	return tools.InteractionScope{ChannelType: target.ChannelType, ChannelID: target.ChannelID, ThreadID: target.ThreadID, SessionID: sessionID, SenderID: target.SenderID}
}
