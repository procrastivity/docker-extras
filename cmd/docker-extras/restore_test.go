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
	"strings"
	"testing"
)

func TestRestoreIntegrityRefusalsPrecedeSurveyAndStop(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func([]byte) ([]byte, string)
		meta       func([]byte) string
		wantError  string
		wantNoHash bool
	}{
		{
			name: "block-aligned truncation",
			mutate: func(data []byte) ([]byte, string) {
				if len(data) < 1024 || len(data)%512 != 0 {
					t.Fatalf("test tar size=%d is not a multi-block tar", len(data))
				}
				return data[:len(data)-512], metadataForSeed(data)
			},
			wantError:  "seed is truncated or damaged",
			wantNoHash: true,
		},
		{
			name: "same-size bit flip",
			mutate: func(data []byte) ([]byte, string) {
				changed := append([]byte(nil), data...)
				changed[700] ^= 0x01
				return changed, metadataForSeed(data)
			},
			wantError: "seed checksum does not match",
		},
		{
			name: "malformed bytes",
			mutate: func(data []byte) ([]byte, string) {
				return data, "image=alpine:3.22\nbytes=12x\nsha256=" + seedDigest(data) + "\n"
			},
			wantError:  "malformed bytes value",
			wantNoHash: true,
		},
		{
			name: "malformed sha256",
			mutate: func(data []byte) ([]byte, string) {
				return data, fmt.Sprintf("image=alpine:3.22\nbytes=%d\nsha256=not-a-digest\n", len(data))
			},
			wantError:  "malformed sha256 value",
			wantNoHash: true,
		},
		{
			name: "bytes without sha256",
			mutate: func(data []byte) ([]byte, string) {
				return data, fmt.Sprintf("image=alpine:3.22\nbytes=%d\n", len(data))
			},
			wantError:  "incomplete seed integrity metadata",
			wantNoHash: true,
		},
		{
			name: "sha256 without bytes",
			mutate: func(data []byte) ([]byte, string) {
				return data, "image=alpine:3.22\nsha256=" + seedDigest(data) + "\n"
			},
			wantError:  "incomplete seed integrity metadata",
			wantNoHash: true,
		},
		{
			name: "integrity keys without values",
			mutate: func(data []byte) ([]byte, string) {
				return data, "image=alpine:3.22\nbytes\nsha256\n"
			},
			wantError:  "malformed bytes value",
			wantNoHash: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, state, _ := installRestoreFakeDocker(t)
			original := createRestoreTar(t, "seed payload")
			archive, explicitMeta := tc.mutate(original)
			dataDir := t.TempDir()
			writeRestoreSeed(t, dataDir, "seed", archive, explicitMeta, tc.meta)
			report := filepath.Join(t.TempDir(), "report.tsv")
			if err := os.WriteFile(report, []byte("stale"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FAKE_RUNNING_USERS", "plain\t\n")
			var stderr bytes.Buffer
			err := restoreVolumeSeed(context.Background(), restoreTestOptions(dataDir, report, &stderr))
			if err == nil || !strings.Contains(err.Error(), tc.wantError) || restoreExitCode(err) != 1 {
				t.Fatalf("restore error=%v exit=%d; want pre-destructive refusal containing %q", err, restoreExitCode(err), tc.wantError)
			}
			if got := string(mustReadFile(t, report)); got != "" {
				t.Fatalf("refusal report=%q, want empty", got)
			}
			assertRestoreNoSurveyOrStops(t, string(mustReadFile(t, log)))
			if tc.wantNoHash && strings.Contains(string(mustReadFile(t, log)), "sha256sum") {
				t.Fatal("invalid metadata or byte-count mismatch reached checksum command")
			}
			if got := string(mustReadFile(t, state)); got != "present\n" {
				t.Fatalf("target state=%q, want unchanged present volume", got)
			}
		})
	}
}

func TestRestoreLegacyPairAbsenceUsesTarListingOnly(t *testing.T) {
	t.Run("valid tar fallback", func(t *testing.T) {
		log, _, _ := installRestoreFakeDocker(t)
		dataDir := t.TempDir()
		archive := createRestoreTar(t, "legacy payload")
		writeRestoreSeed(t, dataDir, "seed", archive, "image=alpine:3.22\n", nil)
		options := restoreTestOptions(dataDir, "", io.Discard)
		options.Yes = true
		if err := restoreVolumeSeed(context.Background(), options); err != nil {
			t.Fatalf("legacy restore error: %v", err)
		}
		calls := string(mustReadFile(t, log))
		if !strings.Contains(calls, "tar\t-tf\t/seed/seed.tar") || strings.Contains(calls, "sha256sum") {
			t.Fatalf("legacy integrity path should run tar -tf only:\n%s", calls)
		}
	})

	t.Run("invalid legacy tar stops before survey", func(t *testing.T) {
		log, _, _ := installRestoreFakeDocker(t)
		dataDir := t.TempDir()
		writeRestoreSeed(t, dataDir, "seed", []byte("not a tar"), "image=alpine:3.22\n", nil)
		options := restoreTestOptions(dataDir, "", io.Discard)
		options.Yes = true
		err := restoreVolumeSeed(context.Background(), options)
		if err == nil || !strings.Contains(err.Error(), "tar -tf failed") || restoreExitCode(err) != 1 {
			t.Fatalf("legacy tar error=%v, want exit 1 header refusal", err)
		}
		assertRestoreNoSurveyOrStops(t, string(mustReadFile(t, log)))
	})
}

