package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMetadataHandshake(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Execute([]string{"docker-cli-plugin-metadata"}, &stdout, &stderr, "1.2.3", "extras")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var got pluginMetadata
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("metadata is not JSON: %v", err)
	}
	want := pluginMetadata{
		SchemaVersion:    "0.1.0",
		Vendor:           "procrastivity",
		Version:          "1.2.3",
		ShortDescription: "Small docker developer-experience utilities",
		URL:              "https://github.com/procrastivity/docker-extras",
	}
	if got != want {
		t.Fatalf("metadata = %#v, want %#v", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("metadata wrote to stderr: %q", stderr.String())
	}
}

func TestNestedHelpAndPluginNameNormalization(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pluginName string
		args       []string
		want       string
	}{
		{"default repeated name", "extras", []string{"extras", "volume", "seed", "--help"}, "capture"},
		{"custom repeated name", "tools", []string{"tools", "volume", "seed", "--help"}, "restore"},
		{"arbitrary repeated name", "lab42", []string{"lab42", "volume", "seed", "--help"}, "capture"},
		{"volume help", "extras", []string{"volume", "--help"}, "seed"},
		{"root help", "extras", []string{"--help"}, "volume"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := Execute(tc.args, &stdout, &stderr, "dev", tc.pluginName); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if !strings.Contains(stdout.String(), tc.want) {
				t.Fatalf("help output %q does not contain %q", stdout.String(), tc.want)
			}
			if tc.name == "root help" && strings.Contains(stdout.String(), "completion") {
				t.Fatalf("root help exposed a completion command: %q", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("help wrote to stderr: %q", stderr.String())
			}
		})
	}
}

func TestCaptureRequiresVolumeAndRestoreStillRefusesWithoutCallingDocker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "docker-called")
	bin := t.TempDir()
	docker := filepath.Join(bin, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf called > \"$DOCKER_SENTINEL\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("DOCKER_SENTINEL", marker)
	for _, leaf := range []string{"capture", "restore"} {
		var stdout, stderr bytes.Buffer
		err := Execute([]string{"volume", "seed", leaf}, &stdout, &stderr, "dev", "extras")
		if leaf == "capture" {
			var exitErr *commandExitError
			if !errors.As(err, &exitErr) || exitErr.Code != 1 {
				t.Fatalf("capture error = %v, want preflight exit 1", err)
			}
			if !strings.Contains(stderr.String(), "capture requires --from-volume") {
				t.Fatalf("capture stderr = %q", stderr.String())
			}
		} else {
			var commandErr *commandError
			if !errors.As(err, &commandErr) || !strings.Contains(stderr.String(), "not implemented yet") {
				t.Fatalf("restore error=%v stderr=%q, want refusing commandError", err, stderr.String())
			}
		}
		if stdout.Len() != 0 {
			t.Fatalf("%s unexpectedly wrote stdout: %q", leaf, stdout.String())
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Docker sentinel exists; leaf touched Docker: %v", err)
	}
}

func TestFlatAliasIsNotRegistered(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Execute([]string{"volume-seed", "capture"}, &stdout, &stderr, "dev", "extras")
	if err == nil || !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("flat command err=%v stderr=%q, want unknown command", err, stderr.String())
	}
}

