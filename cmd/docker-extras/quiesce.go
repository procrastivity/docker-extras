package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

type quiesceMode uint8

const (
	quiesceCapture quiesceMode = iota + 1
	quiesceRestore
)

type quiesceOptions struct {
	Volume              string
	Mode                quiesceMode
	Yes                 bool
	ReportFile          string
	RestoreConfirmation string
	Stdout              io.Writer
	Stderr              io.Writer
	DockerEnv           []string
	MountPreflight      func(context.Context) error
	Confirm             func(string) (bool, error)
}

type volumeUsers struct {
	Projects   []projectUsers
	Containers []string
}

type projectUsers struct {
	Name     string
	Services []string
}

type quiesceAction struct {
	Kind     string
	Project  string
	Services []string
	Name     string
}

type quiesceResult struct {
	Stopped   []quiesceAction
	Uncertain []quiesceAction
}

type quiesceActionError struct {
	Action quiesceAction
	Err    error
}

func (e *quiesceActionError) Error() string {
	if e.Action.Kind == "down" {
		return fmt.Sprintf("docker compose down for project %q failed; stopped-user state is uncertain: %v", e.Action.Project, e.Err)
	}
	return fmt.Sprintf("docker stop for container %q failed; stopped-user state is uncertain: %v", e.Action.Name, e.Err)
}

func (e *quiesceActionError) Unwrap() error { return e.Err }

var errQuiesceDeclined = errors.New("aborted (no changes made)")

const runningVolumeFormat = `{{.Names}}{{"\t"}}{{.Label "com.docker.compose.project"}}`
const composeServiceFormat = `{{.Label "com.docker.compose.service"}}`

// quiesceVolume is called only after the caller's volume and safety gates;
// its required bind-mount preflight runs before it surveys or stops any user.
//
// Successful report records retain the Bash TSV format:
//
//	down<TAB>PROJECT<TAB>service,service
//	stop<TAB>NAME
//
// A command that exits non-zero may already have stopped some users, so it is
// recorded separately as uncertain<TAB>down<TAB>PROJECT<TAB>services or
// uncertain<TAB>stop<TAB>NAME. No action is automatically restarted.
func quiesceVolume(ctx context.Context, options quiesceOptions) (result quiesceResult, retErr error) {
	if options.Volume == "" {
		return result, errors.New("quiesce requires a volume name")
	}
	if options.Mode != quiesceCapture && options.Mode != quiesceRestore {
		return result, errors.New("quiesce requires capture or restore mode")
	}
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	if options.Stderr == nil {
		options.Stderr = io.Discard
	}
	dockerEnv := options.DockerEnv
	if dockerEnv == nil {
		dockerEnv = os.Environ()
	}

	var report *os.File
	if options.ReportFile != "" {
		var err error
		report, err = os.OpenFile(options.ReportFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o666)
		if err != nil {
			return result, fmt.Errorf("cannot write the report file %q: %w", options.ReportFile, err)
		}
	}
	defer func() {
		writeQuiesceRecovery(options.Stderr, result)
		if report != nil {
			if err := report.Close(); err != nil {
				closeErr := fmt.Errorf("close report file %q: %w", options.ReportFile, err)
				retErr = errors.Join(retErr, closeErr)
			}
		}
	}()

	if options.MountPreflight == nil {
		return result, errors.New("quiesce requires a successful daemon bind-mount preflight")
	}
	if err := options.MountPreflight(ctx); err != nil {
		return result, fmt.Errorf("daemon bind-mount preflight failed; no users were surveyed or stopped: %w", err)
	}

	users, err := surveyVolumeUsers(ctx, dockerEnv, options.Volume, options.Stderr)
	if err != nil {
		return result, err
	}
	announceVolumeUsers(options.Stderr, options.Volume, users)

	needsConfirmation := options.Mode == quiesceRestore || len(users.Projects)+len(users.Containers) > 0
	if needsConfirmation && !options.Yes {
		prompt := confirmationPrompt(options)
		confirm := options.Confirm
		if confirm == nil {
			confirm = func(prompt string) (bool, error) { return confirmOnTTY(options.Stderr, prompt) }
		}
		accepted, err := confirm(prompt)
		if err != nil {
			return result, fmt.Errorf("confirm quiesce: %w", err)
		}
		if !accepted {
			return result, errQuiesceDeclined
		}
	}

	for _, project := range users.Projects {
		action := quiesceAction{Kind: "down", Project: project.Name, Services: project.Services}
		_, _ = fmt.Fprintf(options.Stderr, "volume %s is in use — taking down compose project '%s'…\n", options.Volume, project.Name)
		if err := runComposeDown(ctx, dockerEnv, project.Name, options.Stdout, options.Stderr); err != nil {
			result.Uncertain = append(result.Uncertain, action)
			writeErr := writeQuiesceRecord(report, action.uncertainRecord())
			return result, errors.Join(&quiesceActionError{Action: action, Err: err}, writeErr)
		}
		result.Stopped = append(result.Stopped, action)
		if err := writeQuiesceRecord(report, action.successRecord()); err != nil {
			return result, fmt.Errorf("compose project %q was taken down but its report record could not be written: %w", project.Name, err)
		}
	}

	for _, name := range users.Containers {
		action := quiesceAction{Kind: "stop", Name: name}
		_, _ = fmt.Fprintf(options.Stderr, "volume %s is in use — stopping container '%s'…\n", options.Volume, name)
		if err := runContainerStop(ctx, dockerEnv, name, options.Stderr); err != nil {
			result.Uncertain = append(result.Uncertain, action)
			writeErr := writeQuiesceRecord(report, action.uncertainRecord())
			return result, errors.Join(&quiesceActionError{Action: action, Err: err}, writeErr)
		}
		result.Stopped = append(result.Stopped, action)
		if err := writeQuiesceRecord(report, action.successRecord()); err != nil {
			return result, fmt.Errorf("container %q was stopped but its report record could not be written: %w", name, err)
		}
	}

	return result, nil
}