func TestRestoreNoVerifyStillEnforcesImageLockAndForceIsNarrow(t *testing.T) {
	t.Run("no verify does not skip image mismatch", func(t *testing.T) {
		log, _, _ := installRestoreFakeDocker(t)
		t.Setenv("FAKE_TARGET_IMAGE", "mysql:9")
		dataDir := t.TempDir()
		writeRestoreSeed(t, dataDir, "seed", []byte("unchecked data"), metadataForSeed([]byte("unchecked data")), nil)
		options := restoreTestOptions(dataDir, "", io.Discard)
		options.NoVerify = true
		options.Force = false
		err := restoreVolumeSeed(context.Background(), options)
		if err == nil || !strings.Contains(err.Error(), "seed was captured under image") || restoreExitCode(err) != 1 {
			t.Fatalf("--no-verify image mismatch error=%v, want refusal exit 1", err)
		}
		assertRestoreNoSurveyOrStops(t, string(mustReadFile(t, log)))
	})

	t.Run("force bypasses image lock only", func(t *testing.T) {
		log, _, _ := installRestoreFakeDocker(t)
		t.Setenv("FAKE_TARGET_IMAGE", "mysql:9")
		dataDir := t.TempDir()
		writeRestoreSeed(t, dataDir, "seed", []byte("unchecked data"), metadataForSeed([]byte("unchecked data")), nil)
		options := restoreTestOptions(dataDir, "", io.Discard)
		options.NoVerify = true
		options.Force = true
		options.Yes = true
		var stderr bytes.Buffer
		options.Stderr = &stderr
		if err := restoreVolumeSeed(context.Background(), options); err != nil {
			t.Fatalf("forced image mismatch restore: %v", err)
		}
		if !strings.Contains(stderr.String(), "continuing per --force") || !strings.Contains(string(mustReadFile(t, log)), "cmp -s - /seed/") {
			t.Fatalf("force warning or required bind preflight missing: stderr=%q\nlog=%s", stderr.String(), mustReadFile(t, log))
		}
	})

	t.Run("force cannot bypass malformed metadata", func(t *testing.T) {
		log, _, _ := installRestoreFakeDocker(t)
		dataDir := t.TempDir()
		writeRestoreSeed(t, dataDir, "seed", []byte("unchecked data"), "image=alpine:3.22\nbytes=bad\nsha256=bad\n", nil)
		options := restoreTestOptions(dataDir, "", io.Discard)
		options.NoVerify = true
		options.Force = true
		err := restoreVolumeSeed(context.Background(), options)
		if err == nil || !strings.Contains(err.Error(), "malformed bytes") {
			t.Fatalf("--force bypassed malformed metadata: %v", err)
		}
		if strings.Contains(string(mustReadFile(t, log)), "call\tps\t--filter") {
			t.Fatal("malformed metadata reached target-user survey")
		}
	})
}

func TestRestoreUnknownImageLockRequiresExplicitOverride(t *testing.T) {
	t.Run("unknown target image refuses without changing target", func(t *testing.T) {
		log, state, target := installRestoreFakeDocker(t)
		t.Setenv("FAKE_TARGET_IMAGE", "")
		t.Setenv("FAKE_RUNNING_USERS", "plain\t\n")
		dataDir := t.TempDir()
		writeValidRestoreSeed(t, dataDir)
		options := restoreTestOptions(dataDir, "", io.Discard)
		options.NoVerify = true
		err := restoreVolumeSeed(context.Background(), options)
		if err == nil || restoreExitCode(err) != 1 || !strings.Contains(err.Error(), "cannot determine which image") {
			t.Fatalf("unknown target image error=%v exit=%d, want image-lock refusal", err, restoreExitCode(err))
		}
		assertRestoreNoSurveyOrStops(t, string(mustReadFile(t, log)))
		if got := string(mustReadFile(t, state)); got != "present\n" {
			t.Fatalf("target volume state=%q, want present", got)
		}
		if got := string(mustReadFile(t, target)); got != "old\n" {
			t.Fatalf("target contents=%q, want unchanged old contents", got)
		}
	})

	t.Run("unknown seed image refuses without changing target", func(t *testing.T) {
		log, state, target := installRestoreFakeDocker(t)
		dataDir := t.TempDir()
		archive := createRestoreTar(t, "seed with unknown image")
		metadata := fmt.Sprintf("source_volume=source\nbytes=%d\nsha256=%s\n", len(archive), seedDigest(archive))
		writeRestoreSeed(t, dataDir, "seed", archive, metadata, nil)
		options := restoreTestOptions(dataDir, "", io.Discard)
		options.NoVerify = true
		err := restoreVolumeSeed(context.Background(), options)
		if err == nil || restoreExitCode(err) != 1 || !strings.Contains(err.Error(), "seed records no image") {
			t.Fatalf("unknown seed image error=%v exit=%d, want image-lock refusal", err, restoreExitCode(err))
		}
		assertRestoreNoSurveyOrStops(t, string(mustReadFile(t, log)))
		if got := string(mustReadFile(t, state)); got != "present\n" {
			t.Fatalf("target volume state=%q, want present", got)
		}
		if got := string(mustReadFile(t, target)); got != "old\n" {
			t.Fatalf("target contents=%q, want unchanged old contents", got)
		}
	})

	t.Run("expect-image resolves unknown target", func(t *testing.T) {
		log, _, target := installRestoreFakeDocker(t)
		t.Setenv("FAKE_TARGET_IMAGE", "")
		dataDir := t.TempDir()
		writeValidRestoreSeed(t, dataDir)
		options := restoreTestOptions(dataDir, "", io.Discard)
		options.ExpectImage = "alpine:3.22"
		options.Yes = true
		if err := restoreVolumeSeed(context.Background(), options); err != nil {
			t.Fatalf("restore with explicit --expect-image: %v", err)
		}
		if strings.Contains(string(mustReadFile(t, log)), "{{.Image}}") {
			t.Fatal("restore ignored --expect-image and attempted implicit image inference")
		}
		if got := string(mustReadFile(t, target)); got != "restored\n" {
			t.Fatalf("target contents=%q, want restored", got)
		}
	})
}

