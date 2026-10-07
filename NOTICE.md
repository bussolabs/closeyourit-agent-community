# NOTICE

Portions of this software are derived from **Beszel**
(https://github.com/henrygd/beszel), Copyright (c) 2024 henrygd,
licensed under the MIT License (full text in `LICENSE.beszel`).

Derived portions: the metrics-collection code under `internal/agent/`
(cpu, disk, network, sensors, docker, systemd, GPU, SMART, battery, ZFS,
deltatracker, health, utils) and the data structures under
`internal/entities/`. The transport layer (WebSocket/SSH/CBOR) has been
removed and replaced by an HTTP JSON push (`internal/pusher/`) toward
CloseYourIt.
