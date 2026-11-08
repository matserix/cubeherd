# Hosting servers for other people

Cubeherd can run Minecraft servers for customers. This guide describes a setup that keeps the risks small and says
plainly what remains. Read it before you give anyone you do not know an account.

## What you are protecting, and from whom

A customer's server runs in a Docker container on your host, and customers may upload their own JARs. A malicious JAR
is code that runs inside that container. Cubeherd limits what the container may do to the host and to its
neighbours, and what the customer may do in the dashboard. It does not turn a shared kernel into a strong boundary:
containers share the host's Linux kernel, so a kernel vulnerability can still lead out of one. For customers you do not
know, run a sandboxed runtime (below) or give each customer their own virtual machine.

## Recommended layout

- Run the **dashboard on its own VM without the Docker socket** (`CUBEHERD_LOCAL=false`). It then has no way to start
  containers itself; an attacker who takes it over reaches only the agents' API, not a Docker socket.
- Run one **agent per Docker host** that carries customer servers. Put the Minecraft ports on the public address and
  keep the agent port (8421) reachable only from the dashboard (firewall).
- Put the dashboard **behind a reverse proxy with HTTPS** and set the trusted proxies, the public address and HSTS
  (see "Behind a reverse proxy" in the README).
- Keep your own administration on separate accounts with two-factor sign-in.

## Container profile

Set these on every host that runs customer servers (the environment of the `cubeherd` process or agent):

| Variable | Meaning |
|---|---|
| `CUBEHERD_HARDENED=true` | Switch the profile on. |
| `CUBEHERD_RUNTIME=runsc` | Optional OCI runtime, for example gVisor (`runsc`) or Kata. Install and register it in Docker first. |
| `CUBEHERD_PIDS_LIMIT` | Processes per container (default 1024). |
| `CUBEHERD_NOFILE` | Open files per process (default 65536). |
| `CUBEHERD_LOG_MAX_MB` | Size of one log file, three are kept (default 10). |

The profile drops all Linux capabilities except the five the image needs to fix ownership of `/data` and then run
the server as an unprivileged user, sets `no-new-privileges`, bounds processes, open files and log size, mounts a
256 MB private `/tmp` and uses a proper init. It takes effect when a server is next started or recreated.

Two notes from testing against `itzg/minecraft-server`:

- With **autopause** the image needs the raw-socket capability and a file capability on its helper, and
  `no-new-privileges` would switch the latter off. So servers with autopause get `NET_RAW` and no
  `no-new-privileges`. If you want the stricter setting, do not offer autopause to customers.
- `/tmp` is not mounted `noexec`, because Java extracts native libraries there.

A hostile process inside a hardened container (as root in the container) could not mount a file system, open a raw
socket, create a device node, or start more processes than the limit.

## Accounts

1. In *Settings → User accounts* turn on the multi-user mode, in *Access → E-mail* configure SMTP, and set the public
   address in the settings so that links in e-mails never come from the request.
2. In *Access → Account security* choose:
   - **Self-registration:** *With approval by an admin* for a public offer. Require a confirmed e-mail address.
   - **Two-factor sign-in:** at least *Required for accounts with administrative rights*, with a grace period.
   - **E-mail on sign-in:** *On a new device*.
3. A new account has no rights and no server. Approving it only activates it. You, or your billing system once
   there is an API, create the server and give the **Customer** role on that server.

The Customer role runs and configures the game, files, add-ons and backups. It cannot change memory, CPU, ports,
the raw environment, or create and delete servers; those are yours. Customers see only their own servers; a server
they may not see answers 404.

## Limits per server

Set them when you create the server or later in *Settings* of the server (right *Resources*):

- **Memory** and **CPU** are hard Docker limits.
- **Disk space:** measured every minute. From 90 percent a warning, from 100 percent nothing more is written through
  Cubeherd and the server cannot start, from 120 percent a running server is stopped. Data is never deleted. This
  is a watchdog and not a file system quota: a running server can write between two measurements. For a hard limit
  put customer data on a file system with project quotas (XFS) or use one volume per server.
- **Suspend** (right *Suspend servers*) stops a server and blocks starting it until you release it. Suspending an
  account (*Access → Users → Lock account*) ends all of its sessions.

## Checklist before the first customer

- [ ] Dashboard on HTTPS behind a proxy, trusted proxies and public address set
- [ ] Dashboard VM without the Docker socket, agents firewalled to the dashboard
- [ ] `CUBEHERD_HARDENED=true` on all customer hosts (and a sandboxed runtime if customers are strangers)
- [ ] SMTP working, test mail sent
- [ ] Two-factor required for your own admin accounts
- [ ] Registration set to approval, e-mail confirmation on
- [ ] Customer role used for every customer; nobody holds `server.advanced`, `files.write` on the whole host or
      node rights by accident
- [ ] Backups of `cubeherd.db`, `cubeherd.json` and `access.key`
- [ ] Host updates and Docker updates scheduled
