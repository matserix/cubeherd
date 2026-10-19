package manager

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cubeherd/internal/docker"
	"cubeherd/internal/server"
)

// Unmanaged is an itzg container that Cubeherd does not manage yet.
type Unmanaged struct {
	ID      string
	Name    string
	Image   string
	Type    string
	Version string
	Port    string
	State   string
}

func isItzg(image string) bool {
	return strings.HasPrefix(image, server.Image) || strings.HasPrefix(image, "docker.io/"+server.Image)
}

// Discover lists itzg containers without a cubeherd.id label.
func (m *Manager) Discover(ctx context.Context) []Unmanaged {
	list, err := m.Docker.List(ctx, true, nil)
	if err != nil {
		return nil
	}
	var out []Unmanaged
	for _, c := range list {
		if !isItzg(c.Image) || c.Labels["cubeherd.id"] != "" || strings.HasSuffix(c.Name(), "-cubeherd-alt") {
			continue
		}
		u := Unmanaged{ID: c.ID, Name: c.Name(), Image: c.Image, State: c.State}
		for _, p := range c.Ports {
			if p.PrivatePort == 25565 && p.PublicPort > 0 {
				u.Port = strconv.Itoa(p.PublicPort)
			}
		}
		if ci, err := m.Docker.Inspect(ctx, c.ID); err == nil {
			env := ci.EnvMap()
			u.Type, u.Version = env["TYPE"], env["VERSION"]
			if u.Type == "" {
				u.Type = "VANILLA"
			}
			if u.Port == "" {
				if b := ci.HostConfig.PortBindings["25565/tcp"]; len(b) > 0 {
					u.Port = b[0].HostPort
				}
			}
		}
		out = append(out, u)
	}
	return out
}

// envFields are the variables Cubeherd maps to structured fields; everything else stays as extra env.
var envFields = map[string]bool{
	"EULA": true, "TYPE": true, "VERSION": true, "MEMORY": true, "USE_AIKAR_FLAGS": true, "ENABLE_AUTOPAUSE": true,
	"MAX_TICK_TIME": true, "MOTD": true, "MODE": true, "DIFFICULTY": true, "MAX_PLAYERS": true, "VIEW_DISTANCE": true,
	"SIMULATION_DISTANCE": true, "SEED": true, "PVP": true, "ONLINE_MODE": true, "ENABLE_WHITELIST": true,
	"WHITELIST": true, "OPS": true, "EXISTING_WHITELIST_FILE": true, "EXISTING_OPS_FILE": true,
	"MODRINTH_MODPACK": true, "MODRINTH_VERSION": true, "CF_SLUG": true, "CF_FILE_ID": true, "CF_API_KEY": true,
}

func truthy(v string) bool { v = strings.ToLower(v); return v == "true" || v == "1" || v == "yes" }

