# Servers sample contract v1

Bundle canonico, offline e versionato del **campione di server monitoring** che `closeyourit-agent`
(Go) invia a `closeyourit-rails` con `POST /api/v1/servers/samples`. `schema.json` usa JSON Schema
Draft 2020-12; `fixtures/valid` deve validare contro il proprio `$defs`, `fixtures/invalid` descrive
payload che un producer deve rifiutare prima dell'invio.

Producer: `closeyourit-agent/internal/pusher/pusher.go` (struct `Payload`, riga 30). Consumer:
`closeyourit-rails/app/controllers/api/v1/servers/samples_controller.rb` (accettazione HTTP) +
`app/services/servers/ingest/normalize.rb` (traduzione e clamp) + `record.rb` (persistenza).
Prima di questo bundle il contratto viveva solo nella fixture condivisa
`closeyourit-rails/spec/fixtures/servers/agent_payload.json`, che resta il caso "completo" di
riferimento (qui `fixtures/valid/complete.json`, esteso con SMART attributes, GPU, batteria e un
container fermo idle-managed).

## Trasporto

| Aspetto | Valore | Evidenza |
|---|---|---|
| Metodo e path | `POST {endpoint}/api/v1/servers/samples` | `pusher.go:125` |
| Header | `Content-Type: application/json`, `Content-Encoding: gzip`, `Authorization: Bearer …`, `User-Agent: closeyourit-agent/<versione>` | `pusher.go:129-132` |
| Body | JSON dell'oggetto `sample`, SEMPRE gzip (`encode`) | `pusher.go:158-168` |
| Decompressione server | sniff dei magic bytes `1f 8b`, MAI `params`: il body può arrivare anche in chiaro | `samples_controller.rb:127-142` |
| Cadenza | un push per tick, default 60 s (`CYI_INTERVAL`) | `config.go:20` |
| Timeout client | 10 s | `pusher.go:98` |
| Retry client | 2 retry con backoff+jitter nello stesso tick; ogni 403 interrompe (`ErrRevoked`) | `pusher.go:104-117` |
| Cap dimensione | `Content-Length` e body decompresso ≤ `SERVERS_MAX_PAYLOAD_BYTES` = 1 MiB; la lettura gzip è limitata a MAX+1 byte (anti gzip-bomb) | `samples_controller.rb:20-27,127-137`, `app/constants/app/constants.rb:405` |
| Risposta | `202 {"data":{"accepted":true,"host_id":<uuid>,"host_token"?:"cyi_h_…"}}` | `samples_controller.rb:55-61` |
| Errori | `{"error":{"code":"R###-SERVER-###","message":…}}` | `concerns/error_rendering.rb:16-20` |

## Credenziali

Il bearer è una credenziale **agent**, org-scoped, MAI un token di progetto `cyi_`
(`concerns/agent_authentication.rb:3-6`). Due forme:

- **enrollment token** `cyi_s_…`, il codice della flotta, uguale su ogni macchina (`config.go:15`,
  `agent_authentication.rb:27-47`). Con esso l'host viene risolto o **auto-registrato per
  fingerprint** (`samples_controller.rb:40-43`).
- **host token** `cyi_h_…`, credenziale per-macchina emessa UNA volta nella risposta del primo
  arruolamento se `agent_version >= 0.4.0` (`samples_controller.rb:17,56-59,65-69`), salvata
  dall'agent e usata dai push successivi (`main.go:96-103`). Con esso il `fingerprint` del body deve
  coincidere con quello dell'host, altrimenti `403 R403-SERVER-003` (`samples_controller.rb:37-39`).
  Un host che ha già una credenziale viva non ne riceve un'altra (il push resta accettato): serve la
  finestra di riadozione aperta dal pannello (CYRA-245, `samples_controller.rb:87-103`).

`fixtures/http/auth-cases.json` copre entrambe le credenziali, l'assenza (`401 R401-SERVER-001`),
l'enrollment token revocato (stesso 401), il bearer di progetto (rifiutato come sconosciuto), la
credenziale per-host revocata (`401 R401-SERVER-002`, codice dedicato: «ripresentati con l'enrollment
token», `agent_authentication.rb:28-37`), il fingerprint estraneo e l'host revocato
(`403 R403-SERVER-002`, nessun job accodato, `samples_controller.rb:44-46`).

## Semantica

- `additionalProperties` resta consentito ovunque per evoluzioni wire additive: il Go aggiunge chiavi
  senza rinominare (`normalize.rb:5-7`) e Rails ignora ciò che non conosce.
- **Il server rifiuta soltanto**: body non JSON / non-oggetto / gzip corrotto (`422 R422-SERVER-004`),
  `fingerprint` assente o blank (`422 R422-SERVER-001`), payload oltre 1 MiB (`413 R413-SERVER-002`).
  **Tutto il resto viene tollerato, coerciato o troncato** da `Normalize` (vedi «Clamp lato server»).
  I `required`, i tipi e i tetti dello schema sono quindi **obblighi del producer**, ricavati dalle
  struct Go (campo senza `omitempty` = sempre emesso) e dai cap del server: un payload che li viola
  può essere accettato con `202` ma finire persistito monco o troncato.
