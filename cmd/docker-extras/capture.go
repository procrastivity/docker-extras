package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var seedNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type captureOptions struct {
	Volume     string
	Name       string
	DataDir    string
	Image      string
	Yes        bool
	ReportFile string
	Stdout     io.Writer
	Stderr     io.Writer
	DockerEnv  []string
	Confirm    func(string) (bool, error)
}

func runCaptureCommand(cmd *cobra.Command, _ []string) error {
	volume, _ := cmd.Flags().GetString("from-volume")
	name, _ := cmd.Flags().GetString("name")
	dataDir, _ := cmd.Flags().GetString("data-dir")
	image, _ := cmd.Flags().GetString("image")
	yes, _ := cmd.Flags().GetBool("yes")
	reportFile, _ := cmd.Flags().GetString("report")
	for _, flag := range []struct {
		name  string
		value string
	}{
		{name: "from-volume", value: volume},
		{name: "name", value: name},
		{name: "data-dir", value: dataDir},
		{name: "image", value: image},
		{name: "report", value: reportFile},
	} {
		if cmd.Flags().Changed(flag.name) && flag.value == "" {
			return &commandExitError{Code: 1, Err: fmt.Errorf("--%s requires a value", flag.name)}
		}
	}
	err := captureVolumeSeed(cmd.Context(), captureOptions{
		Volume:     volume,
		Name:       name,
		DataDir:    dataDir,
		Image:      image,
		Yes:        yes,
		ReportFile: reportFile,
		Stdout:     cmd.OutOrStdout(),
		Stderr:     cmd.ErrOrStderr(),
	})
	if err == nil {
		return nil
	}
	code := 1
	if errors.Is(err, errQuiesceDeclined) {
		code = 3
	}
	return &commandExitError{Code: code, Err: err}
}

func captureVolumeSeed(ctx context.Context, options captureOptions) (retErr error) {
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	if options.Stderr == nil {
		options.Stderr = io.Discard
	}
	if options.Volume == "" {
		return errors.New("capture requires --from-volume")
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
	if strings.ContainsAny(options.Image, "\r\n") {
		return errors.New("--image must not contain a newline")
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
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create seed directory %q: %w", dataDir, err)
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		return fmt.Errorf("inspect seed directory %q: %w", dataDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("seed path %q is not a directory", dataDir)
	}

	archive := filepath.Join(dataDir, options.Name+".tar")
	meta := filepath.Join(dataDir, options.Name+".meta")
	archiveTmp := archive + ".tmp"
	metaTmp := meta + ".tmp"
	defer func() {
		for _, path := range []string{archiveTmp, metaTmp} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				retErr = errors.Join(retErr, fmt.Errorf("remove temporary file %q: %w", path, err))
			}
		}
	}()
	for _, path := range []string{archiveTmp, metaTmp} {
		if err := removeStaleCaptureTemp(path); err != nil {
			return err
		}
	}

	if _, err := dockerOutput(ctx, dockerEnv, io.Discard, "volume", "inspect", options.Volume); err != nil {
		return fmt.Errorf("volume not found: %s: %w", options.Volume, err)
	}

	image := options.Image
	if image == "" {
		imagesOutput, err := dockerOutput(ctx, dockerEnv, io.Discard,
			"ps", "-a", "--filter", "volume="+options.Volume, "--format", "{{.Image}}")
		if err != nil {
			return fmt.Errorf("infer image for volume %q: %w", options.Volume, err)
		}
		images := sortedUniqueLines(imagesOutput)
		switch len(images) {
		case 0:
			_, _ = fmt.Fprintf(options.Stderr, "note: no container is attached to %s; there is no image to infer\n", options.Volume)
		case 1:
			image = images[0]
		default:
			_, _ = fmt.Fprintf(options.Stderr, "note: containers with different images use %s; not recording an image lock\n", options.Volume)
		}
	}
	if image == "" {
		_, _ = fmt.Fprintln(options.Stderr, "note: no image lock recorded — restoring this seed needs --force (or re-capture with --image IMG)")
	}
	project, err := captureVolumeProject(ctx, dockerEnv, options.Volume)
	if err != nil {
		return err
	}

	_, err = quiesceVolume(ctx, quiesceOptions{
		Volume:         options.Volume,
		Mode:           quiesceCapture,
		Yes:            options.Yes,
		ReportFile:     options.ReportFile,
		Stdout:         options.Stdout,
		Stderr:         options.Stderr,
		DockerEnv:      dockerEnv,
		MountPreflight: func(ctx context.Context) error { return bindMountPreflight(ctx, dockerEnv, dataDir, options.Stderr) },
		Confirm:        options.Confirm,
	})
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(options.Stderr, "archiving volume %s → %s\n", options.Volume, archive)
	started := time.Now()
	uid, gid := os.Getuid(), os.Getgid()
	archivePath := "/seed/" + options.Name + ".tar.tmp"
	script := fmt.Sprintf("set -eu; tar -C /source -cf %q .; chown %d:%d %q; sha256sum %q",
		archivePath, uid, gid, archivePath, archivePath)
	shaOutput, err := dockerOutput(ctx, dockerEnv, options.Stderr,
		"run", "--rm",
		"--mount", "type=volume,src="+options.Volume+",dst=/source,readonly",
		"--mount", "type=bind,src="+dataDir+",dst=/seed",
		"alpine", "sh", "-c", script)
	if err != nil {
		return fmt.Errorf("volume archive (tar) failed: %w", err)
	}
	fields := strings.Fields(shaOutput)
	if len(fields) == 0 || len(fields[0]) != 64 {
		return fmt.Errorf("could not compute the seed checksum (got: %s)", strings.TrimSpace(shaOutput))
	}
	if _, err := hex.DecodeString(fields[0]); err != nil {
		return fmt.Errorf("could not compute the seed checksum (got: %s)", strings.TrimSpace(shaOutput))
	}
	sha := strings.ToLower(fields[0])
	archiveInfo, err := os.Stat(archiveTmp)
	if err != nil {
		return fmt.Errorf("could not read the size of %s: %w", archiveTmp, err)
	}
	if !archiveInfo.Mode().IsRegular() {
		return fmt.Errorf("archive output %s is not a regular file", archiveTmp)
	}
	bytes := archiveInfo.Size()
	date := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	metadata := fmt.Sprintf("source_volume=%s\nimage=%s\nproject=%s\ndate=%s\nbytes=%d\nsha256=%s\n",
		options.Volume, image, project, date, bytes, sha)
	if err := writeCaptureMetadata(metaTmp, metadata); err != nil {
		return fmt.Errorf("could not write seed metadata: %w", err)
	}
	if err := os.Rename(archiveTmp, archive); err != nil {
		return fmt.Errorf("could not move archive into place: %w", err)
	}
	if err := os.Rename(metaTmp, meta); err != nil {
		return fmt.Errorf("could not move metadata into place: %w", err)
	}
	_, _ = fmt.Fprintf(options.Stderr, "done → %s (%s)\n", archive, time.Since(started).Round(time.Second))
	return nil
}