func surveyVolumeUsers(ctx context.Context, dockerEnv []string, volume string, stderr io.Writer) (volumeUsers, error) {
	output, err := dockerQuery(ctx, dockerEnv, stderr,
		"ps", "--filter", "volume="+volume, "--format", runningVolumeFormat)
	if err != nil {
		return volumeUsers{}, fmt.Errorf("survey running users of volume %q: %w", volume, err)
	}

	projectSet := make(map[string]struct{})
	var users volumeUsers
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 || fields[0] == "" {
			return volumeUsers{}, fmt.Errorf("parse running-volume survey row %q", line)
		}
		if fields[1] == "" {
			users.Containers = append(users.Containers, fields[0])
			continue
		}
		projectSet[fields[1]] = struct{}{}
	}

	projects := make([]string, 0, len(projectSet))
	for project := range projectSet {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	for _, project := range projects {
		servicesOutput, err := dockerQuery(ctx, dockerEnv, stderr,
			"ps",
			"--filter", "label=com.docker.compose.project="+project,
			"--filter", "label=com.docker.compose.oneoff=False",
			"--format", composeServiceFormat,
		)
		if err != nil {
			return volumeUsers{}, fmt.Errorf("survey running services in compose project %q: %w", project, err)
		}
		serviceSet := make(map[string]struct{})
		for _, service := range strings.Split(strings.TrimSuffix(servicesOutput, "\n"), "\n") {
			if service != "" {
				serviceSet[service] = struct{}{}
			}
		}
		services := make([]string, 0, len(serviceSet))
		for service := range serviceSet {
			services = append(services, service)
		}
		sort.Strings(services)
		users.Projects = append(users.Projects, projectUsers{Name: project, Services: services})
	}
	return users, nil
}

