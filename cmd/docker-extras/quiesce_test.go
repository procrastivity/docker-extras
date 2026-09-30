package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestQuiesceSurveysGroupsAndAnnouncesBeforeConfirmation(t *testing.T) {
	log := installFakeDocker(t, `
case "$1" in
  ps)
    case "$*" in
      *"volume=survey-volume"*) printf 'z-api\tbeta\na-web\talpha\na-api\talpha\nplain\t\n' ;;
      *"label=com.docker.compose.project=alpha"*) printf 'web\napi\nweb\n' ;;
      *"label=com.docker.compose.project=beta"*) printf 'worker\n' ;;
      *) exit 91 ;;
    esac
    ;;
  compose)
    printf 'compose stdout %s\n' "$3"
    printf 'compose stderr %s\n' "$3" >&2
    ;;
  stop)
    printf 'stopped\n'
    ;;
  *) exit 92 ;;
esac
`)
	t.Setenv("COMPOSE_FILE", "must-not-reach-down.yml")
	t.Setenv("COMPOSE_PATH_SEPARATOR", ";")
	t.Setenv("DOCKER_CONTEXT", "test-selected-context")
	dockerConfig := filepath.Join(t.TempDir(), "docker-config")
	t.Setenv("DOCKER_CONFIG", dockerConfig)

	report := filepath.Join(t.TempDir(), "report.tsv")
	var stdout, stderr bytes.Buffer
	confirmed := false
	result, err := quiesceVolume(context.Background(), quiesceOptions{
		Volume:         "survey-volume",
		Mode:           quiesceCapture,
		ReportFile:     report,
		Stdout:         &stdout,
		Stderr:         &stderr,
		MountPreflight: testMountPreflightPassed,
		Confirm: func(prompt string) (bool, error) {
			confirmed = true
			if prompt != "capture stops all of the above and starts nothing back up — continue?" {
				t.Errorf("confirmation prompt = %q", prompt)
			}
			for _, line := range []string{
				"compose project 'alpha' will be taken down (services: api,web)",
				"compose project 'beta' will be taken down (services: worker)",
				"container 'plain' will be stopped",
			} {
				if !strings.Contains(stderr.String(), line) {
					t.Errorf("confirmation occurred before announcement %q; stderr=%q", line, stderr.String())
				}
			}
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("quiesceVolume() error = %v", err)
	}
	if !confirmed {
		t.Fatal("capture with running users did not ask for confirmation")
	}
	if len(result.Stopped) != 3 || len(result.Uncertain) != 0 {
		t.Fatalf("result = %#v", result)
	}
	if got, want := string(mustReadFile(t, report)), "down\talpha\tapi,web\ndown\tbeta\tworker\nstop\tplain\n"; got != want {
		t.Fatalf("report = %q, want %q", got, want)
	}
	if !strings.Contains(stdout.String(), "compose stdout alpha") || !strings.Contains(stderr.String(), "compose stderr alpha") {
		t.Fatalf("Compose output did not pass through: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	logData := string(mustReadFile(t, log))
	for _, expected := range []string{
		"call\tcompose\t-p\talpha\tdown\tpwd=/\tcompose_file=unset\tcompose_path_separator=unset\tdocker_context=test-selected-context\tdocker_config=" + dockerConfig,
		"call\tcompose\t-p\tbeta\tdown\tpwd=/\tcompose_file=unset\tcompose_path_separator=unset\tdocker_context=test-selected-context\tdocker_config=" + dockerConfig,
		"call\tstop\t-t\t120\tplain",
	} {
		if !strings.Contains(logData, expected) {
			t.Errorf("fake Docker log does not contain %q:\n%s", expected, logData)
		}
	}
}

func TestQuiesceKeepsPriorSuccessAndMarksLaterFailureUncertain(t *testing.T) {
	installFakeDocker(t, `
case "$1" in
  ps)
    case "$*" in
      *"volume=ordered-volume"*) printf 'a-api\ta-first\nz-worker\tz-second\n' ;;
      *"label=com.docker.compose.project=a-first"*) printf 'api\n' ;;
      *"label=com.docker.compose.project=z-second"*) printf 'worker\n' ;;
      *) exit 91 ;;
    esac
    ;;
  compose)
    printf 'partial-or-complete %s\n' "$3"
    if [ "$3" = z-second ]; then
      printf 'some project users already stopped\n' > "$FAKE_PARTIAL_ACTION"
      exit 17
    fi
    ;;
  *) exit 92 ;;
esac
`)
	partialAction := filepath.Join(t.TempDir(), "partial-action")
	t.Setenv("FAKE_PARTIAL_ACTION", partialAction)
	report := filepath.Join(t.TempDir(), "report.tsv")
	var stdout, stderr bytes.Buffer
	result, err := quiesceVolume(context.Background(), quiesceOptions{
		Volume:         "ordered-volume",
		Mode:           quiesceCapture,
		ReportFile:     report,
		Stdout:         &stdout,
		Stderr:         &stderr,
		MountPreflight: testMountPreflightPassed,
		Confirm:        func(string) (bool, error) { return true, nil },
	})
	var actionErr *quiesceActionError
	if !errors.As(err, &actionErr) || actionErr.Action.Project != "z-second" {
		t.Fatalf("error = %v, want uncertain second project failure", err)
	}
	if len(result.Stopped) != 1 || result.Stopped[0].Project != "a-first" || len(result.Uncertain) != 1 || result.Uncertain[0].Project != "z-second" {
		t.Fatalf("partial result = %#v", result)
	}
	if got, want := string(mustReadFile(t, report)), "down\ta-first\tapi\nuncertain\tdown\tz-second\tworker\n"; got != want {
		t.Fatalf("report = %q, want %q", got, want)
	}
	if got := string(mustReadFile(t, partialAction)); got != "some project users already stopped\n" {
		t.Fatalf("second fake Compose action state = %q, want a simulated partial stop", got)
	}
	if !strings.Contains(stderr.String(), "compose project 'a-first' was taken down") || !strings.Contains(stderr.String(), "State is uncertain") || !strings.Contains(stderr.String(), "will not restart anything") {
		t.Fatalf("recovery/uncertainty output missing: %q", stderr.String())
	}
}

func TestSingleCommandPartialFailureIsUncertain(t *testing.T) {
	for _, tc := range []struct {
		name       string
		volume     string
		rows       string
		service    string
		command    string
		wantRecord string
		wantWarn   string
	}{
		{
			name:       "compose down",
			volume:     "partial-down-volume",
			rows:       "web\tpartial-project\n",
			service:    "web\n",
			command:    `if [ "$1" = compose ]; then printf 'some project users already stopped\n' > "$FAKE_PARTIAL_ACTION"; exit 23; fi; exit 92`,
			wantRecord: "uncertain\tdown\tpartial-project\tweb\n",
			wantWarn:   "project 'partial-project'",
		},
		{
			name:       "direct stop",
			volume:     "partial-stop-volume",
			rows:       "plain\t\n",
			command:    `if [ "$1" = stop ]; then printf 'container was stopped before the error\n' > "$FAKE_PARTIAL_ACTION"; exit 24; fi; exit 92`,
			wantRecord: "uncertain\tstop\tplain\n",
			wantWarn:   "container 'plain'",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			partialAction := filepath.Join(t.TempDir(), "partial-action")
			t.Setenv("FAKE_PARTIAL_ACTION", partialAction)
			body := fmt.Sprintf(`
if [ "$1" = ps ]; then
  case "$*" in
    *"volume=%s"*) printf '%%b' %q ;;
    *"label=com.docker.compose.project=partial-project"*) printf '%%b' %q ;;
    *) exit 91 ;;
  esac
  exit 0
fi
%s
`, tc.volume, tc.rows, tc.service, tc.command)
			installFakeDocker(t, body)
			report := filepath.Join(t.TempDir(), "report.tsv")
			var stderr bytes.Buffer
			result, err := quiesceVolume(context.Background(), quiesceOptions{
				Volume:         tc.volume,
				Mode:           quiesceCapture,
				ReportFile:     report,
				Stderr:         &stderr,
				MountPreflight: testMountPreflightPassed,
				Confirm:        func(string) (bool, error) { return true, nil },
			})
			if err == nil {
				t.Fatal("quiesceVolume() unexpectedly succeeded")
			}
			if len(result.Stopped) != 0 || len(result.Uncertain) != 1 {
				t.Fatalf("result = %#v, want only one uncertain action", result)
			}
			if got := string(mustReadFile(t, report)); got != tc.wantRecord {
				t.Fatalf("report = %q, want %q", got, tc.wantRecord)
			}
			if got := strings.TrimSpace(string(mustReadFile(t, partialAction))); got == "" {
				t.Fatal("fake Docker did not carry out the simulated partial action")
			}
			if !strings.Contains(stderr.String(), "State is uncertain") || !strings.Contains(stderr.String(), tc.wantWarn) {
				t.Fatalf("stderr = %q, want an explicit uncertain-state recovery warning", stderr.String())
			}
		})
	}
}

func TestQuiesceRequiresMountPreflightBeforeDockerSurvey(t *testing.T) {
	for _, tc := range []struct {
		name      string
		preflight func(context.Context) error
		wantError string
	}{
		{
			name:      "missing preflight",
			wantError: "quiesce requires a successful daemon bind-mount preflight",
		},
		{
			name: "failed preflight",
			preflight: func(context.Context) error {
				return errors.New("host/container sentinel mismatch")
			},
			wantError: "daemon bind-mount preflight failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := installFakeDocker(t, `exit 94`)
			confirmCalled := false
			_, err := quiesceVolume(context.Background(), quiesceOptions{
				Volume:         "preflight-volume",
				Mode:           quiesceCapture,
				MountPreflight: tc.preflight,
				Confirm: func(string) (bool, error) {
					confirmCalled = true
					return true, nil
				},
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want error containing %q", err, tc.wantError)
			}
			if confirmCalled {
				t.Fatal("confirmation ran after a rejected mount preflight")
			}
			if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Docker was called before a successful mount preflight: %v", err)
			}
		})
	}
}

func TestCaptureSkipsEmptySurveyPromptButRestoreAlwaysPrompts(t *testing.T) {
	installFakeDocker(t, `
if [ "$1" = ps ]; then exit 0; fi
exit 93
`)

	promptCount := 0
	_, err := quiesceVolume(context.Background(), quiesceOptions{
		Volume:         "empty-volume",
		Mode:           quiesceCapture,
		MountPreflight: testMountPreflightPassed,
		Confirm: func(string) (bool, error) {
			promptCount++
			return true, nil
		},
	})
	if err != nil || promptCount != 0 {
		t.Fatalf("empty capture error=%v promptCount=%d, want no prompt", err, promptCount)
	}

	_, err = quiesceVolume(context.Background(), quiesceOptions{
		Volume:              "empty-volume",
		Mode:                quiesceRestore,
		RestoreConfirmation: "restore will replace volume empty-volume — continue?",
		MountPreflight:      testMountPreflightPassed,
		Confirm: func(prompt string) (bool, error) {
			promptCount++
			if prompt != "restore will replace volume empty-volume — continue?" {
				t.Errorf("restore prompt = %q", prompt)
			}
			return true, nil
		},
	})
	if err != nil || promptCount != 1 {
		t.Fatalf("empty restore error=%v promptCount=%d, want one prompt", err, promptCount)
	}
}

func TestQuiesceDaemonBackedComposeDown(t *testing.T) {
	requireDockerDaemon(t)
	defaultID, err := runDockerCommandOutput(t, "info", "--format", "{{.ID}}")
	if err != nil {
		t.Fatalf("inspect default test daemon ID: %v", err)
	}
	configuredEndpoint := os.Getenv("BDS245_TEST_DOCKER_HOST")
	config, selectedContext := createTestDockerContext(t, configuredEndpoint)
	t.Setenv("DOCKER_CONFIG", config)
	t.Setenv("DOCKER_CONTEXT", selectedContext)
	selectedID, err := runDockerCommandOutput(t, "info", "--format", "{{.ID}}")
	if err != nil {
		t.Fatalf("inspect selected test daemon ID: %v", err)
	}
	if configuredEndpoint != "" && strings.TrimSpace(defaultID) == strings.TrimSpace(selectedID) {
		t.Fatalf("configured alternate Docker endpoint %q resolved to default daemon ID %s", configuredEndpoint, strings.TrimSpace(defaultID))
	}
	if configuredEndpoint == "" && strings.TrimSpace(defaultID) != strings.TrimSpace(selectedID) {
		t.Fatalf("selected test context daemon ID %q differs from default endpoint %q", strings.TrimSpace(selectedID), strings.TrimSpace(defaultID))
	}
	t.Logf("Compose quiesce selected context %q resolved to daemon ID %s (default ID %s; alternate endpoint configured=%t)", selectedContext, strings.TrimSpace(selectedID), strings.TrimSpace(defaultID), configuredEndpoint != "")
	project := uniqueDockerName("bds245compose")
	volume := project + "-seed"
	composeFile := filepath.Join(t.TempDir(), "compose.yaml")
	compose := fmt.Sprintf(`services:
  api:
    image: alpine:3.22
    network_mode: none
    init: true
    command: ["sleep", "3600"]
    volumes: ["seed:/seed"]
  worker:
    image: alpine:3.22
    network_mode: none
    init: true
    command: ["sleep", "3600"]
    volumes: ["seed:/seed"]
volumes:
  seed:
    name: %s
`, volume)
	if err := os.WriteFile(composeFile, []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	if dockerObjectExists(t, "volume", volume) {
		t.Fatalf("unique test volume %q already exists; refusing to touch it", volume)
	}
	projectContainers, err := runDockerCommandOutput(t, "ps", "-aq", "--filter", "label=com.docker.compose.project="+project)
	if err != nil || strings.TrimSpace(projectContainers) != "" {
		t.Fatalf("unique test project %q already has containers or cannot be inspected: %q, %v", project, projectContainers, err)
	}
	t.Cleanup(func() {
		_ = runDockerCommand(t, "compose", "-f", composeFile, "-p", project, "down", "-v")
	})
	if err := runDockerCommand(t, "compose", "-f", composeFile, "-p", project, "up", "-d"); err != nil {
		t.Fatalf("create isolated Compose fixture: %v", err)
	}

	var stdout, stderr bytes.Buffer
	result, err := quiesceVolume(context.Background(), quiesceOptions{
		Volume:         volume,
		Mode:           quiesceCapture,
		Stdout:         &stdout,
		Stderr:         &stderr,
		MountPreflight: testMountPreflightPassed,
		Confirm:        func(string) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatalf("daemon-backed quiesceVolume: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	if len(result.Stopped) != 1 || result.Stopped[0].Kind != "down" || result.Stopped[0].Project != project {
		t.Fatalf("result = %#v, want one Compose down for %q", result, project)
	}
	if got, want := strings.Join(result.Stopped[0].Services, ","), "api,worker"; got != want {
		t.Fatalf("surveyed services = %q, want %q", got, want)
	}
	if !strings.Contains(stdout.String()+stderr.String(), "Stopped") {
		t.Fatalf("Compose down output was not passed through: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !dockerObjectExists(t, "volume", volume) {
		t.Fatalf("Compose down removed named test volume %q", volume)
	}
	if dockerObjectExists(t, "container", project+"-api-1") || dockerObjectExists(t, "container", project+"-worker-1") {
		t.Fatalf("Compose down left test project containers running or present")
	}
}

func TestQuiesceDaemonBackedDirectStopPreservesContainer(t *testing.T) {
	requireDockerDaemon(t)
	defaultID, err := runDockerCommandOutput(t, "info", "--format", "{{.ID}}")
	if err != nil {
		t.Fatalf("inspect default test daemon ID: %v", err)
	}
	configuredEndpoint := os.Getenv("BDS245_TEST_DOCKER_HOST")
	config, selectedContext := createTestDockerContext(t, configuredEndpoint)
	t.Setenv("DOCKER_CONFIG", config)
	t.Setenv("DOCKER_CONTEXT", selectedContext)
	selectedID, err := runDockerCommandOutput(t, "info", "--format", "{{.ID}}")
	if err != nil {
		t.Fatalf("inspect selected test daemon ID: %v", err)
	}
	if configuredEndpoint != "" && strings.TrimSpace(defaultID) == strings.TrimSpace(selectedID) {
		t.Fatalf("configured alternate Docker endpoint %q resolved to default daemon ID %s", configuredEndpoint, strings.TrimSpace(defaultID))
	}
	if configuredEndpoint == "" && strings.TrimSpace(defaultID) != strings.TrimSpace(selectedID) {
		t.Fatalf("selected test context daemon ID %q differs from default endpoint %q", strings.TrimSpace(selectedID), strings.TrimSpace(defaultID))
	}
	t.Logf("direct-stop selected context %q resolved to daemon ID %s (default ID %s; alternate endpoint configured=%t)", selectedContext, strings.TrimSpace(selectedID), strings.TrimSpace(defaultID), configuredEndpoint != "")
	name := uniqueDockerName("bds245direct")
	volume := name + "-seed"
	if dockerObjectExists(t, "container", name) || dockerObjectExists(t, "volume", volume) {
		t.Fatalf("unique direct-stop test names already exist; refusing to touch them")
	}
	if output, err := runDockerCommandOutput(t, "volume", "create", volume); err != nil {
		t.Fatalf("create isolated volume: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		_ = runDockerCommand(t, "rm", "-f", name)
		_ = runDockerCommand(t, "volume", "rm", volume)
	})
	if output, err := runDockerCommandOutput(t, "run", "--init", "-d", "--name", name, "--mount", "type=volume,src="+volume+",dst=/seed", "alpine:3.22", "sleep", "3600"); err != nil {
		t.Fatalf("create isolated standalone container: %v\n%s", err, output)
	}

	var stderr bytes.Buffer
	result, err := quiesceVolume(context.Background(), quiesceOptions{
		Volume:         volume,
		Mode:           quiesceCapture,
		Stderr:         &stderr,
		MountPreflight: testMountPreflightPassed,
		Confirm:        func(string) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatalf("daemon-backed quiesceVolume: %v\nstderr=%s", err, stderr.String())
	}
	if len(result.Stopped) != 1 || result.Stopped[0].Kind != "stop" || result.Stopped[0].Name != name {
		t.Fatalf("result = %#v, want stop for %q", result, name)
	}
	if !dockerObjectExists(t, "container", name) {
		t.Fatalf("docker stop removed container %q", name)
	}
	state, err := runDockerCommandOutput(t, "inspect", "--format", "{{.State.Running}}", name)
	if err != nil || strings.TrimSpace(state) != "false" {
		t.Fatalf("container state after quiesce = %q, err=%v; want preserved and stopped", state, err)
	}
}

func installFakeDocker(t *testing.T, body string) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "docker.log")
	script := `#!/bin/sh
{
  printf 'call'
  for arg in "$@"; do printf '\t%s' "$arg"; done
  printf '\tpwd=%s\tcompose_file=%s\tcompose_path_separator=%s\tdocker_context=%s\tdocker_config=%s\n' \
    "$PWD" "${COMPOSE_FILE-unset}" "${COMPOSE_PATH_SEPARATOR-unset}" "${DOCKER_CONTEXT-unset}" "${DOCKER_CONFIG-unset}"
} >> "$FAKE_DOCKER_LOG"
` + body
	docker := filepath.Join(bin, "docker")
	if err := os.WriteFile(docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_DOCKER_LOG", log)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func testMountPreflightPassed(context.Context) error { return nil }

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func requireDockerDaemon(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Run(); err != nil {
		if os.Getenv("REQUIRE_DOCKER") == "1" {
			t.Fatalf("REQUIRE_DOCKER=1 but no Docker daemon is reachable: %v", err)
		}
		t.Skipf("Docker daemon is unavailable: %v", err)
	}
}

func uniqueDockerName(prefix string) string {
	return fmt.Sprintf("%s-%d-%x", prefix, os.Getpid(), time.Now().UnixNano())
}

func dockerObjectExists(t *testing.T, objectType, name string) bool {
	t.Helper()
	cmd := exec.Command("docker", objectType, "inspect", name)
	return cmd.Run() == nil
}

func runDockerCommand(t *testing.T, args ...string) error {
	t.Helper()
	_, err := runDockerCommandOutput(t, args...)
	return err
}

func runDockerCommandOutput(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = envWithout(os.Environ(), "COMPOSE_FILE", "COMPOSE_PATH_SEPARATOR")
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func TestEnvWithoutPreservesDockerContext(t *testing.T) {
	input := []string{"PATH=/bin", "COMPOSE_FILE=compose.yml", "DOCKER_CONTEXT=remote", "COMPOSE_PATH_SEPARATOR=:"}
	got := envWithout(input, "COMPOSE_FILE", "COMPOSE_PATH_SEPARATOR")
	want := []string{"PATH=/bin", "DOCKER_CONTEXT=remote"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("envWithout() = %#v, want %#v", got, want)
	}
}