- `fingerprint` è `hex(sha256(machine-id||hostname+cpu)[:24])` → 48 caratteri esadecimali
  (`internal/agent/fingerprint.go:57-58`, `agent.go:168-172`). Lo schema chiede solo `minLength: 1`
  perché il server accetta qualunque stringa non blank (la fixture Rails storica ne usa una di 24).
- `agent_version` è la versione senza prefisso `v` (`version.go:5`, goreleaser `{{.Version}}` in
  `.goreleaser.yaml:10`): Rails la confronta con `Gem::Version` per decidere l'emissione dello host
  token (`samples_controller.rb:65-69`); una stringa non parsabile vale «vecchio».
- `recorded_at` è `time.Now().UTC()` in RFC 3339 (`main.go:79`). Rails: non parsabile o oltre 1 h nel
  futuro → sostituito con `Time.current` (`normalize.rb:44-45,93-101`). È la chiave di idempotenza del
  campione insieme all'host (`record.rb:170-178`): un retry con lo stesso istante è un no-op silenzioso.
- `host` porta le chiavi **leggibili** (statici della macchina, `pusher.go:53-65`): `reboot_required`,
  `updates_available`, `security_updates_available` sono sempre emessi (niente `omitempty`, per non
  confondere `-1`/`0` con l'assenza); `-1` = sconosciuto (distro non-apt, `updates.go:15,35-63`) e Rails
  lo traduce in `nil` (`normalize.rb:126-131`). `security_updates_available` esiste dall'agent 0.8.0.
- `data` è il blocco **beszel-derived a chiavi compatte** (`system.go:193-200`): `stats` e `info` sono
  sempre presenti; `container` è sempre presente ma vale **`null` quando il demone Docker non è
  interrogabile** (campo senza `omitempty`, `agent.go:226-232`) — Rails distingue `null` (motore giù:
  preserva l'ultimo conteggio, avvisa `server_container_down` se c'erano container attesi) da `[]`
  (nessun container) (`normalize.rb:22-26,268`, `record.rb:126-130,205-224`); `systemd` è omesso quando il
  manager systemd non ha ancora dati (`~1 push su 10` lo porta, `normalize.rb:388-393`) e la sua assenza
  NON azzera lo stato systemd sull'host (`record.rb:112-118`).
- `stats.la` e `info.la` sono array Go a lunghezza fissa `[3]float64`: `omitempty` non li omette mai,
  quindi in pratica sono sempre presenti (`system.go:47,170`); lo schema li tiene opzionali perché Rails
  li legge con fallback (`normalize.rb:134-135`).
- Unità: `stats.m/mu/mb/mz/s/su/d/du` e `efs.d/du` in **GB**; `container.m` in **MB** (Rails ×1 048 576,
  `normalize.rb:236,260`); `systemd.m/mp` e `proc.m` in **byte**; `b`/`dio`/`ni` in byte cumulativi
  `[sent, recv]` / `[read, write]` / `[up, down, total up, total down]` (`system.go:44-51`); `journal.entries[].t`
  in **microsecondi** epoch, `journal.snapshots.*.t` in **secondi** epoch (`journal.go:14-30`,
  `normalize.rb:436-448`).
- Vocabolari chiusi (interi → stringa lato server): `container.h` 0..3 = none/starting/healthy/unhealthy
  (`container.go:128-133`, clamp `normalize.rb:251`); `systemd.s` 0..5 =
  active/inactive/failed/activating/deactivating/reloading e `systemd.ss` 0..4 =
  dead/running/exited/failed/unknown (`systemd.go:12-30`, `normalize.rb:30-32,350-352`); `info.os` 0..3 =
  linux/darwin/windows/freebsd; `info.ct` 0..2 (`system.go:130-145`); `database.role` ∈
  {primary, standby} (`database.go:14`, `normalize.rb:36`); `database.system_identifier` = 1..20 cifre
  come stringa (`database.go:15-19`, `normalize.rb:41,493-495`); `journal.entries[].p` 0..7 (priorità
  syslog, `journal.go:14-15`, clamp `normalize.rb:414`).
- `container.run` è esplicito per i container fermi (raccolti con `all=1`); assente = `true` per gli agent
  vecchi (`container.go:159-161`, `normalize.rb:252-255`). `im` (idle-managed) assente = `false`: un
  container che sparisce resta un guasto (`container.go:156-158,173-189`, `normalize.rb:236-238,252`).
  `img` può arrivare col prefisso del registro ripetuto: Rails lo ripulisce (`normalize.rb:270-282`).
- `smart` è keyed per device; gli `a` (attributes) viaggiano ma **non vengono persistiti**
  (`normalize.rb:367-386`). Uno `s` diverso da `PASSED` (case-insensitive) è «failing» e avvisa alla
  transizione (`record.rb:379-390`).
- `journal` è omesso senza `journalctl` o senza nulla da inviare (`journal.go:1-4`, `agent.go:195-201`);
  `entries[].c` (cursore) è la chiave di idempotenza `[host, cursor]` e l'agent lo avanza su disco solo
  dopo un push riuscito (`main.go:104-105`), quindi lo stream può essere ritrasmesso. Le chiavi di
  `snapshots` sono i nomi wire delle unit (== `systemd[].n`, `journal.go:26-27`).
- `database` è omesso su host senza database o se il collector fallisce (`database.go:1-3`,
  `postgres.go:28-44`); presente con `reachable: false` = DB rilevato ma non sondabile, sotto-oggetti
  possono mancare (`database.go:8-9`, `normalize.rb:451-458`). Solo un booleano esplicito conta: un
  `reachable` malformato NON fa scattare l'avviso di DB giù (`normalize.rb:470-473,483`).
- **Mount read-only**: l'agent li **esclude alla fonte** da `efs` (non possono riempirsi, `disk.go:155-160`),
  quindi il wire non ha alcun marcatore di sola lettura da rappresentare.

## Clamp lato server (defense-in-depth, `normalize.rb:48-63`)

| Lista / campo | Tetto | Comportamento oltre |
|---|---|---|
| `stats.proc` | 40 elementi | troncato (`normalize.rb:225`); l'agent ne manda al più 2×`PROCESSES_TOP_N` (default 10, `processes.go:14-36`) |
| `journal.entries` | 500 elementi, `m` ≤ 1024 caratteri | troncato / tagliato (`normalize.rb:49-50,408-419`); l'agent applica gli stessi cap (`journal.go:25-27`) |
| `journal.snapshots` | 10 unit × 50 righe × 512 caratteri | troncato / tagliato (`normalize.rb:51-53,426-433`); l'agent taglia a 50 righe (`journal.go:24`) |
| `database.databases`, `top_tables` | 50 elementi | troncato (`normalize.rb:58-59,545-565`) |
| `database.replication.replicas` | 20 elementi | troncato (`normalize.rb:60,534-543`) |
| `database.connections.by_state` | 20 chiavi | troncato (`normalize.rb:61,503-506`) |
| nomi/testi del blocco `database` e di `efs` | 256 caratteri | tagliato (`normalize.rb:62,328-330,568-571`) |
| `container.id` | 64 caratteri | tagliato (`normalize.rb:247`) |
| `container.h`, `journal[].p` | 0..3, 0..7 | clamp |
| percentuali derivate (`ip`, `efs.*.ip`, uso connessioni) | 0..100 (0..999.99 per l'uso connessioni) | clamp (`normalize.rb:331,338,519`) |
| testo di qualunque sorgente libera | — | `scrub` + rimozione null-byte (`normalize.rb:566-571`) |

`fixtures/valid/limits.json` sta esattamente su questi tetti (e sugli estremi dei vocabolari) e pesa
~110 KB: i cap sommati restano ben sotto il MiB del trasporto. Le fixture `invalid/*_over_limit.json` li
superano di uno.

## Esiti HTTP e disposizione dell'agent (`fixtures/http/cases.json`)

| status | code | agent (`pusher.go:140-155`, `main.go:86-93`) |
|---|---|---|
| 202 | — | ok; salva `host_token` se presente, poi `CommitJournal` |
| 403 | `R403-SERVER-002` host revocato, `R403-SERVER-003` fingerprint estraneo | `ErrRevoked`: niente retry, idle 15 minuti (`main.go:22`) |
| 401 | `R401-SERVER-001`, `R401-SERVER-002` | errore generico → retry (l'agent 0.9.x non legge il codice) |
| 413 | `R413-SERVER-002` | errore generico → retry (cieco: stesso payload) |
| 422 | `R422-SERVER-001`, `R422-SERVER-004` | errore generico → retry |
| rete | — | retry nello stesso tick, poi il tick successivo |

## File

`manifest.json` elenca ogni fixture e il relativo `$defs`; `SHA256SUMS` protegge lo snapshot
vendorizzato nei consumer (`closeyourit-rails/contracts/servers-sample/`, `closeyourit-agent`).
Per aggiornare il bundle: modificare schema/fixture, rigenerare i checksum
(`cd contracts/servers-sample/v1 && find . -type f ! -name SHA256SUMS | LC_ALL=C sort | xargs shasum -a 256 > SHA256SUMS`)
e ri-pinnare il `LOCK.json` nei consumer. Eseguire `python scripts/verify_servers_sample_contract.py`
dalla root del repository per verificare schema, fixture, tetti, matrice HTTP/auth e checksum.
