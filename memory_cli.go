package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"ageage/config"
	"ageage/tools"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
)

type memoryCLI struct {
	isTTY   func(*cobra.Command) bool
	newRepo func(string) *tools.MemoryRepository
}

func defaultMemoryCLI() memoryCLI {
	return memoryCLI{isTTY: termIsTerminal, newRepo: tools.NewMemoryRepository}
}

func memoryCommand() *cobra.Command { return memoryCommandWith(defaultMemoryCLI()) }

func memoryCommandWith(cli memoryCLI) *cobra.Command {
	root := &cobra.Command{
		Use:   "memory",
		Short: "Browse and manage long-term memory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cli.isTTY(cmd) {
				return memoryNeedsTerminal()
			}
			return runMemoryMenu(cmd, cli)
		},
	}
	root.PersistentFlags().StringP("config", "c", "", "Path to config.toml")

	root.AddCommand(&cobra.Command{Use: "list", Short: "List memory records", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return runMemoryList(cmd, cli)
	}})
	root.AddCommand(&cobra.Command{Use: "search <query>", Short: "Search memory records", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return runMemorySearch(cmd, cli, args[0])
	}})
	root.AddCommand(&cobra.Command{Use: "add", Short: "Add a memory record", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if !cli.isTTY(cmd) {
			return memoryNeedsTerminal()
		}
		return runMemoryAdd(cmd, cli)
	}})
	root.AddCommand(&cobra.Command{Use: "edit <id>", Short: "Edit a memory record", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !cli.isTTY(cmd) {
			return memoryNeedsTerminal()
		}
		return runMemoryEdit(cmd, cli, args[0])
	}})
	remove := &cobra.Command{Use: "remove [id]...", Aliases: []string{"rm"}, Short: "Remove memory records", Args: cobra.ArbitraryArgs, RunE: func(cmd *cobra.Command, args []string) error {
		yes, _ := cmd.Flags().GetBool("yes")
		if len(args) == 0 {
			if !cli.isTTY(cmd) {
				return fmt.Errorf("provide one or more memory IDs, or use an interactive terminal to select records")
			}
			return runMemoryDeletePicker(cmd, cli)
		}
		if !yes && !cli.isTTY(cmd) {
			return fmt.Errorf("removing memory requires a terminal confirmation; pass --yes to confirm explicitly")
		}
		return runMemoryRemoveIDs(cmd, cli, args, yes)
	}}
	remove.Flags().Bool("yes", false, "Confirm removal in scripts")
	root.AddCommand(remove)
	export := &cobra.Command{Use: "export <path>", Short: "Export an exact JSONL snapshot", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		force, _ := cmd.Flags().GetBool("force")
		return runMemoryExport(cmd, cli, args[0], force)
	}}
	export.Flags().Bool("force", false, "Replace an existing export file")
	root.AddCommand(export)
	return root
}

func memoryNeedsTerminal() error {
	return fmt.Errorf("this memory action needs a terminal; use `ageage memory list`, `search`, `export`, or `remove <id> --yes` in scripts")
}

func memoryRepoFromCmd(cmd *cobra.Command, cli memoryCLI) (*tools.MemoryRepository, error) {
	path := configPathFromCmd(cmd)
	cfg, err := config.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return cli.newRepo(cfg.MemoryPath()), nil
}

func reportMemoryWarnings(cmd *cobra.Command, warnings []tools.MemoryWarning) {
	for _, warning := range warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: MEMORY.jsonl line %d: %s\n", warning.Line, warning.Message)
	}
}

func runMemoryList(cmd *cobra.Command, cli memoryCLI) error {
	repo, err := memoryRepoFromCmd(cmd, cli)
	if err != nil {
		return err
	}
	snapshot, err := repo.List(cmd.Context())
	if err != nil {
		return err
	}
	reportMemoryWarnings(cmd, snapshot.Warnings)
	printMemoryRecords(cmd.OutOrStdout(), snapshot.Records)
	return nil
}

func runMemorySearch(cmd *cobra.Command, cli memoryCLI, query string) error {
	repo, err := memoryRepoFromCmd(cmd, cli)
	if err != nil {
		return err
	}
	results, err := repo.Search(cmd.Context(), query)
	if err != nil {
		return err
	}
	snapshot, err := repo.List(cmd.Context())
	if err != nil {
		return err
	}
	reportMemoryWarnings(cmd, snapshot.Warnings)
	printMemoryRecords(cmd.OutOrStdout(), results)
	return nil
}

func printMemoryRecords(out io.Writer, records []tools.MemoryRecord) {
	if len(records) == 0 {
		fmt.Fprintln(out, "No memory records.")
		return
	}
	for i, record := range records {
		entry := record.Entry
		fmt.Fprintf(out, "ID: %s\nTimestamp: %s\nTags: %s\nContent:\n%s\n", entry.ID, entry.Timestamp, entry.Tags, entry.Content)
		if i+1 < len(records) {
			fmt.Fprintln(out)
		}
	}
}