func TestDockerCompletionProtocol(t *testing.T) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("Docker CLI is unavailable")
	}
	if err := exec.Command(docker, "--version").Run(); err != nil {
		t.Skipf("Docker CLI is unavailable: %v", err)
	}

	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "docker-extras")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}

	for _, pluginName := range []string{"extras", "tools", "lab42"} {
		t.Run(pluginName, func(t *testing.T) {
			config := t.TempDir()
			pluginDir := filepath.Join(config, "cli-plugins")
			if err := os.MkdirAll(pluginDir, 0o755); err != nil {
				t.Fatal(err)
			}
			installed := filepath.Join(pluginDir, "docker-"+pluginName)
			if err := copyFile(binary, installed); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command(installed, "docker-cli-plugin-metadata").CombinedOutput(); err != nil || !json.Valid(output) {
				t.Fatalf("direct metadata handshake output=%q err=%v", output, err)
			}
			env := dockerConfigEnv(config)
			help := runDocker(t, docker, env, pluginName, "--help")
			if !strings.Contains(help, "volume") {
				t.Fatalf("Docker did not recognize plugin %q: %s", pluginName, help)
			}
			if output, err := runDockerFailure(docker, env, pluginName, "volume", "seed", "capture"); err == nil || !strings.Contains(output, "capture requires --from-volume") {
				t.Fatalf("Docker dispatch for %q output=%q err=%v, want the capture preflight refusal", pluginName, output, err)
			}
			for _, tc := range []struct {
				args []string
				want []string
			}{
				{[]string{"__complete", pluginName, ""}, []string{"help", "volume"}},
				{[]string{"__completeNoDesc", pluginName, ""}, []string{"help", "volume"}},
				{[]string{"__complete", pluginName, "volume", ""}, []string{"seed"}},
				{[]string{"__completeNoDesc", pluginName, "volume", ""}, []string{"seed"}},
				{[]string{"__complete", pluginName, "volume", "seed", ""}, []string{"capture", "restore"}},
				{[]string{"__completeNoDesc", pluginName, "volume", "seed", ""}, []string{"capture", "restore"}},
				{[]string{"__complete", pluginName, "--"}, []string{"--help"}},
				{[]string{"__completeNoDesc", pluginName, "--"}, []string{"--help"}},
				{[]string{"__complete", pluginName, "volume", "--"}, []string{"--help"}},
				{[]string{"__completeNoDesc", pluginName, "volume", "--"}, []string{"--help"}},
				{[]string{"__complete", pluginName, "volume", "seed", "--"}, []string{"--help"}},
				{[]string{"__completeNoDesc", pluginName, "volume", "seed", "--"}, []string{"--help"}},
				{[]string{"__complete", pluginName, "volume", "seed", "capture", "--"}, []string{"--data-dir", "--from-volume", "--help", "--image", "--name", "--report", "--yes"}},
				{[]string{"__completeNoDesc", pluginName, "volume", "seed", "capture", "--"}, []string{"--data-dir", "--from-volume", "--help", "--image", "--name", "--report", "--yes"}},
				{[]string{"__complete", pluginName, "volume", "seed", "restore", "--"}, []string{"--allow-create", "--data-dir", "--expect-image", "--force", "--help", "--label", "--name", "--no-verify", "--report", "--to-volume", "--yes"}},
				{[]string{"__completeNoDesc", pluginName, "volume", "seed", "restore", "--"}, []string{"--allow-create", "--data-dir", "--expect-image", "--force", "--help", "--label", "--name", "--no-verify", "--report", "--to-volume", "--yes"}},
			} {
				args := tc.args
				got := runDocker(t, docker, env, args...)
				if candidates := completionCandidates(got); !reflect.DeepEqual(candidates, tc.want) {
					t.Errorf("docker %s candidates %q, want exactly %q (full output %q)", strings.Join(args, " "), candidates, tc.want, got)
				}
				if !containsLine(got, ":4") {
					t.Errorf("docker %s output %q missing chosen no-file directive :4", strings.Join(args, " "), got)
				}
			}
		})
	}
}

func dockerConfigEnv(config string) []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "DOCKER_CONFIG=") {
			env = append(env, entry)
		}
	}
	return append(env, "DOCKER_CONFIG="+config)
}

func runDocker(t *testing.T, docker string, env []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, docker, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func runDockerFailure(docker string, env []string, args ...string) (string, error) {
	cmd := exec.Command(docker, args...)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func containsLine(output, candidate string) bool {
	for _, line := range strings.Split(output, "\n") {
		if line == candidate || strings.HasPrefix(line, candidate+"\t") {
			return true
		}
	}
	return false
}

func completionCandidates(output string) []string {
	var candidates []string
	for _, line := range strings.Split(output, "\n") {
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "Completion ended with directive:") {
			continue
		}
		candidate, _, _ := strings.Cut(line, "\t")
		candidates = append(candidates, candidate)
	}
	return candidates
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}
