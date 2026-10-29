# Changelog

## [1.10.1](https://github.com/dehydr8/cproxy/compare/v1.10.0...v1.10.1) (2026-10-29)


### Bug Fixes

* drop the Amp orb integration now that Amp runs the Claude Agent SDK ([#34](https://github.com/dehydr8/cproxy/issues/34)) ([e42e04f](https://github.com/dehydr8/cproxy/commit/e42e04fc26a56684a4bcdef946dd38bdf80422d0))
* poll the usage API within its per-account limit ([#36](https://github.com/dehydr8/cproxy/issues/36)) ([dc6697c](https://github.com/dehydr8/cproxy/commit/dc6697c9841a4e7a7f6eee901a690d5af3495bd7))

## [1.10.0](https://github.com/dehydr8/cproxy/compare/v1.9.0...v1.10.0) (2026-10-28)


### Features

* **launch:** add opt-in entrypoint relabeling via CPROXY_ENTRYPOINT ([#33](https://github.com/dehydr8/cproxy/issues/33)) ([de54322](https://github.com/dehydr8/cproxy/commit/de54322ec47178e6c5f80c3f0922a68e83772721))


### Bug Fixes

* keep Claude Code's 1h prompt cache behind cproxy ([#31](https://github.com/dehydr8/cproxy/issues/31)) ([6e18618](https://github.com/dehydr8/cproxy/commit/6e18618321ebc3951392b8a11bc5ec7fd811707c))

## [1.9.0](https://github.com/dehydr8/cproxy/compare/v1.8.0...v1.9.0) (2026-10-24)


### Features

* `cproxy daemon status/start/stop/restart/logs` control the local daemon ([#29](https://github.com/dehydr8/cproxy/issues/29)) ([ed3f044](https://github.com/dehydr8/cproxy/commit/ed3f04476c5440846f62c08e68a80aefba565966))

## [1.8.0](https://github.com/dehydr8/cproxy/compare/v1.7.1...v1.8.0) (2026-10-24)


### Features

* `cproxy update` updates cproxy to the latest release ([efa1fe9](https://github.com/dehydr8/cproxy/commit/efa1fe983850028af04bffc19d44b0a4dccdc38f))


### Bug Fixes

* usage keeps its last reading through failures and refreshes stale ones on view ([efa1fe9](https://github.com/dehydr8/cproxy/commit/efa1fe983850028af04bffc19d44b0a4dccdc38f))

## [1.7.1](https://github.com/dehydr8/cproxy/compare/v1.7.0...v1.7.1) (2026-10-18)


### Bug Fixes

* `cproxy usage` starts the daemon instead of asking for a token ([95c6d1d](https://github.com/dehydr8/cproxy/commit/95c6d1dbca6d791831a9afbb3c27da3f99dfd9f5))

## [1.7.0](https://github.com/dehydr8/cproxy/compare/v1.6.0...v1.7.0) (2026-10-18)


### Features

* Haiku probe for usage of tokens without user:profile ([#21](https://github.com/dehydr8/cproxy/issues/21)) ([31ab2d4](https://github.com/dehydr8/cproxy/commit/31ab2d4954bbfd4fc2dde8b276839d242c06248f))


### Bug Fixes

* a probed window that rolled over shows empty and is re-read ([8445998](https://github.com/dehydr8/cproxy/commit/84459982cfc912dbc1969587c0f204e01b3739a1))

## [1.6.0](https://github.com/dehydr8/cproxy/compare/v1.5.0...v1.6.0) (2026-10-18)


### Features

* one per-user local daemon for every claude; nicer usage everywhere ([#16](https://github.com/dehydr8/cproxy/issues/16)) ([487df36](https://github.com/dehydr8/cproxy/commit/487df36781e335e599b839a3a4c271a1cea71bd5))


### Bug Fixes

* quiet Claude Code's connectivity probe; say what a config sync changed ([#19](https://github.com/dehydr8/cproxy/issues/19)) ([206dd8a](https://github.com/dehydr8/cproxy/commit/206dd8ac515f5dcaa6c2c9c6c13858519dd98cb4))

## [1.5.0](https://github.com/dehydr8/cproxy/compare/v1.4.0...v1.5.0) (2026-10-18)


### Features

* move sticky clients before their account runs out ([#14](https://github.com/dehydr8/cproxy/issues/14)) ([e3f1f08](https://github.com/dehydr8/cproxy/commit/e3f1f08afe6ad1998eb514136bc5e678d84213ae))

## [1.4.0](https://github.com/dehydr8/cproxy/compare/v1.3.0...v1.4.0) (2026-10-18)


### Features

* live requests, pinned clients and token counts on the dashboard ([#12](https://github.com/dehydr8/cproxy/issues/12)) ([2a98689](https://github.com/dehydr8/cproxy/commit/2a98689d806700ccbc1e906b2a36d0fc19e43a2d))

## [1.3.0](https://github.com/dehydr8/cproxy/compare/v1.2.0...v1.3.0) (2026-10-18)


### Features

* the orb dashboard portal opens without asking for a token ([#9](https://github.com/dehydr8/cproxy/issues/9)) ([0b1fbc8](https://github.com/dehydr8/cproxy/commit/0b1fbc88c5b607d9bb77231c6c6b04677ffe39d4))


### Bug Fixes

* steady dashboard refresh, no scrollbars ([#11](https://github.com/dehydr8/cproxy/issues/11)) ([dd91c34](https://github.com/dehydr8/cproxy/commit/dd91c34061e6e574d2476a3f253a3c6ca7efc8ad))

## [1.2.0](https://github.com/dehydr8/cproxy/compare/v1.1.0...v1.2.0) (2026-10-18)


### Features

* Fly.io template and private dashboard listener for remote serve ([#6](https://github.com/dehydr8/cproxy/issues/6)) ([ca85b38](https://github.com/dehydr8/cproxy/commit/ca85b388655a6b47fa7add885915197f0a71fce5))


### Bug Fixes

* a bare 429 no longer benches accounts; subscription windows, plans, per-client env tokens ([#8](https://github.com/dehydr8/cproxy/issues/8)) ([5cecb94](https://github.com/dehydr8/cproxy/commit/5cecb94fd9cc563ecc1c745549943815fd132b77))

## [1.1.0](https://github.com/dehydr8/cproxy/compare/v1.0.0...v1.1.0) (2026-10-18)


### Features

* per-account env vars, CPROXY_POOL, account emails, new dashboard ([#5](https://github.com/dehydr8/cproxy/issues/5)) ([06f9313](https://github.com/dehydr8/cproxy/commit/06f93133da67b9bf3666c4fcc4dcd45c48a47d43))
* wrapper creates its own local client token ([#3](https://github.com/dehydr8/cproxy/issues/3)) ([d7bb2f0](https://github.com/dehydr8/cproxy/commit/d7bb2f0636c4d4e4ceb5b0fb39cc9085b752e087))

## 1.0.0 (2026-10-18)


### Features

* claude wrapper shim, usage CLI, release CI ([1d9eb59](https://github.com/dehydr8/cproxy/commit/1d9eb5924e2a908a3c8e09b812844ceca5fef6a3))
* real usage API as the preferred usage source ([44414c0](https://github.com/dehydr8/cproxy/commit/44414c04e43e2c49a3d4d293879710c79742ab51))


### Bug Fixes

* failover, auth and wrapper hardening from review ([#2](https://github.com/dehydr8/cproxy/issues/2)) ([a207577](https://github.com/dehydr8/cproxy/commit/a207577da895df0dc7174487c1a6fbfe108eb7b5))
