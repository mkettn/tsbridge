# tsbridge

`tsbridge` exposes TCP services that live on a Tailscale tailnet as local
Unix sockets, from a host that is **not** itself joined to the tailnet at
the OS level. It uses Tailscale's [`tsnet`](https://pkg.go.dev/tailscale.com/tsnet)
library to join the tailnet entirely in-process (userspace WireGuard, no
TUN device, no system-wide network interface, no `tailscaled` daemon), and
proxies raw TCP bytes between each Unix socket and its tailnet target.

Typical use: a reverse proxy or app server on a box that shouldn't be a
full tailnet member connects to `/run/tsbridge/my-service.sock` instead of
directly to `remote-machine:1234` on the tailnet.

## Contents

- `main.go`, `config.go`, `bridge.go` — the `tsbridge` binary
- `config.example.yaml` — annotated example main config
- `config.d/example-service.yml` — annotated example drop-in
- `tsbridge.service` — systemd unit
- `tsbridge-sysusers.conf` — `systemd-sysusers` snippet for the service user

## Building

Requires the Go version pinned in `go.mod` (`go build`/`go get` will
transparently download the matching toolchain if `GOTOOLCHAIN=auto`, the
Go default, is in effect — this is normal and does not require any special
setup).

```sh
go build -o tsbridge .
```

`go vet ./...` and `go test ./...` are clean; the tests cover config
loading, merging, and validation (duplicate detection, malformed YAML,
nested-include rejection, etc.) without requiring network access.

## Generating a Tailscale auth key

Use a **tagged, ephemeral or reusable, ACL-scoped** auth key so the bridge
node has no more tailnet access than it needs and doesn't linger if
de-provisioned uncleanly:

