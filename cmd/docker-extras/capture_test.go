package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestCaptureWritesArchiveMetadataAndQuiesceReportOnSelectedCLIContext(t *testing.T) {
	log := installCaptureFakeDocker(t)
	dataDir := t.TempDir()
	report := filepath.Join(t.TempDir(), "report.tsv")
	payload := "archive bytes from the fake daemon\n"
	t.Setenv("FAKE_CAPTURE_NAME", "fake-seed")
	t.Setenv("FAKE_CAPTURE_PAYLOAD", payload)
	t.Setenv("DOCKER_CONTEXT", "selected-context-for-capture-test")
	config := filepath.Join(t.TempDir(), "docker-config")
	t.Setenv("DOCKER_CONFIG", config)
	t.Setenv("COMPOSE_FILE", "must-not-be-used.yml")
	t.Setenv("COMPOSE_PATH_SEPARATOR", ";")

	var stdout, stderr bytes.Buffer
	confirmations := 0
	err := captureVolumeSeed(context.Background(), captureOptions{
		Volume:     "capture-volume",
		Name:       "fake-seed",
		DataDir:    dataDir,
		ReportFile: report,
		Stdout:     &stdout,
		Stderr:     &stderr,
		Confirm: func(prompt string) (bool, error) {
			confirmations++
			if prompt != "capture stops all of the above and starts nothing back up — continue?" {
				t.Errorf("prompt = %q", prompt)
			}
			for _, expected := range []string{
				"compose project 'alpha' will be taken down (services: api)",
				"container 'plain' will be stopped",
			} {
				if !strings.Contains(stderr.String(), expected) {
					t.Errorf("confirmation happened before announcement %q: %s", expected, stderr.String())
				}
			}
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("captureVolumeSeed() error = %v\nstderr=%s", err, stderr.String())
	}
	if confirmations != 1 {
		t.Fatalf("confirmation count = %d, want one for existing users", confirmations)
	}
	if got, want := string(mustReadFile(t, report)), "down\talpha\tapi\nstop\tplain\n"; got != want {
		t.Fatalf("report = %q, want %q", got, want)
	}
	if !strings.Contains(stdout.String(), "compose stdout alpha") || !strings.Contains(stderr.String(), "compose stderr alpha") {
		t.Fatalf("Compose output not passed through: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "compose project 'alpha' was taken down") || !strings.Contains(stderr.String(), "'docker start plain'") {
		t.Fatalf("successful quiesce recovery guidance missing: %q", stderr.String())
	}

	archive := filepath.Join(dataDir, "fake-seed.tar")
	if got := string(mustReadFile(t, archive)); got != payload {
		t.Fatalf("archive bytes = %q, want %q", got, payload)
	}
	metadata := parseSeedMetadata(t, mustReadFile(t, filepath.Join(dataDir, "fake-seed.meta")))
	sum := sha256.Sum256([]byte(payload))
	want := map[string]string{
		"source_volume": "capture-volume",
		"image":         "alpine:3.22",
		"project":       "alpha",
		"bytes":         fmt.Sprint(len(payload)),
		"sha256":        hex.EncodeToString(sum[:]),
	}
	for key, value := range want {
		if metadata[key] != value {
			t.Errorf("metadata %s = %q, want %q; all=%#v", key, metadata[key], value, metadata)
		}
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`).MatchString(metadata["date"]) {
		t.Errorf("metadata date = %q, want UTC second-resolution timestamp", metadata["date"])
	}
	for _, path := range []string{archive + ".tmp", filepath.Join(dataDir, "fake-seed.meta.tmp")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("temporary file %q remains: %v", path, err)
		}
	}
	assertNoBindProbeArtifacts(t, dataDir)

	logContents := string(mustReadFile(t, log))
	lines := strings.Split(strings.TrimSpace(logContents), "\n")
	if len(lines) < 8 {
		t.Fatalf("too few Docker subprocesses recorded: %s", logContents)
	}
	for _, line := range lines {
		if !strings.Contains(line, "docker_context=selected-context-for-capture-test") || !strings.Contains(line, "docker_config="+config) {
			t.Errorf("Docker operation did not inherit selected context/config: %s", line)
		}
	}
	var composeLine string
	for _, line := range lines {
		if strings.Contains(line, "call\tcompose\t-p\talpha\tdown") {
			composeLine = line
			break
		}
	}
	if composeLine == "" || !strings.Contains(composeLine, "pwd=/\tcompose_file=unset\tcompose_path_separator=unset") {
		t.Errorf("Compose down did not run from / with Compose path variables removed: %q", composeLine)
	}
	preflightIndex, composeIndex := strings.Index(logContents, "call\trun\t--rm\t--mount\ttype=bind"), strings.Index(logContents, "call\tcompose\t-p\talpha\tdown")
	if preflightIndex < 0 || composeIndex < 0 || preflightIndex > composeIndex {
		t.Errorf("bind preflight did not precede Compose down:\n%s", logContents)
	}
}

func TestCaptureBindPreflightFailureStopsBeforeSurveyAndCleansProbes(t *testing.T) {
	log := installCaptureFakeDocker(t)
	t.Setenv("FAKE_BIND_FAIL", "1")
	t.Setenv("FAKE_CAPTURE_NAME", "blocked-seed")
	t.Setenv("FAKE_CAPTURE_PAYLOAD", "unused")
	dataDir := t.TempDir()
	report := filepath.Join(t.TempDir(), "report.tsv")
	if err := os.WriteFile(report, []byte("stale report must be cleared"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := Execute([]string{
		"volume", "seed", "capture", "--from-volume", "capture-volume",
		"--name", "blocked-seed", "--data-dir", dataDir, "--report", report,
	}, &stdout, &stderr, "dev", "extras")
	var exitErr *commandExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("capture error = %v, want pre-destructive exit 1", err)
	}
	if !strings.Contains(stderr.String(), "bind source path does not exist") || !strings.Contains(stderr.String(), "no users were surveyed or stopped") {
		t.Fatalf("preflight failure message = %q", stderr.String())
	}
	if got := string(mustReadFile(t, report)); got != "" {
		t.Fatalf("report after failed preflight = %q, want empty", got)
	}
	assertNoBindProbeArtifacts(t, dataDir)
	logContents := string(mustReadFile(t, log))
	if !strings.Contains(logContents, "--mount\ttype=bind") || strings.Contains(logContents, "call\tcompose") || strings.Contains(logContents, "call\tstop") {
		t.Fatalf("preflight must use --mount and fail before actions:\n%s", logContents)
	}
	if strings.Contains(logContents, "call\tps\t--filter\tvolume=capture-volume") {
		t.Fatalf("users were surveyed despite failed preflight:\n%s", logContents)
	}
	if strings.Contains(logContents, "call\trun\t--rm\t-v") {
		t.Fatalf("preflight used -v, which can create a missing source:\n%s", logContents)
	}
}

func TestCaptureBindPreflightRejectsNewlineSentinelMismatchBeforeSurveyOrStop(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       string
		wantDetail string
	}{
		{name: "extra trailing newline", mode: "extra", wantDetail: "extra trailing newline"},
		{name: "missing trailing newline", mode: "missing", wantDetail: "missing trailing newline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := installCaptureFakeDocker(t)
			t.Setenv("FAKE_SENTINEL_NEWLINE_MODE", tc.mode)
			t.Setenv("FAKE_CAPTURE_NAME", "mismatched-seed")
			t.Setenv("FAKE_CAPTURE_PAYLOAD", "unused")
			dataDir := t.TempDir()
			report := filepath.Join(t.TempDir(), "report.tsv")
			if err := os.WriteFile(report, []byte("stale report must be cleared"), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			err := Execute([]string{
				"volume", "seed", "capture", "--from-volume", "capture-volume",
				"--name", "mismatched-seed", "--data-dir", dataDir, "--report", report,
			}, &stdout, &stderr, "dev", "extras")
			var exitErr *commandExitError
			if !errors.As(err, &exitErr) || exitErr.Code != 1 {
				t.Fatalf("capture error = %v, want pre-destructive exit 1", err)
			}
			if !strings.Contains(stderr.String(), tc.wantDetail) || !strings.Contains(stderr.String(), "no users were surveyed or stopped") {
				t.Fatalf("byte-mismatch preflight output = %q", stderr.String())
			}
			if got := string(mustReadFile(t, report)); got != "" {
				t.Fatalf("report after byte-mismatch preflight = %q, want empty", got)
			}
			assertNoBindProbeArtifacts(t, dataDir)
			logContents := string(mustReadFile(t, log))
			if !strings.Contains(logContents, "cmp -s - /seed/.docker-extras-sentinel-") {
				t.Fatalf("preflight did not use a byte-exact comparison:\n%s", logContents)
			}
			if strings.Contains(logContents, "call\tps\t--filter\tvolume=capture-volume") || strings.Contains(logContents, "call\tcompose") || strings.Contains(logContents, "call\tstop") {
				t.Fatalf("byte-mismatched sentinel must fail before survey/stop:\n%s", logContents)
			}
		})
	}
}

func TestCaptureImageOverrideAndNoUsersSkipInferenceAndPrompt(t *testing.T) {
	log := installCaptureFakeDocker(t)
	t.Setenv("FAKE_CAPTURE_NO_USERS", "1")
	t.Setenv("FAKE_CAPTURE_NAME", "override-seed")
	t.Setenv("FAKE_CAPTURE_PAYLOAD", "override archive")
	dataDir := t.TempDir()
	confirmations := 0
	err := captureVolumeSeed(context.Background(), captureOptions{
		Volume:  "capture-volume",
		Name:    "override-seed",
		DataDir: dataDir,
		Image:   "example/database:9.1",
		Stderr:  io.Discard,
		Confirm: func(string) (bool, error) {
			confirmations++
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("captureVolumeSeed() error = %v", err)
	}
	if confirmations != 0 {
		t.Fatalf("confirmation count = %d, want no prompt without running users", confirmations)
	}
	metadata := parseSeedMetadata(t, mustReadFile(t, filepath.Join(dataDir, "override-seed.meta")))
	if metadata["image"] != "example/database:9.1" {
		t.Fatalf("metadata image = %q, want explicit override", metadata["image"])
	}
	logContents := string(mustReadFile(t, log))
	if strings.Contains(logContents, "call\tps\t-a\t--filter\tvolume=capture-volume") {
		t.Fatalf("explicit image override still inferred attached images:\n%s", logContents)
	}
	if strings.Contains(logContents, "call\tcompose") || strings.Contains(logContents, "call\tstop") {
		t.Fatalf("empty user survey unexpectedly stopped users:\n%s", logContents)
	}
}

func TestCaptureReportsUncertainPartialDownAndPreservesPriorSuccess(t *testing.T) {
	log := installCaptureFakeDocker(t)
	t.Setenv("FAKE_CAPTURE_TWO_PROJECTS", "1")
	t.Setenv("FAKE_SECOND_DOWN_FAIL", "1")
	t.Setenv("FAKE_CAPTURE_NAME", "uncertain-seed")
	t.Setenv("FAKE_CAPTURE_PAYLOAD", "unused")
	dataDir := t.TempDir()
	report := filepath.Join(t.TempDir(), "report.tsv")
	var stderr bytes.Buffer
	err := captureVolumeSeed(context.Background(), captureOptions{
		Volume:     "capture-volume",
		Name:       "uncertain-seed",
		DataDir:    dataDir,
		ReportFile: report,
		Stderr:     &stderr,
		Confirm:    func(string) (bool, error) { return true, nil },
	})
	var actionErr *quiesceActionError
	if !errors.As(err, &actionErr) || actionErr.Action.Project != "zeta" {
		t.Fatalf("capture error = %v, want uncertain second Compose action", err)
	}
	if got, want := string(mustReadFile(t, report)), "down\talpha\tapi\nuncertain\tdown\tzeta\tworker\n"; got != want {
		t.Fatalf("partial report = %q, want %q", got, want)
	}
	if !strings.Contains(stderr.String(), "compose project 'alpha' was taken down") || !strings.Contains(stderr.String(), "State is uncertain") || !strings.Contains(stderr.String(), "will not restart anything") {
		t.Fatalf("partial recovery/uncertainty guidance = %q", stderr.String())
	}
	if strings.Contains(string(mustReadFile(t, log)), "call\trun\t--rm\t--mount\ttype=volume") {
		t.Fatal("archive ran after a failed quiesce action")
	}
	for _, path := range []string{filepath.Join(dataDir, "uncertain-seed.tar.tmp"), filepath.Join(dataDir, "uncertain-seed.meta.tmp")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("temporary file %q remains after failed quiesce: %v", path, err)
		}
	}
}

func TestCaptureArchiveFailureKeepsRecoveryGuidanceAndCleansTemps(t *testing.T) {
	log := installCaptureFakeDocker(t)
	t.Setenv("FAKE_CAPTURE_ARCHIVE_FAIL", "1")
	t.Setenv("FAKE_CAPTURE_NAME", "archive-failure")
	t.Setenv("FAKE_CAPTURE_PAYLOAD", "unused")
	dataDir := t.TempDir()
	report := filepath.Join(t.TempDir(), "report.tsv")
	var stderr bytes.Buffer
	err := captureVolumeSeed(context.Background(), captureOptions{
		Volume:     "capture-volume",
		Name:       "archive-failure",
		DataDir:    dataDir,
		ReportFile: report,
		Yes:        true,
		Stderr:     &stderr,
	})
	if err == nil || !strings.Contains(err.Error(), "volume archive (tar) failed") {
		t.Fatalf("capture error = %v, want archive failure", err)
	}
	if got, want := string(mustReadFile(t, report)), "down\talpha\tapi\nstop\tplain\n"; got != want {
		t.Fatalf("report = %q, want completed stop actions %q", got, want)
	}
	if !strings.Contains(stderr.String(), "compose project 'alpha' was taken down") || !strings.Contains(stderr.String(), "'docker start plain'") {
		t.Fatalf("stopped-user recovery guidance missing after archive failure: %q", stderr.String())
	}
	for _, path := range []string{filepath.Join(dataDir, "archive-failure.tar.tmp"), filepath.Join(dataDir, "archive-failure.meta.tmp")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("temporary file %q remains: %v", path, err)
		}
	}
	if strings.Contains(string(mustReadFile(t, log)), "call\tcompose\t-p\talpha\tdown\tpwd=/\tcompose_file=unset") == false {
		t.Fatal("archive failure fixture did not execute Compose down")
	}
}

func TestCaptureDaemonBackedSelectedContextBindProbeComposeDownAndArchive(t *testing.T) {
	requireDockerDaemon(t)
	defaultID, err := runDockerCommandOutput(t, "info", "--format", "{{.ID}}")
	if err != nil {
		t.Fatalf("inspect default test daemon ID: %v", err)
	}
	configuredEndpoint := os.Getenv("BDS245_TEST_DOCKER_HOST")
	config, selectedContext := createTestDockerContext(t, configuredEndpoint)
	t.Setenv("DOCKER_CONFIG", config)
	t.Setenv("DOCKER_CONTEXT", selectedContext)
	pluginName := "seedstage"
	pluginBinary := filepath.Join(t.TempDir(), "docker-"+pluginName)
	buildPlugin := exec.Command("go", "build", "-o", pluginBinary, ".")
	if output, err := buildPlugin.CombinedOutput(); err != nil {
		t.Fatalf("build Docker CLI plugin: %v\n%s", err, output)
	}
	pluginDir := filepath.Join(config, "cli-plugins")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(pluginBinary, filepath.Join(pluginDir, "docker-"+pluginName)); err != nil {
		t.Fatalf("install disposable plugin: %v", err)
	}

	project := uniqueDockerName("bds245capture")
	volume := project + "-data"
	container := project + "-db-1"
	dataDir := t.TempDir()
	report := filepath.Join(t.TempDir(), "report.tsv")
	composeFile := filepath.Join(t.TempDir(), "compose.yaml")
	compose := fmt.Sprintf(`services:
  db:
    image: alpine:3.22
    network_mode: none
    init: true
    command: ["sleep", "3600"]
    volumes: ["seed:/var/lib/example"]
volumes:
  seed:
    name: %s
`, volume)
	if err := os.WriteFile(composeFile, []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	if dockerObjectExists(t, "volume", volume) || dockerObjectExists(t, "container", container) {
		t.Fatalf("unique capture fixture already exists; refusing to touch it: project=%q volume=%q", project, volume)
	}
	projectContainers, err := runDockerCommandOutput(t, "ps", "-aq", "--filter", "label=com.docker.compose.project="+project)
	if err != nil || strings.TrimSpace(projectContainers) != "" {
		t.Fatalf("unique project %q is not empty or cannot be inspected: %q, %v", project, projectContainers, err)
	}
	t.Cleanup(func() {
		_ = runDockerCommand(t, "compose", "-f", composeFile, "-p", project, "down", "-v", "--remove-orphans")
		_ = runDockerCommand(t, "volume", "rm", volume)
	})
	if err := runDockerCommand(t, "compose", "-f", composeFile, "-p", project, "up", "-d"); err != nil {
		t.Fatalf("create isolated Compose fixture: %v", err)
	}
	fixture := []byte("payload from the selected daemon\n")
	if output, err := runDockerCommandOutput(t, "run", "--rm",
		"--mount", "type=volume,src="+volume+",dst=/seed",
		"alpine:3.22", "sh", "-c", "printf 'payload from the selected daemon\\n' > /seed/proof.txt"); err != nil {
		t.Fatalf("write disposable volume fixture: %v\n%s", err, output)
	}

	selectedID, err := runDockerCommandOutput(t, "info", "--format", "{{.ID}}")
	if err != nil {
		t.Fatalf("inspect selected test daemon ID: %v", err)
	}
	if configuredEndpoint != "" && strings.TrimSpace(defaultID) == strings.TrimSpace(selectedID) {
		t.Fatalf("explicit alternate endpoint %q resolved to the default daemon ID %s", configuredEndpoint, strings.TrimSpace(defaultID))
	}
	if configuredEndpoint == "" && strings.TrimSpace(defaultID) != strings.TrimSpace(selectedID) {
		t.Fatalf("test context daemon ID %q differs from its configured default endpoint %q", selectedID, defaultID)
	}
	t.Logf("selected context %q resolved to daemon ID %s (default context daemon ID %s; alternate endpoint configured=%t)", selectedContext, strings.TrimSpace(selectedID), strings.TrimSpace(defaultID), configuredEndpoint != "")

	missingSource := filepath.Join(t.TempDir(), "absent-daemon-bind-source")
	if _, err := os.Stat(missingSource); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing-source fixture unexpectedly exists: %v", err)
	}
	output, err := runDockerCommandOutput(t, "run", "--rm",
		"--mount", "type=bind,src="+missingSource+",dst=/seed",
		"alpine:3.22", "true")
	if err == nil || !strings.Contains(strings.ToLower(output), "bind source path does not exist") {
		t.Fatalf("--mount absent source result output=%q err=%v, want the engine's missing-source refusal", output, err)
	}
	t.Logf("absent --mount source rejected: %s", strings.TrimSpace(output))
	if _, err := os.Stat(missingSource); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("--mount unexpectedly auto-created absent source %q: %v", missingSource, err)
	}

	output, err = runDockerCommandOutput(t,
		pluginName, "volume", "seed", "capture",
		"--from-volume", volume,
		"--name", "daemon-seed",
		"--data-dir", dataDir,
		"--report", report,
		"--yes",
	)
	if err != nil {
		t.Fatalf("docker %s volume seed capture: %v\n%s", pluginName, err, output)
	}
	if got, want := string(mustReadFile(t, report)), "down\t"+project+"\tdb\n"; got != want {
		t.Fatalf("report = %q, want %q", got, want)
	}
	if !strings.Contains(output, "Stopped") || !strings.Contains(output, "archiving volume "+volume) {
		t.Fatalf("Docker plugin did not pass through Compose/archive output: %s", output)
	}
	if !dockerObjectExists(t, "volume", volume) {
		t.Fatalf("capture's Compose down removed volume %q", volume)
	}
	if dockerObjectExists(t, "container", container) {
		t.Fatalf("capture's Compose down left container %q", container)
	}

	archivePath := filepath.Join(dataDir, "daemon-seed.tar")
	archiveBytes := mustReadFile(t, archivePath)
	reader := tar.NewReader(bytes.NewReader(archiveBytes))
	foundPayload := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read captured tar: %v", err)
		}
		if header.Name == "./proof.txt" {
			contents, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(contents, fixture) {
				t.Fatalf("captured file contents = %q, want %q", contents, fixture)
			}
			foundPayload = true
		}
	}
	if !foundPayload {
		t.Fatal("captured archive did not contain ./proof.txt")
	}
	metadata := parseSeedMetadata(t, mustReadFile(t, filepath.Join(dataDir, "daemon-seed.meta")))
	sum := sha256.Sum256(archiveBytes)
	if metadata["source_volume"] != volume || metadata["image"] != "alpine:3.22" || metadata["project"] != project {
		t.Fatalf("captured provenance = %#v, want volume/image/project %q/alpine:3.22/%q", metadata, volume, project)
	}
	if metadata["bytes"] != fmt.Sprint(len(archiveBytes)) || metadata["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("integrity metadata = bytes:%q sha256:%q, want bytes:%d sha256:%x", metadata["bytes"], metadata["sha256"], len(archiveBytes), sum)
	}
	if _, err := time.Parse("2006-01-02T15:04:05Z", metadata["date"]); err != nil {
		t.Fatalf("metadata date %q is not UTC RFC3339 seconds: %v", metadata["date"], err)
	}
	for _, path := range []string{archivePath + ".tmp", filepath.Join(dataDir, "daemon-seed.meta.tmp")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("temporary file %q remains: %v", path, err)
		}
	}
	assertNoBindProbeArtifacts(t, dataDir)

	// The selected context is a disposable named alias of this orb's local
	// daemon. It exercises DOCKER_CONTEXT/DOCKER_CONFIG propagation, but is not
	// evidence of a distinct engine or a physically remote bind mount.
	if selectedContext == "" || strings.TrimSpace(selectedID) == "" || config == "" {
		t.Fatal("selected-context fixture did not record its identity")
	}
}

func installCaptureFakeDocker(t *testing.T) string {
	t.Helper()
	return installFakeDocker(t, `
case "$1" in
  version)
    printf '29.0.0\n'
    ;;
  volume)
    if [ "$2" = inspect ]; then
      case "$*" in
        *'{{json .Labels}}'*) printf '{"com.docker.compose.project":"alpha"}\n' ;;
        *) printf '{}\n' ;;
      esac
    else
      exit 91
    fi
    ;;
  ps)
    case "$*" in
      *'{{.Image}}'*) printf 'alpine:3.22\n' ;;
      *'label=com.docker.compose.project=alpha'*) printf 'api\n' ;;
	      *'label=com.docker.compose.project=zeta'*) printf 'worker\n' ;;
      *'{{.Names}}'*)
        if [ "${FAKE_CAPTURE_NO_USERS-unset}" = 1 ]; then
          exit 0
        elif [ "${FAKE_CAPTURE_TWO_PROJECTS-unset}" = 1 ]; then
          printf 'a-api\talpha\nz-worker\tzeta\nplain\t\n'
        else
          printf 'api\talpha\nplain\t\n'
        fi
        ;;
      *'volume=capture-volume'*) printf 'alpha\n' ;;
      *) exit 92 ;;
    esac
    ;;
  compose)
    printf 'compose stdout %s\n' "$3"
    printf 'compose stderr %s\n' "$3" >&2
    if [ "$3" = zeta ] && [ "${FAKE_SECOND_DOWN_FAIL-unset}" = 1 ]; then
      printf 'partial action already occurred\n' >&2
      exit 17
    fi
    ;;
  stop)
    printf 'stopped\n'
    ;;
  run)
    case "$*" in
      *'docker-extras-sentinel-'*)
        if [ "${FAKE_BIND_FAIL-unset}" = 1 ]; then
          printf 'invalid mount config for type bind: bind source path does not exist\n' >&2
          exit 125
        fi
        source=${4#*src=}
        source=${source%%,*}
        for sentinel in "$source"/.docker-extras-sentinel-*; do
          [ -f "$sentinel" ] || exit 93
          nonce=${sentinel##*-}
          newline_mode=${FAKE_SENTINEL_NEWLINE_MODE-unset}
          case "$newline_mode" in
            extra)
              printf 'host:%s\n\n' "$nonce" > "$sentinel"
              detail='extra trailing newline'
              ;;
            missing)
              printf 'host:%s' "$nonce" > "$sentinel"
              detail='missing trailing newline'
              ;;
          esac
          if [ "$newline_mode" = extra ] || [ "$newline_mode" = missing ]; then
            # The old command-substitution check accepts either byte-different
            # file because it strips trailing newlines. The exact comparison
            # must reject the daemon's variant before writing the probe.
            [ "$(cat "$sentinel")" = "host:$nonce" ] || exit 96
            case "$8" in
              *'cmp -s - /seed/'*)
                if printf 'host:%s\n' "$nonce" | cmp -s - "$sentinel"; then
                  printf 'exact comparison unexpectedly accepted differing sentinel\n' >&2
                  exit 97
                fi
                printf 'sentinel bytes differ: %s\n' "$detail" >&2
                exit 125
                ;;
              *) ;;
            esac
          fi
          printf 'container:%s\n' "$nonce" > "$source/.docker-extras-probe-$nonce"
        done
        ;;
      *'tar -C /source'*)
        if [ "${FAKE_CAPTURE_ARCHIVE_FAIL-unset}" = 1 ]; then
          printf 'archive container failed\n' >&2
          exit 125
        fi
        source=${6#*src=}
        source=${source%%,*}
        printf '%s' "$FAKE_CAPTURE_PAYLOAD" > "$source/$FAKE_CAPTURE_NAME.tar.tmp"
        sha256sum "$source/$FAKE_CAPTURE_NAME.tar.tmp"
        ;;
      *) exit 94 ;;
    esac
    ;;
  *) exit 95 ;;
