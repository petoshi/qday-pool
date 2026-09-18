![QDAY](internal/web/site/assets/logo.webp)

# QDAY POOL

## POINT THE HASH HERE.

A public, registration-free PPLNS pool for QDAY. The node fills each template
with valid mempool transactions up to the consensus block-weight limit and
adds their fees to the payout. SiaMining hardware does the BLAKE2b-256 work.
The pool counts exact work and pays the QDAY address used as the Stratum
username.

No account. No email. No balance trapped behind a withdrawal button.

```text
QDAY v1.0.0+ node
        ↓ transaction-aware templates
qday-pool
        ↓ SiaMining Stratum · vardiff · independent work
GPU / ASIC miners
        ↓ shares and winning nonces
PPLNS accounting → 60-block maturity → automatic QDAY payout
```

The separate [`qday-stratum`](https://github.com/petoshi/qday-stratum) remains
the local solo bridge. This repository is the public pool: multiple miners,
share accounting, maturity, payouts and a public dashboard.

## Connect

The SiaMining-compatible format begins at block **9,100**. Run qday-pool
`v0.2.0` or newer with QDAY Node `v1.0.0` or newer. The pool never serves the
legacy mining format.

Use the receive address from your QDAY wallet as the username. Add `.worker`
only when you want a name for that machine.

### GPU

Download [QDAY gominer](https://github.com/petoshi/qday-gominer/releases/latest)
for Windows x86-64, Linux x86-64 or Linux ARM64. Install the OpenCL driver
supplied by the GPU vendor, then replace `YOUR_QDAY_ADDRESS`:

```sh
qday-gominer \
  -url stratum+tcp://pool.pqday.com:3333 \
  -user YOUR_QDAY_ADDRESS.gpu1
```

### Sia ASIC

The mining software is already installed in the ASIC. Open its pool settings
and enter:

```text
URL:      stratum+tcp://pool.pqday.com:3333
Username: YOUR_QDAY_ADDRESS.asic1
Password: x
```

Some firmware wants `pool.pqday.com` and port `3333` in separate fields. Use
`x` when it requires a password. Advanced miners may request a starting share
difficulty with a password such as `d=0.01`; VarDiff adjusts it afterward. The
worker label may contain letters, numbers, `_` and `-`; it is optional.

The exact login, job and submission fields are documented in
[Mining protocol](docs/protocol.md).

## Pool rules

- **Method:** PPLNS, `2N`. `N` is the expected work for one current QDAY block.
- **Pool reserve:** 1% of every complete block payout, including transaction
  fees. It pays the on-chain fees for automatic payout batches.
- **Miner credit:** 99%, divided by exact accepted work in the PPLNS window.
- **Maturity:** 60 blocks. Orphaned blocks create no payable balance.
- **Minimum payout:** 1 QDAY under the denomination active when the batch is
  prepared.
- **Payout fee:** paid from the pool reserve, never removed from a displayed
  miner balance.
- **Share difficulty:** VarDiff targets one share every 15 seconds per worker using stable 90 second retarget windows and bounded adjustments.

Every amount is stored as an atomic integer. When PQ Day changes the displayed
unit, the accounting does not guess, round or reinterpret old balances.
[Accounting](docs/accounting.md) specifies the calculation.

## Run an operator instance

The pool needs QDAY Node `v1.0.0` or newer, its own QDAY wallet and a local API
token. The node API and wallet password stay on the same server. Miners receive
only the public Stratum endpoint.

```sh
go test ./...
go build -trimpath -o qday-pool ./cmd/qday-pool

./qday-pool \
  -node http://127.0.0.1:19770 \
  -token-file /var/lib/qday-pool-node/api.token \
  -wallet-password-file /etc/qday-pool/wallet-password \
  -data /var/lib/qday-pool \
  -stratum :3333 \
  -http 127.0.0.1:8080 \
  -public-stratum stratum+tcp://pool.pqday.com:3333 \
  -pool-fee 1
```

The QDAY API must remain on loopback. The dashboard is read-only. Put its HTTP
listener behind HTTPS; expose Stratum directly on TCP 3333.
[Operations](docs/operations.md) covers the complete VPS layout, firewall,
backups and recovery.

## What the node adds

QDAY `v1.0.0` gives a pool the authenticated operations and compact work
required for standard SiaMining hardware:

- `getblocktemplate` accepts a pool-selected `worknonce`, producing independent
  commitments for independent miners;
- the final 33-byte mining-work transaction uses the standard 4-byte server and
  4-byte miner extranonces;
- `blockstatus` reports selected-chain and maturity state for a found block;
- `pool/payout` signs an exact multi-recipient batch with a retry-safe request
  ID and persists it through restarts and reorganizations.

The compact final marker is a consensus rule from block 9,100. The pool and
QDAY gominer accept only that format. Genesis, P2P identity, balances and
ordinary signed transactions stay unchanged.

## Links

- [Pool](https://pool.pqday.com)
- [QDAY](https://pqday.com)
- [Explorer](https://explorer.pqday.com)
- [QDAY source](https://github.com/petoshi/qday)
- [Solo Stratum bridge](https://github.com/petoshi/qday-stratum)
- [petoshi](https://x.com/_petoshi)

## License

MIT. See [LICENSE](LICENSE).