func TestRestoreNoVerifyRefusesArchiveMissingOnSelectedDaemonBeforeSurvey(t *testing.T) {
	log, _, _ := installRestoreFakeDocker(t)
	t.Setenv("FAKE_REMOTE_ARCHIVE_MISSING", "true")
	t.Setenv("FAKE_RUNNING_USERS", "plain\t\n")
	dataDir := t.TempDir()
	writeValidRestoreSeed(t, dataDir)
	report := filepath.Join(t.TempDir(), "report.tsv")
	options := restoreTestOptions(dataDir, report, io.Discard)
	options.NoVerify = true
	options.Confirm = func(string) (bool, error) { return true, nil }
	err := restoreVolumeSeed(context.Background(), options)
	if err == nil || restoreExitCode(err) != 1 || !strings.Contains(err.Error(), "cannot read the seed archive") {
		t.Fatalf("remote-style missing archive error=%v exit=%d, want pre-stop exit 1", err, restoreExitCode(err))
	}
	if got := string(mustReadFile(t, report)); got != "" {
		t.Fatalf("missing selected-daemon archive report=%q, want empty", got)
	}
	calls := string(mustReadFile(t, log))
	if !strings.Contains(calls, "test -f '/seed/seed.tar'") || restoreSurveyWasCalled(calls) || strings.Contains(calls, "call\tstop") || restoreContainerWasCalled(calls) {
		t.Fatalf("missing selected-daemon archive was not rejected before survey/stop:\n%s", calls)
	}
	assertNoBindProbeArtifacts(t, dataDir)
}

func TestRestoreDeclineIsUnconditionalAndDoesNotCreateTarget(t *testing.T) {
	log, state, _ := installRestoreFakeDocker(t)
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	writeValidRestoreSeed(t, dataDir)
	report := filepath.Join(t.TempDir(), "report.tsv")
	if err := os.WriteFile(report, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := restoreTestOptions(dataDir, report, io.Discard)
	options.AllowCreate = true
	options.ExpectImage = "alpine:3.22"
	confirmed := 0
	options.Confirm = func(prompt string) (bool, error) {
		confirmed++
		if !strings.Contains(prompt, "REPLACES all contents of volume 'restore-volume'") {
			t.Errorf("restore prompt = %q", prompt)
		}
		return false, nil
	}
	err := restoreVolumeSeed(context.Background(), options)
	if err == nil || restoreExitCode(err) != 3 || !errors.Is(err, errQuiesceDeclined) || confirmed != 1 {
		t.Fatalf("decline error=%v exit=%d prompt count=%d, want exit 3 and one prompt", err, restoreExitCode(err), confirmed)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("decline created target volume state: %v", err)
	}
	if got := string(mustReadFile(t, report)); got != "" {
		t.Fatalf("decline report=%q, want empty", got)
	}
	calls := string(mustReadFile(t, log))
	if strings.Contains(calls, "call\tvolume\tcreate") || strings.Contains(calls, "call\tcompose") || strings.Contains(calls, "call\tstop") || strings.Contains(calls, "call\trun\t--rm\t--mount\ttype=volume") {
		t.Fatalf("decline caused a target creation, stop, or restore:\n%s", calls)
	}
}

func TestRestoreAcceptsEmptyLabelValueAndRejectsEmptyKey(t *testing.T) {
	t.Run("empty value", func(t *testing.T) {
		log, state, _ := installRestoreFakeDocker(t)
		if err := os.Remove(state); err != nil {
			t.Fatal(err)
		}
		dataDir := t.TempDir()
		writeValidRestoreSeed(t, dataDir)
		var stdout, stderr bytes.Buffer
		err := Execute([]string{
			"volume", "seed", "restore", "--to-volume", "restore-volume", "--name", "seed",
			"--data-dir", dataDir, "--allow-create", "--expect-image", "alpine:3.22", "--label", "com.example.empty=", "--yes",
		}, &stdout, &stderr, "dev", "extras")
		if err != nil {
			t.Fatalf("restore with empty label value: %v\nstderr=%s", err, stderr.String())
		}
		calls := string(mustReadFile(t, log))
		if !strings.Contains(calls, "call\tvolume\tcreate\t--label\tcom.example.empty=\trestore-volume") {
			t.Fatalf("empty label value was not preserved in volume create args:\n%s", calls)
		}
		if got := string(mustReadFile(t, state)); got != "present\n" {
			t.Fatalf("created target state=%q", got)
		}
	})

	t.Run("empty key", func(t *testing.T) {
		log, _, _ := installRestoreFakeDocker(t)
		dataDir := t.TempDir()
		writeValidRestoreSeed(t, dataDir)
		var stdout, stderr bytes.Buffer
		err := Execute([]string{"volume", "seed", "restore", "--to-volume", "restore-volume", "--data-dir", dataDir, "--label", "=bad"}, &stdout, &stderr, "dev", "extras")
		if err == nil || commandExitCode(err) != 1 || !strings.Contains(stderr.String(), "needs a KEY before '='") {
			t.Fatalf("empty label key error=%v stderr=%q", err, stderr.String())
		}
		if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid label reached Docker: %v", err)
		}
	})
}