esac
`)
}

func parseSeedMetadata(t *testing.T, contents []byte) map[string]string {
	t.Helper()
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed metadata line %q", line)
		}
		values[key] = value
	}
	return values
}

func assertNoBindProbeArtifacts(t *testing.T, dataDir string) {
	t.Helper()
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".docker-extras-sentinel-") || strings.HasPrefix(entry.Name(), ".docker-extras-probe-") {
			t.Errorf("bind probe artifact remains: %s", entry.Name())
		}
	}
}

func createTestDockerContext(t *testing.T, endpoint string) (string, string) {
	t.Helper()
	config := t.TempDir()
	name := uniqueDockerName("bds245-selected")
	if endpoint == "" {
		active, err := runDockerCommandOutput(t, "context", "show")
		if err != nil {
			t.Fatalf("read current Docker context: %v", err)
		}
		endpointOutput, err := runDockerCommandOutput(t, "context", "inspect", "--format", `{{(index .Endpoints "docker").Host}}`, strings.TrimSpace(active))
		if err != nil || strings.TrimSpace(endpointOutput) == "" {
			t.Fatalf("read Docker context endpoint: output=%q err=%v", endpointOutput, err)
		}
		endpoint = strings.TrimSpace(endpointOutput)
	}
	// Use a clean config so the selected context is genuinely non-default in
	// Docker's namespace. Tests may supply a separate daemon endpoint to prove
	// selected-context alignment; absent that, this is a same-engine alias.
	env := envWithout(os.Environ(), "DOCKER_CONFIG", "DOCKER_CONTEXT", "DOCKER_HOST")
	env = append(env, "DOCKER_CONFIG="+config)
	cmd := exec.Command("docker", "context", "create", name, "--docker", "host="+endpoint)
	cmd.Env = env
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create disposable selected Docker context: %v\n%s", err, output)
	}
	return config, name
}

func TestCaptureDeclineMapsToExitCode3(t *testing.T) {
	if tty, err := os.Open("/dev/tty"); err == nil {
		_ = tty.Close()
		t.Skip("test requires no controlling terminal to exercise the CLI decline path")
	}
	log := installCaptureFakeDocker(t)
	t.Setenv("FAKE_CAPTURE_NAME", "declined")
	t.Setenv("FAKE_CAPTURE_PAYLOAD", "unused")
	var stdout, stderr bytes.Buffer
	err := Execute([]string{
		"volume", "seed", "capture", "--from-volume", "capture-volume",
		"--name", "declined", "--data-dir", t.TempDir(),
	}, &stdout, &stderr, "dev", "extras")
	if got := commandExitCode(err); got != 3 || !errors.Is(err, errQuiesceDeclined) {
		t.Fatalf("declined command error=%v exit=%d, want errQuiesceDeclined/3", err, got)
	}
	logContents := string(mustReadFile(t, log))
	if strings.Contains(logContents, "call\tcompose") || strings.Contains(logContents, "call\tstop") || strings.Contains(logContents, "call\trun\t--rm\t--mount\ttype=volume") {
		t.Fatalf("decline must not stop users or archive:\n%s", logContents)
	}
}
