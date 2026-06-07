# Long Short Harvest

A long-short equity strategy that harvests alpha from distorted market conditions by combining a regime-driven concentrated long sleeve with a trend-exhaustion short sleeve. The two sleeves operate independently within a single margin-aware framework.

## Long sleeve

Universe: the four largest U.S. equities by market capitalization, recomputed at the start of each month. Recomputation requires the equity to satisfy:

- Market capitalization above $1B
- At least 252 trading days (one trading year) of contiguous price history (used as a proxy for IPO age)
- Prior close above $5
- Trailing dollar volume above $20M

Sleeve allocation is decided daily from a regime model on SPY and VIX:

| Regime trigger | Equity weight | Gold (GLD) weight |
|---|---|---|
| Panic (`VIX > 80th-pct of trailing 100d` and `SPY 5d return < -3%`) | 0.85 (1.00 if RF bullish) | residual to long-gross cap |
| Melt-up (`VIX < 13` and `SPY > 1.05 * SMA50`) | 0.40 of long-gross | 0.40 of long-gross |
| Calm vol (`20 < VIX < SMA20`) | 0.70 (0.85 if RF bullish) | residual to long-gross cap |
| Vol spike (`VIX > 1.20 * SMA20`) | 0.00 | 0.50 of long-gross |
| Default trend on (`SPY > SMA200`) | 0.70 (0.90 if RF bullish) | residual to long-gross cap |
| Default trend off | 0.30 of long-gross | 0.50 of long-gross |

A random-forest classifier (100 trees, depth 5) is retrained at the start of each month on 11 VIX/SPY-derived features against the label `SPY 21d forward return > 2%`. It outputs a binary "bullish" flag when its predicted probability exceeds 0.6; the flag biases the regime toward higher equity weight and overweights the largest of the top-4 by `ml-tilt` (default 0.25).

Per-name long positions track a 3-stage trailing stop: at -9.5% drawdown the position is trimmed to two-thirds of its target weight, at a further -7% it is trimmed to one-third, and at a further -4.85% it is liquidated.

## Short sleeve

Universe: the top 150 liquid U.S. equities by trailing dollar volume satisfying the same fundamental gates as the long sleeve, recomputed at the start of each month.

Every Monday a composite score is computed for each candidate:

```
score = mean(H_n  for n in {10,10,40,60,90,100}) + 0.02 * max(0, agree - 3)
```

`H_n` is a Hurst-like persistence measure based on the ratio of the n-day price range to a constant-multiple of the n-day ATR. `agree` is the number of `H_n` values exceeding 0.6. Candidates must additionally satisfy:

- Extension: `(close - SMA195) > ext_k * ATR20` (default ext_k = 2.0)
- Momentum: `(close - close[-5]) > mom_k * ATR20` (default mom_k = 1.75)

The top-N candidates by score above `score-threshold` (default 0.85) are shorted, equally weighted to a combined gross of `short-gross` (default 0.6).

A short position is covered immediately if `(price - entry) >= stop-atr * ATR_at_entry` (default 2.0).

## Combined gross

By construction the strategy is up to ~1.5x gross (0.9 long + 0.6 short) and is intended to be deployed inside a Reg-T or portfolio-margin account that can support the leverage.

## Schedule

The strategy runs `@daily`. Internal date gates trigger:

- Daily: long signal evaluation, long trailing-stop risk check, short stop check
- Mondays: short rebalance
- First trading day of each month: random-forest retrain and universe refresh

## Parameters

See `--help` for the complete parameter table.
