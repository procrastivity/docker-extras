package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type restoreOptions struct {
	Volume      string
	Name        string
	DataDir     string
	AllowCreate bool
	Labels      []string
	ExpectImage string
	Force       bool
	NoVerify    bool
	Yes         bool
	ReportFile  string
	Stdout      io.Writer
	Stderr      io.Writer
	DockerEnv   []string
	Confirm     func(string) (bool, error)
}

type restoreTargetChangedError struct{ Err error }

func (e *restoreTargetChangedError) Error() string { return e.Err.Error() }
func (e *restoreTargetChangedError) Unwrap() error { return e.Err }

type restoreMetadata struct {
	Image         string
	ImagePresent  bool
	Bytes         string
	BytesPresent  bool
	SHA256        string
	SHA256Present bool
}

type dockerVolumeInfo struct {
	Name    string            `json:"Name"`
	Driver  string            `json:"Driver"`
	Labels  map[string]string `json:"Labels"`
	Options map[string]string `json:"Options"`
}

func runRestoreCommand(cmd *cobra.Command, _ []string) error {
	volume, _ := cmd.Flags().GetString("to-volume")
	name, _ := cmd.Flags().GetString("name")
	dataDir, _ := cmd.Flags().GetString("data-dir")
	allowCreate, _ := cmd.Flags().GetBool("allow-create")
	labels, _ := cmd.Flags().GetStringArray("label")
	expectImage, _ := cmd.Flags().GetString("expect-image")
	force, _ := cmd.Flags().GetBool("force")
	noVerify, _ := cmd.Flags().GetBool("no-verify")
	yes, _ := cmd.Flags().GetBool("yes")
	reportFile, _ := cmd.Flags().GetString("report")
	for _, flag := range []struct {
		name  string
		value string
	}{
		{name: "to-volume", value: volume},
		{name: "name", value: name},
		{name: "data-dir", value: dataDir},
		{name: "expect-image", value: expectImage},
		{name: "report", value: reportFile},
	} {
		if cmd.Flags().Changed(flag.name) && flag.value == "" {
			return &commandExitError{Code: 1, Err: fmt.Errorf("--%s requires a value", flag.name)}
		}
	}
	err := restoreVolumeSeed(cmd.Context(), restoreOptions{
		Volume:      volume,
		Name:        name,
		DataDir:     dataDir,
		AllowCreate: allowCreate,
		Labels:      labels,
		ExpectImage: expectImage,
		Force:       force,
		NoVerify:    noVerify,
		Yes:         yes,
		ReportFile:  reportFile,
		Stdout:      cmd.OutOrStdout(),
		Stderr:      cmd.ErrOrStderr(),
	})
	if err == nil {
		return nil
	}
	return &commandExitError{Code: restoreExitCode(err), Err: err}
}

func restoreExitCode(err error) int {
	if errors.Is(err, errQuiesceDeclined) {
		return 3
	}
	var changed *restoreTargetChangedError
	if errors.As(err, &changed) {
		return 2
	}
	return 1
}