func TestRestorePreflightRefusalPrecedesSurveyAndCleansProbes(t *testing.T) {
	log, state, target := installRestoreFakeDocker(t)
	t.Setenv("FAKE_BIND_FAIL", "true")
	t.Setenv("FAKE_RUNNING_USERS", "plain\t\n")
	dataDir := t.TempDir()
	writeValidRestoreSeed(t, dataDir)
	report := filepath.Join(t.TempDir(), "report.tsv")
	if err := os.WriteFile(report, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := restoreTestOptions(dataDir, report, io.Discard)
	options.Confirm = func(string) (bool, error) { return true, nil }
	err := restoreVolumeSeed(context.Background(), options)
	if err == nil || restoreExitCode(err) != 1 || !strings.Contains(err.Error(), "bind-mount preflight failed") {
		t.Fatalf("preflight error=%v exit=%d, want pre-destructive exit 1", err, restoreExitCode(err))
	}
	if got := string(mustReadFile(t, report)); got != "" {
		t.Fatalf("preflight failure report=%q, want empty", got)
	}
	if got := string(mustReadFile(t, state)); got != "present\n" {
		t.Fatalf("preflight refusal target volume state=%q, want unchanged present volume", got)
	}
	if got := string(mustReadFile(t, target)); got != "old\n" {
		t.Fatalf("preflight refusal target contents=%q, want unchanged old contents", got)
	}
	assertNoBindProbeArtifacts(t, dataDir)
	calls := string(mustReadFile(t, log))
	if !strings.Contains(calls, "--mount\ttype=bind") || !strings.Contains(calls, "cmp -s - /seed/.docker-extras-sentinel-") {
		t.Fatalf("preflight did not exercise strict --mount behavior:\n%s", calls)
	}
	if restoreSurveyWasCalled(calls) || strings.Contains(calls, "call\tcompose") || strings.Contains(calls, "call\tstop") || strings.Contains(calls, "call\tvolume\tcreate") || restoreContainerWasCalled(calls) {
		t.Fatalf("preflight refusal surveyed/stopped/created/restored:\n%s", calls)
	}
}

func TestRestoreSelectedCLIContextProbeBeforeQuiesceAndPartialReport(t *testing.T) {
	log, _, _ := installRestoreFakeDocker(t)
	config := filepath.Join(t.TempDir(), "docker-config")
	t.Setenv("DOCKER_CONTEXT", "restore-selected-context")
	t.Setenv("DOCKER_CONFIG", config)
	t.Setenv("COMPOSE_FILE", "must-not-be-used.yml")
	t.Setenv("COMPOSE_PATH_SEPARATOR", ";")
	t.Setenv("FAKE_RUNNING_USERS", "api\talpha\nplain\t\n")
	t.Setenv("FAKE_FAIL_STOP", "true")
	dataDir := t.TempDir()
	writeValidRestoreSeed(t, dataDir)
	report := filepath.Join(t.TempDir(), "report.tsv")
	var stdout, stderr bytes.Buffer
	options := restoreTestOptions(dataDir, report, &stderr)
	options.Stdout = &stdout
	options.Confirm = func(prompt string) (bool, error) {
		if !strings.Contains(prompt, "REPLACES all contents") {
			t.Errorf("restore prompt = %q", prompt)
		}
		return true, nil
	}
	err := restoreVolumeSeed(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "docker stop") || restoreExitCode(err) != 1 {
		t.Fatalf("partial quiesce error=%v, want target-safe exit 1", err)
	}
	if got, want := string(mustReadFile(t, report)), "down\talpha\tapi\nuncertain\tstop\tplain\n"; got != want {
		t.Fatalf("partial restore report=%q, want %q", got, want)
	}
	if !strings.Contains(stderr.String(), "compose project 'alpha' was taken down") || !strings.Contains(stderr.String(), "State is uncertain") {
		t.Fatalf("recovery output=%q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "compose stdout alpha") || !strings.Contains(stderr.String(), "compose stderr alpha") {
		t.Fatalf("Compose output did not pass through: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	calls := string(mustReadFile(t, log))
	probeIndex := strings.Index(calls, "cmp -s - /seed/.docker-extras-sentinel-")
	surveyIndex := restoreSurveyIndex(calls)
	if probeIndex < 0 || surveyIndex < 0 || probeIndex > surveyIndex {
		t.Fatalf("bind probe did not precede running-user survey:\n%s", calls)
	}
	for _, line := range strings.Split(strings.TrimSpace(calls), "\n") {
		if !strings.Contains(line, "docker_context=restore-selected-context") || !strings.Contains(line, "docker_config="+config) {
			t.Errorf("Docker operation lost selected CLI context/config: %s", line)
		}
	}
	var composeCall string
	for _, line := range strings.Split(calls, "\n") {
		if strings.HasPrefix(line, "call\tcompose\t-p\talpha\tdown\t") {
			composeCall = line
			break
		}
	}
	if composeCall == "" || !strings.Contains(composeCall, "pwd=/") || !strings.Contains(composeCall, "compose_file=unset") || !strings.Contains(composeCall, "compose_path_separator=unset") {
		t.Fatalf("Compose down did not use isolated project context:\n%s", calls)
	}
	if restoreContainerWasCalled(calls) {
		t.Fatal("restore ran after a later quiesce failure")
	}
}

func TestRestoreRelabelAndRunFailureExitClassification(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rmMode      string
		createFail  bool
		runExit     string
		wantExit    int
		wantNoRun   bool
		wantPresent bool
	}{
		{name: "rm success then run cannot start", runExit: "125", wantExit: 2, wantPresent: true},
		{name: "rm success then recreate fails", createFail: true, wantExit: 2, wantNoRun: true, wantPresent: false},
		{name: "rm reports error after possible deletion", rmMode: "ambiguous", wantExit: 2, wantNoRun: true, wantPresent: false},
		{name: "rm error with same name remains ambiguous", rmMode: "retained", wantExit: 2, wantNoRun: true, wantPresent: true},
		{name: "rm error followed by same-name replacement", rmMode: "replacement", wantExit: 2, wantNoRun: true, wantPresent: true},
		{name: "run partial action has unknown status", runExit: "42", wantExit: 2, wantPresent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, state, target := installRestoreFakeDocker(t)
			t.Setenv("FAKE_VOLUME_LABELS_JSON", `{"owner":"old"}`)
			t.Setenv("FAKE_RM_MODE", tc.rmMode)
			t.Setenv("FAKE_CREATE_FAIL", fmt.Sprint(tc.createFail))
			t.Setenv("FAKE_RESTORE_RUN_EXIT", tc.runExit)
			dataDir := t.TempDir()
			writeValidRestoreSeed(t, dataDir)
			options := restoreTestOptions(dataDir, "", io.Discard)
			options.Labels = []string{"owner=new"}
			options.Yes = true
			err := restoreVolumeSeed(context.Background(), options)
			if err == nil || restoreExitCode(err) != tc.wantExit {
				t.Fatalf("restore error=%v exit=%d, want %d", err, restoreExitCode(err), tc.wantExit)
			}
			calls := string(mustReadFile(t, log))
			if tc.wantNoRun && restoreContainerWasCalled(calls) {
				t.Fatalf("restore container ran after relabel operation failed:\n%s", calls)
			}
			if tc.rmMode == "" {
				rmIndex := strings.Index(calls, "call\tvolume\trm\trestore-volume")
				createIndex := strings.Index(calls, "call\tvolume\tcreate\t--label\towner=new\trestore-volume")
				runIndex := strings.Index(calls, "rm -rf /target/..?*")
				if rmIndex < 0 || createIndex < rmIndex || (!tc.wantNoRun && runIndex < createIndex) {
					t.Fatalf("relabel was not ordered rm, recreate, restore:\n%s", calls)
				}
			}
			stateContents, stateErr := os.ReadFile(state)
			if stateErr != nil {
				t.Fatalf("read target state: %v", stateErr)
			}
			if tc.wantPresent && string(stateContents) != "present\n" {
				t.Fatalf("expected recreated/preserved target state, got %q", stateContents)
			}
			if !tc.wantPresent && string(stateContents) != "missing\n" {
				t.Fatalf("target state should be absent after uncertain rm/recreate failure, got %q", stateContents)
			}
			if tc.runExit == "42" {
				if got := string(mustReadFile(t, target)); got != "partial\n" {
					t.Fatalf("simulated target contents=%q, want partial", got)
				}
			}
			if tc.rmMode == "replacement" {
				if got := string(mustReadFile(t, target)); got != "replacement-empty\n" {
					t.Fatalf("fake rm did not replace the old volume with an empty same-name volume: %q", got)
				}
			}
		})
	}
}

func TestRestoreAllowCreateArmsChangedTargetExit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		createMode  string
		runExit     string
		wantExit    int
		wantPresent bool
		wantRun     bool
	}{
		{name: "successful create then run cannot start", runExit: "125", wantExit: 2, wantPresent: true, wantRun: true},
		{name: "create fails before effect", createMode: "before-error", wantExit: 2},
		{name: "create errors after creating same name", createMode: "after-error", wantExit: 2, wantPresent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, state, _ := installRestoreFakeDocker(t)
			if err := os.Remove(state); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FAKE_CREATE_MODE", tc.createMode)
			t.Setenv("FAKE_RESTORE_RUN_EXIT", tc.runExit)
			dataDir := t.TempDir()
			writeValidRestoreSeed(t, dataDir)
			options := restoreTestOptions(dataDir, "", io.Discard)
			options.AllowCreate = true
			options.ExpectImage = "alpine:3.22"
			options.Yes = true
			err := restoreVolumeSeed(context.Background(), options)
			if err == nil || restoreExitCode(err) != tc.wantExit {
				t.Fatalf("restore error=%v exit=%d, want conservative exit %d", err, restoreExitCode(err), tc.wantExit)
			}
			if !tc.wantRun && !strings.Contains(err.Error(), "creation outcome is uncertain") {
				t.Fatalf("failed create error=%q lacks uncertainty guidance", err)
			}
			calls := string(mustReadFile(t, log))
			if strings.Contains(calls, "call\tvolume\tcreate") == false {
				t.Fatalf("allow-create never issued volume create:\n%s", calls)
			}
			if restoreContainerWasCalled(calls) != tc.wantRun {
				t.Fatalf("restore-run present=%t, want %t:\n%s", restoreContainerWasCalled(calls), tc.wantRun, calls)
			}
			stateContents, readErr := os.ReadFile(state)
			if tc.wantPresent && (readErr != nil || string(stateContents) != "present\n") {
				t.Fatalf("expected created volume state, got %q err=%v", stateContents, readErr)
			}
			if !tc.wantPresent && !errors.Is(readErr, os.ErrNotExist) {
				t.Fatalf("expected absent volume after pre-effect create failure, got %q err=%v", stateContents, readErr)
			}
		})
	}
}

