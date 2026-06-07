# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-05-06

### Added
- Initial release of Long Short Harvest strategy
- Long sleeve: regime-driven allocation across the top-4 US equities by market cap, gold (GLD), and cash, with a random-forest probability tilt and 3-stage trailing stop
- Short sleeve: weekly Hurst-like trend-exhaustion screen on liquid US equities with ATR-scaled extension and momentum filters and ATR-at-entry stops

[0.1.0]: https://github.com/penny-vault/long-short-harvest/releases/tag/v0.1.0