func restoreVolumeSeed(ctx context.Context, options restoreOptions) error {
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	if options.Stderr == nil {
		options.Stderr = io.Discard
	}
	if options.Volume == "" {
		return errors.New("restore requires --to-volume")
	}
	if strings.ContainsAny(options.Volume, "\r\n") {
		return errors.New("--to-volume must not contain a newline")
	}
	if options.Name == "" {
		options.Name = "db-seed"
	}
	if !seedNamePattern.MatchString(options.Name) {
		return fmt.Errorf("--name must be a plain file base name (got: %s)", options.Name)
	}
	if options.DataDir == "" {
		return errors.New("--data-dir requires a value")
	}
	if strings.ContainsAny(options.ExpectImage, "\r\n") {
		return errors.New("--expect-image must not contain a newline")
	}
	for _, label := range options.Labels {
		key, _, ok := strings.Cut(label, "=")
		if !ok {
			return fmt.Errorf("--label must be KEY=VALUE (got: %s)", label)
		}
		if key == "" {
			return fmt.Errorf("--label needs a KEY before '=' (got: %s)", label)
		}
		if strings.ContainsAny(label, "\r\n") {
			return errors.New("--label must not contain a newline")
		}
	}
	if options.ReportFile != "" {
		if err := truncateCaptureReport(options.ReportFile); err != nil {
			return fmt.Errorf("cannot write the report file %q: %w", options.ReportFile, err)
		}
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("docker not found on PATH")
	}
	dockerEnv := options.DockerEnv
	if dockerEnv == nil {
		dockerEnv = os.Environ()
	}
	if _, err := dockerOutput(ctx, dockerEnv, io.Discard, "version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("cannot reach the docker daemon (is it running? check DOCKER_HOST): %w", err)
	}

	dataDir, err := filepath.Abs(options.DataDir)
	if err != nil {
		return fmt.Errorf("resolve seed directory %q: %w", options.DataDir, err)
	}
	if strings.Contains(dataDir, ",") {
		return fmt.Errorf("seed directory %q contains a comma unsupported by Docker --mount", dataDir)
	}
	dataDirInfo, err := os.Stat(dataDir)
	if err != nil {
		return fmt.Errorf("inspect seed directory %q: %w", dataDir, err)
	}
	if !dataDirInfo.IsDir() {
		return fmt.Errorf("seed path %q is not a directory", dataDir)
	}
	archive := filepath.Join(dataDir, options.Name+".tar")
	archiveInfo, err := os.Stat(archive)
	if err != nil {
		return fmt.Errorf("seed not found: %s: %w", archive, err)
	}
	if !archiveInfo.Mode().IsRegular() {
		return fmt.Errorf("seed archive %q is not a regular file", archive)
	}

	metaPath := filepath.Join(dataDir, options.Name+".meta")
	metadata, metadataExists, err := readRestoreMetadata(metaPath)
	if err != nil {
		return err
	}
	legacy := false
	if !metadataExists {
		if !options.NoVerify {
			return fmt.Errorf("no seed metadata: %s; without it the seed cannot be verified and the image cannot be locked (--no-verify skips the integrity check; --force is then needed for the image too)", metaPath)
		}
	} else {
		if metadata.BytesPresent != metadata.SHA256Present {
			return fmt.Errorf("incomplete seed integrity metadata: %s must contain both bytes= and sha256=, or neither", metaPath)
		}
		if metadata.BytesPresent {
			if !isDecimal(metadata.Bytes) {
				return fmt.Errorf("malformed bytes value in %s: %q", metaPath, metadata.Bytes)
			}
			if _, err := strconv.ParseUint(metadata.Bytes, 10, 64); err != nil {
				return fmt.Errorf("malformed bytes value in %s: %q", metaPath, metadata.Bytes)
			}
			if len(metadata.SHA256) != sha256.Size*2 {
				return fmt.Errorf("malformed sha256 value in %s: %q", metaPath, metadata.SHA256)
			}
			if _, err := hex.DecodeString(metadata.SHA256); err != nil || strings.ToLower(metadata.SHA256) != metadata.SHA256 {
				return fmt.Errorf("malformed sha256 value in %s: %q", metaPath, metadata.SHA256)
			}
		} else {
			legacy = true
		}
	}

	if !options.NoVerify {
		if legacy {
			_, _ = fmt.Fprintf(options.Stderr, "note: %s predates the integrity fields — checking the tar header only\n", metaPath)
			if err := validateRestoreTar(ctx, dockerEnv, dataDir, options.Name); err != nil {
				return fmt.Errorf("seed is corrupt: %s (tar -tf failed): %w", archive, err)
			}
		} else if metadataExists {
			actualBytes := strconv.FormatInt(archiveInfo.Size(), 10)
			if metadata.Bytes != actualBytes {
				return fmt.Errorf("seed is truncated or damaged: %s holds %s bytes, the sidecar records %s", archive, actualBytes, metadata.Bytes)
			}
			got, err := restoreArchiveSHA256(ctx, dockerEnv, dataDir, options.Name, options.Stderr)
			if err != nil {
				return fmt.Errorf("could not compute the checksum of %s: %w", archive, err)
			}
			if got != metadata.SHA256 {
				return fmt.Errorf("seed checksum does not match: %s (sidecar records %s, file computes %s)", archive, metadata.SHA256, got)
			}
		}
	}

	volumeInfo, volumeExists, err := inspectRestoreVolume(ctx, dockerEnv, options.Volume)
	if err != nil {
		return err
	}
	if !volumeExists && !options.AllowCreate {
		return fmt.Errorf("volume not found: %s (pass --allow-create, or create it with 'docker volume create %s')", options.Volume, options.Volume)
	}

	targetImage := options.ExpectImage
	if targetImage == "" {
		targetImage, err = inferRestoreTargetImage(ctx, dockerEnv, options.Volume)
		if err != nil {
			return err
		}
	}
	if metadata.Image == "" || targetImage == "" || metadata.Image != targetImage {
		if options.Force {
			_, _ = fmt.Fprintf(options.Stderr, "warning: image lock not satisfied (seed '%s', target '%s') — continuing per --force\n", knownOrUnknown(metadata.Image), knownOrUnknown(targetImage))
		} else if metadata.Image == "" {
			return fmt.Errorf("the seed records no image, so the image lock cannot be checked: %s; re-capture it with --image IMG, or restore anyway with --force", metaPath)
		} else if targetImage == "" {
			return fmt.Errorf("cannot determine which image the target %s runs, so the image lock cannot be checked; pass --expect-image IMG, or restore anyway with --force", options.Volume)
		} else {
			return fmt.Errorf("seed was captured under image '%s' but the target of %s runs '%s'; a physical data dir only starts cleanly under the same image (--force to override)", metadata.Image, options.Volume, targetImage)
		}
	}

	relabel := volumeExists && !restoreLabelsMatch(volumeInfo.Labels, options.Labels)
	prompt := fmt.Sprintf("this REPLACES all contents of volume '%s' with %s", options.Volume, archive)
	if relabel {
		prompt += ", and RECREATES the volume object to set the labels given"
	}
	_, err = quiesceVolume(ctx, quiesceOptions{
		Volume:              options.Volume,
		Mode:                quiesceRestore,
		Yes:                 options.Yes,
		ReportFile:          options.ReportFile,
		RestoreConfirmation: prompt + " — continue?",
		Stdout:              options.Stdout,
		Stderr:              options.Stderr,
		DockerEnv:           dockerEnv,
		MountPreflight: func(ctx context.Context) error {
			if err := bindMountPreflight(ctx, dockerEnv, dataDir, options.Stderr); err != nil {
				return err
			}
			if options.NoVerify {
				return verifyRestoreArchiveMounted(ctx, dockerEnv, dataDir, options.Name, options.Stderr)
			}
			return nil
		},
		Confirm: options.Confirm,
	})
	if err != nil {
		return err
	}

	targetChanged := false
	if !volumeExists {
		if err := createRestoreVolume(ctx, dockerEnv, options.Volume, options.Labels, options.Stderr); err != nil {
			return &restoreTargetChangedError{Err: fmt.Errorf("could not create volume %s; creation outcome is uncertain and an empty target may have been created: %w", options.Volume, err)}
		}
		targetChanged = true
	}
	if relabel {
		var targetRemoved bool
		targetRemoved, err = relabelRestoreVolume(ctx, dockerEnv, options.Volume, options.Labels, options.Stderr)
		targetChanged = targetChanged || targetRemoved
		if err != nil {
			if targetRemoved {
				return &restoreTargetChangedError{Err: err}
			}
			return err
		}
	}

	_, _ = fmt.Fprintf(options.Stderr, "restoring %s → %s\n", archive, options.Volume)
	started := time.Now()
	runErr := runRestoreContainer(ctx, dockerEnv, options.Stdout, options.Stderr, options.Volume, dataDir, options.Name)
	if runErr != nil {
		message, changed := restoreRunFailureMessage(runErr, options.Volume)
		if targetChanged || changed {
			return &restoreTargetChangedError{Err: message}
		}
		return message
	}
	_, _ = fmt.Fprintf(options.Stderr, "done → %s restored from %s (%s)\n", options.Volume, archive, time.Since(started).Round(time.Second))
	_, _ = fmt.Fprintln(options.Stderr, "nothing was started back up; restart the stack that uses this volume yourself")
	return nil
}

