# closeyourit-agent

Server monitoring agent for [CloseYourIt](https://closeyour.it), the open source tracker for errors,
logs, uptime and servers. Every 60 seconds it collects CPU, memory, disks (I/O and SMART), network,
sensors, GPUs, Docker/Podman containers, systemd services, journald errors and PostgreSQL health,
and pushes them as JSON to `POST /api/v1/servers/samples` of your CloseYourIt.

Collection is derived from the [Beszel](https://github.com/henrygd/beszel) agent (MIT, see
`NOTICE.md`); the transport is a plain HTTPS push with a bearer token.

## Install

Copy the agent token from **Monitor › Servers › Agent tokens** in your CloseYourIt, then pick one:

- **Docker**: `examples/docker-compose.yml` (image `ghcr.io/bussolabs/closeyourit-agent`).
- **Without Docker**: the binary from the [latest release](../../releases/latest) plus
  `examples/closeyourit-agent.service`.

## Configuration

| Variable | Meaning |
|---|---|
| `CYI_ENDPOINT` | Base URL of your CloseYourIt, for example `https://closeyourit.example.com`. Required. |
| `CYI_TOKEN` | Enrollment token `cyi_s_…`. Required (or `CYI_TOKEN_FILE`). |
| `CYI_INTERVAL` | Push interval, default `60s`. |
| `CYI_HOST_NAME` | Overrides the reported host name. |
| `LOG_LEVEL` | `debug`, `info`, `warn` or `error`. |

Collector settings take the `CYI_AGENT_` prefix (for example `CYI_AGENT_GPU_COLLECTOR`); the
`BESZEL_AGENT_` prefix still works as a deprecated alias. Keys: `GPU_COLLECTOR`,
`EXCLUDE_CONTAINERS`, `DOCKER_HOST`, `EXTRA_FILESYSTEMS`, `SENSORS`, `PRIMARY_SENSOR`,
`SMART_INTERVAL`, `SMART_DEVICES`, `EXCLUDE_SMART`, `MEM_CALC`, `DISK_USAGE_CACHE`, `SKIP_GPU`.

- **Journald**: `SKIP_JOURNAL=true` turns it off; `JOURNAL_PRIORITY` (default `3`) and
  `JOURNAL_TAIL` (default `50`).
- **PostgreSQL**: `CYI_AGENT_DB_ENABLE` (`auto`, `true`, `false`), `CYI_AGENT_DB_SOCKET`,
  `CYI_AGENT_DB_USER`, `CYI_AGENT_DB_DBNAME`, `CYI_AGENT_DB_TOP_TABLES`; the standard libpq
  variables work too.

## Actions

After the first push the agent keeps a per-host credential in
`/var/lib/closeyourit-agent/credentials.json` (mode 0600). It only runs allow-listed actions
requested from CloseYourIt, never arbitrary commands:

| Action | Command |
|---|---|
| `apply_security_updates` | `unattended-upgrade --verbose` |
| `apply_all_updates` | `apt-get -y upgrade` (non-interactive, keeps existing config files) |
| `reboot` | `systemctl reboot` |

## Development

```bash
mise install   # Go version pinned in mise.toml
make test
make build     # ./closeyourit-agent
```

The wire format is the `servers-sample/v1` contract in `contracts/servers-sample/v1`.

The Go module path is `github.com/bussolabs/closeyourit-agent`, the upstream repository, not this
one: build from a clone of this repository with `make build`; `go install` by module path is not
supported.

## Contributing

Pull requests are welcome. This repository is a published copy: accepted changes are applied to the
upstream source and come back here with the next release.

## License

MIT, see `LICENSE`. Portions derived from Beszel keep their MIT notice in `LICENSE.beszel`.