func runMemoryAdd(cmd *cobra.Command, cli memoryCLI) error {
	content, tags := "", ""
	if err := memoryContentForm(cmd, &content); err != nil {
		if errors.Is(err, errFormCancelled) {
			return nil
		}
		return err
	}
	if err := inputForm(cmd, "Tags (comma-separated, optional)", &tags, false); err != nil {
		if errors.Is(err, errFormCancelled) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("memory content cannot be empty")
	}
	repo, err := memoryRepoFromCmd(cmd, cli)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(repo.Path()), 0o755); err != nil {
		return fmt.Errorf("create memory data directory: %w", err)
	}
	entry, err := repo.Add(cmd.Context(), content, tags)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Added memory %s\n", entry.ID)
	return nil
}

func runMemoryEdit(cmd *cobra.Command, cli memoryCLI, id string) error {
	repo, err := memoryRepoFromCmd(cmd, cli)
	if err != nil {
		return err
	}
	snapshot, err := repo.List(cmd.Context())
	if err != nil {
		return err
	}
	reportMemoryWarnings(cmd, snapshot.Warnings)
	record, err := uniqueMemoryRecord(snapshot.Records, id)
	if err != nil {
		return err
	}
	content, tags := record.Entry.Content, record.Entry.Tags
	if err := memoryContentForm(cmd, &content); err != nil {
		if errors.Is(err, errFormCancelled) {
			return nil
		}
		return err
	}
	if err := inputForm(cmd, "Tags (comma-separated, optional)", &tags, false); err != nil {
		if errors.Is(err, errFormCancelled) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("memory content cannot be empty")
	}
	if err := repo.Edit(cmd.Context(), id, record.Revision, content, tags); err != nil {
		return memoryConflictError(err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Updated memory %s\n", id)
	return nil
}

func memoryContentForm(cmd *cobra.Command, content *string) error {
	field := huh.NewInput().Title("Memory content").Value(content).Validate(validateMemoryContent)
	return runHuh(huh.NewForm(huh.NewGroup(field)), cmd)
}

func validateMemoryContent(content string) error {
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("memory content cannot be empty")
	}
	return nil
}

func uniqueMemoryRecord(records []tools.MemoryRecord, id string) (tools.MemoryRecord, error) {
	var found *tools.MemoryRecord
	for i := range records {
		if records[i].Entry.ID == id {
			if found != nil {
				return tools.MemoryRecord{}, fmt.Errorf("memory ID %q is ambiguous", id)
			}
			found = &records[i]
		}
	}
	if found == nil {
		return tools.MemoryRecord{}, fmt.Errorf("memory ID %q not found", id)
	}
	return *found, nil
}

func memoryConflictError(err error) error {
	if errors.Is(err, tools.ErrMemoryConflict) {
		return fmt.Errorf("memory changed while you were editing; reload it and try again")
	}
	return err
}

func runMemoryRemoveIDs(cmd *cobra.Command, cli memoryCLI, ids []string, confirmed bool) error {
	repo, err := memoryRepoFromCmd(cmd, cli)
	if err != nil {
		return err
	}
	snapshot, err := repo.List(cmd.Context())
	if err != nil {
		return err
	}
	reportMemoryWarnings(cmd, snapshot.Warnings)
	revisions := make(map[string]string, len(ids))
	for _, id := range ids {
		record, err := uniqueMemoryRecord(snapshot.Records, id)
		if err != nil {
			return err
		}
		revisions[id] = record.Revision
	}
	if !confirmed {
		choice := "No, keep records"
		if err := selectForm(cmd, fmt.Sprintf("Remove %d memory record(s)?", len(revisions)), optionChoices([]string{"No, keep records", "Yes, remove records"}), &choice); err != nil {
			if errors.Is(err, errFormCancelled) {
				return nil
			}
			return err
		}
		if choice != "Yes, remove records" {
			return nil
		}
	}
	if err := repo.Remove(cmd.Context(), revisions); err != nil {
		return memoryConflictError(err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Removed memory IDs: %s\n", strings.Join(orderedUniqueIDs(ids), ", "))
	return nil
}

func runMemoryDeletePicker(cmd *cobra.Command, cli memoryCLI) error {
	repo, err := memoryRepoFromCmd(cmd, cli)
	if err != nil {
		return err
	}
	snapshot, err := repo.List(cmd.Context())
	if err != nil {
		return err
	}
	reportMemoryWarnings(cmd, snapshot.Warnings)
	if len(snapshot.Records) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No memory records.")
		return nil
	}
	options := make([]huh.Option[string], 0, len(snapshot.Records))
	revisions := make(map[string]string, len(snapshot.Records))
	for _, record := range snapshot.Records {
		entry := record.Entry
		label := fmt.Sprintf("%s | %s | %s | %s", entry.ID, entry.Timestamp, entry.Tags, entry.Content)
		options = append(options, huh.NewOption(label, entry.ID))
		revisions[entry.ID] = record.Revision
	}
	selected := []string{}
	field := huh.NewMultiSelect[string]().Title("Select records to remove (Space toggles)").Options(options...).Filterable(false).Value(&selected)
	if err := runHuh(huh.NewForm(huh.NewGroup(field)), cmd); err != nil {
		if errors.Is(err, errFormCancelled) {
			return nil
		}
		return err
	}
	if len(selected) == 0 {
		return nil
	}
	choice := "No, keep records"
	if err := selectForm(cmd, fmt.Sprintf("Remove %d selected record(s)?", len(selected)), optionChoices([]string{"No, keep records", "Yes, remove records"}), &choice); err != nil {
		if errors.Is(err, errFormCancelled) {
			return nil
		}
		return err
	}
	if choice != "Yes, remove records" {
		return nil
	}
	selectedRevisions := make(map[string]string, len(selected))
	for _, id := range selected {
		revision, ok := revisions[id]
		if !ok {
			return fmt.Errorf("selected memory record no longer exists")
		}
		selectedRevisions[id] = revision
	}
	if err := repo.Remove(cmd.Context(), selectedRevisions); err != nil {
		return memoryConflictError(err)
	}
	removedIDs := make([]string, 0, len(selectedRevisions))
	for id := range selectedRevisions {
		removedIDs = append(removedIDs, id)
	}
	slices.Sort(removedIDs)
	fmt.Fprintf(cmd.OutOrStdout(), "Removed memory IDs: %s\n", strings.Join(removedIDs, ", "))
	return nil
}

func orderedUniqueIDs(ids []string) []string {
	// Keep first-seen CLI argument order so script output stays deterministic.
	seen := make(map[string]struct{}, len(ids))
	ordered := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ordered = append(ordered, id)
	}
	return ordered
}

func runMemoryExport(cmd *cobra.Command, cli memoryCLI, destination string, force bool) error {
	repo, err := memoryRepoFromCmd(cmd, cli)
	if err != nil {
		return err
	}
	if _, err := os.Stat(destination); err == nil && !force {
		if !cli.isTTY(cmd) {
			return fmt.Errorf("export destination already exists; use --force to replace it")
		}
		choice := "No, keep existing file"
		if err := selectForm(cmd, "Export file exists. Replace it?", optionChoices([]string{"No, keep existing file", "Yes, replace it"}), &choice); err != nil {
			if errors.Is(err, errFormCancelled) {
				return nil
			}
			return err
		}
		if choice != "Yes, replace it" {
			return nil
		}
		force = true
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if force {
		err = repo.WriteExport(cmd.Context(), destination)
	} else {
		err = repo.WriteExportIfAbsent(cmd.Context(), destination)
	}
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("export destination already exists; use --force to replace it")
		}
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Exported memory to %s\n", destination)
	return nil
}

func runMemoryMenu(cmd *cobra.Command, cli memoryCLI) error {
	for {
		action := "Browse"
		choices := []string{"Browse", "Search", "Add", "Edit", "Delete", "Export", "Exit"}
		if err := selectForm(cmd, "Memory", optionChoices(choices), &action); err != nil {
			if errors.Is(err, errFormCancelled) {
				return nil
			}
			return err
		}
		switch action {
		case "Browse":
			if err := runMemoryList(cmd, cli); err != nil {
				return err
			}
		case "Search":
			query := ""
			if err := inputForm(cmd, "Search memory", &query, false); err != nil {
				if errors.Is(err, errFormCancelled) {
					continue
				}
				return err
			}
			if err := runMemorySearch(cmd, cli, query); err != nil {
				return err
			}
		case "Add":
			if err := runMemoryAdd(cmd, cli); err != nil {
				return err
			}
		case "Edit":
			id, err := chooseMemoryID(cmd, cli)
			if errors.Is(err, errFormCancelled) {
				continue
			}
			if err != nil {
				return err
			}
			if err := runMemoryEdit(cmd, cli, id); err != nil {
				return err
			}
		case "Delete":
			if err := runMemoryDeletePicker(cmd, cli); err != nil {
				return err
			}
		case "Export":
			path := ""
			if err := inputForm(cmd, "Export path", &path, false); err != nil {
				if errors.Is(err, errFormCancelled) {
					continue
				}
				return err
			}
			if strings.TrimSpace(path) == "" {
				continue
			}
			if err := runMemoryExport(cmd, cli, path, false); err != nil {
				return err
			}
		case "Exit":
			return nil
		}
	}
}

func chooseMemoryID(cmd *cobra.Command, cli memoryCLI) (string, error) {
	repo, err := memoryRepoFromCmd(cmd, cli)
	if err != nil {
		return "", err
	}
	snapshot, err := repo.List(cmd.Context())
	if err != nil {
		return "", err
	}
	reportMemoryWarnings(cmd, snapshot.Warnings)
	if len(snapshot.Records) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No memory records.")
		return "", errFormCancelled
	}
	options := make([]huh.Option[string], 0, len(snapshot.Records))
	for _, record := range snapshot.Records {
		entry := record.Entry
		options = append(options, huh.NewOption(fmt.Sprintf("%s | %s | %s | %s", entry.ID, entry.Timestamp, entry.Tags, entry.Content), entry.ID))
	}
	selected := ""
	field := huh.NewSelect[string]().Title("Choose a memory record").Options(options...).Filtering(false).Value(&selected)
	if err := runHuh(huh.NewForm(huh.NewGroup(field)), cmd); err != nil {
		return "", err
	}
	return selected, nil
}
