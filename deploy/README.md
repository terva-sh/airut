# Deploying the sample bot with systemd

This guide installs `cmd/airut-example` as a systemd service. The bot is
one static binary that runs as its own system user. Its secrets are
systemd credentials, encrypted on the host and decrypted only for the
bot's service. The files are:

| File | Install as |
|---|---|
| [systemd/airut-example.service](systemd/airut-example.service) | `/etc/systemd/system/airut-example.service` |
| [systemd/airut-example.sysusers](systemd/airut-example.sysusers) | `/etc/sysusers.d/airut-example.conf` |
| [../docs/config.example.toml](../docs/config.example.toml), edited | `/etc/airut-example/airut.toml` |

Copy the unit rather than linking it to a checkout, and treat your copy
as the host's own file. Paths, the user, and credential names are facts
about the host, and a deployment should not change because airut's
reference unit did. Run every step below as root on the host that runs
the bot.

## Build and install the binary

The build must be cgo-free and use the goolm tag, as every airut build
does:

```sh
CGO_ENABLED=0 go build -tags goolm -trimpath -o airut-example ./cmd/airut-example
install -D -m 0755 airut-example /usr/local/lib/airut-example/airut-example
```

To build a pinned release instead of a checkout, use
`CGO_ENABLED=0 GOBIN="$PWD" go install -tags goolm -trimpath terva.sh/airut/cmd/airut-example@vX.Y.Z`,
then install the binary as above.

## Create the user

```sh
install -m 0644 deploy/systemd/airut-example.sysusers /etc/sysusers.d/airut-example.conf
systemd-sysusers
```

The unit uses this static user rather than `DynamicUser=`, so the state
directory keeps one owner across runs and a restored backup needs no
`chown`. `DynamicUser=` also works if you prefer it. State then lives
under `/var/lib/private/airut-example`.

## Write the configuration

Start from `docs/config.example.toml` and keep only the tables the bot
uses.

- **State paths.** Give the session store and the Matrix state directory
  absolute paths under `/var/lib/airut-example`, the unit's
  `StateDirectory`. Everywhere else is read-only to the service.
- **Secrets.** Write each secret as a credential reference, such as
  `token = { credential = "discord-token" }`. The id is the name before
  the colon on the unit's `LoadCredentialEncrypted=` line.
- **Owners.** List who may change the bot's settings from chat in
  `[app] owners`.
- **No recovery key.** Leave `[matrix] recovery_key` commented out, as
  the example does. The bot refuses to start on a credential it cannot
  read, and the unit does not load the recovery key.

```sh
install -d -m 0755 /etc/airut-example
install -m 0640 -o root -g airut-example airut.toml /etc/airut-example/airut.toml
```

The file holds no secrets. Keeping it unreadable to other users still
keeps the account names and access lists private.

## Add the credentials

Encrypt each secret on the host itself, because `systemd-creds` binds a
credential to the host's key. The `--name` must be the id the unit
loads it as.

```sh
install -d -m 0700 /etc/credstore.encrypted
systemd-ask-password -n "discord-token:" \
  | systemd-creds encrypt --name=discord-token - /etc/credstore.encrypted/airut-example-discord-token
```

Repeat for `matrix-password` and `proxy-api-key`. Remove the unit's line
for any credential the bot does not use, because the unit does not start
while one it loads is missing. Never put a secret on a command line or in
a file in the clear, since both outlast the command.

## Start it

```sh
install -m 0644 deploy/systemd/airut-example.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now airut-example
journalctl -u airut-example -f
```

The log holds ids, kinds, and sizes, and never a token or message text.
On its first start the Matrix adapter logs in with the password and
stores its session and crypto keys in the state directory.

## Cross-sign the Matrix device

Clients show an unsigned device's messages as unverified. Sign the device
once, with the bot stopped, in the service's own context, so the state
directory and credentials are the service's:

```sh
systemctl stop airut-example
systemd-run --pty --wait --collect \
  -p User=airut-example -p Group=airut-example -p StateDirectory=airut-example \
  -p LoadCredentialEncrypted=discord-token:/etc/credstore.encrypted/airut-example-discord-token \
  -p LoadCredentialEncrypted=matrix-password:/etc/credstore.encrypted/airut-example-matrix-password \
  -p LoadCredentialEncrypted=proxy-api-key:/etc/credstore.encrypted/airut-example-proxy-api-key \
  /usr/local/lib/airut-example/airut-example -config /etc/airut-example/airut.toml -cross-sign
systemctl start airut-example
```

Pass one `LoadCredentialEncrypted=` for each credential the configuration
names, because the setup reads the whole file.

- **An account with no cross-signing identity.** The setup makes one and
  prints the recovery key once. Keep it off the host, in a password
  manager or similar. It can sign any device as the bot's, so it is never
  written to the host, its backups, or a log.
- **An account that has one.** If the state directory was re-created,
  run the same command again. It asks for the recovery key without echo
  and signs the new device, so the key still never rests on the host.

The unit's commented `matrix-recovery-key` line, with the example's
commented `recovery_key`, lets the bot sign itself at start instead.
Uncomment both only if you accept the key living on the host, and comment
both out again afterwards.

## Back it up

Back up `/var/lib/airut-example`. Its Matrix crypto store cannot be
rebuilt. Lose it, and the bot can no longer read encrypted history it
could read before. The session database holds conversations, memories,
and settings changed from chat.

Both are SQLite databases in WAL mode, so copying them while the bot runs
can catch them mid-write. Stop the service for the copy, or snapshot the
filesystem with the service stopped, and start it again afterwards.

A backup of the whole host also holds `/etc/credstore.encrypted` and the
host key that decrypts it, at `/var/lib/systemd/credential.secret`.
Anyone with such a backup can read every credential, so protect it as a
secret.

## Upgrade

Back up the state directory first: a new version can add migrations to
the session database, and a database migrated forward does not open in
an older build. Then install the new binary and restart:

```sh
install -m 0755 airut-example /usr/local/lib/airut-example/airut-example
systemctl restart airut-example
```

## Choices left to the host

- **Egress.** Discord's addresses are not fixed, so a bot that serves
  Discord cannot restrict its outgoing traffic by address. A bot that
  serves only Matrix and local models can: add `IPAddressAllow=` for the
  homeserver and the model providers, and `IPAddressDeny=any`.
- **Metrics.** Bind `[metrics] listen` to loopback, or to an address
  only the scraper reaches, since nothing guards the endpoint.
- **Delivery.** Build the binary where you trust the toolchain, and
  install it with a pinned checksum.