func dockerQuery(ctx context.Context, dockerEnv []string, stderr io.Writer, args ...string) (string, error) {
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

func announceVolumeUsers(stderr io.Writer, volume string, users volumeUsers) {
	for _, project := range users.Projects {
		services := strings.Join(project.Services, ",")
		if services == "" {
			_, _ = fmt.Fprintf(stderr, "volume %s is in use — compose project '%s' will be taken down\n", volume, project.Name)
			continue
		}
		_, _ = fmt.Fprintf(stderr, "volume %s is in use — compose project '%s' will be taken down (services: %s)\n", volume, project.Name, services)
	}
	for _, name := range users.Containers {
		_, _ = fmt.Fprintf(stderr, "volume %s is in use — container '%s' will be stopped\n", volume, name)
	}
}

func confirmationPrompt(options quiesceOptions) string {
	if options.Mode == quiesceCapture {
		return "capture stops all of the above and starts nothing back up — continue?"
	}
	if options.RestoreConfirmation != "" {
		return options.RestoreConfirmation
	}
	return "restore replaces the target volume contents and starts nothing back up — continue?"
}

func confirmOnTTY(stderr io.Writer, prompt string) (bool, error) {
	_, _ = fmt.Fprintf(stderr, "%s [y/N] ", prompt)
	tty, err := os.Open("/dev/tty")
	if err != nil {
		_, _ = fmt.Fprintln(stderr)
		return false, nil
	}
	defer tty.Close()
	answer, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return len(answer) > 0 && (answer[0] == 'y' || answer[0] == 'Y'), nil
}

func runComposeDown(ctx context.Context, dockerEnv []string, project string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "compose", "-p", project, "down")
	cmd.Dir = "/"
	cmd.Env = envWithout(dockerEnv, "COMPOSE_FILE", "COMPOSE_PATH_SEPARATOR")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func runContainerStop(ctx context.Context, dockerEnv []string, name string, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "stop", "-t", "120", name)
	cmd.Env = dockerEnv
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	return cmd.Run()
}

func envWithout(environment []string, names ...string) []string {
	remove := make(map[string]struct{}, len(names))
	for _, name := range names {
		remove[name] = struct{}{}
	}
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if _, excluded := remove[name]; !excluded {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func (action quiesceAction) successRecord() string {
	if action.Kind == "down" {
		return fmt.Sprintf("down\t%s\t%s\n", action.Project, strings.Join(action.Services, ","))
	}
	return fmt.Sprintf("stop\t%s\n", action.Name)
}

func (action quiesceAction) uncertainRecord() string {
	if action.Kind == "down" {
		return fmt.Sprintf("uncertain\tdown\t%s\t%s\n", action.Project, strings.Join(action.Services, ","))
	}
	return fmt.Sprintf("uncertain\tstop\t%s\n", action.Name)
}

func writeQuiesceRecord(report *os.File, record string) error {
	if report == nil {
		return nil
	}
	n, err := io.WriteString(report, record)
	if err != nil {
		return err
	}
	if n != len(record) {
		return io.ErrShortWrite
	}
	if err := report.Sync(); err != nil {
		return err
	}
	return nil
}

func writeQuiesceRecovery(stderr io.Writer, result quiesceResult) {
	for _, action := range result.Stopped {
		if action.Kind == "down" {
			_, _ = fmt.Fprintf(stderr, "compose project '%s' was taken down; restart it yourself when needed\n", action.Project)
		} else {
			_, _ = fmt.Fprintf(stderr, "container '%s' was stopped; 'docker start %s' brings it back\n", action.Name, action.Name)
		}
	}
	for _, action := range result.Uncertain {
		if action.Kind == "down" {
			_, _ = fmt.Fprintf(stderr, "warning: docker compose down for project '%s' failed; some users may already be stopped or removed. State is uncertain. Inspect the project and restart only what is needed; docker-extras will not restart anything.\n", action.Project)
		} else {
			_, _ = fmt.Fprintf(stderr, "warning: docker stop for container '%s' failed; it may already be stopped. State is uncertain. Inspect it and restart it manually if needed; docker-extras will not restart anything.\n", action.Name)
		}
	}
}
