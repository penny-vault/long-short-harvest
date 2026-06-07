# Long Short Harvest (LSH)

This is a long-short equity strategy designed to harvest alpha from distorted market conditions. The long sleeve concentrates exposure in a small set of the largest U.S. equities, dynamically adjusting position sizes based on market regime signals. On the short side, the algorithm scans a broad, liquid equity universe for extended, trend-exhaustion candidates using multi-horizon Hurst-style measures, ATR-scaled extension filters, and momentum confirmation, with disciplined ATR-based stop exits. Together, the two sleeves operate independently but within a unified margin-aware framework, allowing the strategy to adapt across market environments while maintaining tight control over leverage, drawdowns, and execution risk.

See [`lsh/README.md`](lsh/README.md) for the full mechanical description embedded in the strategy binary.

## Build

```sh
make build
```

## Test

```sh
make test
```
