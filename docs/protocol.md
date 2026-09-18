# Mining protocol

The public endpoint follows the
[SiaMining Stratum specification](https://github.com/SiaMining/Stratum/blob/master/Stratum.md)
over newline-delimited JSON-RPC.

## Login

Connect to `stratum+tcp://pool.pqday.com:3333`, subscribe, then authorize with:

```text
YOUR_QDAY_ADDRESS.rig-name
```

The first 64 characters must be a canonical QDAY address. The optional worker
label follows a dot and may contain letters, digits, `_` and `-`. The password
is ignored unless it contains a requested starting difficulty such as
`d=35000`. Requests outside the pool's configured range are rejected.

The subscription response assigns a four-byte first extranonce and reports a
four-byte second extranonce. Together they fill the nonce in QDAY's compact
final mining-work transaction. Each connection receives a different first
extranonce.

## Jobs

Before each job the pool sends `mining.set_difficulty`, followed by:

```json
{
  "id":null,
  "method":"mining.notify",
  "params":[
    "job-id",
    "<32-byte parent ID>",
    "<23-byte mining transaction prefix>",
    "<2-byte mining transaction suffix>",
    ["<left Merkle root>"],
    "",
    "<compact target>",
    "<8-byte little-endian Unix time>",
    true
  ]
}
```

The work header remains the standard 80-byte Sia layout:

```text
parentID[32] || nonceLE64 || timestampLE64 || commitment[32]
```

Hash it with BLAKE2b-256. The pool verifies every share against the exact share
target and verifies winning hashes again against QDAY's exact network target.
VarDiff aims for one accepted share every 15 seconds per connection. Normal
retargeting measures a 90 second window and limits each adjustment to half or
double the previous difficulty. A sustained startup burst can rise fourfold so
ASICs leave the low initial difficulty quickly. An idle connection steps down
gradually. A single lucky or delayed share cannot send difficulty across the
full range.

The compact transaction is 23 fixed bytes, the four-byte pool extranonce, the
four-byte miner extranonce and two fixed trailing bytes. It is exactly 33 bytes
after reconstruction. The payout marker and selected mempool transactions are
committed on its left side and remain in a winning block.

The pool opens when it can build block 9,100. It accepts only the QDAY v1
four-byte pool extranonce plus four-byte miner extranonce layout. Before that
height, subscriptions return an activation error and no mining job is issued.

## Submission

Submit the five SiaMining fields:

```json
{
  "id":3,
  "method":"mining.submit",
  "params":[
    "YOUR_QDAY_ADDRESS.rig-name",
    "job-id",
    "<4-byte miner extranonce>",
    "<8-byte little-endian Unix time>",
    "<8-byte little-endian nonce>"
  ]
}
```

Unknown, stale, duplicate and low-difficulty shares receive no credit. Jobs
belong to the connection that received them. A winning share freezes its PPLNS
window in SQLite before the complete block is submitted to the local QDAY
node.

The canonical field order is defined by the
[SiaMining Stratum specification](https://github.com/SiaMining/Stratum/blob/master/Stratum.md).
