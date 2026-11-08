// Package access is the optional multi-user mode: accounts, roles, grants, sessions, the audit log and
// single sign-on. Everything lives in one SQLite file; without it Cubeherd keeps its single password.
package access

import "slices"

// Perm is one kind of action a role can allow.
type Perm string

const (
	ServerView      Perm = "server.view"
	ServerPower     Perm = "server.power"
	ServerConsole   Perm = "server.console"
	ServerCommand   Perm = "server.command"
	ServerPlayers   Perm = "server.players"
	ServerSettings  Perm = "server.settings"
	ServerResources Perm = "server.resources"
	ServerNetwork   Perm = "server.network"
	ServerAdvanced  Perm = "server.advanced"
	ServerSuspend   Perm = "server.suspend"
	ServerUpdate    Perm = "server.update"
	ServerCreate    Perm = "server.create"
	ServerDelete    Perm = "server.delete"
	FilesRead       Perm = "files.read"
	FilesWrite      Perm = "files.write"
	WorldExport     Perm = "world.export"
	WorldImport     Perm = "world.import"
	AddonsManage    Perm = "addons.manage"
	BackupsView     Perm = "backups.view"
	BackupsCreate   Perm = "backups.create"
	BackupsRestore  Perm = "backups.restore"
	BackupsDelete   Perm = "backups.delete"
	StatsServer     Perm = "stats.server"
	StatsNode       Perm = "stats.node"
	NodesView       Perm = "nodes.view"
	NodesManage     Perm = "nodes.manage"
	SettingsManage  Perm = "settings.manage"
	UsersManage     Perm = "users.manage"
	RolesManage     Perm = "roles.manage"
	SSOManage       Perm = "sso.manage"
	UsersApprove    Perm = "users.approve"
	AuditView       Perm = "audit.view"
	AuditExport     Perm = "audit.export"
)

// Level is how wide a scope is. A permission is only effective in grants that are at least as wide as its Lowest level.
type Level int

const (
	LevelGlobal Level = iota
	LevelNode
	LevelServer
)

// Info describes a permission for the role editor.
type Info struct {
	ID        Perm
	Group     string
	Lowest    Level // the narrowest scope it can be granted on
	Sensitive bool  // gives access to secrets or operator control
}

// Catalog lists every permission in the order the role editor shows them.
var Catalog = []Info{
	{ServerView, "server", LevelServer, false},
	{ServerPower, "server", LevelServer, false},
	{ServerConsole, "server", LevelServer, false},
	{ServerCommand, "server", LevelServer, true},
	{ServerPlayers, "server", LevelServer, false},
	{ServerSettings, "server", LevelServer, false},
	{ServerResources, "server", LevelServer, false},
	{ServerNetwork, "server", LevelServer, false},
	{ServerAdvanced, "server", LevelServer, true},
	{ServerSuspend, "server", LevelServer, true},
	{ServerUpdate, "server", LevelServer, false},
	{ServerCreate, "server", LevelNode, false},
	{ServerDelete, "server", LevelServer, false},
	{FilesRead, "files", LevelServer, true},
	{FilesWrite, "files", LevelServer, true},
	{WorldExport, "files", LevelServer, false},
	{WorldImport, "files", LevelServer, false},
	{AddonsManage, "addons", LevelServer, false},
	{BackupsView, "backups", LevelServer, false},
	{BackupsCreate, "backups", LevelServer, false},
	{BackupsRestore, "backups", LevelServer, false},
	{BackupsDelete, "backups", LevelServer, false},
	{StatsServer, "stats", LevelServer, false},
	{StatsNode, "stats", LevelNode, false},
	{NodesView, "admin", LevelNode, false},
	{NodesManage, "admin", LevelGlobal, true},
	{SettingsManage, "admin", LevelGlobal, true},
	{UsersManage, "access", LevelGlobal, true},
	{RolesManage, "access", LevelGlobal, true},
	{SSOManage, "access", LevelGlobal, true},
	{UsersApprove, "access", LevelGlobal, false},
	{AuditView, "access", LevelServer, false},
	{AuditExport, "access", LevelServer, false},
}

// Groups are the sections of the catalogue, in display order.
var Groups = []string{"server", "files", "addons", "backups", "stats", "admin", "access"}

var catalog = func() map[Perm]Info {
	m := make(map[Perm]Info, len(Catalog))
	for _, i := range Catalog {
		m[i.ID] = i
	}
	return m
}()

// Lookup returns the catalogue entry of a permission.
func Lookup(p Perm) (Info, bool) { i, ok := catalog[p]; return i, ok }

// Valid reports whether p is a known permission.
func Valid(p Perm) bool { _, ok := catalog[p]; return ok }

// Built-in role names. They are read-only; clone one to adapt it.
const (
	RoleOwner     = "Owner"
	RoleAdmin     = "Admin"
	RoleOperator  = "Operator"
	RoleModerator = "Moderator"
	RoleViewer    = "Viewer"
	RoleCustomer  = "Customer"
)

type builtin struct {
	Name, Description string
	Perms             []Perm
}

var viewer = []Perm{ServerView, ServerConsole, BackupsView}
var moderator = append(slices.Clone(viewer), ServerPower, ServerPlayers, ServerCommand)
var operator = append(slices.Clone(moderator), ServerSettings, ServerResources, ServerNetwork, ServerAdvanced, ServerUpdate, AddonsManage, BackupsCreate, BackupsRestore,
	FilesRead, FilesWrite, WorldExport, WorldImport, StatsServer)

// customer is for people who rent a server: they run and configure the game, but memory, CPU, ports and the
// raw environment are set by whoever hosts it. Create and delete stay with the host as well.
var customer = append(slices.Clone(moderator), ServerSettings, ServerUpdate, AddonsManage, BackupsCreate, BackupsRestore, BackupsDelete,
	FilesRead, FilesWrite, WorldExport, WorldImport, StatsServer)

func allPerms(without ...Perm) []Perm {
	var out []Perm
	for _, i := range Catalog {
		if !slices.Contains(without, i.ID) {
			out = append(out, i.ID)
		}
	}
	return out
}

var builtins = []builtin{
	{RoleOwner, "Everything, including accounts, roles, sign-in providers and the audit log.", allPerms()},
	{RoleAdmin, "Servers, nodes and global settings, but no accounts or audit log.", allPerms(UsersManage, RolesManage, SSOManage, AuditView, AuditExport)},
	{RoleOperator, "Runs servers: power, console, settings, files, add-ons and backups. Cannot create or delete servers.", operator},
	{RoleModerator, "Starts and stops servers, manages players and sends commands.", moderator},
	{RoleCustomer, "For people who rent a server: runs and configures the game, files, add-ons and backups, but not memory, CPU, ports or raw environment.", customer},
	{RoleViewer, "Read-only: status, live console and the list of backups.", viewer},
}
