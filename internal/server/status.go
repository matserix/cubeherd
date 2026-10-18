package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"forgecraft/internal/docker"
)

type Status string

const (
	StatusRunning    Status = "running"
	StatusStarting   Status = "starting"
	StatusPaused     Status = "paused"
	StatusStopped    Status = "stopped"
	StatusCrashed    Status = "crashed"
	StatusMissing    Status = "missing"
	StatusUnhealthy  Status = "unhealthy"
	StatusStopping   Status = "stopping"
	StatusUpdating   Status = "updating"
	StatusInstalling Status = "installing"
	StatusPreparing  Status = "preparing" // container runs, itzg still downloads server/modpack files
	StatusBackingUp  Status = "backingup"
	StatusRestoring  Status = "restoring"
	StatusImporting  Status = "importing"
)

var statusLabel = map[Status]string{
	StatusRunning: "Läuft", StatusStarting: "Startet", StatusPaused: "Pausiert", StatusStopped: "Gestoppt",
	StatusCrashed: "Abgestürzt", StatusMissing: "Nicht erstellt", StatusUnhealthy: "Reagiert nicht",
	StatusStopping: "Stoppt", StatusUpdating: "Aktualisiert", StatusInstalling: "Wird eingerichtet",
	StatusPreparing: "Wird eingerichtet", StatusBackingUp: "Sichert", StatusRestoring: "Stellt wieder her", StatusImporting: "Welt wird eingespielt",
}

func (s Status) Label() string { return statusLabel[s] }

// CSS maps the status to the prototype's pill classes.
func (s Status) CSS() string {
	switch s {
	case StatusStopping, StatusUpdating, StatusInstalling, StatusUnhealthy, StatusPreparing, StatusBackingUp, StatusRestoring, StatusImporting:
		return "starting"
	case StatusMissing:
		return "stopped"
	}
	return string(s)
}

// Live reports whether the server process accepts commands.
func (s Status) Live() bool { return s == StatusRunning || s == StatusUnhealthy }

// Up reports whether the container is running (including paused and starting).
func (s Status) Up() bool {
	return s == StatusRunning || s == StatusUnhealthy || s == StatusPaused || s == StatusStarting || s == StatusPreparing
}

// Busy reports a Forgecraft action in progress.
func (s Status) Busy() bool {
	return s == StatusStopping || s == StatusUpdating || s == StatusInstalling || s == StatusBackingUp || s == StatusRestoring || s == StatusImporting
}

