# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Cross-platform release pipeline: GoReleaser builds standalone binaries for
  Windows/macOS/Linux on x64 and ARM64, with SHA-256 `checksums.txt`, published
  via a tag-triggered workflow. Adds an `agentgpu version` / `--version` command
  and a per-PR cross-compile dry run. See [docs/releasing.md](docs/releasing.md).
- Repository hardening and community-health files: OpenSSF Scorecard workflow,
  Conventional Commits PR-title check, stale bot, `.gitignore`, `.editorconfig`,
  `.gitattributes`, `SUPPORT.md`, and automated release-notes config.

[Unreleased]: https://github.com/jaypetez/agent-gpu/commits/main