func TestRestoreRelabelRefusesAttachedOrNonPlainVolumes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		driver     string
		options    string
		attached   string
		wantReason string
	}{
		{name: "attached stopped container", driver: "local", options: "null", attached: "old-container", wantReason: "still attached"},
		{name: "non-local driver", driver: "nfs", options: "null", wantReason: "non-default driver"},
		{name: "local driver options", driver: "local", options: `{"type":"nfs"}`, wantReason: "driver options"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, _, _ := installRestoreFakeDocker(t)
			t.Setenv("FAKE_VOLUME_LABELS_JSON", `{"owner":"old"}`)
			t.Setenv("FAKE_VOLUME_DRIVER", tc.driver)
			t.Setenv("FAKE_VOLUME_OPTIONS_JSON", tc.options)
			t.Setenv("FAKE_ATTACHED", tc.attached)
			dataDir := t.TempDir()
			writeValidRestoreSeed(t, dataDir)
			options := restoreTestOptions(dataDir, "", io.Discard)
			options.Labels = []string{"owner=new"}
			options.Yes = true
			var stderr bytes.Buffer
			options.Stderr = &stderr
			if err := restoreVolumeSeed(context.Background(), options); err != nil {
				t.Fatalf("restore keeping labels in place: %v", err)
			}
			if !strings.Contains(stderr.String(), tc.wantReason) {
				t.Fatalf("label safety note=%q, want %q", stderr.String(), tc.wantReason)
			}
			calls := string(mustReadFile(t, log))
			if strings.Contains(calls, "call\tvolume\trm") || strings.Contains(calls, "call\tvolume\tcreate") {
				t.Fatalf("unsafe volume relabel was attempted:\n%s", calls)
			}
		})
	}
}