// Derive computes the display status. op is a running Forgecraft action ("" if none).
func Derive(c *docker.ContainerJSON, op Status, dataDir string) Status {
	if op != "" {
		return op
	}
	if c == nil {
		return StatusMissing
	}
	st := c.State
	switch {
	case st.Restarting:
		return StatusCrashed
	case st.Running:
		// itzg autopause SIGSTOPs Java inside the container and drops this marker file.
		if _, err := os.Stat(filepath.Join(dataDir, ".paused")); dataDir != "" && err == nil {
			return StatusPaused
		}
		if st.Health != nil {
			switch st.Health.Status {
			case "starting":
				return StatusStarting
			case "unhealthy":
				return StatusUnhealthy
			}
		}
		return StatusRunning
	case st.Status == "created":
		return StatusStopped
	case st.ExitCode == 0 || st.ExitCode == 143 || st.ExitCode == 137 && !st.OOMKilled:
		return StatusStopped
	}
	return StatusCrashed
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?$>=]*[A-Za-z~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[()][A-Za-z0-9]|\x1b[=>]`)

// CleanLine strips ANSI codes and carriage returns from a log line.
func CleanLine(l string) string {
	l = ansi.ReplaceAllString(l, "")
	if i := strings.LastIndexByte(l, '\r'); i >= 0 && i < len(l)-1 {
		l = l[i+1:]
	}
	// Paper's console prompt leaks into tty logs as ">....".
	return strings.TrimPrefix(strings.TrimRight(l, "\r"), ">....")
}

// Level classifies a log line for colouring: info, warn, error, chat, init.
func Level(l string) string {
	switch {
	case strings.Contains(l, "ERROR") || strings.Contains(l, "/FATAL") || strings.Contains(l, "requires running the server with Java"):
		return "error"
	case strings.Contains(l, "WARN"):
		return "warn"
	case strings.Contains(l, "Exception"):
		return "error"
	case chatLine.MatchString(l):
		return "chat"
	case strings.HasPrefix(l, "[init]") || strings.HasPrefix(l, "[mc-image-helper]") || strings.HasPrefix(l, "[Autopause"):
		return "init"
	}
	return "info"
}

var chatLine = regexp.MustCompile(`\]: <[A-Za-z0-9_]{2,16}> `)

// crashPatterns are telling messages, most specific first.
var crashPatterns = []struct {
	match string
	hint  string
	next  int // following lines to include
}{
	{"requires running the server with Java", "", 0},
	{"The server has stopped responding", "Der Watchdog hat den Server beendet (oft nach dem Aufwachen aus der Autopause oder bei starker Überlast).", 0},
	{"Missing or unsupported mandatory dependencies", "", 2},
	{"OutOfMemoryError", "Zu wenig Arbeitsspeicher. RAM in den Einstellungen erhöhen.", 0},
	{"FAILED TO BIND TO PORT", "Der Port ist schon belegt.", 0},
	{"Address already in use", "Der Port ist schon belegt.", 0},
	{"agree to the EULA", "", 0},
	{"Exception in server tick loop", "", 1},
	{"Encountered an unexpected exception", "", 1},
	{"Failed to start the minecraft server", "", 0},
}

// CrashReason picks the most telling lines from the end of a log.
func CrashReason(lines []string) string {
	if r := KnownCrashReason(lines); r != "" {
		return r
	}
	return fallbackReason(lines)
}

// KnownCrashReason only returns a reason when a well-known failure message is found.
func KnownCrashReason(lines []string) string {
	for _, p := range crashPatterns {
		for i := len(lines) - 1; i >= 0; i-- {
			if !strings.Contains(lines[i], p.match) {
				continue
			}
			out := []string{trimLogPrefix(lines[i])}
			for k := 1; k <= p.next && i+k < len(lines); k++ {
				out = append(out, strings.TrimSpace(trimLogPrefix(lines[i+k])))
			}
			if p.hint != "" {
				out = append(out, p.hint)
			}
			return strings.Join(out, "\n")
		}
	}
	return ""
}

func fallbackReason(lines []string) string {
	// Fallback: the last error line that is not part of a stack trace or thread dump.
	for i := len(lines) - 1; i >= 0; i-- {
		l := trimLogPrefix(lines[i])
		t := strings.TrimSpace(l)
		if Level(lines[i]) != "error" || t == "" || l != strings.TrimLeft(l, " \t") ||
			strings.HasPrefix(t, "at ") || strings.HasPrefix(t, "---") || strings.HasPrefix(t, "PID:") ||
			strings.HasPrefix(t, "Stack:") || strings.HasPrefix(t, "Current Thread") || strings.Contains(t, "mc-server-runner") {
			continue
		}
		return t
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.Contains(l, "mc-server-runner") {
			return trimLogPrefix(l)
		}
	}
	return ""
}

var logPrefix = regexp.MustCompile(`^(>\.{4})?\[[0-9:]+ [A-Z]+\]: |^\[[0-9:]+\] \[[^\]]+\]: `)

func trimLogPrefix(l string) string { return logPrefix.ReplaceAllString(l, "") }

var rconNoise = regexp.MustCompile(`Thread RCON Client .* (started|shutting down)$`)

// DashboardNoise reports log lines caused by Forgecraft's own RCON queries.
func DashboardNoise(l string) bool { return rconNoise.MatchString(l) }

// StartupLine classifies a log line during startup: "done" once the server accepts players,
// "install" while itzg's init/mc-image-helper still prepares files, "server" for server output.
func StartupLine(l string) string {
	switch {
	case strings.Contains(l, "Done (") && strings.Contains(l, "help"):
		return "done"
	case strings.HasPrefix(l, "[init]") || strings.HasPrefix(l, "[mc-image-helper]"):
		return "install"
	case strings.Contains(l, "INFO]") || strings.Contains(l, "/INFO]") || strings.Contains(l, "WARN]"):
		return "server"
	}
	return ""
}
