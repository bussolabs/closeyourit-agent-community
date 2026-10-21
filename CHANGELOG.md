# Changelog

## [Unreleased]

## [0.11.3] - 2026-10-21

### Fixed

- **L'agente si scarica senza accesso ai nostri sistemi.** Ogni versione esce anche sulla copia pubblica su GitHub, con i file per Linux e l'immagine Docker. Aggiornati anche gli strumenti di base, che avevano falle note. (CYAG-24)

## [0.11.2] - 2026-10-07

### Fixed

- **La copia pubblica dell'agente supera di nuovo i controlli di rilascio.** Passando da un passo all'altro gli script perdevano il permesso di esecuzione e la verifica si fermava. Ora la copia viaggia in un unico archivio che conserva i permessi.

## [0.11.1] - 2026-10-07

### Fixed

- **La copia pubblica dell'agente torna a uscire a ogni rilascio.** Un test citava un indirizzo interno e il controllo che protegge la copia pubblica bloccava la pubblicazione. (CYAG-23)

## [0.11.0] - 2026-10-07

### Changed

- **Gli agent della flotta passano ad app.closeyour.it con un comando.** Il rollout sposta l'indirizzo del server su ogni macchina, e se una non risponde torna da sola a www. (CYRA-995)

### Fixed

- **L'agente non scrive più file con nome fisso in /tmp.** Lo stato di salute sta nella cartella dati dell'agente e un collegamento piantato al suo posto viene rifiutato. Un server senza cifratura è accettato solo su localhost e il rollout verifica la chiave del bastion invece di fidarsi ogni volta. (CYAG-23)

## [0.10.0] - 2026-10-01

### Added

- **I tre computer fuori dalla flotta si aggiornano con un comando.** DGX e i due Mac vivono su
  un'altra rete e vogliono programmi diversi: restavano indietro senza che nessuno se ne
  accorgesse. Ora c'e' una procedura sola, che salta chi e' gia' aggiornato e torna indietro da
  sola se qualcosa va storto.

### Changed

- **L'agent pesa meno sul server.** Il conteggio degli aggiornamenti disponibili non si rifà più ogni minuto: si rifà quando i pacchetti cambiano, o al massimo dopo un'ora. Dopo un aggiornamento il numero cambia subito. (CYAG-21)

## [0.9.7] - 2026-09-14

### Fixed

- **Un server a cui vengono rifiutate le credenziali si ricollega da solo.** Prima smetteva di mandare dati per sempre e serviva un intervento a mano sulla macchina. Ora rifà la registrazione al giro successivo. (CYAG-19)
- **Gli invii falliti dicono il motivo.** Nel registro compare anche il codice restituito dal server, così si distingue un dato troppo grande da uno scritto male. (CYAG-19)
- **I controlli automatici tornano a girare su ogni proposta di modifica.** Le verifiche erano ferme e le modifiche passavano senza prova. Ora ogni proposta viene controllata prima di essere unita. (CITM-8)

## [0.9.6] - 2026-09-07

### Fixed

- Report systemd services that fail before their first successful activation.
- Clear the failed-service count when a never-started service returns to inactive.