func readRestoreMetadata(path string) (restoreMetadata, bool, error) {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return restoreMetadata{}, false, nil
	}
	if err != nil {
		return restoreMetadata{}, false, fmt.Errorf("read seed metadata %q: %w", path, err)
	}
	var metadata restoreMetadata
	for _, line := range strings.Split(string(contents), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			switch line {
			case "bytes":
				if !metadata.BytesPresent {
					metadata.BytesPresent = true
				}
			case "sha256":
				if !metadata.SHA256Present {
					metadata.SHA256Present = true
				}
			}
			continue
		}
		switch key {
		case "image":
			if !metadata.ImagePresent {
				metadata.ImagePresent = true
				metadata.Image = value
			}
		case "bytes":
			if !metadata.BytesPresent {
				metadata.BytesPresent = true
				metadata.Bytes = value
			}
		case "sha256":
			if !metadata.SHA256Present {
				metadata.SHA256Present = true
				metadata.SHA256 = value
			}
		}
	}
	return metadata, true, nil
}

func isDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func inspectRestoreVolume(ctx context.Context, dockerEnv []string, volume string) (dockerVolumeInfo, bool, error) {
	output, err := dockerOutput(ctx, dockerEnv, io.Discard, "volume", "ls", "--format", "{{.Name}}")
	if err != nil {
		return dockerVolumeInfo{}, false, fmt.Errorf("list Docker volumes: %w", err)
	}
	found := false
	for _, name := range strings.Split(output, "\n") {
		if strings.TrimSpace(name) == volume {
			found = true
			break
		}
	}
	if !found {
		return dockerVolumeInfo{}, false, nil
	}
	output, err = dockerOutput(ctx, dockerEnv, io.Discard, "volume", "inspect", "--format", "{{json .}}", volume)
	if err != nil {
		return dockerVolumeInfo{}, false, fmt.Errorf("inspect target volume %q: %w", volume, err)
	}
	var info dockerVolumeInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &info); err != nil {
		return dockerVolumeInfo{}, false, fmt.Errorf("parse target volume %q inspection %q: %w", volume, strings.TrimSpace(output), err)
	}
	return info, true, nil
}

