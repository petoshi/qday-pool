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

The subscription response uses an empty first extranonce and a zero-byte
second extranonce. QDAY transactions are already signed, so both extranonces
remain empty in every submission.

## Jobs

Before each job the pool sends `mining.set_difficulty`, followed by:

```json
{
  "id":null,
  "method":"mining.notify",
  "params":[
    "job-id",
    "<32-byte parent ID>",
    "<serialized rightmost QDAY transaction>",
    "",
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
VarDiff aims for one accepted share every 15 seconds per connection.

## Submission

Submit the five SiaMining fields:

```json
{
  "id":3,
  "method":"mining.submit",
  "params":[
    "YOUR_QDAY_ADDRESS.rig-name",
    "job-id",
    "",
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