func truncateCaptureReport(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	return file.Close()
}

func removeStaleCaptureTemp(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect temporary file %q: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale temporary file %q: %w", path, err)
	}
	return nil
}

func captureVolumeProject(ctx context.Context, dockerEnv []string, volume string) (string, error) {
	labelsOutput, err := dockerOutput(ctx, dockerEnv, io.Discard,
		"volume", "inspect", "--format", "{{json .Labels}}", volume)
	if err != nil {
		return "", fmt.Errorf("read project provenance for volume %q: %w", volume, err)
	}
	var labels map[string]string
	if trimmed := strings.TrimSpace(labelsOutput); trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal([]byte(trimmed), &labels); err != nil {
			return "", fmt.Errorf("parse labels for volume %q: %w", volume, err)
		}
	}
	if project := labels["com.docker.compose.project"]; project != "" {
		return project, nil
	}

	projectsOutput, err := dockerOutput(ctx, dockerEnv, io.Discard,
		"ps", "-a", "--filter", "volume="+volume, "--format", `{{.Label "com.docker.compose.project"}}`)
	if err != nil {
		return "", fmt.Errorf("infer Compose project for volume %q: %w", volume, err)
	}
	projects := sortedUniqueLines(projectsOutput)
	if len(projects) == 0 {
		return "", nil
	}
	return projects[0], nil
}

func sortedUniqueLines(output string) []string {
	set := make(map[string]struct{})
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			set[line] = struct{}{}
		}
	}
	lines := make([]string, 0, len(set))
	for line := range set {
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return lines
}

func dockerOutput(ctx context.Context, dockerEnv []string, stderr io.Writer, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = dockerEnv
	var stdout strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}

func bindMountPreflight(ctx context.Context, dockerEnv []string, dataDir string, stderr io.Writer) (retErr error) {
	var nonceBytes [16]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		return fmt.Errorf("generate bind-mount probe name: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes[:])
	sentinel := filepath.Join(dataDir, ".docker-extras-sentinel-"+nonce)
	probe := filepath.Join(dataDir, ".docker-extras-probe-"+nonce)
	created := make([]string, 0, 2)
	defer func() {
		for _, path := range created {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				retErr = errors.Join(retErr, fmt.Errorf("remove bind-mount probe artifact %q: %w", path, err))
			}
		}
	}()

	sentinelFile, err := os.OpenFile(sentinel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create host bind-mount sentinel: %w", err)
	}
	created = append(created, sentinel)
	if _, err := fmt.Fprintf(sentinelFile, "host:%s\n", nonce); err != nil {
		_ = sentinelFile.Close()
		return fmt.Errorf("write host bind-mount sentinel: %w", err)
	}
	if err := sentinelFile.Sync(); err != nil {
		_ = sentinelFile.Close()
		return fmt.Errorf("sync host bind-mount sentinel: %w", err)
	}
	if err := sentinelFile.Close(); err != nil {
		return fmt.Errorf("close host bind-mount sentinel: %w", err)
	}
	probeFile, err := os.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create host bind-mount probe target: %w", err)
	}
	created = append(created, probe)
	if err := probeFile.Close(); err != nil {
		return fmt.Errorf("close host bind-mount probe target: %w", err)
	}

	sentinelBase, probeBase := filepath.Base(sentinel), filepath.Base(probe)
	script := fmt.Sprintf("set -eu; printf 'host:%s\\n' | cmp -s - /seed/%s; printf 'container:%s\\n' > /seed/%s",
		nonce, sentinelBase, nonce, probeBase)
	if _, err := dockerOutput(ctx, dockerEnv, stderr,
		"run", "--rm",
		"--mount", "type=bind,src="+dataDir+",dst=/seed",
		"alpine", "sh", "-c", script); err != nil {
		return fmt.Errorf("daemon bind mount could not read the host sentinel and write a probe: %w", err)
	}
	probeBytes, err := os.ReadFile(probe)
	if err != nil {
		return fmt.Errorf("read container bind-mount probe on host: %w", err)
	}
	if want := []byte("container:" + nonce + "\n"); !bytes.Equal(probeBytes, want) {
		return fmt.Errorf("daemon bind-mount probe mismatch: got %q, want %q", probeBytes, want)
	}
	return nil
}

func writeCaptureMetadata(path, contents string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(file, contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
