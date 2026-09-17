# Pool accounting

QDAY Pool uses pay-per-last-N-shares accounting. It stores integers throughout
the calculation. Floating-point Stratum difficulty is never used to divide a
block reward.

## Accepted work

For a share target `T`, credited work is:

```text
floor(2^256 / (T + 1))
```

The server reconstructs the complete 80-byte header and verifies its
BLAKE2b-256 hash before recording the share. A job belongs to one connection,
one QDAY payout address, one worker label, one parent and one pool-selected
work nonce. Unknown, stale, duplicate and low-difficulty shares receive no
credit.

VarDiff changes only how often a miner submits shares. A share at twice the
difficulty carries twice the expected work.

## The `2N` window

`N` is the expected hashes represented by the network target of the found
block. The default window is `2N`.

When a worker finds a network block, the pool records that winning share and
freezes the window before submitting the complete block to QDAY. It reads
accepted shares newest first until their total reaches `2N`. If the oldest
selected share would cross the boundary, only the work needed to reach the
boundary is counted. This makes the window exactly `2N` when enough history
exists. A young pool uses all work available so far.

The winning share's database ID is the cutoff. A share arriving later cannot
enter the frozen allocation even if both shares have the same timestamp. If
submission times out after the node accepted the block, the allocation remains
pending until `blockstatus` resolves it.

The database retains at least 100 current PPLNS windows of accepted work. Old
share rows outside that reserve are deleted; frozen block allocations and
payout records remain.

## Block division

The distributable value starts with the complete on-chain miner payout:

```text
8 QDAY block reward + every transaction fee in the block
```

The default pool reserve is 1%. With basis-point fee `F`, the pool computes:

```text
miner credit = floor(block payout × (10,000 - F) / 10,000)
pool reserve = block payout - miner credit
```

Miner credit is divided by selected work. Integer quotients are assigned first.
Remaining atomic units go to the largest fractional remainders; the canonical
QDAY address breaks an exact tie. The allocations always sum to miner credit.

The reserve stays in the pool wallet. It pays the network fees for payout
batches and the server's operating cost. A payout does not subtract another
fee from the miner's credited balance.

## Maturity, reorganizations and payouts

A found block begins as pending. The pool asks its QDAY node whether the block
is known and remains on the selected chain. At height `block + 60`, its frozen
allocations become mature. If the block leaves the selected chain, they never
become payable.

When a mature unpaid address reaches the default 1 QDAY threshold, it enters a
batch of at most 100 recipients. The pool reserves the complete credited
amount, assigns a random request ID and sends exact atomic outputs to the local
QDAY wallet. The request ID is idempotent: retrying identical contents returns
the original transaction, while reusing it for different contents is rejected.

The QDAY node persists the signed batch before relaying it. It updates UTXO
proofs after new blocks, survives restarts and resumes after a reorganization.
The pool keeps retrying until the node reports six-block finality, then marks
the batch confirmed.

## PQ Day denomination

Before PQ Day, one displayed QDAY is `10^24` atomic units. From the PQ Day
block onward it is `10^18`. Existing atomic allocations do not change. The
dashboard formats them using the current unit, so the same stored balance then
displays one million times as many QDAY.

The minimum payout and new batch fee are configured in displayed QDAY and
converted with the node's current unit when a batch is prepared. The batch
stores exact atomic values after that point.
