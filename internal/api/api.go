// Package api holds the types that cross the wire between the dashboard and an agent.
//
// They are plain data (no behaviour that depends on the manager), so a remote node can
// decode them without importing the manager.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"cubeherd/internal/docker"
	"cubeherd/internal/server"
)

// Version is the API major. Dashboard and agent must agree on it.
const Version = 1

// Header carries the caller's API major on every request.
const Header = "X-Cubeherd-API"

// Info identifies an agent and tells the dashboard what it can reach.
type Info struct {
	Version    string // agent build version
	API        int
	Hostname   string
	DockerHost string
	DockerOK   bool
	DockerErr  string
	DataDir    string
	Timezone   string // IANA name of the agent's display time zone
}

// View is everything the UI shows for one server.
type View struct {
	S           *server.Server
	Status      server.Status
	C           *docker.ContainerJSON
	Online      []string
	MaxPlayers  int
	MemUsed     uint64
	CrashReason string
	Since       time.Time
	OpError     string
	Restarts    int  // unexpected restarts of the current container (restart policy)
	AutoStopped bool // stopped by itzg autostop after idling
}

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

// Entry is one item of a directory listing below /data.
type Entry struct {
	Name    string
	Dir     bool
	Size    int64
	ModTime time.Time
}

// AddonFile is a JAR in the mods or plugins folder.
type AddonFile struct {
	Name     string
	Size     int64
	Uploaded bool // uploaded through Cubeherd (removable here); other files belong to itzg or the modpack
}

// Health is the backup banner of a server.
type Health struct {
	Level  string // ok | warn | crit | off
	Title  string
	Detail string
}

// Error is the JSON body of a failed request.
type Error struct {
	Error string
	Code  string // busy | notfound | dockernotfound | "" for anything else
}

// Fingerprint is the SHA-256 of a certificate's DER bytes as lower-case hex pairs joined by colons.
// The dashboard pins it when a node is added, so no certificate authority is involved.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	h := hex.EncodeToString(sum[:])
	parts := make([]string, 0, len(sum))
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// Small request and response bodies.
type (
	// ImportRequest adopts an existing itzg container.
	ImportRequest struct{ ContainerID, Name string }
	// RCONRequest and RCONReply carry one console command.
	RCONRequest struct{ Cmd string }
	RCONReply   struct{ Out string }
	// Players lists who is online.
	Players struct {
		Names []string
		Max   int
	}
)
