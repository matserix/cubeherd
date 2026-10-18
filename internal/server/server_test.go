package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forgecraft/internal/docker"
)

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Survival 2026":                            "survival-2026",
		"Größe & Spaß!":                            "groesse-spass",
		"  --All the Mods 10--  ":                  "all-the-mods-10",
		"Ein sehr langer Servername mit Überlänge": "ein-sehr-langer-serverna",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnvAndCompose(t *testing.T) {
	s := &Server{ID: "test", Kind: KindSoftware, Type: "PAPER", Version: "1.21.10", JavaTag: "java21", Port: 25570, MemoryGB: 6,
		Autopause: true, Props: Props{MOTD: `Preis: 5$ "neu"`, MaxPlayers: 10, OnlineMode: true}, Whitelist: []string{"Steve"},
		ExtraEnv: []KV{{"MAX_PLAYERS", "12"}, {"SPAWN_PROTECTION", "0"}}}
	env := map[string]string{}
	for _, kv := range s.Env("") {
		env[kv.Key] = kv.Value
	}
	if env["TYPE"] != "PAPER" || env["MEMORY"] != "4864M" || env["ENABLE_AUTOPAUSE"] != "TRUE" {
		t.Errorf("env: %v", env)
	}
	if env["MAX_PLAYERS"] != "12" || env["SPAWN_PROTECTION"] != "0" {
		t.Errorf("extra env must override and append: %v", env)
	}
	if env["EXISTING_WHITELIST_FILE"] != "SYNCHRONIZE" {
		t.Errorf("whitelist sync missing")
	}
	c := Compose(s, "")
	for _, want := range []string{"name: mc-test", `mem_limit: "6g"`, "image: itzg/minecraft-server:java21", `MOTD: "Preis: 5$$ \"neu\""`, `- "25570:25565"`, `forgecraft.id: "test"`} {
		if !strings.Contains(c, want) {
			t.Errorf("compose missing %q:\n%s", want, c)
		}
	}
	s.DataVol = "abc123"
	if c := Compose(s, ""); !strings.Contains(c, "external: true") || !strings.Contains(c, `"abc123:/data"`) {
		t.Errorf("volume compose:\n%s", c)
	}
}

func TestModpackEnv(t *testing.T) {
	s := &Server{Kind: KindCurseForge, Pack: &Pack{Slug: "all-the-mods-10", Version: "123"}, MemoryGB: 8}
	env := FormatEnv(s.Env("key$1"))
	for _, want := range []string{"TYPE=AUTO_CURSEFORGE", "CF_SLUG=all-the-mods-10", "CF_FILE_ID=123", "CF_API_KEY=key$1"} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %s in\n%s", want, env)
		}
	}
	if !strings.Contains(env, "MALLOC_ARENA_MAX=2") {
		t.Errorf("modpack servers need MALLOC_ARENA_MAX:\n%s", env)
	}
	if strings.Contains(env, "VERSION=") {
		t.Errorf("modpack servers must not pin VERSION:\n%s", env)
	}
}

func TestParseEnv(t *testing.T) {
	kvs, err := ParseEnv("# Kommentar\nA=1\n\nB = x=y\n")
	if err != nil || len(kvs) != 2 || kvs[1].Key != "B" || kvs[1].Value != " x=y" {
		t.Fatalf("got %v %v", kvs, err)
	}
	if _, err := ParseEnv("kein gleich"); err == nil {
		t.Error("expected error")
	}
}

func TestCleanLineAndLevel(t *testing.T) {
	raw := "\x1b[?2026$p\x1b[?2027$p\x1b[0;39m[15:05:32 WARN]: Can't keep up!\x1b[m"
	if got := CleanLine(raw); got != "[15:05:32 WARN]: Can't keep up!" {
		t.Errorf("CleanLine = %q", got)
	}
	if Level("[15:05:32 WARN]: x") != "warn" || Level("[12:00:00 INFO]: <Steve> hallo") != "chat" || Level("[init] Starting") != "init" {
		t.Error("levels")
	}
}

func TestCrashReason(t *testing.T) {
	lines := []string{
		"[init] Starting the Minecraft server...",
		"Minecraft 26.1 and newer requires running the server with Java 25 or above.",
		"2026-10-18T14:42:44.235Z\tWARN\tmc-server-runner\tMinecraft server failed. DO NOT report this line as an error.",
	}
	if got := CrashReason(lines); !strings.Contains(got, "Java 25") {
		t.Errorf("CrashReason = %q", got)
	}
	dump := []string{
		">....[15:18:33 ERROR]: The server has stopped responding! This is (probably) not a Paper bug.",
		">....[15:18:33 ERROR]: Current Thread: Server thread",
		">....[15:18:33 ERROR]: \tPID: 171 | Suspended: false",
		">....[15:18:33 ERROR]: ------------------------------",
	}
	if got := CrashReason(dump); !strings.HasPrefix(got, "The server has stopped responding") {
		t.Errorf("CrashReason(dump) = %q", got)
	}
}

func TestDerive(t *testing.T) {
	dir := t.TempDir()
	c := &docker.ContainerJSON{}
	c.State.Running = true
	c.State.Health = &struct{ Status string }{"healthy"}
	if got := Derive(c, "", dir); got != StatusRunning {
		t.Errorf("running: %s", got)
	}
	os.WriteFile(filepath.Join(dir, ".paused"), nil, 0o644)
	if got := Derive(c, "", dir); got != StatusPaused {
		t.Errorf("paused: %s", got)
	}
	c.State.Running, c.State.ExitCode = false, 1
	if got := Derive(c, "", dir); got != StatusCrashed {
		t.Errorf("crashed: %s", got)
	}
	c.State.ExitCode = 0
	if got := Derive(c, "", dir); got != StatusStopped {
		t.Errorf("stopped: %s", got)
	}
	if got := Derive(nil, StatusInstalling, dir); got != StatusInstalling {
		t.Errorf("op wins: %s", got)
	}
}

func TestHeapMB(t *testing.T) {
	for _, c := range []struct {
		gb     int
		modded bool
		want   int
	}{{2, false, 1024}, {4, false, 3072}, {6, true, 4096}, {7, true, 4864}, {10, true, 7168}, {12, true, 8448}, {1, false, 512}} {
		if got := HeapMB(c.gb, c.modded); got != c.want {
			t.Errorf("HeapMB(%d, %v) = %d, want %d", c.gb, c.modded, got, c.want)
		}
	}
}

func TestStartupLine(t *testing.T) {
	for l, want := range map[string]string{
		"[mc-image-helper] 15:57:03.417 INFO  : Downloaded mod file mods/x.jar": "install",
		"[init] Starting the Minecraft server...":                               "install",
		"[16:05:39] [main/INFO] [minecraft/Bootstrap]: ModernFix reached":       "server",
		`[15:31:05] [Server thread/INFO]: Done (1.586s)! For help, type "help"`: "done",
		"Starting net.neoforged.serverstarterjar":                               "",
	} {
		if got := StartupLine(l); got != want {
			t.Errorf("StartupLine(%q) = %q, want %q", l, got, want)
		}
	}
	if Level("[16:05:39] [main/WARN] [mixin/]: Error loading class (java.lang.RuntimeException: x)") != "warn" {
		t.Error("WARN lines with exceptions must stay warnings")
	}
}