1. Go to the [Tailscale admin console → Settings → Keys](https://login.tailscale.com/admin/settings/keys).
2. Generate an auth key with:
   - a tag applied (e.g. `tag:tsbridge`) instead of leaving it untagged,
     so ACLs can target it precisely (see the ACL snippet below),
   - **Ephemeral** on if you're running `tsbridge` with `ephemeral: true`
     in its config (see [Ephemeral vs. persistent](#ephemeral-vs-persistent-identity)
     below) — leave it off for a persistent node,
   - **Reusable** on only if you expect to reprovision this node from
     scratch periodically; otherwise a single-use key is fine.
3. Put the generated key in the bridge's `EnvironmentFile` (see
   [Install](#install) below) as `TS_AUTHKEY=tskey-auth-...`. Never put it
   in `config.yaml` or commit it anywhere.

### Ephemeral vs. persistent identity

- **Persistent** (`ephemeral: false`, the default if unset): the node
  keeps a stable identity and tailnet IP across restarts, stored in
  `state_dir`. Prefer this if any ACLs, DNS, or firewall rules pin to this
  node's hostname or IP — you don't want those to break every time
  `tsbridge` restarts.
- **Ephemeral** (`ephemeral: true`): the node is automatically removed
  from the tailnet on clean shutdown and re-registers fresh each start.
  Simpler if the bridge box itself is disposable/frequently rebuilt and
  nothing depends on its tailnet identity being stable, but you'll want a
  reusable auth key since a fresh registration happens on every restart.

## Config file format

Top-level fields in `config.yaml`:

| Field          | Default          | Meaning                                                                 |
|----------------|------------------|--------------------------------------------------------------------------|
| `hostname`     | `tsbridge`       | Node name shown in the tailnet / admin console                          |
| `state_dir`    | (tsnet default)  | Directory for persistent tsnet state; matters when `ephemeral: false`   |
| `ephemeral`    | `false`          | Register as an ephemeral tailnet node                                   |
| `socket_group` | (unset)          | Unix group to own every bridge socket                                   |
| `socket_mode`  | `"0660"`         | Permission bits applied to every bridge socket                          |
| `include`      | (unset)          | Glob, or list of globs, of additional files to merge in                 |
| `bridges`      | `[]`             | Inline list of `{name, listen, target}` bridge mappings                 |

Each bridge entry:

```yaml
- name: my-service          # unique identifier, used in logs and error messages
  listen: /run/tsbridge/my-service.sock   # Unix socket path to create
  target: remote-machine:1234             # host:port reachable over the tailnet
```

### `include` and `config.d/`

`include` merges in extra files that each contain their own `bridges:`
list, using the same schema as inline entries:

```yaml
include: /etc/tsbridge/config.d/*.yml
# or:
include:
  - /etc/tsbridge/config.d/*.yml
  - /etc/tsbridge/config.d/extra/*.yml
```

Rules, enforced at startup:

- Files are loaded in **sorted filename order**, globs processed in the
  order listed.
- A glob matching **zero files** is fine (e.g. an empty `config.d/` on
  first install).
- An included file that fails to parse is a **fatal startup error** — the
  log names the file and the parse error, and `tsbridge` exits non-zero.
  It does not silently skip bad files.
- An included file may **not** itself set `include` — only one level of
  nesting is supported. `tsbridge` rejects this at startup with an error
  naming the offending file.
- A duplicate `name` or `listen` path — whether both copies are inline,
  both in included files, or split across inline and included — is a
  fatal startup error naming both conflicting sources.

### Two ways to add a bridged service

**a. Edit the main config directly** — add an entry to `bridges:` in
`config.yaml`. Straightforward for a small, mostly-static set of bridges
that one person or team maintains by hand.

**b. Drop a file into `config.d/`** — create
`/etc/tsbridge/config.d/new-service.yml` with its own `bridges:` list.
Prefer this when:
- a config management tool (Ansible, Puppet, etc.) or a per-service
  installer script owns one bridge definition and shouldn't need to
  parse/edit the shared main config,
- different teams or packages each want to own their own bridge file
  without merge conflicts,
- you want to add/remove a bridge by adding/removing a single file rather
  than editing YAML in place.

Either way, `tsbridge` must be restarted to pick up the change — there is
no hot-reload (see [Non-goals](#non-goals)).

## Install

1. Build and install the binary:

   ```sh
   go build -o tsbridge .
   sudo install -m 0755 tsbridge /usr/local/bin/tsbridge
   ```

2. Create the dedicated unprivileged service user. Either:

   - **systemd-sysusers** (preferred where available):

     ```sh
     sudo install -m 0644 tsbridge-sysusers.conf /etc/sysusers.d/tsbridge.conf
     sudo systemd-sysusers
     ```

   - or plain `useradd`:

     ```sh
     sudo useradd --system --home-dir /var/lib/tsbridge --shell /usr/sbin/nologin tsbridge
     ```

3. Install the config:

   ```sh
   sudo mkdir -p /etc/tsbridge/config.d
   sudo install -m 0644 config.example.yaml /etc/tsbridge/config.yaml
   sudo install -m 0644 config.d/example-service.yml /etc/tsbridge/config.d/  # optional example
   sudo vi /etc/tsbridge/config.yaml    # edit hostname/bridges for your setup
   ```

4. Create the auth key environment file (see [above](#generating-a-tailscale-auth-key)):

   ```sh
   sudo install -m 0600 -o tsbridge -g tsbridge /dev/null /etc/tsbridge/tsbridge.env
   echo 'TS_AUTHKEY=tskey-auth-xxxxx' | sudo tee /etc/tsbridge/tsbridge.env >/dev/null
   sudo chmod 0600 /etc/tsbridge/tsbridge.env
   sudo chown tsbridge:tsbridge /etc/tsbridge/tsbridge.env
   ```

   This file must never be committed to a repository or shared config
   management tree in plaintext; treat it like any other credential.

5. Install and start the systemd unit:

   ```sh
   sudo install -m 0644 tsbridge.service /etc/systemd/system/tsbridge.service
   sudo systemctl daemon-reload
   sudo systemctl enable --now tsbridge
   ```

`RuntimeDirectory=tsbridge` and `StateDirectory=tsbridge` in the unit mean
systemd creates `/run/tsbridge` and `/var/lib/tsbridge` with the right
ownership before `tsbridge` starts — you don't need to `mkdir` them
yourself, and the Go program doesn't attempt to create them either (only
the leaf socket files inside `/run/tsbridge`, which must already exist as
a directory).

## Suggested tailnet ACL snippet

Restrict the bridge node (tagged `tag:tsbridge` when you generated its
auth key) to only the specific hosts/ports it actually proxies to. Add
something like this to your tailnet policy file, adjusting the tag
definitions and destinations for your setup:

```jsonc
{
  // Who is allowed to own/re-tag nodes tagged tag:tsbridge.
  "tagOwners": {
    "tag:tsbridge": ["autogroup:admin"],
  },

  "acls": [
    // tsbridge may only reach the specific service ports it's configured
    // to bridge -- not the whole tailnet.
    {
      "action": "accept",
      "src": ["tag:tsbridge"],
      "dst": [
        "remote-machine:1234",
        "other-machine:5678",
      ],
    },
  ],
}
```

Adjust `dst` to match every `target:` in your `bridges:`/`config.d/`
entries, and nothing more.

## Signal handling / shutdown

`tsbridge` handles `SIGTERM` and `SIGINT` by: closing every bridge's Unix
socket listener (which unlinks the socket file), waiting for in-flight
connection handlers to finish their current copy, and exiting. Under
systemd this is the normal `systemctl stop`/`restart` path.

To verify manually:

```sh
sudo systemctl stop tsbridge
ls /run/tsbridge/    # should be empty -- no leftover .sock files
```

or, running the binary directly: start it, `kill -TERM <pid>`, then check
that the configured `listen:` paths no longer exist.

## Manual end-to-end test

This exercises real bridge wiring — Unix socket in, tailnet `Dial` out —
using a throwaway TCP echo listener as the "service" instead of a real
production service. You still need a real Tailscale account and an auth
key, since joining a tailnet is exactly what `tsnet` does; there's no way
to exercise `srv.Dial` without it.

1. On any machine already on your tailnet (or the test box itself, once
   `tsbridge` joins), start a throwaway echo listener to stand in for the
   tailnet target:

   ```sh
   python3 -c '
   import socket
   s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
   s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
   s.bind(("0.0.0.0", 9999)); s.listen(1)
   while True:
       c, _ = s.accept()
       data = c.recv(4096)
       c.sendall(data)
       c.close()
   '
   ```

   Note that machine's tailnet hostname (e.g. `echo-host`).

2. Set up a test config with one inline bridge and one `config.d/` bridge,
   both pointed at the echo listener:

   ```sh
   mkdir -p /tmp/tsbridge-test/config.d /tmp/tsbridge-test/run
   cat > /tmp/tsbridge-test/config.yaml <<'EOF'
   hostname: tsbridge-manual-test
   ephemeral: true
   include: /tmp/tsbridge-test/config.d/*.yml
   bridges:
     - name: echo-inline
       listen: /tmp/tsbridge-test/run/echo-inline.sock
       target: echo-host:9999
   EOF
   cat > /tmp/tsbridge-test/config.d/echo-dropin.yml <<'EOF'
   bridges:
     - name: echo-dropin
       listen: /tmp/tsbridge-test/run/echo-dropin.sock
       target: echo-host:9999
   EOF
   ```

3. Run `tsbridge` with a real (throwaway/ephemeral) auth key:

   ```sh
   TS_AUTHKEY=tskey-auth-xxxxx ./tsbridge -config /tmp/tsbridge-test/config.yaml
   ```

   Confirm the startup log lists both bridges, one marked `[inline]` and
   one marked with the `config.d` file path.

4. From another terminal, round-trip data through both sockets:

   ```sh
   echo -n "hello via inline" | nc -U /tmp/tsbridge-test/run/echo-inline.sock
   echo -n "hello via dropin" | nc -U /tmp/tsbridge-test/run/echo-dropin.sock
   ```

   Each should echo the same bytes back. For an HTTP-shaped target instead
   of raw echo, `curl --unix-socket /tmp/tsbridge-test/run/echo-inline.sock http://localhost/` works the same way.

5. `Ctrl-C` the `tsbridge` process and confirm both `.sock` files under
   `/tmp/tsbridge-test/run/` are gone.

## Troubleshooting

**Socket permission mismatches** (client gets `EACCES`/`permission
denied`): check `socket_group`/`socket_mode` in `config.yaml` match the
group your client process runs as, and that the client's user is actually
a member of that group (`id <user>`). Remember `tsbridge` itself must be a
member of `socket_group` too, since it's the one chowning the socket.

**Checking bridge logs**:

```sh
journalctl -u tsbridge -f          # follow live
journalctl -u tsbridge -e          # jump to the end
journalctl -u tsbridge --since -10m
```

Startup logs include the fully resolved bridge list (name, listen path,
target, and source file) — check that first if a bridge you expect isn't
there.

**Verifying a client can reach the socket**:

```sh
nc -U /run/tsbridge/my-service.sock
# or
curl --unix-socket /run/tsbridge/my-service.sock http://localhost/some/path
```

If this hangs or refuses, check `journalctl -u tsbridge` for `dial ...
failed` lines — that means the Unix socket side is fine but the tailnet
target is unreachable (wrong `target:`, target service down, or an ACL
blocking `tag:tsbridge` from reaching it).

**Diagnosing include-glob / config-merge errors**: `tsbridge` fails fast
on any config problem and logs it to stderr/journal before exiting
non-zero. Common messages and what they mean:

- `loading included file .../foo.yml: parsing YAML: ...` — malformed YAML
  in that specific file; the error includes the underlying YAML parser
  message and line.
- `included file .../foo.yml sets 'include', but included files cannot
  themselves include other files` — remove the `include:` key from that
  drop-in file; only the main config may use `include`.
- `duplicate bridge name "x": defined in both inline and .../foo.yml` (or
  `duplicate listen path "..."`) — two bridge entries collide; rename or
  remove one. The message names both sources so you don't have to search.

An `include` glob that matches nothing is *not* an error — if you expected
files to be picked up and they weren't, check the glob pattern and that
the files actually end in `.yml` (or whatever extension your glob uses).

## Non-goals

- No full `tailscaled`/TUN-based setup — `tsnet` only, userspace, no
  system network interface.
- No hot-reload of `config.d/` — config errors are always fail-fast at
  startup; restart `tsbridge` to pick up config changes.
- No protocol awareness in the bridge itself — it copies raw TCP bytes in
  both directions. Anything protocol-specific (HTTP, TLS termination,
  etc.) belongs in whatever connects to the Unix socket, not here.
