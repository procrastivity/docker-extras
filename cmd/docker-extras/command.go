package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

type pluginMetadata struct {
	SchemaVersion    string `json:"SchemaVersion"`
	Vendor           string `json:"Vendor"`
	Version          string `json:"Version"`
	ShortDescription string `json:"ShortDescription"`
	URL              string `json:"URL"`
}

type commandError struct {
	Command string
}

func (e *commandError) Error() string {
	return fmt.Sprintf("%s is not implemented yet", e.Command)
}

func Execute(args []string, stdout, stderr io.Writer, version, pluginName string) error {
	args = normalizePluginArgs(args, pluginName)
	if len(args) == 1 && args[0] == "docker-cli-plugin-metadata" {
		return writeMetadata(stdout, version)
	}

	root := newRootCommand(stdout, stderr, pluginName)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		var commandErr *commandError
		if errors.As(err, &commandErr) {
			_, _ = fmt.Fprintf(stderr, "docker-%s: %s\n", pluginName, commandErr)
			return err
		}
		_, _ = fmt.Fprintln(stderr, err)
		return err
	}
	return nil
}

func normalizePluginArgs(args []string, pluginName string) []string {
	if len(args) > 1 && args[0] == "__complete" && args[1] == pluginName {
		return append(args[:1], args[2:]...)
	}
	if len(args) > 0 && args[0] == pluginName {
		return args[1:]
	}
	return args
}

func writeMetadata(stdout io.Writer, version string) error {
	metadata := pluginMetadata{
		SchemaVersion:    "0.1.0",
		Vendor:           "procrastivity",
		Version:          version,
		ShortDescription: "Small docker developer-experience utilities",
		URL:              "https://github.com/procrastivity/docker-extras",
	}
	return json.NewEncoder(stdout).Encode(metadata)
}

func newRootCommand(stdout, stderr io.Writer, pluginName string) *cobra.Command {
	root := &cobra.Command{
		Use:           "docker-" + pluginName,
		Short:         "Small Docker developer-experience utilities",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(newVolumeCommand())
	return root
}

func pluginNameFromExecutable(path string) string {
	name := filepath.Base(path)
	if strings.HasPrefix(name, "docker-") {
		return strings.TrimPrefix(name, "docker-")
	}
	return name
}

func newVolumeCommand() *cobra.Command {
	volume := &cobra.Command{
		Use:   "volume",
		Short: "Manage volume seed archives",
		Args:  cobra.NoArgs,
	}
	seed := &cobra.Command{
		Use:   "seed",
		Short: "Capture or restore a named-volume seed",
		Args:  cobra.NoArgs,
	}
	seed.AddCommand(newLeafCommand("capture"))
	seed.AddCommand(newLeafCommand("restore"))
	volume.AddCommand(seed)
	return volume
}

func newLeafCommand(name string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   name,
		Short: fmt.Sprintf("%s a volume seed (not implemented yet)", name),
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return &commandError{Command: "volume seed " + name}
		},
	}
	flags := cmd.Flags()
	flags.String("name", "db-seed", "seed base name: NAME.tar + NAME.meta")
	flags.String("data-dir", defaultDataDir(), "seed directory")
	flags.BoolP("yes", "y", false, "do not ask for confirmation")
	flags.String("report", "", "record which users were stopped")
	if name == "capture" {
		flags.String("from-volume", "", "existing volume to archive")
		flags.String("image", "", "image lock to record instead of inferring")
	} else {
		flags.String("to-volume", "", "volume to restore into")
		flags.Bool("allow-create", false, "create the target volume when it does not exist")
		flags.StringArray("label", nil, "label to set on the target volume (repeatable)")
		flags.String("expect-image", "", "image required for the restored data")
		flags.Bool("force", false, "override an unknown or mismatched image lock")
		flags.Bool("no-verify", false, "skip the seed integrity gate")
	}
	return cmd
}

func defaultDataDir() string {
	if dataHome := os.Getenv("XDG_DATA_HOME"); dataHome != "" {
		return filepath.Join(dataHome, "docker-extras-volume-seed")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "docker-extras-volume-seed")
}