func TestRestoreDaemonBackedPluginReplacesVolumeAfterDirectStop(t *testing.T) {
	requireDockerDaemon(t)
	defaultDockerEnv := os.Environ()
	defaultID, err := runDockerCommandOutput(t, "info", "--format", "{{.ID}}")
	if err != nil {
		t.Fatalf("read default daemon ID: %v", err)
	}
	configuredEndpoint := os.Getenv("BDS245_TEST_DOCKER_HOST")
	config, selectedContext := createTestDockerContext(t, configuredEndpoint)
	t.Setenv("DOCKER_CONFIG", config)
	t.Setenv("DOCKER_CONTEXT", selectedContext)
	selectedID, err := runDockerCommandOutput(t, "info", "--format", "{{.ID}}")
	if err != nil {
		t.Fatalf("read selected daemon ID: %v", err)
	}
	if configuredEndpoint != "" && strings.TrimSpace(defaultID) == strings.TrimSpace(selectedID) {
		t.Fatalf("configured alternate Docker endpoint %q resolved to default daemon ID %s", configuredEndpoint, strings.TrimSpace(defaultID))
	}
	if configuredEndpoint == "" && strings.TrimSpace(defaultID) != strings.TrimSpace(selectedID) {
		t.Fatalf("selected test context daemon ID %q differs from default endpoint %q", strings.TrimSpace(selectedID), strings.TrimSpace(defaultID))
	}
	pluginName := "restorestage"
	binary := filepath.Join(t.TempDir(), "docker-"+pluginName)
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, output)
	}
	pluginDir := filepath.Join(config, "cli-plugins")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(binary, filepath.Join(pluginDir, "docker-"+pluginName)); err != nil {
		t.Fatal(err)
	}

	volume := uniqueDockerName("bds245restore")
	container := volume + "-user"
	if dockerObjectExists(t, "volume", volume) || dockerObjectExists(t, "container", container) {
		t.Fatal("unique restore fixture already exists; refusing to touch it")
	}
	t.Cleanup(func() {
		_ = runDockerCommand(t, "rm", "-f", container)
		_ = runDockerCommand(t, "volume", "rm", volume)
	})
	if output, err := runDockerCommandOutput(t, "volume", "create", volume); err != nil {
		t.Fatalf("create disposable volume: %v\n%s", err, output)
	}
	if output, err := runDockerCommandOutput(t, "run", "--rm", "--mount", "type=volume,src="+volume+",dst=/target", "alpine:3.22", "sh", "-c", "printf old > /target/old.txt"); err != nil {
		t.Fatalf("write disposable old target: %v\n%s", err, output)
	}
	if output, err := runDockerCommandOutput(t, "run", "--init", "-d", "--name", container,
		"--mount", "type=volume,src="+volume+",dst=/target", "alpine:3.22", "sleep", "3600"); err != nil {
		t.Fatalf("start disposable direct user: %v\n%s", err, output)
	}

	dataDir := t.TempDir()
	archive := createRestoreTar(t, "restored payload")
	writeRestoreSeed(t, dataDir, "daemon-seed", archive, "", metadataForSeed)
	report := filepath.Join(t.TempDir(), "report.tsv")
	output, err := runDockerCommandOutput(t, pluginName, "volume", "seed", "restore",
		"--to-volume", volume, "--name", "daemon-seed", "--data-dir", dataDir,
		"--expect-image", "alpine:3.22", "--report", report, "--yes")
	if err != nil {
		t.Fatalf("docker %s volume seed restore: %v\n%s", pluginName, err, output)
	}
	if got, want := string(mustReadFile(t, report)), "stop\t"+container+"\n"; got != want {
		t.Fatalf("daemon restore report=%q, want %q", got, want)
	}
	if !strings.Contains(output, "stopping container '") || !strings.Contains(output, "restoring ") || !strings.Contains(output, "nothing was started back up") {
		t.Fatalf("restore plugin output omitted stop/recovery contract: %s", output)
	}
	state, err := runDockerCommandOutput(t, "inspect", "--format", "{{.State.Running}}", container)
	if err != nil || strings.TrimSpace(state) != "false" {
		t.Fatalf("direct container after restore state=%q err=%v, want preserved and stopped", state, err)
	}
	contents, err := runDockerCommandOutput(t, "run", "--rm", "--mount", "type=volume,src="+volume+",dst=/target", "alpine:3.22", "sh", "-c", "cat /target/seed.txt && test ! -e /target/old.txt")
	if err != nil || contents != "restored payload" {
		t.Fatalf("restored volume verification output=%q err=%v", contents, err)
	}
	if err := dockerObjectExistsWithEnv(defaultDockerEnv, "volume", volume); err == nil && configuredEndpoint != "" {
		t.Fatalf("selected volume %q also appeared on default daemon %s", volume, strings.TrimSpace(defaultID))
	}
	t.Logf("installed custom plugin %q restored through selected context %q to daemon ID %s (default ID %s; alternate endpoint configured=%t)", pluginName, selectedContext, strings.TrimSpace(selectedID), strings.TrimSpace(defaultID), configuredEndpoint != "")
	assertNoBindProbeArtifacts(t, dataDir)
}

