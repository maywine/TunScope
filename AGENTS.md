# Repository Constraints

- Windows Store/MSIX applications must be selected by `PackageFamilyName`, not by a versioned `WindowsApps` path or a broad path wildcard.
- Windows per-application routing must keep absolute executable-path compatibility and apply both path and package-family matches to descendant processes.
- Releases must include Windows x64, macOS Apple Silicon, and macOS Intel archives with SHA-256 checksums, built from the same version tag.

# Repository Guidance

- Keep durable repository design constraints in this root `AGENTS.md`; do not recreate `CLAUDE.md` or store temporary plans and progress here.
- Keep sensitive information out of project documentation. Use abstract descriptions, environment variable names, or clearly fictional placeholders.
- Run builds, tests, and validation locally unless the user explicitly authorizes remote execution for the current task.