func inferRestoreTargetImage(ctx context.Context, dockerEnv []string, volume string) (string, error) {
	output, err := dockerOutput(ctx, dockerEnv, io.Discard, "ps", "-a", "--filter", "volume="+volume, "--format", "{{.Image}}")
	if err != nil {
		return "", fmt.Errorf("infer image for target volume %q: %w", volume, err)
	}
	images := sortedUniqueLines(output)
	if len(images) == 1 {
		return images[0], nil
	}
	return "", nil
}

func knownOrUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func restoreLabelsMatch(have map[string]string, requested []string) bool {
	for _, label := range requested {
		key, value, _ := strings.Cut(label, "=")
		actual, ok := have[key]
		if !ok || actual != value {
			return false
		}
	}
	return true
}

func restoreArchiveSHA256(ctx context.Context, dockerEnv []string, dataDir, name string, stderr io.Writer) (string, error) {
	output, err := dockerOutput(ctx, dockerEnv, stderr,
		"run", "--rm", "--mount", "type=bind,src="+dataDir+",dst=/seed,readonly",
		"alpine", "sha256sum", "/seed/"+name+".tar")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(output)
	if len(fields) == 0 || len(fields[0]) != sha256.Size*2 {
		return "", fmt.Errorf("invalid sha256sum output %q", strings.TrimSpace(output))
	}
	if _, err := hex.DecodeString(fields[0]); err != nil || strings.ToLower(fields[0]) != fields[0] {
		return "", fmt.Errorf("invalid sha256sum output %q", strings.TrimSpace(output))
	}
	return fields[0], nil
}

func validateRestoreTar(ctx context.Context, dockerEnv []string, dataDir, name string) error {
	_, err := dockerOutput(ctx, dockerEnv, io.Discard,
		"run", "--rm", "--mount", "type=bind,src="+dataDir+",dst=/seed,readonly",
		"alpine", "tar", "-tf", "/seed/"+name+".tar")
	return err
}

