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
- **Owners.** List who owns the bot in `[bot] owners`, or claim it once
  it runs, as [Claim the bot](#claim-the-bot) shows. Owners may change
  the bot's settings and profile from chat. Without one, the bot warns at
  every start, and anyone can run `status` to see that it has no owner.
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

## Claim the bot

To make someone an owner without editing the configuration, claim the
bot on the host. Run the command as the service's user, so the session
database's files stay the service's. It reads no credential, and the bot
can keep running:

```sh
runuser -u airut-example -- /usr/local/lib/airut-example/airut-example \
  claim -config /etc/airut-example/airut.toml @you:example.org
```

The running bot honours the claim within a minute. `unclaim` in place of
`claim` removes it. `status`, run by an owner, lists each claimed owner
with the time of the claim.

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

## Verify the bot to its owner

Cross-signing the device makes the bot trust its own device. Your
clients still show the bot as unverified until you verify it with emoji.
This happens once per bot, and covers every device you use. You compare
the emoji on the host's terminal, so a homeserver that tampers with the
exchange cannot also change what you compare.

Before you start:

- **Cross-sign the device first,** as above. The bot signs you with its
  account's user-signing key, and only a cross-signed device holds it.
- **Open an encrypted direct chat with the bot** from your Element, and
  let the running bot join it. Element ignores a verification request
  from a user it shares no encrypted room with. `-verify` refuses, and
  sends nothing, when it finds no such chat. It does not make one,
  because you would have to join it while the stopped bot waits.
- **Use a device whose id differs from the bot's.** A defect in the Matrix
  library the bot uses confuses a device of yours with the bot's own
  device when both have the same id. `-verify` refuses, and sends
  nothing, when one of your devices does. Device ids are random, so this
  is rare. Sign out that device, or verify from another one.

Then, with the bot stopped, in the service's own context:

```sh
systemctl stop airut-example
systemd-run --pty --wait --collect \
  -p User=airut-example -p Group=airut-example -p StateDirectory=airut-example \
  -p LoadCredentialEncrypted=discord-token:/etc/credstore.encrypted/airut-example-discord-token \
  -p LoadCredentialEncrypted=matrix-password:/etc/credstore.encrypted/airut-example-matrix-password \
  -p LoadCredentialEncrypted=proxy-api-key:/etc/credstore.encrypted/airut-example-proxy-api-key \
  /usr/local/lib/airut-example/airut-example -config /etc/airut-example/airut.toml -verify @you:example.org
systemctl start airut-example
```

1. Element shows a verification request from the bot. Accept it, and
   choose to compare emoji.
2. The terminal prints seven emoji with their names. Compare them with
   the ones Element shows, in order.
3. If they are the same, type `yes` on the terminal and confirm in
   Element. If they differ, answer anything else and cancel in Element.
   Nothing is signed, and someone may be interfering with the exchange.

On success the terminal prints your master key, which the bot has now
signed, and Element shows the bot as verified. If Element never shows the
request, the verification ends after ten minutes. The likely cause is
the direct chat: check that it is encrypted and that you are in it.

## Cross-sign sibling bots

Bots of one owner can sign each other's Matrix identities, so each trusts
the others' devices. Cross-sign each bot's device first, as above. Then:

1. Make the coordination room in your client: invite-only, encrypted, with
   you and every sibling invited. The bot joins only if its invite rules
   accept your invite.
2. Make the fleet secret once, and encrypt it on each sibling's host:

   ```sh
   openssl rand -base64 48 \
     | systemd-creds encrypt --name=matrix-fleet-secret - /etc/credstore.encrypted/airut-example-matrix-fleet-secret
   ```

   Carry it to the other hosts the way you carry other secrets, never in
   chat or a ticket.
3. Uncomment the `[matrix.siblings]` table in each bot's configuration,
   with `secret`, and the unit's `matrix-fleet-secret` line. Restart.

Each bot posts a proof in the room at start and signs the siblings that
answer. To rotate the secret, encrypt the old one as
`matrix-fleet-secret-previous` beside the new one, uncomment
`previous_secret` and its unit line, and restart each bot in turn. Remove
both once every sibling has the new secret.

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