// Import converts an existing container into a managed server. The old container is
// renamed and stopped first and only removed once the new one is up.
func (m *Manager) Import(ctx context.Context, containerID, name string) (*server.Server, error) {
	ci, err := m.Docker.Inspect(ctx, containerID)
	if err != nil {
		return nil, err
	}
	id := server.Slug(name)
	if id == "" {
		return nil, errors.New(m.t("Name fehlt"))
	}
	if m.Store.Exists(id) {
		return nil, fmt.Errorf(m.t("Die ID „%s“ ist schon vergeben"), id)
	}
	env := ci.EnvMap()
	s := &server.Server{
		ID: id, Name: name, Kind: server.KindSoftware, Type: strings.ToUpper(env["TYPE"]), Version: env["VERSION"],
		MemoryGB: 4, Aikar: truthy(env["USE_AIKAR_FLAGS"]), Autopause: truthy(env["ENABLE_AUTOPAUSE"]),
		Autostart: ci.HostConfig.RestartPolicy.Name == "unless-stopped" || ci.HostConfig.RestartPolicy.Name == "always",
		Imported:  true, EULAPending: !truthy(env["EULA"]), CreatedAt: time.Now(),
	}
	if s.Type == "" {
		s.Type = "VANILLA"
	}
	if s.Version == "" {
		s.Version = "LATEST" // keep the container's behaviour: itzg resolves the newest supported version
	}
	switch s.Type {
	case "MODRINTH":
		s.Kind, s.Type = server.KindModrinth, "FABRIC"
		s.Pack = &server.Pack{Slug: env["MODRINTH_MODPACK"], Name: env["MODRINTH_MODPACK"], Version: env["MODRINTH_VERSION"], VersionLabel: env["MODRINTH_VERSION"]}
	case "AUTO_CURSEFORGE":
		s.Kind, s.Type = server.KindCurseForge, "FORGE"
		s.Pack = &server.Pack{Slug: env["CF_SLUG"], Name: env["CF_SLUG"], Version: env["CF_FILE_ID"], VersionLabel: env["CF_FILE_ID"]}
	}
	if mem := strings.TrimSuffix(strings.ToUpper(env["MEMORY"]), "G"); mem != "" {
		if n, err := strconv.Atoi(mem); err == nil && n > 0 {
			s.MemoryGB = n + max(1, n/4) // MEMORY was the heap; Cubeherd's value is the container limit
		}
	}
	if _, tag, ok := strings.Cut(ci.Config.Image, ":"); ok {
		s.JavaTag = tag
	} else {
		s.JavaTag = "latest"
	}
	s.Port = 25565
	if b := ci.HostConfig.PortBindings["25565/tcp"]; len(b) > 0 {
		if p, err := strconv.Atoi(b[0].HostPort); err == nil {
			s.Port = p
		}
	}
	atoi := func(k string) int { n, _ := strconv.Atoi(env[k]); return n }
	s.Props = server.Props{
		MOTD: env["MOTD"], Mode: strings.ToLower(env["MODE"]), Difficulty: strings.ToLower(env["DIFFICULTY"]),
		MaxPlayers: atoi("MAX_PLAYERS"), ViewDistance: atoi("VIEW_DISTANCE"), SimDistance: atoi("SIMULATION_DISTANCE"),
		Seed: env["SEED"], PVP: env["PVP"] == "" || truthy(env["PVP"]), Whitelist: truthy(env["ENABLE_WHITELIST"]),
		OnlineMode: env["ONLINE_MODE"] == "" || truthy(env["ONLINE_MODE"]),
	}
	if s.Props.MaxPlayers == 0 {
		s.Props.MaxPlayers = 20
	}
	if s.Props.Mode == "" {
		s.Props.Mode = "survival"
	}
	if s.Props.Difficulty == "" {
		s.Props.Difficulty = "easy" // Minecraft's default
	}
	if w := env["WHITELIST"]; w != "" {
		s.Whitelist = server.NormalizeNames(strings.Split(w, ","))
	}
	if o := env["OPS"]; o != "" {
		s.Ops = server.NormalizeNames(strings.Split(o, ","))
	}
	// Keep only variables the user set, not those baked into the image.
	imageEnv := map[string]bool{}
	if ie, err := m.Docker.ImageEnv(ctx, ci.Config.Image); err == nil {
		for _, kv := range ie {
			imageEnv[kv] = true
		}
	}
	for _, kv := range ci.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		if envFields[k] || imageEnv[kv] {
			continue
		}
		s.ExtraEnv = append(s.ExtraEnv, server.KV{Key: k, Value: v})
	}
	for _, mt := range ci.Mounts {
		if mt.Destination != "/data" {
			continue
		}
		if mt.Type == "volume" {
			s.DataVol = mt.Name
		} else {
			s.DataDir = mt.Source
		}
	}
	if s.DataDir == "" && s.DataVol == "" {
		return nil, errors.New(m.t("Container hat kein /data-Volume; Daten würden verloren gehen"))
	}
	if err := m.Store.Save(s, m.globals()); err != nil {
		return nil, err
	}

	oldName := strings.TrimPrefix(ci.Name, "/")
	err = m.run(s.ID, server.StatusUpdating, func(o *Op) error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		o.add("[cubeherd] " + m.t("Stoppe %s", oldName))
		if err := m.Docker.Stop(ctx, ci.ID, time.Minute); err != nil {
			return err
		}
		backup := oldName + "-cubeherd-alt"
		if err := m.Docker.Rename(ctx, ci.ID, backup); err != nil {
			return err
		}
		if err := m.compose(ctx, s, o, "up", "-d"); err != nil {
			o.add("[cubeherd] " + m.t("Übernahme fehlgeschlagen, stelle alten Container wieder her"))
			_ = m.Docker.Rename(ctx, ci.ID, oldName)
			_ = m.Docker.Start(ctx, ci.ID)
			_ = m.Store.RemoveDefinition(s.ID)
			return err
		}
		o.add("[cubeherd] " + m.t("Entferne alten Container %s", backup))
		if err := m.Docker.Remove(ctx, ci.ID, false); err != nil && !errors.Is(err, docker.ErrNotFound) {
			o.add("[cubeherd] " + m.t("Alter Container bleibt als %s erhalten: %s", backup, err.Error()))
		}
		return nil
	})
	return s, err
}