func verifyRestoreArchiveMounted(ctx context.Context, dockerEnv []string, dataDir, name string, stderr io.Writer) error {
	script := "test -f '/seed/" + name + ".tar' && test -r '/seed/" + name + ".tar'"
	if _, err := dockerOutput(ctx, dockerEnv, stderr,
		"run", "--rm", "--mount", "type=bind,src="+dataDir+",dst=/seed,readonly",
		"alpine", "sh", "-c", script); err != nil {
		return fmt.Errorf("selected daemon cannot read the seed archive through the host bind mount: %w", err)
	}
	return nil
}

func createRestoreVolume(ctx context.Context, dockerEnv []string, volume string, labels []string, stderr io.Writer) error {
	args := []string{"volume", "create"}
	for _, label := range labels {
		args = append(args, "--label", label)
	}
	args = append(args, volume)
	_, err := dockerOutput(ctx, dockerEnv, stderr, args...)
	return err
}

func relabelRestoreVolume(ctx context.Context, dockerEnv []string, volume string, labels []string, stderr io.Writer) (bool, error) {
	attachedOutput, err := dockerOutput(ctx, dockerEnv, io.Discard,
		"ps", "-a", "--filter", "volume="+volume, "--format", "{{.Names}}")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "note: labels left alone — could not verify that containers are detached from %s: %v\n", volume, err)
		return false, nil
	}
	if attached := sortedUniqueLines(attachedOutput); len(attached) > 0 {
		_, _ = fmt.Fprintf(stderr, "note: labels left alone — container '%s' is still attached to %s\n      (remove it and re-run to stamp them; the contents are replaced regardless)\n", attached[0], volume)
		return false, nil
	}
	info, exists, err := inspectRestoreVolume(ctx, dockerEnv, volume)
	if err != nil || !exists {
		_, _ = fmt.Fprintf(stderr, "note: labels left alone — could not verify that %s is a plain local volume\n", volume)
		return false, nil
	}
	if info.Driver != "local" || len(info.Options) != 0 {
		_, _ = fmt.Fprintf(stderr, "note: labels left alone — %s has a non-default driver or driver options\n", volume)
		return false, nil
	}
	_, _ = fmt.Fprintf(stderr, "recreating volume %s to set its labels…\n", volume)
	_, rmErr := dockerOutput(ctx, dockerEnv, stderr, "volume", "rm", volume)
	if rmErr != nil {
		return true, fmt.Errorf("could not remove volume %s to relabel it; its previous contents may be gone even if a volume with the same name still exists: %w", volume, rmErr)
	}
	if err := createRestoreVolume(ctx, dockerEnv, volume, labels, stderr); err != nil {
		return true, fmt.Errorf("removed %s but could not recreate it — the volume is gone; re-run to rebuild it from the seed: %w", volume, err)
	}
	return true, nil
}

func runRestoreContainer(ctx context.Context, dockerEnv []string, stdout, stderr io.Writer, volume, dataDir, name string) error {
	script := fmt.Sprintf("rm -rf /target/..?* /target/.[!.]* /target/* 2>/dev/null || exit 21; tar -C /target -xf '/seed/%s.tar' || exit 22", name)
	cmd := exec.CommandContext(ctx, "docker",
		"run", "--rm",
		"--mount", "type=volume,src="+volume+",dst=/target",
		"--mount", "type=bind,src="+dataDir+",dst=/seed,readonly",
		"alpine", "sh", "-c", script)
	cmd.Env = dockerEnv
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func restoreRunFailureMessage(err error, volume string) (error, bool) {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.ExitCode() {
		case 21:
			return fmt.Errorf("could not clear %s before the extract; target state is not intact", volume), true
		case 22:
			return fmt.Errorf("restore (tar extract) into %s failed; the target may be empty or partial", volume), true
		case 125, 126, 127:
			return fmt.Errorf("restore container did not start (docker exit %d) — %s was not cleared", exitErr.ExitCode(), volume), false
		default:
			return fmt.Errorf("restore container ended unexpectedly (docker exit %d) — %s may be empty or partial", exitErr.ExitCode(), volume), true
		}
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return fmt.Errorf("restore container did not start — %s was not cleared: %w", volume, err), false
	}
	return fmt.Errorf("restore container outcome is uncertain — %s may be empty or partial: %w", volume, err), true
}