func dockerObjectExistsWithEnv(environment []string, objectType, name string) error {
	cmd := exec.Command("docker", objectType, "inspect", name)
	cmd.Env = environment
	return cmd.Run()
}

func restoreTestOptions(dataDir, report string, stderr io.Writer) restoreOptions {
	return restoreOptions{
		Volume:     "restore-volume",
		Name:       "seed",
		DataDir:    dataDir,
		ReportFile: report,
		Stderr:     stderr,
		Confirm:    func(string) (bool, error) { return true, nil },
	}
}

func installRestoreFakeDocker(t *testing.T) (log, state, target string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "docker.log")
	state = filepath.Join(bin, "volume-state")
	target = filepath.Join(bin, "target-content")
	if err := os.WriteFile(state, []byte("present\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	docker := filepath.Join(bin, "docker")
	script := `#!/bin/sh
{
  printf 'call'
  for arg in "$@"; do printf '\t%s' "$arg"; done
  printf '\tpwd=%s\tdocker_context=%s\tdocker_config=%s\tcompose_file=%s\tcompose_path_separator=%s\n' \
    "$PWD" "${DOCKER_CONTEXT-unset}" "${DOCKER_CONFIG-unset}" "${COMPOSE_FILE-unset}" "${COMPOSE_PATH_SEPARATOR-unset}"
} >> "$FAKE_DOCKER_LOG"

volume_present() {
  [ -f "$FAKE_VOLUME_STATE" ] && [ "$(cat "$FAKE_VOLUME_STATE")" = present ]
}
bind_source() {
  waiting=
  for arg in "$@"; do
    if [ "$waiting" = yes ]; then
      case "$arg" in
        type=bind,src=*) spec=${arg#type=bind,src=}; printf '%s\n' "${spec%%,dst=*}"; return 0 ;;
      esac
      waiting=
    fi
    [ "$arg" = --mount ] && waiting=yes
  done
  return 1
}

case "$1" in
  version) printf '29.0.0\n' ;;
  volume)
    case "$2" in
      ls)
        if volume_present; then printf '%s\n' "$FAKE_RESTORE_VOLUME"; fi
        ;;
      inspect)
        if ! volume_present; then exit 1; fi
        printf '{"Name":"%s","Driver":"%s","Labels":%s,"Options":%s}\n' \
          "$FAKE_RESTORE_VOLUME" "$FAKE_VOLUME_DRIVER" \
          "$FAKE_VOLUME_LABELS_JSON" "$FAKE_VOLUME_OPTIONS_JSON"
        ;;
      create)
        case "${FAKE_CREATE_MODE-}" in
          before-error) echo 'simulated create failure before effect' >&2; exit 31 ;;
          after-error) printf 'present\n' > "$FAKE_VOLUME_STATE"; printf 'empty-new-volume\n' > "$FAKE_TARGET_CONTENT"; echo 'simulated create error after effect' >&2; exit 32 ;;
        esac
        if [ "${FAKE_CREATE_FAIL-false}" = true ]; then echo 'simulated create failure' >&2; exit 31; fi
        printf 'present\n' > "$FAKE_VOLUME_STATE"
        printf 'empty-new-volume\n' > "$FAKE_TARGET_CONTENT"
        printf '%s\n' "$FAKE_RESTORE_VOLUME"
        ;;
      rm)
        case "${FAKE_RM_MODE-}" in
          retained) echo 'simulated rm failure, target retained' >&2; exit 32 ;;
          ambiguous) printf 'missing\n' > "$FAKE_VOLUME_STATE"; echo 'simulated rm failure after removal' >&2; exit 33 ;;
          replacement) printf 'missing\n' > "$FAKE_VOLUME_STATE"; printf 'replacement-empty\n' > "$FAKE_TARGET_CONTENT"; printf 'present\n' > "$FAKE_VOLUME_STATE"; echo 'simulated rm failure after same-name replacement' >&2; exit 34 ;;
          *) printf 'missing\n' > "$FAKE_VOLUME_STATE"; printf '%s\n' "$FAKE_RESTORE_VOLUME" ;;
        esac
        ;;
      *) exit 81 ;;
    esac
    ;;
  ps)
    case "$*" in
      *'{{.Names}}'*'com.docker.compose.project'*) printf '%b' "${FAKE_RUNNING_USERS-}" ;;
      *'label=com.docker.compose.project=alpha'*) printf 'api\n' ;;
      *'{{.Image}}'*) printf '%s\n' "${FAKE_TARGET_IMAGE-alpine:3.22}" ;;
      *'{{.Names}}'*) printf '%s\n' "${FAKE_ATTACHED-}" ;;
      *) exit 82 ;;
    esac
    ;;
  compose)
    printf 'compose stdout %s\n' "$3"
    printf 'compose stderr %s\n' "$3" >&2
    ;;
  stop)
    if [ "${FAKE_FAIL_STOP-false}" = true ]; then echo 'simulated stop error after partial action' >&2; exit 34; fi
    ;;
  run)
    case "$*" in
      *'cmp -s - /seed/'*)
        if [ "${FAKE_BIND_FAIL-false}" = true ]; then echo 'bind source path does not exist' >&2; exit 125; fi
        source=$(bind_source "$@") || exit 84
        for sentinel in "$source"/.docker-extras-sentinel-*; do
          [ -f "$sentinel" ] || exit 85
          nonce=${sentinel##*-}
          printf 'host:%s\n' "$nonce" | cmp -s - "$sentinel" || exit 86
          printf 'container:%s\n' "$nonce" > "$source/.docker-extras-probe-$nonce"
        done
        ;;
      *'sha256sum /seed/'*)
        source=$(bind_source "$@") || exit 87
        sha256sum "$source/$FAKE_RESTORE_NAME.tar"
        ;;
      *'tar -tf /seed/'*)
        source=$(bind_source "$@") || exit 88
        tar -tf "$source/$FAKE_RESTORE_NAME.tar" >/dev/null
        ;;
      *"test -f '/seed/"*)
        if [ "${FAKE_REMOTE_ARCHIVE_MISSING-false}" = true ]; then echo 'selected daemon archive path missing' >&2; exit 125; fi
        ;;
      *'rm -rf /target/..?*'*)
        case "${FAKE_RESTORE_RUN_EXIT-}" in
          21) printf 'cleared\n' > "$FAKE_TARGET_CONTENT"; exit 21 ;;
          22) printf 'partial\n' > "$FAKE_TARGET_CONTENT"; exit 22 ;;
          125|126|127) echo 'simulated container start failure' >&2; exit "$FAKE_RESTORE_RUN_EXIT" ;;
          42) printf 'partial\n' > "$FAKE_TARGET_CONTENT"; exit 42 ;;
          *) printf 'restored\n' > "$FAKE_TARGET_CONTENT" ;;
        esac
        ;;
      *) exit 89 ;;
    esac
    ;;
  *) exit 90 ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_DOCKER_LOG", log)
	t.Setenv("FAKE_VOLUME_STATE", state)
	t.Setenv("FAKE_TARGET_CONTENT", target)
	t.Setenv("FAKE_RESTORE_VOLUME", "restore-volume")
	t.Setenv("FAKE_RESTORE_NAME", "seed")
	t.Setenv("FAKE_VOLUME_LABELS_JSON", `{}`)
	t.Setenv("FAKE_VOLUME_OPTIONS_JSON", "null")
	t.Setenv("FAKE_VOLUME_DRIVER", "local")
	t.Setenv("FAKE_RUNNING_USERS", "")
	t.Setenv("FAKE_ATTACHED", "")
	t.Setenv("FAKE_TARGET_IMAGE", "alpine:3.22")
	t.Setenv("FAKE_BIND_FAIL", "false")
	t.Setenv("FAKE_FAIL_STOP", "false")
	t.Setenv("FAKE_RM_MODE", "")
	t.Setenv("FAKE_CREATE_FAIL", "false")
	t.Setenv("FAKE_CREATE_MODE", "")
	t.Setenv("FAKE_RESTORE_RUN_EXIT", "")
	t.Setenv("FAKE_REMOTE_ARCHIVE_MISSING", "false")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log, state, target
}

func createRestoreTar(t *testing.T, contents string) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	if err := writer.WriteHeader(&tar.Header{Name: "seed.txt", Mode: 0o600, Size: int64(len(contents))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, contents); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func writeValidRestoreSeed(t *testing.T, dataDir string) {
	t.Helper()
	writeRestoreSeed(t, dataDir, "seed", createRestoreTar(t, "restored payload"), "", metadataForSeed)
}

func writeRestoreSeed(t *testing.T, dataDir, name string, archive []byte, explicitMeta string, metaFunc func([]byte) string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dataDir, name+".tar"), archive, 0o600); err != nil {
		t.Fatal(err)
	}
	meta := explicitMeta
	if metaFunc != nil {
		meta = metaFunc(archive)
	}
	if meta == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dataDir, name+".meta"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
}

func metadataForSeed(archive []byte) string {
	return fmt.Sprintf("source_volume=source\nimage=alpine:3.22\nbytes=%d\nsha256=%s\n", len(archive), seedDigest(archive))
}

func seedDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func assertRestoreNoSurveyOrStops(t *testing.T, calls string) {
	t.Helper()
	if restoreSurveyWasCalled(calls) || strings.Contains(calls, "call\tcompose") || strings.Contains(calls, "call\tstop") || restoreContainerWasCalled(calls) {
		t.Fatalf("pre-destructive refusal surveyed/stopped/restored:\n%s", calls)
	}
}

func restoreSurveyWasCalled(calls string) bool {
	return restoreSurveyIndex(calls) >= 0
}

func restoreSurveyIndex(calls string) int {
	searchFrom := 0
	for _, line := range strings.Split(calls, "\n") {
		if strings.HasPrefix(line, "call\tps\t--filter\tvolume=restore-volume\t--format\t") &&
			strings.Contains(line, "{{.Names}}") && strings.Contains(line, "com.docker.compose.project") {
			return searchFrom
		}
		searchFrom += len(line) + 1
	}
	return -1
}

func restoreContainerWasCalled(calls string) bool {
	return strings.Contains(calls, "call\trun\t--rm\t--mount\ttype=volume,src=restore-volume,dst=/target")
}
