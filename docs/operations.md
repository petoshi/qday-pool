# Pool operations

This layout runs one validating QDAY node, one pool process and Caddy on a
Linux VPS. Two CPU cores, 4 GB RAM, 40–60 GB SSD and 2 GB swap are enough for
the initial network. The server does not mine.

## Network ports

| Port | Exposure | Purpose |
| --- | --- | --- |
| `22/TCP` | operator IP where possible | SSH |
| `80/TCP`, `443/TCP` | public | dashboard and HTTPS |
| `3333/TCP` | public | SiaMining Stratum |
| `19771/TCP` | public | QDAY P2P |
| `19770/TCP` | loopback only | authenticated QDAY wallet API |
| `8080/TCP` | loopback only | pool dashboard origin |

Do not proxy Stratum through ordinary HTTP. Do not expose port 19770.

## Files and users

Create one unprivileged `qday` service account. Install binaries in
`/usr/local/bin`, the fixed mainnet manifest at
`/etc/qday-pool/qday-mainnet.json`, and use:

```text
/var/lib/qday-pool-node/       QDAY chain, wallet and api.token
/var/lib/qday-pool/            pool.sqlite3 and its WAL
/etc/qday-pool/wallet-password root:qday, mode 0640
```

The wallet password file contains the local encryption password on one line.
The 24-word seed phrase does not belong on the server after wallet creation.
Back it up offline.

## Create the pool wallet

Start the QDAY node once. Read its `api.token` as the `qday` user and call the
loopback API:

```sh
TOKEN=$(sudo -u qday cat /var/lib/qday-pool-node/api.token)
curl --fail-with-body \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"password":"REPLACE_WITH_A_LONG_PASSWORD","phrase":""}' \
  http://127.0.0.1:19770/api/create
```

The response contains the only seed phrase for the pool wallet. Save it before
continuing. Put the password, not the seed phrase, in
`/etc/qday-pool/wallet-password`.

## Services

Copy [qday-pool-node.service](../deploy/qday-pool-node.service) and
[qday-pool.service](../deploy/qday-pool.service) to `/etc/systemd/system`.
Review paths, then run:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now qday-pool-node
sudo systemctl enable --now qday-pool
sudo systemctl status qday-pool-node qday-pool
```

The pool refuses to start with another network, a missing wallet, a locked
wallet or invalid amount settings. During a peer outage the QDAY node refuses
new templates, so the pool stops issuing work instead of building an isolated
chain.

## HTTPS

Caddy can publish the read-only dashboard:

```caddyfile
pool.pqday.com {
    encode zstd gzip
    reverse_proxy 127.0.0.1:8080
    header Strict-Transport-Security "max-age=31536000; includeSubDomains"
}
```

Keep any reverse-proxy request limits generous enough for the dashboard and
public account lookups.

## Backups

Back up these items separately:

1. the pool wallet seed phrase, offline;
2. `/var/lib/qday-pool/pool.sqlite3` with a SQLite-aware snapshot;
3. service configuration and the encrypted wallet data.

The chain database can be downloaded again. Pool shares, frozen allocations
and payout request IDs cannot. Stop `qday-pool`, copy the SQLite database and
its WAL files together, then restart it. Test restoration before the database
becomes interesting.

## Monitoring

Use `GET /healthz` for process health and `GET /readyz` for synchronized pool
readiness. Alert on:

- readiness remaining false;
- no QDAY peers;
- repeated template or payout errors;
- disk growth or free space below 20%;
- `solved block was not submitted because accounting failed`.

The pool freezes the PPLNS allocation before sending a solved block to QDAY.
This ordering prevents an accepted block from outrunning durable accounting.
The message means the block was withheld because SQLite could not record that
allocation; repair the database before accepting more miners.
