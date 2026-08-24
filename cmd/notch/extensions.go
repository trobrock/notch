package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/trobrock/notch/internal/config"
	"github.com/trobrock/notch/internal/extpkg"
)

const extensionsUsage = `usage: notch extensions COMMAND

Commands:
  install SOURCE       install a local, GitHub, or Git package
  list                 list installed packages and integrity state
  update [NAME...]     update selected packages, or all packages
  remove NAME          remove an installed package
  init [DIRECTORY]     create a shareable extension package
  validate [DIRECTORY] validate a package without installing it`

func runExtensions(args []string) error {
	if len(args) == 0 || args[0] == "list" || args[0] == "ls" {
		if len(args) == 0 {
			return runExtensionsList(nil)
		}
		return runExtensionsList(args[1:])
	}
	switch args[0] {
	case "install", "add":
		return runExtensionsInstall(args[1:])
	case "update", "upgrade":
		return runExtensionsUpdate(args[1:])
	case "remove", "rm", "uninstall":
		return runExtensionsRemove(args[1:])
	case "init":
		return runExtensionsInit(args[1:])
	case "validate", "check":
		return runExtensionsValidate(args[1:])
	case "help", "--help", "-h":
		fmt.Println(extensionsUsage)
		return nil
	default:
		return fmt.Errorf("unknown extensions command %q\n%s", args[0], extensionsUsage)
	}
}

func runExtensionsInstall(args []string) error {
	flags := flag.NewFlagSet("notch extensions install", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	ref := flags.String("ref", "", "Git branch, tag, or commit to install")
	subdir := flags.String("subdir", "", "package subdirectory within the source repository")
	jsonOutput := flags.Bool("json", false, "emit the installed lock record as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: notch extensions install [--ref REF] [--subdir PATH] [--json] SOURCE")
	}
	store, cwd, err := extensionStore()
	if err != nil {
		return err
	}
	source, err := extpkg.ParseSource(flags.Arg(0), cwd, *ref, *subdir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	installed, err := store.Install(ctx, source)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(installed)
	}
	fmt.Printf("Installed %s %s from %s\n", installed.Name, installed.Version, installed.Source.String())
	fmt.Println("Restart Notch to load it. Extensions are trusted code; review third-party packages before use.")
	return nil
}

func runExtensionsList(args []string) error {
	flags := flag.NewFlagSet("notch extensions list", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	jsonOutput := flags.Bool("json", false, "emit installed package status as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: notch extensions list [--json]")
	}
	store, _, err := extensionStore()
	if err != nil {
		return err
	}
	statuses, err := store.List()
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(struct {
			Version  int             `json:"version"`
			Packages []extpkg.Status `json:"packages"`
		}{Version: 1, Packages: statuses})
	}
	if len(statuses) == 0 {
		fmt.Println("No extension packages installed.")
		return nil
	}
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "NAME\tVERSION\tSTATE\tSOURCE\tRESOLVED")
	for _, status := range statuses {
		resolved := status.Resolved
		if len(resolved) > 12 {
			resolved = resolved[:12]
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", status.Name, status.Version, status.State, status.Source.String(), resolved)
	}
	return writer.Flush()
}

func runExtensionsUpdate(args []string) error {
	flags := flag.NewFlagSet("notch extensions update", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	force := flags.Bool("force", false, "allow a package version downgrade")
	jsonOutput := flags.Bool("json", false, "emit update results as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	store, _, err := extensionStore()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	results, err := store.Update(ctx, flags.Args(), *force)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(struct {
			Version int                   `json:"version"`
			Updates []extpkg.UpdateResult `json:"updates"`
		}{Version: 1, Updates: results})
	}
	if len(results) == 0 {
		fmt.Println("No extension packages installed.")
		return nil
	}
	for _, result := range results {
		if !result.Changed {
			fmt.Printf("%s %s is up to date.\n", result.Before.Name, result.Before.Version)
			continue
		}
		if result.Before.Version == result.After.Version {
			fmt.Printf("Reconciled %s %s.\n", result.Before.Name, result.After.Version)
			continue
		}
		fmt.Printf("Updated %s %s -> %s.\n", result.Before.Name, result.Before.Version, result.After.Version)
	}
	fmt.Println("Restart Notch to load updated extensions.")
	return nil
}

func runExtensionsRemove(args []string) error {
	flags := flag.NewFlagSet("notch extensions remove", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	jsonOutput := flags.Bool("json", false, "emit the removed lock record as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: notch extensions remove [--json] NAME")
	}
	store, _, err := extensionStore()
	if err != nil {
		return err
	}
	removed, err := store.Remove(flags.Arg(0))
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(removed)
	}
	fmt.Printf("Removed %s %s. Restart Notch to unload it.\n", removed.Name, removed.Version)
	return nil
}

func runExtensionsInit(args []string) error {
	flags := flag.NewFlagSet("notch extensions init", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	name := flags.String("name", "", "package name (defaults to the directory name)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("usage: notch extensions init [--name NAME] [DIRECTORY]")
	}
	directory := "."
	if flags.NArg() == 1 {
		directory = flags.Arg(0)
	}
	manifest, err := extpkg.Init(directory, *name)
	if err != nil {
		return err
	}
	absolute, _ := filepath.Abs(directory)
	fmt.Printf("Created %s %s in %s\n", manifest.Name, manifest.Version, absolute)
	fmt.Println("Commit the package to Git, then share its repository URL for `notch extensions install`.")
	return nil
}

func runExtensionsValidate(args []string) error {
	flags := flag.NewFlagSet("notch extensions validate", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	jsonOutput := flags.Bool("json", false, "emit validation details as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("usage: notch extensions validate [--json] [DIRECTORY]")
	}
	directory := "."
	if flags.NArg() == 1 {
		directory = flags.Arg(0)
	}
	validation, err := extpkg.Validate(directory)
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(validation)
	}
	fmt.Printf("Valid package %s %s (%s)\n", validation.Manifest.Name, validation.Manifest.Version, validation.Integrity)
	return nil
}

func extensionStore() (*extpkg.Store, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, "", err
	}
	return extpkg.New(config.HomeDir(home)), cwd, nil
}
